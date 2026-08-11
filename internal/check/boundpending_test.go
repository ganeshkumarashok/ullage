package check_test

import (
	"testing"
	"time"

	"github.com/ganeshkumarashok/ullage/internal/check"
	"github.com/ganeshkumarashok/ullage/internal/inventory"
	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// A pod bound to a node and wedged on ImagePullBackOff is phase Pending. The
// scheduler has already committed that node's accelerator to it, so nothing
// else can be placed there and the hardware is doing nothing at anyone's
// expense -- the exact waste this tool exists to name.
//
// "Pending" used to mean "phase Pending OR unscheduled", which put this pod in
// the same bucket as one that has never been placed. The consequences ran in
// both directions at once: unused-node saw an empty node and offered to delete
// it, while stuck-pod, the check written for precisely this pod, skipped it.
// The one node in the cluster that was definitely wasting money was reported
// as reclaimable rather than as wedged, with no mention of the pod holding it.
func TestBoundPendingPodOccupiesItsNode(t *testing.T) {
	pod := runningPod("ml", "trainer", "node-a", 1)
	pod.Phase = "Pending"
	pod.Pending = false // bound: the scheduler placed it
	pod.WedgedReason = "ImagePullBackOff"
	started := now.Add(-6 * time.Hour)
	pod.StartTime = &started

	nodes := []inventory.NodeView{{
		Name: "node-a", Pool: "gpu", Ready: true, Accelerators: 1,
		Age: 20 * 24 * time.Hour, Allocation: api.AllocExclusive,
		Model: "NVIDIA-A100-SXM4-80GB", Vendor: "nvidia",
	}}
	devices := []inventory.Device{
		device("node-a/0", "node-a", "gpu", nil, idleStats(20*24*time.Hour, false)),
	}
	cl := cluster(devices, []inventory.PodView{pod}, nodes)

	if got := find(t, check.UnusedNode{}, cl); len(got) != 0 {
		t.Fatalf("unused-node reported %q on a node whose accelerator the scheduler has "+
			"already committed to a bound pod; running this recommendation deletes a node "+
			"that Kubernetes considers full", got[0].Summary)
	}

	stuck := find(t, check.StuckPod{}, cl)
	if len(stuck) != 1 {
		t.Fatalf("stuck-pod found %d findings, want 1: a bound pod wedged in "+
			"ImagePullBackOff for six hours while holding an A100 is the single clearest "+
			"finding this tool can produce, and it was silently skipped", len(stuck))
	}
}

// The complement, and the reason the old conflation existed: a pod that has
// never been scheduled holds nothing at all. It is the victim of the waste,
// not its cause, and counting it would claim recoverable hours against devices
// that were never allocated to anyone.
func TestUnscheduledPodHoldsNothing(t *testing.T) {
	pod := runningPod("ml", "queued", "", 1)
	pod.Phase = "Pending"
	pod.Pending = true
	pod.Node = ""
	pod.WedgedReason = "ImagePullBackOff"

	nodes := []inventory.NodeView{{
		Name: "node-a", Pool: "gpu", Ready: true, Accelerators: 1,
		Age: 20 * 24 * time.Hour, Allocation: api.AllocExclusive,
		Model: "NVIDIA-A100-SXM4-80GB", Vendor: "nvidia",
	}}
	devices := []inventory.Device{
		device("node-a/0", "node-a", "gpu", nil, idleStats(20*24*time.Hour, false)),
	}
	cl := cluster(devices, []inventory.PodView{pod}, nodes)

	if got := find(t, check.StuckPod{}, cl); len(got) != 0 {
		t.Fatalf("stuck-pod reported an unscheduled pod, claiming recoverable "+
			"accelerator-hours against a device that was never allocated: %q", got[0].Summary)
	}
	if got := find(t, check.UnusedNode{}, cl); len(got) != 1 {
		t.Fatalf("unused-node found %d findings, want 1: nothing is scheduled on this "+
			"node, so it really is empty", len(got))
	}
}
