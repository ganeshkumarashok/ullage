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

func init() { Register(IdlePod{}) }

// idlePowerRatio is the share of board TDP below which a device is considered
// electrically idle. An A100 doing nothing draws roughly 50-60 W of its 400 W
// board power; anything running work sits far above this.
const idlePowerRatio = 0.20

// minCompleteness is the sample coverage below which no claim is made. A
// dcgm-exporter restart or a scrape gap produces *absent* samples, not zero
// samples, and absent is not idle.
const minCompleteness = 0.80

// IdlePod finds pods holding accelerators on which no work ran.
type IdlePod struct{}

func (IdlePod) Describe() Descriptor {
	return Descriptor{
		ID:       api.CheckIdlePod,
		Title:    "Idle pod",
		Question: "Is anything holding an accelerator without using it?",
		Claim: "Every utilization sample for these devices read exactly zero for the whole period. " +
			"This is not a claim that the workload is unimportant, and not an estimate of how " +
			"efficiently it ran — GPU utilization is a poor measure of that. It is the one thing " +
			"the metric can prove: no kernel was resident at any sampled moment.",
		Risk: "These pods are Running, not Completed. State held only in the container filesystem " +
			"will be lost. ullage measures idleness, not intent, and cannot distinguish an " +
			"abandoned session from capacity held warm on purpose.",
		Prevention: "Interactive GPU sessions are the most common source. A TTL controller, an " +
			"activity-based idle culler, or a scheduled scale-to-zero for notebook workloads " +
			"removes the class of problem rather than this instance of it.",
		Docs: docsBase + api.CheckIdlePod,
	}
}

// Applicable excludes shared devices. On a time-sliced or MIG node the
// device-level metric reflects every co-tenant, so an idle pod sharing a device
// with a busy one is invisible and a busy pod sharing with an idle one looks
// idle. Neither error is acceptable, so the check declines rather than guesses.
func (IdlePod) Applicable(d inventory.Device) bool {
	return d.Analyzable && d.Holder != nil
}

func (c IdlePod) Run(ctx context.Context, cl *inventory.Cluster, p Params) ([]RawFinding, error) {
	if !cl.MetricsAttributed {
		return nil, nil
	}

	type group struct {
		pods    []inventory.PodRef
		devices []inventory.Device
		fallow  time.Duration
	}
	groups := map[string]*group{}
	order := []string{}

	for _, pod := range cl.Pods {
		if pod.Phase != "Running" || pod.Pending || pod.Accelerators == 0 {
			continue
		}
		devs := cl.DevicesOf(pod.Ref)
		if len(devs) == 0 {
			continue
		}

		// The claim is about the trailing run of zeroes, not about the whole
		// window. A pod that worked nine days ago and has read exactly zero
		// since is idle now, and a rule that demanded zero across the entire
		// window would silently miss it — the longer the window, the more it
		// would miss, which is the wrong way round.
		//
		// What is never relaxed is the zero itself. No average is taken and no
		// threshold is applied to the utilization value, because "low" is a
		// judgement about someone else's workload and "zero" is not.
		// Coverage is judged over the pod's own observable lifetime, not over
		// the scan window. Against the window, a pod that has existed for two
		// days of a fortnight tops out near 14% coverage however completely it
		// was watched, so a fixed threshold would discard every young pod —
		// and "someone started an expensive pod last week and forgot it" is
		// both the most actionable finding this tool has and the first thing
		// an evaluator will test.
		span := cl.Window
		if pod.StartTime != nil {
			if lived := cl.Now.Sub(*pod.StartTime); lived < span {
				span = lived
			}
		}

		idle, fallow, completeness := true, cl.Window, 1.0
		for _, d := range devs {
			if !c.Applicable(d) {
				idle = false
				break
			}
			since, ok := d.Util.FallowFor(cl.Now)
			if !ok {
				idle = false
				break
			}
			if since < fallow {
				fallow = since
			}
			// The holder's own series, not the physical device's summed
			// coverage: a GPU recycled from a previous job carries that job's
			// samples, and letting them count here would have this pod borrow
			// coverage it never had.
			if cov := d.Util.CoverageOver(span, cl.Step); cov < completeness {
				completeness = cov
			}
		}
		// A gap large enough to hide a working period disqualifies the finding
		// outright rather than merely lowering its confidence.
		// A pod cannot have been idle for longer than it has existed.
		if fallow > span {
			fallow = span
		}
		if !idle || completeness < minCompleteness || fallow < p.IdleThreshold {
			continue
		}

		// Grouping by root owner is what turns forty rows into one. Reporting
		// per-pod would bury the reader in a list whose every entry has the
		// same cause and the same fix.
		key := groupKeyFor(pod)
		g, ok := groups[key]
		if !ok {
			g = &group{}
			groups[key] = g
			order = append(order, key)
		}
		g.pods = append(g.pods, pod.Ref)
		g.devices = append(g.devices, devs...)
		if fallow > g.fallow {
			g.fallow = fallow
		}
	}

	sort.Strings(order)
	var out []RawFinding
	for _, key := range order {
		g := groups[key]
		sort.Slice(g.pods, func(i, j int) bool { return g.pods[i].Name < g.pods[j].Name })

		pod := podByRef(cl, g.pods[0])
		ev, conf := summariseIdle(g.devices, cl.Window, g.fallow)

		subject := Subject{
			Kind:      "workload",
			Namespace: pod.Ref.Namespace,
			Name:      subjectName(pod),
			Pods:      g.pods,
		}
		out = append(out, RawFinding{
			Check:      api.CheckIdlePod,
			Subject:    subject,
			Devices:    deviceIDs(g.devices),
			Fallow:     g.fallow,
			Confidence: conf,
			Evidence:   ev,
			Summary: fmt.Sprintf("%d accelerators held with no work for %s",
				len(g.devices), humanize.Duration(g.fallow)),
		})
	}
	return out, nil
}

// summariseIdle folds a group's devices into one evidence record and decides
// how much to trust it.
func summariseIdle(devices []inventory.Device, window, fallow time.Duration) (api.Evidence, string) {
	ev := api.Evidence{
		Window:             api.ISODuration(window),
		FallowDuration:     api.ISODuration(fallow),
		SampleCompleteness: 1,
	}
	powerSum, powerN := 0.0, 0
	tdp := 0.0
	for _, d := range devices {
		if d.Util.Max > ev.UtilizationMax {
			ev.UtilizationMax = d.Util.Max
		}
		if d.Util.Completeness < ev.SampleCompleteness {
			ev.SampleCompleteness = d.Util.Completeness
		}
		if d.Util.LastNonZero != nil && (ev.LastNonZeroUtilization == nil || d.Util.LastNonZero.After(*ev.LastNonZeroUtilization)) {
			ev.LastNonZeroUtilization = d.Util.LastNonZero
		}
		if len(ev.Sparkline) == 0 {
			ev.Sparkline = d.Util.Buckets
		}
		if d.Power.Samples > 0 {
			powerSum += d.Power.Mean
			powerN++
			if d.TDPWatts > tdp {
				tdp = d.TDPWatts
			}
		}
	}

	// Power draw is independent corroboration from a different sensor. It is
	// what separates "the utilization metric says zero" from "the device is
	// demonstrably doing nothing", and it catches a broken exporter reporting
	// zeroes for a device that is in fact busy.
	corroborated := false
	if powerN > 0 {
		ev.PowerDrawWatts = powerSum / float64(powerN)
		if tdp > 0 {
			ev.PowerDrawTDPRatio = ev.PowerDrawWatts / tdp
			corroborated = ev.PowerDrawTDPRatio < idlePowerRatio
			if !corroborated {
				ev.Notes = append(ev.Notes, fmt.Sprintf(
					"power draw is %.0f%% of TDP, higher than an idle device should draw",
					ev.PowerDrawTDPRatio*100))
			}
		}
	}

	// Zero across the entire window is a stronger statement than zero since a
	// point inside it, and only the former can be made without relying on the
	// resolution of the range query.
	allWindow := true
	for _, d := range devices {
		if !d.Util.ZeroThroughout {
			allWindow = false
			break
		}
	}

	switch {
	case ev.SampleCompleteness < 0.95:
		ev.Notes = append(ev.Notes, "sample coverage is incomplete over the window")
		return ev, api.EvidenceMedium
	case powerN == 0:
		ev.Notes = append(ev.Notes, "no power series available to corroborate the utilization reading")
		return ev, api.EvidenceMedium
	case !corroborated:
		return ev, api.EvidenceMedium
	case !allWindow && fallow < window/2:
		return ev, api.EvidenceMedium
	default:
		return ev, api.EvidenceHigh
	}
}

func deviceIDs(devices []inventory.Device) []string {
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		out = append(out, d.ID)
	}
	sort.Strings(out)
	return out
}

// groupKeyFor groups by root owner, so one Deployment is one finding no matter
// how many pods it has.
func groupKeyFor(p inventory.PodView) string {
	if p.Provenance.Controlled && p.Provenance.RootName != "" {
		return p.Ref.Namespace + "|" + p.Provenance.RootKind + "|" + p.Provenance.RootName
	}
	return p.Ref.Namespace + "|Pod|" + p.Ref.Name
}

func subjectName(p inventory.PodView) string {
	if p.Provenance.Controlled && p.Provenance.RootName != "" {
		return p.Provenance.RootName
	}
	return p.Ref.Name
}

func podByRef(cl *inventory.Cluster, ref inventory.PodRef) inventory.PodView {
	for _, p := range cl.Pods {
		if p.Ref.Namespace == ref.Namespace && p.Ref.Name == ref.Name {
			return p
		}
	}
	return inventory.PodView{Ref: ref}
}

const docsBase = "https://ullage.dev/checks/"

var _ = context.Background
