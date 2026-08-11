package check_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ullage-project/ullage/internal/check"
	"github.com/ullage-project/ullage/internal/inventory"
	"github.com/ullage-project/ullage/internal/scan"
	"github.com/ullage-project/ullage/pkg/ullage/api"
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
