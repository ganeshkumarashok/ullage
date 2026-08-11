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
		Docs: docsURLFor(api.CheckUnusedNode),
	}
}

// Applicable is about devices; this check reasons about nodes, and every
// accelerator on an empty node counts regardless of allocation model.
func (UnusedNode) Applicable(d inventory.Device) bool { return true }

// nodeWork is everything the utilization series can say about one node.
type nodeWork struct {
	// since is the shortest time since any accelerator here did work.
	// Shortest, because one busy device makes the whole node occupied.
	since time.Duration
	// completeness is the coverage of the *least* observed accelerator, since
	// a claim about the node is only as good as its weakest measurement.
	completeness float64
	// covered counts distinct accelerators that yielded a usable answer.
	covered int
}

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
	// lastWork holds, per node, the shortest time since any accelerator on it
	// was seen doing work. Shortest, because one busy device is enough to make
	// the whole node occupied — taking anything but the minimum would let a
	// node with three idle GPUs and one working one read as idle.
	//
	// It also records how well observed the node was, because "no accelerator
	// here did work" is only a measurement if every accelerator here was
	// actually watched. A node with four GPUs where three produced no series
	// at all cannot support that claim on the strength of the fourth.
	lastWork := map[string]nodeWork{}
	countedDevice := map[string]bool{}
	for _, d := range cl.Devices {
		if d.Util.Samples == 0 {
			continue
		}
		// A series that stopped reporting cannot vouch for the present, in
		// either direction: it neither proves work nor proves idleness. Skip
		// it, which leaves the node on the weaker node-age path.
		if d.Util.Stale {
			continue
		}

		var since time.Duration
		if d.Util.Max > 0 {
			// The device did work at some point. How long ago is the trailing
			// zero run; if it is still working, that is zero.
			if idle, ok := d.Util.FallowFor(cl.Now); ok {
				since = idle
			}
		} else {
			// The device read zero throughout, but a zero reading is only
			// evidence in proportion to how much of the window was observed.
			// One sample out of a fortnight is not a fortnight of measured
			// idleness, and without this gate it became one: the node was
			// credited with the full window and the evidence called the number
			// "measured", which is the overstatement this check was rewritten
			// to remove.
			//
			// Skipping is deliberately not the same as dropping the node. It
			// falls through to the node-age path, which makes a weaker claim
			// and labels itself as such.
			if d.Util.Completeness < minCompleteness {
				continue
			}
			idle, ok := d.Util.FallowFor(cl.Now)
			if !ok {
				continue
			}
			since = idle
		}

		w, seen := lastWork[d.Node]
		if !seen || since < w.since {
			w.since = since
		}
		if !seen || d.Util.Completeness < w.completeness {
			w.completeness = d.Util.Completeness
		}
		// Counted once per physical accelerator, not once per series. A GPU
		// reused by a second pod inside the window returns a series per
		// holder, and counting those separately would let one well observed
		// device satisfy the coverage test for a node full of unobserved ones.
		if !countedDevice[d.ID] {
			countedDevice[d.ID] = true
			w.covered++
		}
		lastWork[d.Node] = w
	}

	type poolAgg struct {
		nodes    []string
		devices  int
		oldest   time.Duration
		blockers []api.Blocker

		// The reported fallow duration is the longest across the pool, so the
		// evidence label has to describe *that* node. Recording only that some
		// node in the pool was measured lets one node's measurement vouch for
		// a different node's unmeasured age — which is precisely the
		// overstatement this check keeps having to unlearn.
		oldestNode         string
		oldestMeasured     bool
		oldestCompleteness float64

		// Whether the node whose duration is being reported produced any
		// utilization series at all. Without one, the duration is not a weak
		// measurement -- it is not a measurement, and the accelerator-hours
		// and the cost derived from it are inference presented as observation.
		oldestAnySeries bool

		// Whether the pool's coverage is uniform, which is worth saying out
		// loud when it is not.
		anyMeasured   bool
		anyUnmeasured bool

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
		// Any evidence of recent work excludes the node, whether or not the
		// coverage was good enough to make a positive claim. Evidence that
		// something ran is not held to the same bar as evidence that nothing
		// did, because the two errors are not symmetric.
		if w, seen := lastWork[node.Name]; seen && w.since < p.IdleThreshold {
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
		nodeMeasured := false
		anySeries := false
		completeness := 0.0
		// Every accelerator on the node has to have been observed before the
		// trailing zero run counts as a measurement of the node: three GPUs
		// that produced no series at all cannot be vouched for by the fourth.
		//
		// The count is only comparable under exclusive allocation, where the
		// advertised extended resource is the physical card. Under MIG or
		// time-slicing the node advertises many more units than DCGM has
		// series for, so requiring parity there would permanently deny a
		// truthful label; one observed device is the best available bar.
		want := 1
		if node.Allocation == api.AllocExclusive {
			want = node.Accelerators
		}
		// `<=` and not `<`: when the measurement and the age agree on the
		// number, the number is still backed by a measurement, and the
		// stronger provenance is the true one.
		if w, seen := lastWork[node.Name]; seen {
			anySeries = w.covered > 0
			if w.covered >= want && w.since <= empty {
				empty = w.since
				nodeMeasured = true
				completeness = w.completeness
			}
		}
		if nodeMeasured {
			agg.anyMeasured = true
		} else {
			agg.anyUnmeasured = true
		}
		// Provenance travels with the number it describes.
		if empty > agg.oldest {
			agg.oldest = empty
			agg.oldestNode = node.Name
			agg.oldestMeasured = nodeMeasured
			agg.oldestCompleteness = completeness
			agg.oldestAnySeries = anySeries
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

		// The reported duration is one specific node's, so the note names it
		// rather than implying the whole pool was observed the same way.
		longestNode := agg.oldestNode
		if longestNode == "" {
			longestNode = "these nodes"
		}

		notes := []string{"no pod holding an accelerator has been scheduled here"}
		// Saying which of the two numbers this is matters, because they mean
		// very different things and only one of them is an observation.
		if agg.oldestMeasured {
			notes = append(notes,
				"the duration is the trailing run of zero utilization measured on "+
					longestNode+", not the age of the nodes")
		} else {
			notes = append(notes,
				"no utilization series covered every accelerator on "+longestNode+", so the duration "+
					"is that node's age — an upper bound on how long it could have been empty, "+
					"not a measurement")
		}
		if agg.anyMeasured && agg.anyUnmeasured {
			notes = append(notes,
				"accelerator coverage is uneven across this pool: some nodes here were measured "+
					"and others were not")
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
			Confidence: durationConfidence(agg.oldestMeasured, agg.oldestAnySeries),
			Evidence: api.Evidence{
				Window:             api.ISODuration(cl.Window),
				FallowDuration:     api.ISODuration(fallow),
				SampleCompleteness: agg.oldestCompleteness,
				Notes:              notes,
			},
			Blockers: dedupeBlockers(agg.blockers),
			Summary: fmt.Sprintf("%d %s on pool/%s have had no workload scheduled for %s",
				agg.devices, humanize.Plural(agg.devices, "accelerator"),
				pool, humanize.Duration(fallow)),
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
			f.Summary = fmt.Sprintf("%d %s on pool/%s are held empty deliberately",
				agg.devices, humanize.Plural(agg.devices, "accelerator"), pool)
		} else if len(f.Blockers) > 0 {
			f.Evidence.Notes = append(f.Evidence.Notes, fmt.Sprintf(
				"%d %s prevent the autoscaler from draining these nodes",
				len(f.Blockers), humanize.Plural(len(f.Blockers), "pod")))
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
				f.Confidence = atMost(f.Confidence, api.EvidenceMedium)
			}
		} else if cl.Autoscaler == nil {
			// Absence of autoscaler status is unknown, never permission. Saying
			// so is the difference between a cautious tool and a reckless one.
			f.Evidence.Notes = append(f.Evidence.Notes,
				"no autoscaler status was readable, so a deliberate minimum size cannot be ruled out")
			f.Confidence = atMost(f.Confidence, api.EvidenceMedium)
		}

		out = append(out, f)
	}
	return out, nil
}

// atMost caps a confidence, so that a caveat can only ever lower it.
//
// Two independent doubts are expressed through this one field -- how well the
// duration is evidenced, and whether the autoscaler's intent could be read --
// and they are written in that order. Plain assignment let the second one
// promote a duration that was never measured up to medium.
func atMost(current, cap string) string {
	rank := map[string]int{api.EvidenceLow: 1, api.EvidenceMedium: 2, api.EvidenceHigh: 3}
	if rank[cap] < rank[current] {
		return cap
	}
	return current
}

// durationConfidence rates the reported fallow duration, not the emptiness.
//
// That the pool holds no accelerator workload is read from the API server and
// is not in doubt. How long that has been true is a different claim with
// different provenance, and it is the one that gets multiplied by a price and
// printed as money.
//
// A node whose accelerators produced no series at all has an unmeasured
// duration: the age is an upper bound on how long it *could* have been empty.
// An exporter that was never installed on one node would otherwise turn "empty
// right now" into "wasted for a fortnight" at full confidence. Rating that low
// puts it under the default floor, so it has to be asked for -- which is the
// right trade for a number that cannot be checked.
func durationConfidence(measured, anySeries bool) string {
	switch {
	case measured:
		return api.EvidenceHigh
	case anySeries:
		// Some accelerators on the node were observed but not all of them, so
		// there is partial evidence behind the age.
		return api.EvidenceMedium
	default:
		return api.EvidenceLow
	}
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
