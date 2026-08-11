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
			"requesting an accelerator has been placed on them. They are not draining, not " +
			"cordoned, and past the initialisation grace period.",
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
	occupied := map[string]bool{}
	for _, pod := range cl.Pods {
		if pod.Node != "" && pod.Accelerators > 0 && !pod.Pending {
			occupied[pod.Node] = true
		}
	}

	type poolAgg struct {
		nodes    []string
		devices  int
		oldest   time.Duration
		blockers []api.Blocker
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
		if node.Age > agg.oldest {
			agg.oldest = node.Age
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
				Notes:              []string{"no pod requesting an accelerator has been scheduled here"},
			},
			Blockers: dedupeBlockers(agg.blockers),
			Summary: fmt.Sprintf("%d accelerators on pool/%s have had no workload scheduled for %s",
				agg.devices, pool, humanize.Duration(fallow)),
		}

		// A deliberate floor is capacity doing its job. Reporting it as waste,
		// with a removal command attached, is the fastest way for a tool to be
		// classified as not understanding the business — so it is separated
		// out, explained, and kept out of every waste total.
		if min, floored := cl.Autoscaler.Floor(pool); floored && min > 0 {
			f.ByDesign = true
			f.Because = fmt.Sprintf(
				"pool/%s is held at a minimum of %d nodes by the cluster autoscaler, so these nodes "+
					"are kept on purpose. ullage cannot tell whether that reservation is still "+
					"needed — this is shown so the decision stays visible, not because it is wrong.",
				pool, min)
			f.Summary = fmt.Sprintf("%d accelerators on pool/%s are held empty by a minimum-size floor of %d",
				agg.devices, pool, min)
		} else if len(f.Blockers) > 0 {
			f.Evidence.Notes = append(f.Evidence.Notes, fmt.Sprintf(
				"%d pods prevent the autoscaler from draining these nodes", len(f.Blockers)))
		} else if cl.Autoscaler == nil {
			// Absence of autoscaler status is unknown, never permission. Saying
			// so is the difference between a cautious tool and a reckless one.
			f.Evidence.Notes = append(f.Evidence.Notes,
				"no cluster-autoscaler status was readable, so a deliberate minimum size cannot be ruled out")
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
