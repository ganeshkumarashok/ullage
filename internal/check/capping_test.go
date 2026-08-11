package check_test

import (
	"testing"
	"time"

	"github.com/ganeshkumarashok/ullage/internal/check"
	"github.com/ganeshkumarashok/ullage/internal/inventory"
	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// The same overstatement TestGroupedFallowHoursAreSummedNotMultiplied pins for
// idle-pod, in the check that never populated the field built to prevent it.
//
// Two pods of one Deployment are wedged: one for a fortnight, one for an hour.
// That is 336 + 1 = 337 device-hours. Billing the group's longest stall
// against both devices gives 672 — almost exactly double.
func TestStuckPodGroupedHoursAreSummedNotMultiplied(t *testing.T) {
	var pods []inventory.PodView

	add := func(name string, held time.Duration) {
		p := runningPod("research", name, "node-a", 1)
		p.Provenance = api.Provenance{
			Controlled: true, Recognized: true,
			RootKind: "Deployment", RootName: "trainer",
		}
		start := now.Add(-held)
		p.StartTime = &start
		p.WedgedReason = "CrashLoopBackOff"
		pods = append(pods, p)
	}

	add("long", 336*time.Hour)
	add("short", time.Hour)

	found := find(t, check.StuckPod{}, cluster(nil, pods, nil))
	if len(found) != 1 {
		t.Fatalf("got %d findings, want the two replicas grouped into 1", len(found))
	}

	const want = 336 + 1
	if got := found[0].FallowHours; got != want {
		t.Fatalf("FallowHours = %.0f, want %d.\n"+
			"One device wedged 336h beside one wedged 1h is %d device-hours. "+
			"%.0f is the group's longest stall billed against both devices, which "+
			"nearly doubles the reported waste and outranks findings that are real.",
			got, want, want, got)
	}
}

// Hours are capped per pod, not per group. A pod that has been wedged since
// before the window started cannot have wasted more time than was examined.
func TestStuckPodCapsEachPodAtTheWindow(t *testing.T) {
	var pods []inventory.PodView
	for _, tc := range []struct {
		name string
		held time.Duration
	}{{"ancient", 30 * 24 * time.Hour}, {"recent", 24 * time.Hour}} {
		p := runningPod("research", tc.name, "node-a", 1)
		p.Provenance = api.Provenance{
			Controlled: true, Recognized: true,
			RootKind: "Deployment", RootName: "trainer",
		}
		start := now.Add(-tc.held)
		p.StartTime = &start
		p.WedgedReason = "CrashLoopBackOff"
		pods = append(pods, p)
	}

	found := find(t, check.StuckPod{}, cluster(nil, pods, nil))
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1", len(found))
	}

	const want = 336 + 24 // the 30d pod contributes only the 14d window
	if got := found[0].FallowHours; got != want {
		t.Fatalf("FallowHours = %.0f, want %d: a pod cannot waste more time than was examined", got, want)
	}
}

// A terminal pod has already released its devices, so it is not holding
// anything for anyone to reclaim. Reporting it would send someone to delete a
// pod to free capacity that is free.
func TestStuckPodIgnoresTerminalPods(t *testing.T) {
	for _, phase := range []string{"Failed", "Succeeded"} {
		t.Run(phase, func(t *testing.T) {
			p := runningPod("research", "batch", "node-a", 1)
			p.Phase = phase
			start := now.Add(-48 * time.Hour)
			p.StartTime = &start
			// The inventory layer is what clears this for terminal pods; a
			// check that relied on the phase alone would still be wrong for a
			// pod the API reported as wedged.
			p.WedgedReason = ""

			if found := find(t, check.StuckPod{}, cluster(nil, []inventory.PodView{p}, nil)); len(found) != 0 {
				t.Fatalf("got %d findings for a %s pod, want 0: it holds nothing", len(found), phase)
			}
		})
	}
}

// Each node's fallow time is capped at the window before it joins the total.
// Scaling the finished aggregate instead shrinks the nodes that were already
// inside the window to pay for the one that was not.
//
// A node idle 28 days and a node idle 7 days, over a 14-day window, is
// 336 + 168 = 504 accelerator-hours. Proportional scaling of the aggregate
// reports 420.
func TestUnusedNodeCapsEachNodeBeforeSumming(t *testing.T) {
	var devices []inventory.Device
	var nodes []inventory.NodeView

	add := func(name string, idle time.Duration) {
		nodes = append(nodes, inventory.NodeView{
			Name: name, Pool: "gpu-pool", Model: "NVIDIA-A100-SXM4-80GB",
			Vendor: "nvidia", Accelerators: 1, Allocation: api.AllocExclusive,
			TDPWatts: 400, Ready: true, Age: idle,
		})
		devices = append(devices, device(name+"/0", name, "gpu-pool", nil, idleStats(idle, false)))
	}

	add("node-old", 28*24*time.Hour)
	add("node-new", 7*24*time.Hour)

	found := find(t, check.UnusedNode{}, cluster(devices, nil, nodes))
	if len(found) != 1 {
		t.Fatalf("got %d findings, want the pool grouped into 1", len(found))
	}

	const want = 336 + 168
	if got := found[0].FallowHours; got != want {
		t.Fatalf("FallowHours = %.0f, want %d.\n"+
			"The 28d node contributes the 14d window (336h) and the 7d node contributes 168h. "+
			"%.0f is the aggregate scaled down by window/oldest, which charges the newer "+
			"node for the older one being outside the window.",
			got, want, got)
	}
}
