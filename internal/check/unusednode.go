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

func init() { Register(UnusedNode{}) }

// UnusedNode finds accelerator nodes nothing has been scheduled on, and — the
// part that matters — says why they are still there.
//
// "This node is empty" is visible in any dashboard. "These two pods with
// safe-to-evict: false are why the autoscaler cannot remove this node" is not
// visible anywhere, is causal rather than descriptive, and is safe to act on.
// The blocker diagnosis is the reason this check exists.
type UnusedNode struct{}

func (UnusedNode) Describe() Descriptor {
	return Descriptor{
		ID:       api.CheckUnusedNode,
		Title:    "Unused node",
		Question: "Is the cluster holding accelerator capacity nothing asked for?",
		Claim: "These nodes advertise accelerators and are Ready and schedulable, but no pod " +
			"holding an accelerator — by extended resource, MIG profile, time-sliced replica or " +
			"DRA claim — has been placed on them, and no accelerator on them has reported work " +
			"within the window. They are not draining, not cordoned, and past the " +
			"initialisation grace period.",
		Risk: "Removing capacity is a capacity decision, not only a cost one. Confirm the pool is " +
			"not reserved for a launch, a failover, or a periodic job before scaling it down.",
		Prevention: "Confirm the autoscaler's minimum size for this pool matches the floor you " +
			"actually want, and that scale-down is not blocked by pods with no eviction toleration.",
		Docs: docsBase + api.CheckUnusedNode,
	}
}

// Applicable is about devices; this check reasons about nodes, and every
// accelerator on an empty node counts regardless of allocation model.
func (UnusedNode) Applicable(d inventory.Device) bool { return true }

func (UnusedNode) Run(ctx context.Context, cl *inventory.Cluster, p Params) ([]RawFinding, error) {
	// Occupancy is asked in the broadest possible terms, because every way of
	// getting this wrong ends with the tool recommending the deletion of
	// hardware that is in use. A pod counts if it holds accelerator capacity of
	// any shape: whole devices, DRA claims, MIG profiles, or time-sliced
	// replicas.
	occupied := map[string]bool{}
	for _, pod := range cl.Pods {
		if pod.Node != "" && pod.Occupies() && !pod.Pending {
			occupied[pod.Node] = true
		}
	}

	// Second, independent line of defence: measurement. If any accelerator on a
	// node has ever reported non-zero utilization inside the window, work ran
	// there, whatever the Kubernetes object model said. This catches allocation
	// models nobody has taught the tool about yet — which is the failure mode
	// that keeps recurring, because the allocation model is the part of this
	// domain that changes fastest.
	lastWork := map[string]time.Duration{} // node -> time since work was last seen
	for _, d := range cl.Devices {
		if d.Util.Samples == 0 {
			continue
		}
		if d.Util.Max <= 0 {
			if _, seen := lastWork[d.Node]; !seen {
				lastWork[d.Node] = cl.Window
			}
			continue
		}
		since := cl.Window
		if idle, ok := d.Util.FallowFor(cl.Now); ok {
			since = idle
		} else {
			since = 0
		}
		if cur, seen := lastWork[d.Node]; !seen || since < cur {
			lastWork[d.Node] = since
		}
	}

	type poolAgg struct {
		nodes    []string
		devices  int
		oldest   time.Duration
		blockers []api.Blocker

		// measured records that at least one node's fallow duration came from
		// the utilization series rather than from node age alone.
		measured   bool
		unmeasured bool

		provider string
		model    string
		vendor   string
		alloc    string
		tdp      float64
	}
	pools := map[string]*poolAgg{}
	order := []string{}

	for _, node := range cl.Nodes {
		if node.Accelerators == 0 || occupied[node.Name] {
			continue
		}
		// Measured work overrides an empty pod list. A batch pool that is empty
		// at this instant but ran a job an hour ago is not idle capacity, and
		// calling it idle for the node's whole lifetime — which is what the age
		// alone would say — is an assertion the evidence contradicts.
		if since, measured := lastWork[node.Name]; measured && since < p.IdleThreshold {
			continue
		}
		// Each of these is a normal state that looks identical to waste from a
		// distance, and reporting any of them would be a false positive that
		// costs more trust than the finding is worth.
		switch {
		case !node.Ready, node.Unschedulable, node.Initialising, node.ScaleDownDisabled:
			continue
		case node.Age < p.IdleThreshold:
			continue
		}

		agg, ok := pools[node.Pool]
		if !ok {
			agg = &poolAgg{
				provider: node.Provider, model: node.Model,
				vendor: node.Vendor, alloc: node.Allocation, tdp: node.TDPWatts,
			}
			pools[node.Pool] = agg
			order = append(order, node.Pool)
		}
		agg.nodes = append(agg.nodes, node.Name)
		agg.devices += node.Accelerators

		// How long has this been fallow? The node's age is only an upper bound
		// — it says how long the node *could* have been empty, not how long it
		// was. Where the accelerators on it were measured, the trailing run of
		// zero utilization is the actual observation, and the shorter of the
		// two is the only number the evidence supports.
		//
		// Reporting the age alone would let an hour-old idle period be billed
		// as a fortnight of waste, which is exactly the kind of inflated number
		// that gets a tool disbelieved on the number the reader can check.
		empty := node.Age
		if since, measured := lastWork[node.Name]; measured && since < empty {
			empty = since
			agg.measured = true
		} else if !measured {
			agg.unmeasured = true
		}
		if empty > agg.oldest {
			agg.oldest = empty
		}

		for _, pod := range cl.PodsOnNode(node.Name) {
			// DaemonSet pods are never blockers, and the rule is restated here
			// rather than trusted from upstream because getting it wrong is not
			// a subtle failure: dcgm-exporter, the device plugin, the CNI and
			// the CSI driver are on every node, so naming them would attach a
			// spurious blocker to every finding the check ever produces.
			if pod.IsDaemonSet || pod.Evictable || pod.BlockReason == "" {
				continue
			}
			agg.blockers = append(agg.blockers, api.Blocker{
				Object: pod.Ref.String(),
				Reason: pod.BlockReason,
			})
		}
	}

	sort.Strings(order)
	var out []RawFinding
	for _, pool := range order {
		agg := pools[pool]
		sort.Strings(agg.nodes)

		fallow := agg.oldest
		if fallow > cl.Window {
			fallow = cl.Window
		}

		notes := []string{"no pod holding an accelerator has been scheduled here"}
		switch {
		case agg.measured:
			notes = append(notes,
				"the duration is the trailing run of zero utilization measured on these accelerators, "+
					"not the age of the nodes")
		case agg.unmeasured:
			// Saying which of the two numbers this is matters, because they
			// mean very different things and only one of them is an
			// observation.
			notes = append(notes,
				"no utilization series covered these accelerators, so the duration is the age of the "+
					"nodes — an upper bound on how long they could have been empty, not a measurement")
		}

		f := RawFinding{
			Check: api.CheckUnusedNode,
			Subject: Subject{
				Kind:  "node-pool",
				Name:  "pool/" + pool,
				Pool:  pool,
				Nodes: agg.nodes,
			},
			Devices:    nodeDeviceIDs(cl, agg.nodes),
			Fallow:     fallow,
			Confidence: api.EvidenceHigh,
			Evidence: api.Evidence{
				Window:             api.ISODuration(cl.Window),
				FallowDuration:     api.ISODuration(fallow),
				SampleCompleteness: 1,
				Notes:              notes,
			},
			Blockers: dedupeBlockers(agg.blockers),
			Summary: fmt.Sprintf("%d accelerators on pool/%s have had no workload scheduled for %s",
				agg.devices, pool, humanize.Duration(fallow)),
		}

		// A deliberate floor is capacity doing its job. Reporting it as waste,
		// with a removal command attached, is the fastest way for a tool to be
		// classified as not understanding the business — so it is separated
		// out, explained, and kept out of every waste total.
		if reason, held := cl.Autoscaler.Held(pool); held {
			f.ByDesign = true
			f.Because = fmt.Sprintf(
				"pool/%s is %s, so these nodes are kept on purpose. ullage cannot tell whether "+
					"that reservation is still needed — this is shown so the decision stays "+
					"visible, not because it is wrong.", pool, reason)
			f.Summary = fmt.Sprintf("%d accelerators on pool/%s are held empty deliberately",
				agg.devices, pool)
		} else if len(f.Blockers) > 0 {
			f.Evidence.Notes = append(f.Evidence.Notes, fmt.Sprintf(
				"%d pods prevent the autoscaler from draining these nodes", len(f.Blockers)))
		} else if cl.Autoscaler.Reclaims() {
			// Karpenter has no minimum size to rule out. An empty node it has
			// not consolidated is a stronger finding, not a weaker one.
			f.Evidence.Notes = append(f.Evidence.Notes,
				"Karpenter manages this pool and has no minimum size, so these nodes should already "+
					"have been consolidated")
			if cl.Autoscaler.Scheduled[pool] {
				f.Evidence.Notes = append(f.Evidence.Notes,
					"a scheduled disruption budget applies to this NodePool; ullage does not evaluate "+
						"cron windows, so consolidation may simply be paused")
				f.Confidence = api.EvidenceMedium
			}
		} else if cl.Autoscaler == nil {
			// Absence of autoscaler status is unknown, never permission. Saying
			// so is the difference between a cautious tool and a reckless one.
			f.Evidence.Notes = append(f.Evidence.Notes,
				"no autoscaler status was readable, so a deliberate minimum size cannot be ruled out")
			f.Confidence = api.EvidenceMedium
		}

		out = append(out, f)
	}
	return out, nil
}

func nodeDeviceIDs(cl *inventory.Cluster, nodes []string) []string {
	set := map[string]bool{}
	for _, n := range nodes {
		set[n] = true
	}
	var ids []string
	for _, d := range cl.Devices {
		if set[d.Node] {
			ids = append(ids, d.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func dedupeBlockers(in []api.Blocker) []api.Blocker {
	seen := map[string]bool{}
	var out []api.Blocker
	for _, b := range in {
		if seen[b.Object] {
			continue
		}
		seen[b.Object] = true
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Object < out[j].Object })
	return out
}

var _ = context.Background
