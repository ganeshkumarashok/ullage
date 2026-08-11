package demo

import (
	"fmt"
	"sort"
	"time"
)

// Build constructs the demo cluster.
//
// Every scenario here exists to exercise one decision the pipeline makes, and
// several exist specifically to prove ullage does NOT fire where a naive tool
// would. A demo that only shows findings proves the tool can produce output; a
// demo that shows correct silences proves it can be trusted.
func Build(now time.Time) *Cluster {
	c := &Cluster{
		Now:    now,
		series: []*seriesSpec{},
		floors: map[string]int{"h100-reserve": 2},
	}
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	// ---- Pools -------------------------------------------------------------

	// a100-research: exclusive A100s, where most findings live.
	c.addPool("a100-research", "NVIDIA-A100-SXM4-80GB", 4, 4, ago(60*24*time.Hour), nil)
	// h100-train: exclusive H100s running real work.
	c.addPool("h100-train", "NVIDIA-H100-SXM5-80GB", 2, 8, ago(45*24*time.Hour), nil)
	// h100-reserve: empty on purpose, held by an autoscaler floor.
	c.addPool("h100-reserve", "NVIDIA-H100-SXM5-80GB", 2, 8, ago(30*24*time.Hour), nil)
	// l4-serving: empty, and something is blocking scale-down.
	c.addPool("l4-serving", "NVIDIA-L4", 2, 4, ago(21*24*time.Hour), nil)
	// t4-shared: time-sliced, so per-pod claims are refused.
	c.addPool("t4-shared", "Tesla-T4", 1, 2, ago(40*24*time.Hour), map[string]string{
		"nvidia.com/gpu.replicas":         "4",
		"nvidia.com/gpu.sharing-strategy": "time-slicing",
	})
	// a100-mig: MIG, also refused.
	c.addPool("a100-mig", "NVIDIA-A100-SXM4-40GB", 1, 2, ago(35*24*time.Hour), map[string]string{
		"nvidia.com/mig.capable":  "true",
		"nvidia.com/mig.strategy": "mixed",
		"nvidia.com/mig.config":   "all-1g.5gb",
	})
	// l40s-dra: allocated through DRA, which ullage does analyse.
	c.addDRAPool("l40s-dra", "NVIDIA-L40S", 1, 4, ago(15*24*time.Hour))
	// a10-new: hardware present, driver still initialising.
	c.addInitialisingPool("a10-new", "NVIDIA-A10", 1, 4, ago(20*time.Minute))

	// ---- Scenario 1: the headline ------------------------------------------
	// A StatefulSet-owned notebook, three pods, nothing for a fortnight.
	// A naive tool prints `kubectl delete pod`, the StatefulSet recreates them
	// within seconds, and nothing is freed. ullage resolves the root owner and
	// targets the controller instead.
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("jupyter-alice-%d", i)
		c.addPod(pod{
			namespace: "research", name: name, node: "a100-research-0", gpus: 1,
			phase: "Running", started: ago(19 * 24 * time.Hour),
			labels: map[string]string{"app": "jupyter"},
			annotations: map[string]string{
				"ullage.dev/owner": "alice@example.com",
			},
			owner: &ownerRef{kind: "StatefulSet", name: "jupyter-alice", apiVersion: "apps/v1"},
		})
		c.addSeries("a100-research-0", gpuIndex(i), "NVIDIA-A100-SXM4-80GB", 400,
			"research", name, idleSince(19*24*time.Hour))
	}

	// ---- Scenario 2: a bare pod, where delete genuinely is correct ---------
	c.addPod(pod{
		namespace: "research", name: "scratch-pod-bob", node: "a100-research-1", gpus: 2,
		phase: "Running", started: ago(9 * 24 * time.Hour),
		labels: map[string]string{"owner": "bob@example.com"},
	})
	c.addSeries("a100-research-1", "0", "NVIDIA-A100-SXM4-80GB", 400,
		"research", "scratch-pod-bob", idleSince(9*24*time.Hour))
	c.addSeries("a100-research-1", "1", "NVIDIA-A100-SXM4-80GB", 400,
		"research", "scratch-pod-bob", idleSince(9*24*time.Hour))

	// ---- Scenario 3: an unrecognised CRD -----------------------------------
	// Owned by a Notebook (kubeflow.org/v1). ullage does not know what deleting
	// a Notebook does, so it names the resource and emits no command at all.
	// Refusing to guess is a stronger trust signal than the command would be.
	c.addPod(pod{
		namespace: "ml-platform", name: "finetune-notebook-carol-0", node: "a100-research-2", gpus: 1,
		phase: "Running", started: ago(16 * 24 * time.Hour),
		owner: &ownerRef{kind: "Notebook", name: "finetune-carol", apiVersion: "kubeflow.org/v1"},
	})
	c.addSeries("a100-research-2", "0", "NVIDIA-A100-SXM4-80GB", 400,
		"ml-platform", "finetune-notebook-carol-0", idleSince(16*24*time.Hour))

	// ---- Scenario 4: genuinely busy — must NOT be flagged ------------------
	for i := 0; i < 8; i++ {
		c.addSeries("h100-train-0", gpuIndex(i), "NVIDIA-H100-SXM5-80GB", 700,
			"training", "llama-pretrain-0", busy(92))
	}
	c.addPod(pod{
		namespace: "training", name: "llama-pretrain-0", node: "h100-train-0", gpus: 8,
		phase: "Running", started: ago(12 * 24 * time.Hour),
		labels: map[string]string{"owner": "training-team"},
		owner:  &ownerRef{kind: "Job", name: "llama-pretrain", apiVersion: "batch/v1"},
	})

	// Job churn: an earlier job held GPU 0 of this node and finished. Its
	// samples keep the pod label they were scraped with, so this one physical
	// device returns two series over the window.
	//
	// Present because it is what every real cluster looks like at fourteen
	// days, and because a fixture that emits exactly one series per device
	// makes the census look correct while it double-counts. Any code that
	// treats a metric series as a physical accelerator breaks here.
	churn := c.addSeries("h100-train-0", "0", "NVIDIA-H100-SXM5-80GB", 700,
		"training", "llama-pretrain-prev-0", busy(90))
	churn.gapFrom = ago(9 * 24 * time.Hour)
	churn.gapTo = ago(0)

	// ---- Scenario 5: low average, but not idle — must NOT be flagged -------
	// Mean utilization around 4%. A tool thresholding on "average util < 5%"
	// flags this and is wrong: the workload runs in bursts and its owner would
	// lose real work. The strict-zero rule is what prevents the false positive.
	for i := 0; i < 8; i++ {
		c.addSeries("h100-train-1", gpuIndex(i), "NVIDIA-H100-SXM5-80GB", 700,
			"training", "dataprep-worker-0", bursty())
	}
	c.addPod(pod{
		namespace: "training", name: "dataprep-worker-0", node: "h100-train-1", gpus: 8,
		phase: "Running", started: ago(20 * 24 * time.Hour),
		labels: map[string]string{"owner": "training-team"},
		owner:  &ownerRef{kind: "Deployment", name: "dataprep", apiVersion: "apps/v1"},
	})

	// ---- Scenario 6: a crash loop, resolved to the root owner --------------
	// Pod → ReplicaSet → Deployment. Scaling the ReplicaSet would be undone by
	// its Deployment on the next reconcile, so the root is what matters.
	c.addPod(pod{
		namespace: "serving", name: "embed-v2-7d9f8-abcde", node: "a100-research-3", gpus: 1,
		phase: "Running", started: ago(4 * 24 * time.Hour), restarts: 148,
		waiting:    "CrashLoopBackOff",
		terminated: &terminated{reason: "OOMKilled", exitCode: 137, finished: ago(11 * time.Minute)},
		owner: &ownerRef{
			kind: "ReplicaSet", name: "embed-v2-7d9f8", apiVersion: "apps/v1",
			root: &ownerRef{kind: "Deployment", name: "embed-v2", apiVersion: "apps/v1"},
		},
	})

	// ---- Scenario 7: unmet demand, shown as context, never as a finding ----
	// These pods hold nothing. They are the reason the idle capacity matters.
	for i := 0; i < 4; i++ {
		c.addPod(pod{
			namespace: "research", name: fmt.Sprintf("sweep-worker-%d", i),
			gpus: 1, phase: "Pending",
			labels: map[string]string{"owner": "dana@example.com"},
			owner:  &ownerRef{kind: "Job", name: "hparam-sweep", apiVersion: "batch/v1"},
		})
	}

	// ---- Scenario 8: what blocks the autoscaler ---------------------------
	// The l4-serving pool is empty, and two pods are why it cannot be reclaimed.
	// This is the finding no dashboard produces: causal, not descriptive.
	c.addPod(pod{
		namespace: "monitoring", name: "log-shipper-l4-0", node: "l4-serving-0", gpus: 0,
		phase: "Running", started: ago(21 * 24 * time.Hour),
		annotations: map[string]string{
			"cluster-autoscaler.kubernetes.io/safe-to-evict": "false",
		},
		owner: &ownerRef{kind: "Deployment", name: "log-shipper", apiVersion: "apps/v1"},
	})
	c.addPod(pod{
		namespace: "monitoring", name: "metrics-agent-l4-1", node: "l4-serving-1", gpus: 0,
		phase: "Running", started: ago(21 * 24 * time.Hour),
		annotations: map[string]string{
			"cluster-autoscaler.kubernetes.io/safe-to-evict": "false",
		},
		owner: &ownerRef{kind: "Deployment", name: "metrics-agent", apiVersion: "apps/v1"},
	})
	// Infrastructure DaemonSets are on every node and must never be reported as
	// blockers: the autoscaler ignores them, so naming them would be a false
	// alarm on every node in the cluster.
	for _, n := range []string{"l4-serving-0", "l4-serving-1", "h100-reserve-0"} {
		c.addPod(pod{
			namespace: "gpu-operator", name: "dcgm-exporter-" + n, node: n, gpus: 0,
			phase: "Running", started: ago(21 * 24 * time.Hour),
			owner: &ownerRef{kind: "DaemonSet", name: "dcgm-exporter", apiVersion: "apps/v1"},
		})
	}

	// ---- Scenario 9: shared devices, where no per-pod claim is possible ----
	c.addPod(pod{
		namespace: "inference", name: "batch-scorer-0", node: "t4-shared-0", gpus: 1,
		phase: "Running", started: ago(30 * 24 * time.Hour),
		owner: &ownerRef{kind: "Deployment", name: "batch-scorer", apiVersion: "apps/v1"},
	})
	c.addSeries("t4-shared-0", "0", "Tesla-T4", 70, "inference", "batch-scorer-0", busy(60))
	c.addSeries("a100-mig-0", "0", "NVIDIA-A100-SXM4-40GB", 400, "", "", busy(30))
	// Two tenants holding MIG profiles on that node. Under the mixed strategy
	// they request nvidia.com/mig-1g.5gb and never nvidia.com/gpu, so a
	// whole-device count reads this node — which is subscribed and busy — as
	// completely empty, and recommends deleting it.
	for _, name := range []string{"mig-tenant-a", "mig-tenant-b"} {
		c.addPod(pod{
			namespace: "research", name: name, node: "a100-mig-0", gpus: 0,
			migSlices: 1, migProfile: "1g.5gb",
			phase: "Running", started: ago(12 * 24 * time.Hour), uid: "uid-" + name,
			owner: &ownerRef{kind: "Deployment", name: name, apiVersion: "apps/v1"},
		})
	}

	// ---- Scenario 10: DRA, which ullage does analyse ----------------------
	// DRA went GA in Kubernetes 1.34, and a claim reserves whole devices, so
	// exclusivity holds and an idleness claim is supportable. What DRA changes
	// is discovery, not exclusivity.
	c.addPod(pod{
		namespace: "research", name: "dra-sandbox-erin", node: "l40s-dra-0", gpus: 0,
		phase: "Running", started: ago(11 * 24 * time.Hour), uid: "uid-dra-sandbox",
		draDevices: 4,
		labels:     map[string]string{"owner": "erin@example.com"},
	})
	for i := 0; i < 4; i++ {
		c.addSeries("l40s-dra-0", gpuIndex(i), "NVIDIA-L40S", 350,
			"research", "dra-sandbox-erin", idleSince(11*24*time.Hour))
	}

	// ---- Scenario 11: a scrape gap, which must not read as idle -----------
	// Samples are absent for three days, not zero. Absent is not idle, and the
	// finding is either reported at lower confidence or not at all.
	c.addPod(pod{
		namespace: "research", name: "gappy-session-frank", node: "a100-research-3", gpus: 1,
		phase: "Running", started: ago(10 * 24 * time.Hour),
	})
	gap := c.addSeries("a100-research-3", "1", "NVIDIA-A100-SXM4-80GB", 400,
		"research", "gappy-session-frank", idleSince(10*24*time.Hour))
	gap.gapFrom = ago(8 * 24 * time.Hour)
	gap.gapTo = ago(3 * 24 * time.Hour)

	// The crash-looping pod's device: zero utilization, because a container
	// that never starts never does any work.
	c.addSeries("a100-research-3", "0", "NVIDIA-A100-SXM4-80GB", 400, "", "", busy(0))

	c.fillUnallocated()
	return c
}

// fillUnallocated emits a series for every remaining physical device.
//
// dcgm-exporter reports every GPU on a node whether or not a pod holds it, so a
// fixture that only emits series for busy devices is not a fixture of a real
// cluster — it is one where two thirds of the fleet is invisible, and it hides
// exactly the accounting bug this exists to catch. An unheld device reads zero
// and carries no pod labels, which is what makes it a node-level question
// rather than a workload one.
func (c *Cluster) fillUnallocated() {
	for _, n := range c.nodes {
		physical := 0
		fmt.Sscanf(n.labels["nvidia.com/gpu.count"], "%d", &physical)
		if physical == 0 {
			physical = n.gpus
		}
		if n.labels["nvidia.com/mig.capable"] == "true" {
			continue // MIG instances are not one series per physical device
		}
		for i := 0; i < physical; i++ {
			if c.hasSeries(n.name, gpuIndex(i)) {
				continue
			}
			tdp := 400.0
			if t, ok := demoTDP[n.model]; ok {
				tdp = t
			}
			c.addSeries(n.name, gpuIndex(i), n.model, tdp, "", "", busy(0))
		}
	}
}

var demoTDP = map[string]float64{
	"NVIDIA-A100-SXM4-80GB": 400,
	"NVIDIA-A100-SXM4-40GB": 400,
	"NVIDIA-H100-SXM5-80GB": 700,
	"NVIDIA-L40S":           350,
	"NVIDIA-L4":             72,
	"NVIDIA-A10":            150,
	"Tesla-T4":              70,
}

// idleSince returns a pattern that was busy before the given point and has read
// exactly zero ever since.
func idleSince(d time.Duration) func(t, now time.Time) float64 {
	return func(t, now time.Time) float64 {
		if t.Before(now.Add(-d)) {
			return 88
		}
		return 0
	}
}

func busy(level float64) func(t, now time.Time) float64 {
	return func(t, now time.Time) float64 { return level }
}

// bursty runs hard for one hour in every twenty-four. Mean utilization is about
// four percent, which is exactly the shape that fools an average-based tool.
func bursty() func(t, now time.Time) float64 {
	return func(t, now time.Time) float64 {
		if t.Hour() == 3 {
			return 96
		}
		return 0
	}
}

func gpuIndex(i int) string { return fmt.Sprintf("%d", i) }

func (c *Cluster) addPool(pool, model string, count, gpusPerNode int, created time.Time, extra map[string]string) {
	for i := 0; i < count; i++ {
		labels := map[string]string{
			"nvidia.com/gpu.product":           model,
			"nvidia.com/gpu.count":             fmt.Sprintf("%d", gpusPerNode),
			"agentpool":                        pool,
			"kubernetes.azure.com/agentpool":   pool,
			"node.kubernetes.io/instance-type": "Standard_ND96asr_v4",
			"topology.kubernetes.io/region":    "westus3",
		}
		for k, v := range extra {
			labels[k] = v
		}
		advertised := gpusPerNode
		if r, ok := extra["nvidia.com/gpu.replicas"]; ok {
			n := 0
			fmt.Sscanf(r, "%d", &n)
			advertised = gpusPerNode * n
		}
		c.nodes = append(c.nodes, node{
			name: fmt.Sprintf("%s-%d", pool, i), pool: pool, model: model,
			gpus: advertised, labels: labels, created: created, ready: true,
		})
	}
}

func (c *Cluster) addDRAPool(pool, model string, count, gpusPerNode int, created time.Time) {
	for i := 0; i < count; i++ {
		c.nodes = append(c.nodes, node{
			name: fmt.Sprintf("%s-%d", pool, i), pool: pool, model: model,
			// No nvidia.com/gpu in allocatable: with DRA the extended resource
			// is gone entirely, which is why a census that only counts it
			// reports a cluster with no accelerators.
			gpus: 0,
			labels: map[string]string{
				"nvidia.com/gpu.product":         model,
				"nvidia.com/gpu.count":           fmt.Sprintf("%d", gpusPerNode),
				"agentpool":                      pool,
				"kubernetes.azure.com/agentpool": pool,
				"dra.nvidia.com/kubelet-plugin":  "true",
			},
			created: created, ready: true,
		})
	}
}

func (c *Cluster) addInitialisingPool(pool, model string, count, gpusPerNode int, created time.Time) {
	for i := 0; i < count; i++ {
		c.nodes = append(c.nodes, node{
			name: fmt.Sprintf("%s-%d", pool, i), pool: pool, model: model,
			gpus: 0,
			labels: map[string]string{
				"nvidia.com/gpu.product":         model,
				"nvidia.com/gpu.count":           fmt.Sprintf("%d", gpusPerNode),
				"agentpool":                      pool,
				"kubernetes.azure.com/agentpool": pool,
			},
			created: created, ready: true,
		})
	}
}

func (c *Cluster) addPod(p pod) {
	if p.uid == "" {
		p.uid = "uid-" + p.namespace + "-" + p.name
	}
	c.pods = append(c.pods, p)
}

func (c *Cluster) addSeries(host, gpu, model string, tdp float64, ns, podName string, pattern func(t, now time.Time) float64) *seriesSpec {
	s := &seriesSpec{
		host: host, gpu: gpu, model: model, powerW: tdp,
		ns: ns, pod: podName, pattern: pattern,
	}
	c.series = append(c.series, s)
	return s
}

func (c *Cluster) hasSeries(host, gpu string) bool {
	for _, s := range c.series {
		if s.host == host && s.gpu == gpu {
			return true
		}
	}
	return false
}

func (c *Cluster) sortedSeries() []*seriesSpec {
	out := append([]*seriesSpec(nil), c.series...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].host != out[j].host {
			return out[i].host < out[j].host
		}
		if out[i].gpu != out[j].gpu {
			return out[i].gpu < out[j].gpu
		}
		return out[i].pod < out[j].pod
	})
	return out
}
