package check_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ganeshkumarashok/ullage/internal/check"
	"github.com/ganeshkumarashok/ullage/internal/inventory"
	"github.com/ganeshkumarashok/ullage/internal/scan"
	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// These tests build clusters from literals. That is the point of the fact
// layer: a check's behaviour is decided by facts, so its tests need no API
// server, no Prometheus, no fixtures and no golden files. Adding a check should
// mean adding a table entry here, not standing up an environment.

var now = time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)

const window = 336 * time.Hour

func params() check.Params {
	return check.Params{
		IdleThreshold:  72 * time.Hour,
		StuckThreshold: time.Hour,
		InitGrace:      2 * time.Hour,
	}
}

func idleStats(since time.Duration, everBusy bool) inventory.Stats {
	s := inventory.Stats{
		Samples:        40320,
		Completeness:   1,
		ZeroThroughout: !everBusy,
		FallowSince:    now.Add(-since),
	}
	if everBusy {
		t := now.Add(-since)
		s.LastNonZero = &t
		s.Max = 88
	}
	return s
}

func device(id, node, pool string, holder *inventory.PodRef, util inventory.Stats) inventory.Device {
	return inventory.Device{
		ID: id, Node: node, Pool: pool, Model: "NVIDIA-A100-SXM4-80GB",
		Vendor: "nvidia", Allocation: api.AllocExclusive, Analyzable: true,
		TDPWatts: 400, Holder: holder,
		Util:  util,
		Power: inventory.Stats{Samples: 1, Mean: 56, Max: 56},
	}
}

func runningPod(ns, name, node string, gpus int) inventory.PodView {
	start := now.Add(-30 * 24 * time.Hour)
	return inventory.PodView{
		Ref:          inventory.PodRef{Namespace: ns, Name: name, UID: "uid-" + name},
		Node:         node,
		Phase:        "Running",
		Accelerators: gpus,
		StartTime:    &start,
		Provenance:   api.Provenance{Controlled: false, Recognized: true, RootKind: "Pod", RootName: name},
	}
}

func cluster(devices []inventory.Device, pods []inventory.PodView, nodes []inventory.NodeView) *inventory.Cluster {
	return &inventory.Cluster{
		Context: "test", Now: now, Window: window, Step: scan.ScrapeInterval,
		Devices: devices, Pods: pods, Nodes: nodes,
		MetricsAttributed: true,
	}
}

func find(t *testing.T, c check.Check, cl *inventory.Cluster) []check.RawFinding {
	t.Helper()
	out, err := c.Run(context.Background(), cl, params())
	if err != nil {
		t.Fatalf("check %s returned an error: %v", c.Describe().ID, err)
	}
	return out
}

func TestIdlePodRequiresStrictZero(t *testing.T) {
	pod := runningPod("research", "notebook", "node-a", 1)
	ref := pod.Ref

	cases := []struct {
		name  string
		util  inventory.Stats
		found bool
		why   string
	}{
		{
			name:  "zero for the whole window",
			util:  idleStats(window, false),
			found: true,
		},
		{
			name:  "zero for nine days after earlier work",
			util:  idleStats(9*24*time.Hour, true),
			found: true,
			why:   "the claim is about the trailing run of zeroes, not the whole window",
		},
		{
			name:  "zero for only two days",
			util:  idleStats(48*time.Hour, true),
			found: false,
			why:   "below the 72h threshold",
		},
		{
			// The single most important negative case. A workload averaging 4%
			// is the shape that a threshold-on-average tool flags and destroys
			// real work over. Utilization is a bad measure of how hard a device
			// is working, and only its zero is trustworthy.
			name: "low average but non-zero",
			util: inventory.Stats{
				Samples: 40320, Completeness: 1, Max: 96, Mean: 4,
				ZeroThroughout: false,
				LastNonZero:    ptr(now.Add(-90 * time.Minute)),
				FallowSince:    now.Add(-90 * time.Minute),
			},
			found: false,
			why:   "a low mean is not idleness",
		},
		{
			name: "no samples at all",
			util: inventory.Stats{Samples: 0, Completeness: 0},
			// An exporter that died is not a fleet that went idle. Reporting
			// this would recommend deleting everything in the cluster.
			found: false,
			why:   "absent is not zero",
		},
		{
			name: "large scrape gap",
			util: inventory.Stats{
				Samples: 20000, Completeness: 0.5,
				ZeroThroughout: true, FallowSince: now.Add(-window),
			},
			found: false,
			why:   "a gap could hide a working period",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := cluster(
				[]inventory.Device{device("node-a/0", "node-a", "p", &ref, tc.util)},
				[]inventory.PodView{pod},
				nil,
			)
			got := find(t, check.IdlePod{}, cl)
			if (len(got) > 0) != tc.found {
				t.Fatalf("got %d findings, want found=%v (%s)", len(got), tc.found, tc.why)
			}
		})
	}
}

func TestIdlePodDeclinesSharedDevices(t *testing.T) {
	pod := runningPod("inference", "scorer", "node-t4", 1)
	ref := pod.Ref
	for _, alloc := range []string{api.AllocTimeSliced, api.AllocMIG} {
		t.Run(alloc, func(t *testing.T) {
			d := device("node-t4/0", "node-t4", "t4", &ref, idleStats(window, false))
			d.Allocation = alloc
			d.Analyzable = false
			cl := cluster([]inventory.Device{d}, []inventory.PodView{pod}, nil)
			if got := find(t, check.IdlePod{}, cl); len(got) != 0 {
				t.Fatalf("got %d findings on a %s device; device-level utilization "+
					"reflects every co-tenant, so no per-pod claim is supportable", len(got), alloc)
			}
		})
	}
}

func TestIdlePodGroupsByRootOwner(t *testing.T) {
	var devices []inventory.Device
	var pods []inventory.PodView
	for i, name := range []string{"jupyter-0", "jupyter-1", "jupyter-2"} {
		p := runningPod("research", name, "node-a", 1)
		p.Provenance = api.Provenance{
			Controlled: true, Recognized: true,
			RootKind: "StatefulSet", RootName: "jupyter", APIVersion: "apps/v1",
		}
		pods = append(pods, p)
		ref := p.Ref
		devices = append(devices, device(
			"node-a/"+string(rune('0'+i)), "node-a", "p", &ref, idleStats(window, false)))
	}

	got := find(t, check.IdlePod{}, cluster(devices, pods, nil))
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: three pods of one StatefulSet share a cause and a fix", len(got))
	}
	if n := len(got[0].Devices); n != 3 {
		t.Fatalf("grouped finding covers %d devices, want 3", n)
	}
	if got[0].Subject.Name != "jupyter" {
		t.Fatalf("subject is %q, want the root owner %q", got[0].Subject.Name, "jupyter")
	}
}

func TestUnusedNodeRespectsAutoscalerFloor(t *testing.T) {
	nodes := []inventory.NodeView{
		{Name: "reserve-0", Pool: "h100-reserve", Accelerators: 8, Ready: true,
			Age: 30 * 24 * time.Hour, Model: "NVIDIA-H100-SXM5-80GB"},
		{Name: "reserve-1", Pool: "h100-reserve", Accelerators: 8, Ready: true,
			Age: 30 * 24 * time.Hour, Model: "NVIDIA-H100-SXM5-80GB"},
	}
	cl := cluster(nil, nil, nodes)
	cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{"h100-reserve": 2}}

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if !got[0].ByDesign {
		t.Fatal("a pool held at its autoscaler minimum is reserved capacity, not waste; " +
			"reporting it as waste with a scale-down command is how a tool gets uninstalled")
	}
}

func TestUnusedNodeWithoutAutoscalerIsNotConfident(t *testing.T) {
	nodes := []inventory.NodeView{
		{Name: "gpu-0", Pool: "gpu", Accelerators: 4, Ready: true, Age: 20 * 24 * time.Hour},
	}
	cl := cluster(nil, nil, nodes)
	cl.Autoscaler = nil

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if got[0].Confidence == api.EvidenceHigh {
		t.Fatal("without the autoscaler status a minimum-size floor is unknown, and " +
			"unknown is not permission to recommend removing capacity")
	}
}

func TestUnusedNodeSkipsNodesThatMerelyLookIdle(t *testing.T) {
	cases := []struct {
		name string
		node inventory.NodeView
	}{
		{"not ready", inventory.NodeView{Name: "n", Pool: "p", Accelerators: 4, Ready: false, Age: 20 * 24 * time.Hour}},
		{"cordoned", inventory.NodeView{Name: "n", Pool: "p", Accelerators: 4, Ready: true, Unschedulable: true, Age: 20 * 24 * time.Hour}},
		{"still initialising", inventory.NodeView{Name: "n", Pool: "p", Accelerators: 4, Ready: true, Initialising: true, Age: 20 * 24 * time.Hour}},
		{"scale-down disabled", inventory.NodeView{Name: "n", Pool: "p", Accelerators: 4, Ready: true, ScaleDownDisabled: true, Age: 20 * 24 * time.Hour}},
		{"newer than the threshold", inventory.NodeView{Name: "n", Pool: "p", Accelerators: 4, Ready: true, Age: time.Hour}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := cluster(nil, nil, []inventory.NodeView{tc.node})
			cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}
			if got := find(t, check.UnusedNode{}, cl); len(got) != 0 {
				t.Fatalf("got %d findings; this is a normal state that looks like waste "+
					"from a distance, and reporting it costs more trust than it is worth", len(got))
			}
		})
	}
}

func TestUnusedNodeNamesBlockersButNeverDaemonSets(t *testing.T) {
	nodes := []inventory.NodeView{
		{Name: "l4-0", Pool: "l4", Accelerators: 4, Ready: true, Age: 20 * 24 * time.Hour},
	}
	blocker := runningPod("monitoring", "log-shipper", "l4-0", 0)
	blocker.Evictable = false
	blocker.BlockReason = "annotated cluster-autoscaler.kubernetes.io/safe-to-evict: false"

	ds := runningPod("gpu-operator", "dcgm-exporter", "l4-0", 0)
	ds.IsDaemonSet = true
	ds.Evictable = false
	ds.BlockReason = "part of a DaemonSet"

	cl := cluster(nil, []inventory.PodView{blocker, ds}, nodes)
	cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if len(got[0].Blockers) != 1 {
		t.Fatalf("got %d blockers, want 1: the autoscaler ignores DaemonSets, so naming "+
			"them would fire on every node in the cluster", len(got[0].Blockers))
	}
	if got[0].Blockers[0].Object != "monitoring/log-shipper" {
		t.Fatalf("named %q as the blocker", got[0].Blockers[0].Object)
	}
}

func TestUnusedNodeIgnoresOccupiedNodes(t *testing.T) {
	nodes := []inventory.NodeView{
		{Name: "gpu-0", Pool: "gpu", Accelerators: 4, Ready: true, Age: 20 * 24 * time.Hour},
	}
	// A DRA pod requests no extended resource at all, so its accelerator count
	// comes from its ResourceClaim. If that plumbing breaks, this node reads as
	// empty and gets recommended for deletion while it is fully in use.
	pod := runningPod("research", "dra-workload", "gpu-0", 4)
	cl := cluster(nil, []inventory.PodView{pod}, nodes)
	cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

	if got := find(t, check.UnusedNode{}, cl); len(got) != 0 {
		t.Fatalf("got %d findings for a node holding a workload", len(got))
	}
}

func TestStuckPodNeedsAWedgedState(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*inventory.PodView)
		found  bool
	}{
		{
			name: "crash looping",
			mutate: func(p *inventory.PodView) {
				p.WedgedReason = "CrashLoopBackOff"
				p.Restarts = 148
			},
			found: true,
		},
		{
			name:   "image pull failure",
			mutate: func(p *inventory.PodView) { p.WedgedReason = "ImagePullBackOff" },
			found:  true,
		},
		{
			name:   "healthy",
			mutate: func(p *inventory.PodView) {},
			found:  false,
		},
		{
			// Pulling model weights routinely takes hours while legitimately
			// holding the device.
			name: "initialising inside the grace period",
			mutate: func(p *inventory.PodView) {
				p.Initialising = true
				s := now.Add(-30 * time.Minute)
				p.StartTime = &s
			},
			found: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := runningPod("serving", "embed", "node-a", 1)
			tc.mutate(&pod)
			cl := cluster(nil, []inventory.PodView{pod}, nil)
			if got := find(t, check.StuckPod{}, cl); (len(got) > 0) != tc.found {
				t.Fatalf("got %d findings, want found=%v", len(got), tc.found)
			}
		})
	}
}

// TestEveryCheckDescribesItself enforces the contract that makes a new check
// safe to merge: it must say what it claims and what could go wrong if the
// reader acts on it. A check with nothing to warn about has not thought about
// being wrong.
func TestEveryCheckDescribesItself(t *testing.T) {
	all := check.All()
	if len(all) == 0 {
		t.Fatal("no checks are registered")
	}
	seen := map[string]bool{}
	for _, c := range all {
		d := c.Describe()
		switch {
		case d.ID == "":
			t.Errorf("%T has no ID", c)
		case d.Title == "":
			t.Errorf("%s has no title", d.ID)
		case d.Claim == "":
			t.Errorf("%s does not state what it claims", d.ID)
		case d.Risk == "":
			t.Errorf("%s does not state the risk of acting on it", d.ID)
		case d.Docs == "":
			t.Errorf("%s has no documentation link", d.ID)
		}
		if seen[d.ID] {
			t.Errorf("duplicate check id %q", d.ID)
		}
		seen[d.ID] = true
	}
}

func ptr[T any](v T) *T { return &v }

// Karpenter is the other half of the autoscaling world, and on AWS it is
// increasingly the default. It has no minimum-size concept at all, so a tool
// that only knows how to read the cluster-autoscaler ConfigMap concludes "no
// autoscaler" and stamps a hedge on every finding in the cluster.
func TestUnusedNodeUnderKarpenter(t *testing.T) {
	nodes := []inventory.NodeView{
		{Name: "gpu-0", Pool: "gpu-a10", Accelerators: 4, Ready: true, Age: 20 * 24 * time.Hour},
	}
	// Every accelerator on the node reports, and reports zero. Without series
	// the duration would be the node's age rather than a measurement, and the
	// finding would be rated low for that reason alone -- which is correct
	// behaviour, but it is not what these subtests are about.
	measured := []inventory.Device{
		device("gpu-0-0", "gpu-0", "gpu-a10", nil, idleStats(window, false)),
		device("gpu-0-1", "gpu-0", "gpu-a10", nil, idleStats(window, false)),
		device("gpu-0-2", "gpu-0", "gpu-a10", nil, idleStats(window, false)),
		device("gpu-0-3", "gpu-0", "gpu-a10", nil, idleStats(window, false)),
	}

	t.Run("an unconsolidated empty node is a stronger finding, not a hedged one", func(t *testing.T) {
		cl := cluster(measured, nil, nodes)
		cl.Autoscaler = &inventory.AutoscalerView{Kind: "karpenter", Pinned: map[string]bool{}}

		got := find(t, check.UnusedNode{}, cl)
		if len(got) != 1 {
			t.Fatalf("got %d findings, want 1", len(got))
		}
		if got[0].ByDesign {
			t.Fatal("Karpenter has no minimum size, so nothing here is reserved by design")
		}
		if got[0].Confidence != api.EvidenceHigh {
			t.Fatal("the cluster-autoscaler hedge — 'a deliberate minimum cannot be ruled out' — " +
				"is meaningless under Karpenter, where no minimum exists to rule out. Printing it " +
				"anyway would put a false caveat on every finding on every AWS cluster")
		}
	})

	t.Run("a zero-node disruption budget is Karpenter's floor", func(t *testing.T) {
		cl := cluster(measured, nil, nodes)
		cl.Autoscaler = &inventory.AutoscalerView{
			Kind:   "karpenter",
			Pinned: map[string]bool{"gpu-a10": true},
		}

		got := find(t, check.UnusedNode{}, cl)
		if len(got) != 1 {
			t.Fatalf("got %d findings, want 1", len(got))
		}
		if !got[0].ByDesign {
			t.Fatal("a NodePool whose disruption budget allows zero nodes is deliberately pinned; " +
				"it is the Karpenter equivalent of a minimum size and must not be called waste")
		}
	})

	t.Run("a scheduled hold is context, not a conclusion", func(t *testing.T) {
		cl := cluster(measured, nil, nodes)
		cl.Autoscaler = &inventory.AutoscalerView{
			Kind:      "karpenter",
			Pinned:    map[string]bool{},
			Scheduled: map[string]bool{"gpu-a10": true},
		}

		got := find(t, check.UnusedNode{}, cl)
		if len(got) != 1 {
			t.Fatalf("got %d findings, want 1", len(got))
		}
		if got[0].ByDesign {
			t.Fatal("ullage does not evaluate cron windows, so it must not claim the pool is " +
				"deliberately held right now")
		}
		if got[0].Confidence == api.EvidenceHigh {
			t.Fatal("consolidation may simply be paused by the schedule, which the finding " +
				"cannot see; that uncertainty belongs in the confidence")
		}
	})
}

// The worst answer this tool can give is "delete this", about hardware that is
// running at capacity. Both regressions below produced exactly that.
func TestUnusedNodeNeverRecommendsDeletingBusyHardware(t *testing.T) {
	t.Run("a MIG node packed with slice-holding pods is not empty", func(t *testing.T) {
		// Under the MIG mixed strategy a pod requests nvidia.com/mig-1g.5gb and
		// never nvidia.com/gpu, so its whole-device count is zero. A check that
		// asks "does any pod hold a device here?" concludes a fully subscribed
		// MIG node has nothing on it at all.
		pod := runningPod("research", "mig-tenant", "mig-0", 0)
		pod.Slices, pod.SliceRes = 1, "nvidia.com/mig-1g.5gb"

		nodes := []inventory.NodeView{
			{Name: "mig-0", Pool: "a100-mig", Accelerators: 2, Ready: true,
				Age: 30 * 24 * time.Hour, Allocation: api.AllocMIG},
		}
		cl := cluster(nil, []inventory.PodView{pod}, nodes)
		cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

		if got := find(t, check.UnusedNode{}, cl); len(got) != 0 {
			t.Fatalf("got %d findings for a MIG node running at capacity; "+
				"this recommends deleting hardware that is in use, which is worse "+
				"than reporting nothing at all", len(got))
		}
	})

	t.Run("measured work overrides an empty pod list", func(t *testing.T) {
		// A batch pool empty at this instant but busy an hour ago is not idle
		// capacity. The pods are gone, so only the metrics can say so.
		nodes := []inventory.NodeView{
			{Name: "batch-0", Pool: "batch", Accelerators: 4, Ready: true, Age: 30 * 24 * time.Hour},
		}
		devices := []inventory.Device{
			device("batch-0/0", "batch-0", "batch", nil, idleStats(time.Hour, true)),
		}
		cl := cluster(devices, nil, nodes)
		cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

		if got := find(t, check.UnusedNode{}, cl); len(got) != 0 {
			t.Fatalf("got %d findings for a pool that ran work an hour ago; a snapshot "+
				"of the pod list is not evidence of a fortnight of idleness", len(got))
		}
	})

	t.Run("the reported duration is measured, not the age of the node", func(t *testing.T) {
		// A month-old node whose accelerators last did work five days ago has
		// been fallow for five days, not thirty. Billing the node's whole life
		// as waste inflates the one number a reader can check by hand.
		nodes := []inventory.NodeView{
			{Name: "gpu-0", Pool: "gpu", Accelerators: 4, Ready: true, Age: 30 * 24 * time.Hour},
		}
		devices := []inventory.Device{
			device("gpu-0/0", "gpu-0", "gpu", nil, idleStats(5*24*time.Hour, true)),
		}
		cl := cluster(devices, nil, nodes)
		cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

		got := find(t, check.UnusedNode{}, cl)
		if len(got) != 1 {
			t.Fatalf("got %d findings, want 1", len(got))
		}
		if got[0].Fallow != 5*24*time.Hour {
			t.Fatalf("fallow duration %s, want 120h: the node age is an upper bound on how "+
				"long it could have been empty, not a measurement of how long it was",
				got[0].Fallow)
		}
	})

	t.Run("an unmeasured node says so rather than passing age off as evidence", func(t *testing.T) {
		nodes := []inventory.NodeView{
			{Name: "gpu-0", Pool: "gpu", Accelerators: 4, Ready: true, Age: 30 * 24 * time.Hour},
		}
		cl := cluster(nil, nil, nodes)
		cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

		got := find(t, check.UnusedNode{}, cl)
		if len(got) != 1 {
			t.Fatalf("got %d findings, want 1", len(got))
		}
		var said bool
		for _, n := range got[0].Evidence.Notes {
			if strings.Contains(n, "not a measurement") {
				said = true
			}
		}
		if !said {
			t.Fatalf("with no metrics the duration is the node's age, and the evidence must "+
				"say so; presenting an upper bound as an observation is the overstatement "+
				"this tool exists to avoid. notes=%q", got[0].Evidence.Notes)
		}
		// The rendered evidence must not contradict the note it sits beside.
		if got[0].Evidence.SampleCompleteness != 0 {
			t.Fatalf("SampleCompleteness=%v on a node with no series at all; `explain` prints "+
				"this as a coverage percentage, so a non-zero value here tells the reader the "+
				"opposite of the note directly above it",
				got[0].Evidence.SampleCompleteness)
		}
	})
}

// Under-observed capacity is the failure mode that survived the first fix for
// this: the node-age overstatement was removed, and then reappeared as a
// "measured" duration derived from almost no samples at all.
func TestUnusedNodeDoesNotCallOneSampleAMeasurement(t *testing.T) {
	t.Run("a barely-sampled zero does not become a measured fortnight", func(t *testing.T) {
		// One zero sample out of a fortnight. The device is not evidence of
		// anything; treating it as a full window of observed idleness turns a
		// monitoring gap into a scale-to-zero recommendation.
		sparse := inventory.Stats{
			Samples:        1,
			Completeness:   1.0 / 40320.0,
			ZeroThroughout: true,
			FallowSince:    now.Add(-window),
		}
		nodes := []inventory.NodeView{
			{Name: "gpu-0", Pool: "gpu", Accelerators: 4, Ready: true, Age: 30 * 24 * time.Hour},
		}
		devices := []inventory.Device{device("gpu-0/0", "gpu-0", "gpu", nil, sparse)}
		cl := cluster(devices, nil, nodes)
		cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

		got := find(t, check.UnusedNode{}, cl)
		if len(got) != 1 {
			t.Fatalf("got %d findings, want 1", len(got))
		}
		for _, n := range got[0].Evidence.Notes {
			if strings.Contains(n, "measured on these accelerators") {
				t.Fatal("a single sample was reported as a measured trailing run of zero " +
					"utilization; sample coverage is what makes a zero mean anything, and " +
					"without it this is the node's age wearing a measurement's label")
			}
		}
	})

	t.Run("a well-sampled zero is still measured", func(t *testing.T) {
		// The gate must not swallow the real case it is protecting.
		nodes := []inventory.NodeView{
			{Name: "gpu-0", Pool: "gpu", Accelerators: 4, Ready: true, Age: 30 * 24 * time.Hour},
		}
		devices := []inventory.Device{
			device("gpu-0/0", "gpu-0", "gpu", nil, idleStats(5*24*time.Hour, true)),
		}
		cl := cluster(devices, nil, nodes)
		cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

		got := find(t, check.UnusedNode{}, cl)
		if len(got) != 1 || got[0].Fallow != 5*24*time.Hour {
			t.Fatalf("well-sampled node lost its measured duration: %+v", got)
		}
	})

	t.Run("one busy device keeps the whole node off the list", func(t *testing.T) {
		// Three idle accelerators and one working one is a node in use. The
		// per-node answer has to be the minimum across its devices; any other
		// combination lets a busy node be recommended for deletion.
		nodes := []inventory.NodeView{
			{Name: "gpu-0", Pool: "gpu", Accelerators: 4, Ready: true, Age: 30 * 24 * time.Hour},
		}
		devices := []inventory.Device{
			device("gpu-0/0", "gpu-0", "gpu", nil, idleStats(10*24*time.Hour, true)),
			device("gpu-0/1", "gpu-0", "gpu", nil, idleStats(10*24*time.Hour, true)),
			device("gpu-0/2", "gpu-0", "gpu", nil, idleStats(10*24*time.Hour, true)),
			device("gpu-0/3", "gpu-0", "gpu", nil, idleStats(time.Minute, true)),
		}
		cl := cluster(devices, nil, nodes)
		cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

		if got := find(t, check.UnusedNode{}, cl); len(got) != 0 {
			t.Fatalf("got %d findings for a node with an accelerator working a minute ago", len(got))
		}
	})
}

// One Kubernetes pool is routinely several autoscaler node groups — AKS makes
// one VMSS per zone, EKS one ASG per zone — and GPU capacity is zone
// constrained, so this is the normal shape rather than an edge case.
func TestAutoscalerFloorSumsZonalNodeGroups(t *testing.T) {
	a := &inventory.AutoscalerView{Pools: []string{"h100", "other"}, Floors: map[string]int{
		"aks-h100-12345678-vmss-eastus2-1": 0,
		"aks-h100-12345678-vmss-eastus2-2": 0,
		"aks-h100-12345678-vmss-eastus2-3": 2,
		"aks-other-87654321-vmss":          9,
	}}
	got, ok := a.Floor("h100")
	if !ok {
		t.Fatal("the pool's node groups were not matched at all")
	}
	if got != 2 {
		t.Fatalf("floor %d, want 2: picking one zone's minimum instead of summing them "+
			"either calls reserved capacity waste (when the zero wins) or overstates the "+
			"reservation threefold (when the two does)", got)
	}
}

// Pool names nest. Summing the floors of every node group whose name contains
// the pool sums pool "gpu-big" into pool "gpu", which is worse than picking
// one — and it fails silently, in the direction of hiding real waste.
func TestAutoscalerFloorDoesNotStealAnotherPoolsNodeGroups(t *testing.T) {
	a := &inventory.AutoscalerView{
		Pools: []string{"gpu", "gpu-big"},
		Floors: map[string]int{
			"aks-gpu-11111111-vmss":     1,
			"aks-gpu-big-22222222-vmss": 4,
		},
	}
	if got, _ := a.Floor("gpu"); got != 1 {
		t.Fatalf("floor for pool gpu is %d, want 1: the gpu-big node group belongs to "+
			"pool gpu-big, and counting its floor here overstates what is reserved and "+
			"hides genuinely idle capacity", got)
	}
	if got, _ := a.Floor("gpu-big"); got != 4 {
		t.Fatalf("floor for pool gpu-big is %d, want 4", got)
	}
}

// A pool reports the longest fallow duration across its nodes, and the evidence
// label has to describe the node that duration came from. Recording only that
// *some* node in the pool was measured lets a well-observed node vouch for an
// unobserved sibling's age — which republishes, at pool level, the exact
// overstatement the per-device completeness gate was added to remove. Partial
// dcgm-exporter coverage inside one pool is common.
func TestOneNodesMeasurementDoesNotVouchForAnothersAge(t *testing.T) {
	nodes := []inventory.NodeView{
		{Name: "gpu-measured", Pool: "mixed", Accelerators: 1, Ready: true,
			Allocation: api.AllocExclusive, Age: 100 * time.Hour},
		// No device series at all, and much older.
		{Name: "gpu-dark", Pool: "mixed", Accelerators: 1, Ready: true,
			Allocation: api.AllocExclusive, Age: 480 * time.Hour},
	}
	devices := []inventory.Device{
		device("gpu-measured/0", "gpu-measured", "mixed", nil, idleStats(100*time.Hour, false)),
	}

	cl := cluster(devices, nil, nodes)
	cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	f := got[0]

	var claimsMeasured bool
	for _, n := range f.Evidence.Notes {
		if strings.Contains(n, "measured on") {
			claimsMeasured = true
		}
	}
	if claimsMeasured {
		t.Fatalf("the reported duration (%s) is gpu-dark's age — it produced no samples at "+
			"all — yet the evidence calls it measured. notes=%q",
			f.Fallow, f.Evidence.Notes)
	}
	if f.Evidence.SampleCompleteness != 0 {
		t.Fatalf("SampleCompleteness=%v; the duration came from a node with no series",
			f.Evidence.SampleCompleteness)
	}

	var saidUneven bool
	for _, n := range f.Evidence.Notes {
		if strings.Contains(n, "uneven") {
			saidUneven = true
		}
	}
	if !saidUneven {
		t.Fatalf("a pool with one measured and one unmeasured node should say coverage is "+
			"uneven, so the reader knows the pool was not observed uniformly. notes=%q",
			f.Evidence.Notes)
	}
}

// Three GPUs that produced no series at all cannot be vouched for by the
// fourth: "no accelerator here did any work" is only a measurement if every
// accelerator here was actually watched.
func TestPartiallyObservedNodeDoesNotClaimAWholeNodeMeasurement(t *testing.T) {
	nodes := []inventory.NodeView{
		{Name: "gpu-0", Pool: "gpu", Accelerators: 4, Ready: true,
			Allocation: api.AllocExclusive, Age: 30 * 24 * time.Hour},
	}
	devices := []inventory.Device{
		device("gpu-0/0", "gpu-0", "gpu", nil, idleStats(100*time.Hour, false)),
	}

	cl := cluster(devices, nil, nodes)
	cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	for _, n := range got[0].Evidence.Notes {
		if strings.Contains(n, "measured on") {
			t.Fatalf("1 of 4 accelerators was observed, but the finding claims the node was "+
				"measured. notes=%q", got[0].Evidence.Notes)
		}
	}
}

// Under MIG the node advertises far more units than DCGM has series for, so
// demanding parity between them would permanently deny a truthful label.
func TestMIGNodeCanStillReportAMeasurement(t *testing.T) {
	nodes := []inventory.NodeView{
		{Name: "mig-0", Pool: "mig", Accelerators: 28, Ready: true,
			Allocation: api.AllocMIG, Age: 30 * 24 * time.Hour},
	}
	devices := []inventory.Device{
		device("mig-0/0", "mig-0", "mig", nil, idleStats(100*time.Hour, false)),
	}
	devices[0].Allocation = api.AllocMIG

	cl := cluster(devices, nil, nodes)
	cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if got[0].Fallow != 100*time.Hour {
		t.Fatalf("fallow=%s, want the measured 100h; 28 advertised MIG slices are not 28 "+
			"cards to observe", got[0].Fallow)
	}
}

// Coverage judged against the scan window makes the tool blind to every young
// pod. A pod that has existed for two days of a fortnight can never exceed
// about 14% window coverage however completely it was watched, so a fixed
// threshold silently discards it — and "someone started an expensive pod last
// week and forgot about it" is both the most actionable thing this tool finds
// and the first thing anyone evaluating it will try.
func TestYoungIdlePodIsJudgedOnItsOwnLifetime(t *testing.T) {
	// Five days: comfortably past the 72h idle threshold, but only 36% of a
	// fortnight — so a window-relative coverage gate of 0.80 discards it.
	const lived = 5 * 24 * time.Hour

	pod := runningPod("ml", "forgotten-notebook", "gpu-0", 1)
	start := now.Add(-lived)
	pod.StartTime = &start

	// Fully observed for every minute it has existed, at a 30s scrape.
	util := idleStats(lived, false)
	util.Samples = int(lived / scan.ScrapeInterval)
	// Its coverage of the 14-day window is only 2/14 — which is the whole
	// point. The finding must survive that.
	util.Completeness = float64(lived) / float64(window)

	devices := []inventory.Device{
		device("gpu-0/0", "gpu-0", "gpu", &pod.Ref, util),
	}
	nodes := []inventory.NodeView{
		{Name: "gpu-0", Pool: "gpu", Accelerators: 1, Ready: true,
			Allocation: api.AllocExclusive, Age: 30 * 24 * time.Hour},
	}

	got := find(t, check.IdlePod{}, cluster(devices, []inventory.PodView{pod}, nodes))
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: a five-day-old pod, idle and fully observed for "+
			"every minute of its life, was discarded because five days is a small fraction "+
			"of a fortnight", len(got))
	}
	if got[0].Fallow > lived {
		t.Fatalf("fallow=%s but the pod has only existed for %s; a pod cannot have been idle "+
			"for longer than it has existed", got[0].Fallow, lived)
	}
}

// The counterpart: a pod nobody was watching must still be refused. The
// lifetime denominator must not become a way to launder thin evidence.
func TestBarelyObservedPodIsStillRefused(t *testing.T) {
	const lived = 5 * 24 * time.Hour

	pod := runningPod("ml", "unwatched", "gpu-0", 1)
	start := now.Add(-lived)
	pod.StartTime = &start

	util := idleStats(lived, false)
	util.Samples = 3 // three readings across five days
	util.Completeness = 1

	devices := []inventory.Device{device("gpu-0/0", "gpu-0", "gpu", &pod.Ref, util)}
	nodes := []inventory.NodeView{
		{Name: "gpu-0", Pool: "gpu", Accelerators: 1, Ready: true,
			Allocation: api.AllocExclusive, Age: 30 * 24 * time.Hour},
	}

	if got := find(t, check.IdlePod{}, cluster(devices, []inventory.PodView{pod}, nodes)); len(got) != 0 {
		t.Fatalf("got %d findings, want 0: three samples across five days is not evidence that "+
			"a GPU did nothing, and a lifetime-relative denominator must not launder it",
			len(got))
	}
}

// A GPU recycled from a finished job carries that job's samples on its own
// series. The new holder must be judged on its own coverage, not on the
// device's summed history.
func TestPodDoesNotBorrowCoverageFromThePreviousHolder(t *testing.T) {
	const lived = 5 * 24 * time.Hour

	pod := runningPod("ml", "new-holder", "gpu-0", 1)
	start := now.Add(-lived)
	pod.StartTime = &start

	util := idleStats(lived, false)
	util.Samples = 2 // two readings since it took the device over: thin
	// The physical device was watched all fortnight — by somebody else.
	util.Completeness = 1

	devices := []inventory.Device{device("gpu-0/0", "gpu-0", "gpu", &pod.Ref, util)}
	nodes := []inventory.NodeView{
		{Name: "gpu-0", Pool: "gpu", Accelerators: 1, Ready: true,
			Allocation: api.AllocExclusive, Age: 30 * 24 * time.Hour},
	}

	if got := find(t, check.IdlePod{}, cluster(devices, []inventory.PodView{pod}, nodes)); len(got) != 0 {
		t.Fatalf("got %d findings, want 0: the device's full-window coverage belongs to the "+
			"previous holder, and this pod has two samples of its own", len(got))
	}
}

// dcgm-exporter stamps the holding pod onto the series, the series lingers in
// Prometheus after the holder goes away, and pod names repeat constantly under
// StatefulSets and Jobs. A series claiming a pod held a device on a node the
// pod is not running on is stale, and believing it attributes a finished job's
// device to a running namesake.
//
// Found live: it made a GPU sitting at 78% utilization report as a pod that had
// done no work since the window began.
func TestMetricsCannotAttributeADeviceOnAnotherNode(t *testing.T) {
	pod := runningPod("ml", "llama-train-0", "gpu-0", 1)

	busy := device("gpu-0/0", "gpu-0", "gpu", &pod.Ref, idleStats(0, true))
	// A namesake's leftover series on a node this pod has never been on.
	stale := device("gpu-9/0", "gpu-9", "gpu", &pod.Ref, idleStats(9*24*time.Hour, true))

	cl := cluster([]inventory.Device{busy, stale}, []inventory.PodView{pod}, nil)

	got := cl.DevicesOf(pod.Ref)
	if len(got) != 1 {
		t.Fatalf("DevicesOf returned %d devices for a pod holding one GPU on gpu-0; a series "+
			"from gpu-9 cannot describe a pod that is not running there", len(got))
	}
	if got[0].Node != "gpu-0" {
		t.Fatalf("attributed %s, want the device on the pod's own node", got[0].Node)
	}

	if f := find(t, check.IdlePod{}, cl); len(f) != 0 {
		t.Fatalf("got %d findings: the pod's own GPU is at work, and only a stale namesake "+
			"series made it look idle", len(f))
	}
}

// An exporter that dies, a node that leaves, or a holder label that stops being
// emitted all present as a device that has been perfectly idle ever since.
// That is a recommendation to delete hardware, generated by the monitoring
// breaking.
func TestAStoppedSeriesIsUnknownNotIdle(t *testing.T) {
	pod := runningPod("ml", "notebook", "gpu-0", 1)
	util := idleStats(9*24*time.Hour, false)
	util.Stale = true
	last := now.Add(-9 * 24 * time.Hour)
	util.LastSample = &last

	devices := []inventory.Device{device("gpu-0/0", "gpu-0", "gpu", &pod.Ref, util)}
	nodes := []inventory.NodeView{
		{Name: "gpu-0", Pool: "gpu", Accelerators: 1, Ready: true,
			Allocation: api.AllocExclusive, Age: 30 * 24 * time.Hour},
	}
	cl := cluster(devices, []inventory.PodView{pod}, nodes)

	if got := find(t, check.IdlePod{}, cl); len(got) != 0 {
		t.Fatalf("got %d findings from a series that stopped reporting nine days ago; the "+
			"device was not measured to be idle, it stopped being measured at all", len(got))
	}
}

// The same rule at node level: a stopped series must not support a measured
// claim, but must not hide the node either.
func TestStaleDeviceLeavesTheNodeOnTheWeakerPath(t *testing.T) {
	util := idleStats(9*24*time.Hour, false)
	util.Stale = true

	devices := []inventory.Device{device("gpu-0/0", "gpu-0", "gpu", nil, util)}
	nodes := []inventory.NodeView{
		{Name: "gpu-0", Pool: "gpu", Accelerators: 1, Ready: true,
			Allocation: api.AllocExclusive, Age: 30 * 24 * time.Hour},
	}
	cl := cluster(devices, nil, nodes)
	cl.Autoscaler = &inventory.AutoscalerView{Floors: map[string]int{}}

	got := find(t, check.UnusedNode{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: an empty node must still be reported, just with "+
			"weaker evidence", len(got))
	}
	for _, n := range got[0].Evidence.Notes {
		if strings.Contains(n, "measured on") {
			t.Fatalf("a stopped series was presented as a measurement. notes=%q",
				got[0].Evidence.Notes)
		}
	}
}

// Max comes from an aggregate query over the window; LastNonZero comes from a
// stepped range query that is downsampled and chunked. When the aggregate
// proves the device did work and the shape cannot say when, they disagree —
// and only one of the two readings ends in a recommendation to delete
// something.
//
// Found live at a 14-day window: the range query exceeded Prometheus's point
// limit, the shape came back without its non-zero samples, and a GPU running
// at 78% utilization was reported as idle by a finding whose own evidence
// block printed "peak utilization 78%".
func TestAProvenPeakBeatsAShapeThatCannotSeeIt(t *testing.T) {
	pod := runningPod("ml", "llama-train-0", "gpu-0", 1)

	util := idleStats(9*24*time.Hour, false)
	util.Max = 0.78 // the aggregate saw work
	util.LastNonZero = nil

	cl := cluster(
		[]inventory.Device{device("gpu-0/0", "gpu-0", "gpu", &pod.Ref, util)},
		[]inventory.PodView{pod}, nil)

	if got := find(t, check.IdlePod{}, cl); len(got) != 0 {
		t.Fatalf("reported %d findings for a device the aggregate measured at 78%% peak "+
			"utilization; the shape's silence is missing data, not proof of idleness",
			len(got))
	}
}

// The completeness printed in the evidence must be the completeness the gate
// judged. Deriving it a second way put "0.4% coverage" next to a claim that
// had just cleared an 80% bar.
func TestReportedCoverageIsTheCoverageThatWasJudged(t *testing.T) {
	started := now.Add(-4 * 24 * time.Hour)
	pod := runningPod("ml", "notebook", "gpu-0", 1)
	pod.StartTime = &started

	util := idleStats(4*24*time.Hour, false)
	// Fully covered for its own life, but a small slice of a 14-day window.
	util.Samples = int(((4 * 24 * time.Hour) / (30 * time.Second)))
	util.Completeness = 4.0 / 14.0

	cl := cluster(
		[]inventory.Device{device("gpu-0/0", "gpu-0", "gpu", &pod.Ref, util)},
		[]inventory.PodView{pod}, nil)
	cl.Window = 14 * 24 * time.Hour

	got := find(t, check.IdlePod{}, cl)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1 for a four-day-old pod idle its whole life", len(got))
	}
	if c := got[0].Evidence.SampleCompleteness; c < 0.8 {
		t.Fatalf("evidence reports %.3f coverage on a finding that passed an 80%% gate; "+
			"the number shown must be the number judged", c)
	}
}

// Summaries are the one sentence most people read about a finding, and three
// of them interpolated a count straight into a hard-coded plural noun -- "1
// accelerators held with no work". A helper for this already existed; the
// summaries just did not use it.
//
// This matches the pattern in the source rather than the rendered output, so a
// check added later is covered without anyone remembering to write a fixture
// that happens to produce a count of one.
func TestSummariesAgreeInNumber(t *testing.T) {
	pattern := regexp.MustCompile(`"[^"]*%d (accelerators|pods|nodes|devices)\b[^"]*"`)

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range pattern.FindAllString(string(src), -1) {
			t.Errorf("%s: %s pairs %%d with a fixed plural; use humanize.Plural",
				filepath.Base(path), m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
