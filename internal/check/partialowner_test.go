package check_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ganeshkumarashok/ullage/internal/check"
	"github.com/ganeshkumarashok/ullage/internal/inventory"
	"github.com/ganeshkumarashok/ullage/internal/scan"
	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// controlledPod is a pod owned by a controller, which is what makes the
// scale-to-zero remediation available -- and dangerous.
func controlledPod(ns, name, node, kind, root string, gpus int) inventory.PodView {
	p := runningPod(ns, name, node, gpus)
	p.Provenance = api.Provenance{
		Controlled: true, Recognized: true,
		RootKind: kind, RootName: root,
	}
	return p
}

// The worst thing this tool could do is tell someone to stop a job that is
// doing work. A controller with one idle replica and three busy ones produces
// an idle-pod finding covering the one, and the obvious remediation for a
// controller -- scale it to zero -- stops all four.
//
// The finding is still worth reporting: an idle rank in a training job is real
// waste and often a real bug. What must not happen is a copyable command that
// takes down the other three.
func TestIdlePodWillNotOfferToStopBusySiblings(t *testing.T) {
	pods := []inventory.PodView{
		controlledPod("research", "trainer-0", "node-a", "StatefulSet", "trainer", 1),
		controlledPod("research", "trainer-1", "node-a", "StatefulSet", "trainer", 1),
		controlledPod("research", "trainer-2", "node-b", "StatefulSet", "trainer", 1),
	}
	idleRef, busyRef1, busyRef2 := pods[0].Ref, pods[1].Ref, pods[2].Ref

	cl := cluster([]inventory.Device{
		device("gpu-0", "node-a", "pool/research", &idleRef, idleStats(window, false)),
		device("gpu-1", "node-a", "pool/research", &busyRef1, idleStats(time.Minute, true)),
		device("gpu-2", "node-b", "pool/research", &busyRef2, idleStats(time.Minute, true)),
	}, pods, nil)

	found := find(t, check.IdlePod{}, cl)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1 covering the idle replica", len(found))
	}

	f := found[0]
	if len(f.Subject.Pods) != 1 || f.Subject.Pods[0].Name != "trainer-0" {
		t.Fatalf("finding covers %v, want only trainer-0", f.Subject.Pods)
	}
	if !f.Subject.PartialOwner {
		t.Fatal("PartialOwner is false: the finding covers 1 of 3 running replicas, " +
			"so the pipeline would happily print `kubectl scale statefulset trainer --replicas=0` " +
			"and stop two working pods")
	}

	// And the pipeline must act on it.
	fix := scan.SynthesiseFix(pods[0].Provenance, "research", []string{"trainer-0"},
		api.Owner{Identity: "alice@example.com"}, "", f.Subject.PartialOwner)

	if fix.Command != "" {
		t.Errorf("a partially idle controller produced a runnable command: %q", fix.Command)
	}
	if fix.Targets != api.FixTargetNone {
		t.Errorf("Targets = %q, want %q", fix.Targets, api.FixTargetNone)
	}
	if !strings.Contains(fix.Rationale, "Only some") {
		t.Errorf("the refusal does not explain itself: %q", fix.Rationale)
	}
}

// The refusal must be narrow. When every replica is idle, scaling the
// controller is exactly the right advice and withholding it would make the
// tool useless for its single most common finding.
func TestIdlePodStillScalesAFullyIdleController(t *testing.T) {
	pods := []inventory.PodView{
		controlledPod("research", "trainer-0", "node-a", "StatefulSet", "trainer", 1),
		controlledPod("research", "trainer-1", "node-a", "StatefulSet", "trainer", 1),
	}
	a, b := pods[0].Ref, pods[1].Ref

	cl := cluster([]inventory.Device{
		device("gpu-0", "node-a", "pool/research", &a, idleStats(window, false)),
		device("gpu-1", "node-a", "pool/research", &b, idleStats(window, false)),
	}, pods, nil)

	found := find(t, check.IdlePod{}, cl)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1", len(found))
	}
	if found[0].Subject.PartialOwner {
		t.Fatal("every running replica is idle, but the finding is marked partial")
	}

	fix := scan.SynthesiseFix(pods[0].Provenance, "research",
		[]string{"trainer-0", "trainer-1"}, api.Owner{}, "", found[0].Subject.PartialOwner)

	want := "kubectl scale statefulset -n research trainer --replicas=0"
	if fix.Command != want {
		t.Errorf("Command = %q, want %q", fix.Command, want)
	}
}

// Same class of bug on the other owner-grouping check: a Deployment with one
// crash-looping replica and two serving replicas must not be scaled to zero to
// fix the crash loop.
func TestStuckPodWillNotOfferToStopServingReplicas(t *testing.T) {
	serving := controlledPod("serving", "api-1", "node-a", "Deployment", "api", 1)
	ref := serving.Ref

	broken := controlledPod("serving", "api-0", "node-a", "Deployment", "api", 1)
	broken.WedgedReason = "CrashLoopBackOff"
	broken.Restarts = 148
	brokenRef := broken.Ref

	cl := cluster([]inventory.Device{
		device("gpu-0", "node-a", "pool/serving", &brokenRef, idleStats(6*time.Hour, false)),
		device("gpu-1", "node-a", "pool/serving", &ref, idleStats(time.Minute, true)),
	}, []inventory.PodView{broken, serving}, nil)

	found := find(t, check.StuckPod{}, cl)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1 for the crash-looping replica", len(found))
	}
	if !found[0].Subject.PartialOwner {
		t.Fatal("PartialOwner is false, so ullage would advise scaling the Deployment to zero " +
			"and take down the replica that is serving traffic")
	}
}

// A node whose accelerators never reported is empty *now*; how long it has been
// empty is not something the evidence says.
//
// The check falls back to node age for the duration, which is right as an upper
// bound and wrong as a measurement, because that duration is multiplied by a
// price and printed as money. An exporter that was never installed on one node
// would otherwise turn "nothing is scheduled here at this instant" into "you
// wasted a fortnight of H100 time", at full confidence, with a command to
// delete the pool attached.
func TestUnusedNodeWillNotPriceAnUnmeasuredNodeConfidently(t *testing.T) {
	nodes := []inventory.NodeView{
		{Name: "gpu-9", Pool: "gpu-h100", Accelerators: 8, Ready: true, Age: 20 * 24 * time.Hour},
	}

	// No devices at all: the exporter is missing from this node.
	got := find(t, check.UnusedNode{}, cluster(nil, nil, nodes))
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1 — the node really is empty and that is worth saying", len(got))
	}
	if got[0].Confidence != api.EvidenceLow {
		t.Fatalf("Confidence = %q for a node with no utilization series at all; the reported "+
			"%s of fallow is the node's age, not a measurement, and at medium or above it is "+
			"shown by default and priced as though it had been observed",
			got[0].Confidence, got[0].Fallow)
	}
}

// The downgrade must not become an upgrade. Two different doubts are written to
// the same confidence field, and the autoscaler one is written second.
func TestUnusedNodeCaveatsOnlyEverLowerConfidence(t *testing.T) {
	nodes := []inventory.NodeView{
		{Name: "gpu-9", Pool: "gpu-h100", Accelerators: 8, Ready: true, Age: 20 * 24 * time.Hour},
	}
	cl := cluster(nil, nil, nodes)
	cl.Autoscaler = nil // unreadable autoscaler status, which appends its own caveat

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if got[0].Confidence != api.EvidenceLow {
		t.Fatalf("Confidence = %q: an unmeasured duration plus an unreadable autoscaler is two "+
			"reasons to doubt the finding, and it came out more confident than either", got[0].Confidence)
	}
}

// busyStats is a device that did work recently, so its node counts as in use.
func busyStats() inventory.Stats {
	t := now.Add(-time.Hour)
	return inventory.Stats{Samples: 40320, Completeness: 1, Max: 88, LastNonZero: &t}
}

// An autoscaler minimum reserves a *number of nodes*, not a pool.
//
// Held(pool) answered a yes/no question, so a pool of ten nodes with a floor of
// two filed all its empty nodes as deliberate reserved capacity — including the
// eight the floor says nothing about. That is the failure mode that quietly
// makes the tool useless: the pools nobody remembers are the large ones, and a
// large forgotten pool with any floor at all disappeared from the report.
func TestUnusedNodeCountsTheFloorRatherThanExemptingThePool(t *testing.T) {
	const pool = "h100-reserve"

	var nodes []inventory.NodeView
	var devices []inventory.Device
	var pods []inventory.PodView
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("reserve-%d", i)
		nodes = append(nodes, inventory.NodeView{
			Name: name, Pool: pool, Accelerators: 1, Ready: true,
			Age: 30 * 24 * time.Hour, Model: "NVIDIA-H100-SXM5-80GB",
		})
		// Two nodes are working, so the floor of two is entirely accounted for
		// by capacity that is already in use and reserves nothing further.
		if i < 2 {
			pods = append(pods, runningPod("ml", name+"-job", name, 1))
			devices = append(devices, device(name+"-gpu", name, pool,
				&inventory.PodRef{Namespace: "ml", Name: name + "-job", UID: "uid-" + name + "-job"},
				busyStats()))
			continue
		}
		devices = append(devices, device(name+"-gpu", name, pool, nil, idleStats(10*24*time.Hour, false)))
	}

	cl := cluster(devices, pods, nodes)
	cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{pool: 2}}

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	f := got[0]
	if f.ByDesign {
		t.Fatal("a floor of 2 on a 10-node pool with 2 nodes working reserves nothing further, " +
			"but all 8 empty nodes were filed as deliberate and kept out of every waste total")
	}
	if len(f.Devices) != 8 {
		t.Fatalf("the finding covers %d accelerators; 10 nodes minus a floor of 2 leaves 8 "+
			"the floor does not explain", len(f.Devices))
	}
}

// The floor still has to work when it genuinely explains the whole pool, or the
// fix above would just be the old over-reporting bug in reverse.
func TestUnusedNodeStillHonoursAFloorThatExplainsEveryNode(t *testing.T) {
	const pool = "h100-reserve"
	var nodes []inventory.NodeView
	var devices []inventory.Device
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("reserve-%d", i)
		nodes = append(nodes, inventory.NodeView{
			Name: name, Pool: pool, Accelerators: 1, Ready: true, Age: 30 * 24 * time.Hour,
		})
		devices = append(devices, device(name+"-gpu", name, pool, nil, idleStats(10*24*time.Hour, false)))
	}
	cl := cluster(devices, nil, nodes)
	cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{pool: 3}}

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if !got[0].ByDesign {
		t.Fatal("all three nodes are held by a floor of three, so this is reserved capacity; " +
			"reporting it as waste with a scale-down command is how a tool gets uninstalled")
	}
}

// A Karpenter disruption budget that allows zero disruptions is not a count.
// There is no arithmetic to do and no partial answer to give.
func TestUnusedNodeTreatsAPinnedKarpenterPoolAsWhollyDeliberate(t *testing.T) {
	const pool = "gpu"
	var nodes []inventory.NodeView
	var devices []inventory.Device
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("gpu-%d", i)
		nodes = append(nodes, inventory.NodeView{
			Name: name, Pool: pool, Accelerators: 1, Ready: true, Age: 30 * 24 * time.Hour,
		})
		devices = append(devices, device(name+"-gpu", name, pool, nil, idleStats(10*24*time.Hour, false)))
	}
	cl := cluster(devices, nil, nodes)
	cl.Autoscaler = &inventory.AutoscalerView{
		Kind: "karpenter", Floors: map[string]int{}, Pinned: map[string]bool{pool: true}}

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if !got[0].ByDesign {
		t.Fatal("a NodePool pinned by a zero-disruption budget is held on purpose in full; " +
			"there is no per-node arithmetic that makes part of it reclaimable")
	}
}

// The pod-level idle check reads CoverageOver, not Completeness, so the cap
// that a partly-failed Prometheus read puts on a device's coverage has to reach
// both or it protects only half the tool -- and the half it was missing is the
// one that prints `kubectl scale --replicas=0`.
//
// The shape here is the dangerous one: a young pod with a dense series over the
// interval that *was* readable. Sample count alone says 100% coverage. The
// device could have been at full utilization for every hour that failed.
func TestIdlePodRejectsADenseSeriesOverAnUnansweredWindow(t *testing.T) {
	start := now.Add(-10 * 24 * time.Hour)
	pod := inventory.PodView{
		Ref:          inventory.PodRef{Namespace: "research", Name: "trainer", UID: "uid-trainer"},
		Node:         "gpu-a",
		Phase:        "Running",
		Accelerators: 1,
		StartTime:    &start,
		Provenance:   api.Provenance{Controlled: false, Recognized: true, RootKind: "Pod", RootName: "trainer"},
	}
	nodes := []inventory.NodeView{{Name: "gpu-a", Pool: "gpu", Accelerators: 1, Ready: true, Age: 30 * 24 * time.Hour}}

	// Enough samples to look complete for this pod's ten-day life.
	st := idleStats(10*24*time.Hour, false)

	answered := func(a float64) []inventory.Device {
		s := st
		s.Answered = a
		return []inventory.Device{device("gpu-a-0", "gpu-a", "gpu",
			&inventory.PodRef{Namespace: "research", Name: "trainer", UID: "uid-trainer"}, s)}
	}

	// Control: fully answered, and the finding is exactly what ullage is for.
	if got := find(t, check.IdlePod{}, cluster(answered(1), []inventory.PodView{pod}, nodes)); len(got) != 1 {
		t.Fatalf("a fully answered idle pod produced %d findings, want 1", len(got))
	}

	// Half the window went unanswered by the max query.
	got := find(t, check.IdlePod{}, cluster(answered(0.5), []inventory.PodView{pod}, nodes))
	if len(got) != 0 {
		t.Fatalf("got %d findings from a device whose 'was it ever busy' question was only "+
			"answered for half the window; the sample count says full coverage and cannot "+
			"tell a quiet interval from an unread one", len(got))
	}
}
