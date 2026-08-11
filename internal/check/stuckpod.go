package check

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ullage-project/ullage/internal/humanize"
	"github.com/ullage-project/ullage/internal/inventory"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

func init() { Register(StuckPod{}) }

// StuckPod finds pods holding allocated accelerators while wedged.
//
// This check does not consult metrics at all, and that is the point: a
// crash-looping pod holds its device whatever the utilization series says. It
// is also the check most likely to fire on a cluster with no history, because
// it needs only the current pod state.
type StuckPod struct{}

func (StuckPod) Describe() Descriptor {
	return Descriptor{
		ID:       api.CheckStuckPod,
		Title:    "Stuck pod",
		Question: "Is anything holding an accelerator it cannot use?",
		Claim: "These pods are scheduled and holding allocated accelerators, but their containers " +
			"are not running. The devices are unavailable to anything else for as long as this " +
			"continues. This is a state claim, not a utilization judgement.",
		Risk: "A crash loop is never intentional, but the fix belongs to whoever owns the workload. " +
			"The logs are the starting point, not a deletion.",
		Prevention: "Set a backoffLimit on Jobs and an appropriate restartPolicy, so a failing " +
			"workload eventually stops holding its device instead of crash-looping indefinitely.",
		Docs: docsURLFor(api.CheckStuckPod),
	}
}

// Applicable is true for any held device: a wedged pod occupies its device
// under every allocation model, including shared ones.
func (StuckPod) Applicable(d inventory.Device) bool { return d.Holder != nil }

func (StuckPod) Run(ctx context.Context, cl *inventory.Cluster, p Params) ([]RawFinding, error) {
	type group struct {
		pods     []inventory.PodRef
		devices  int
		held     time.Duration
		restarts int
		reason   string
		term     *inventory.Termination
	}
	groups := map[string]*group{}
	order := []string{}

	for _, pod := range cl.Pods {
		// A pod that was never scheduled holds nothing. Pending pods are the
		// victims of this waste, not its cause, and counting them here would
		// claim recoverable hours against devices that were never allocated.
		if pod.Pending || pod.Node == "" || pod.Accelerators == 0 {
			continue
		}
		if pod.WedgedReason == "" {
			continue
		}

		held := time.Duration(0)
		if pod.StartTime != nil {
			held = cl.Now.Sub(*pod.StartTime)
		}
		// A long init is not automatically stuck. Pulling a large image or
		// downloading model weights routinely takes hours, and the device is
		// legitimately reserved throughout.
		threshold := p.StuckThreshold
		if pod.Initialising {
			threshold = p.InitGrace
		}
		if held < threshold {
			continue
		}

		key := groupKeyFor(pod)
		g, ok := groups[key]
		if !ok {
			g = &group{}
			groups[key] = g
			order = append(order, key)
		}
		g.pods = append(g.pods, pod.Ref)
		g.devices += pod.Accelerators
		if held > g.held {
			g.held = held
		}
		if pod.Restarts > g.restarts {
			g.restarts = pod.Restarts
		}
		if g.reason == "" {
			g.reason, g.term = pod.WedgedReason, pod.Terminated
		}
	}

	sort.Strings(order)
	var out []RawFinding
	// A wedged pod is still Phase=Running, so it is counted here too; the
	// comparison below is against the group's own size for that reason. Any
	// surplus is a replica that is not stuck, and scaling the controller to
	// zero to fix a crash loop would stop it.
	running := runningAcceleratorPodsByOwner(cl)

	for _, key := range order {
		g := groups[key]
		sort.Slice(g.pods, func(i, j int) bool { return g.pods[i].Name < g.pods[j].Name })
		pod := podByRef(cl, g.pods[0])

		// Hours are capped at the window: claiming a pod wasted more time than
		// was examined would make the total exceed the hours paid for.
		held := g.held
		if held > cl.Window {
			held = cl.Window
		}

		notes := []string{"container state: " + g.reason}
		if g.restarts > 0 {
			notes = append(notes, fmt.Sprintf("%d restarts", g.restarts))
		}
		if g.term != nil {
			notes = append(notes, fmt.Sprintf("last exit %d (%s) %s ago",
				g.term.ExitCode, g.term.Reason, humanize.Duration(cl.Now.Sub(g.term.FinishedAt))))
		}

		out = append(out, RawFinding{
			Check: api.CheckStuckPod,
			Subject: Subject{
				Kind:      "workload",
				Namespace: pod.Ref.Namespace,
				Name:      subjectName(pod),
				Pods:      g.pods,
				// A controller with one broken replica usually has healthy
				// ones. Scaling it to zero to fix a crash loop would stop them.
				PartialOwner: len(g.pods) < running[key],
			},
			Fallow: held,
			// State is directly observed from the API server rather than
			// inferred from a sampled series, so there is nothing to be
			// uncertain about.
			Confidence: api.EvidenceHigh,
			Evidence: api.Evidence{
				Window:             api.ISODuration(cl.Window),
				FallowDuration:     api.ISODuration(held),
				SampleCompleteness: 1,
				Notes:              notes,
			},
			Summary: fmt.Sprintf("%d %s held for %s while %s",
				g.devices, humanize.Plural(g.devices, "accelerator"),
				humanize.Duration(held), g.reason),
		})
		// Device count travels through the subject's pods; record it so the
		// pipeline does not have to re-derive it for pods whose devices the
		// metrics never attributed.
		out[len(out)-1].Devices = syntheticDeviceIDs(cl, g.pods, g.devices)
	}
	return out, nil
}

// syntheticDeviceIDs returns the real device IDs where metrics attributed them,
// and otherwise placeholder IDs so the count is still right.
//
// A stuck pod frequently has no metrics at all — a container that never started
// never produced any — so requiring attributed devices here would silently drop
// the most clear-cut findings the tool has.
func syntheticDeviceIDs(cl *inventory.Cluster, pods []inventory.PodRef, want int) []string {
	var ids []string
	for _, ref := range pods {
		for _, d := range cl.DevicesOf(ref) {
			ids = append(ids, d.ID)
		}
	}
	for i := len(ids); i < want; i++ {
		ids = append(ids, fmt.Sprintf("unattributed/%s/%d", pods[0], i))
	}
	sort.Strings(ids)
	return ids
}

var _ = context.Background
