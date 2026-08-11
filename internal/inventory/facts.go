package inventory

import (
	"sort"
	"time"

	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// The fact layer.
//
// Everything below is backend-neutral: no Kubernetes types, no Prometheus
// types, no wire formats. Checks read these and nothing else, which is what
// makes a check a self-contained contribution instead of a change that reaches
// into six subsystems. It is also what makes checks testable from a literal —
// a table test constructs a Cluster and asserts findings, with no server, no
// fixtures, and no HTTP.

// Cluster is the complete normalized view a check analyses.
type Cluster struct {
	Context string
	Now     time.Time
	Window  time.Duration

	Devices []Device
	Nodes   []NodeView
	Pods    []PodView

	// Autoscaler is nil when no cluster-autoscaler status could be read. Nil
	// means unknown, never "no floors exist" — a check must not read absence as
	// permission to recommend removing capacity.
	Autoscaler *AutoscalerView

	// MetricsAttributed reports whether GPU metrics carried pod identity. When
	// false, per-pod checks must not run rather than report an empty result.
	MetricsAttributed bool

	// ProfilingMetrics records whether DCGM profiling series are present. v0.1
	// makes no claim from them; their availability gates everything after it,
	// so it is worth reporting.
	ProfilingMetrics bool
}

// Device is one physical accelerator, with its measurement already summarised.
type Device struct {
	ID     string // stable within a scan: "node/index"
	Node   string
	Pool   string
	Model  string
	Vendor string

	TDPWatts   float64
	Allocation string // api.Alloc*

	// Holder is the pod occupying the device, or nil when nothing holds it.
	Holder *PodRef

	Util  Stats
	Power Stats

	// Analyzable is false where the allocation model makes a per-device claim
	// unsupportable. Such devices are counted in the exclusions, never dropped.
	Analyzable bool
}

// Stats is a summarised measurement series.
type Stats struct {
	Samples      int
	Max          float64
	Mean         float64
	Completeness float64

	// ZeroThroughout is the load-bearing fact of the idle check, and it is true
	// only when every sample read exactly zero. GPU utilization is a poor
	// measure of how hard a device is working — a single-thread kernel reads
	// 100% — but it is completely reliable when it reads zero.
	ZeroThroughout bool

	LastNonZero *time.Time
	FallowSince time.Time
	Buckets     []float64
}

// FallowFor returns how long the device has read exactly zero up to now, and
// whether it has read zero at all.
//
// The distinction that matters is between "no samples" and "zero samples". A
// series that returned nothing is unknown, and unknown must never be reported
// as idle: an exporter that crashed a week ago would otherwise produce a
// cluster-wide recommendation to delete everything.
func (s Stats) FallowFor(now time.Time) (time.Duration, bool) {
	if s.Samples == 0 {
		return 0, false
	}
	if s.LastNonZero != nil && !s.LastNonZero.Before(now) {
		return 0, false
	}
	if s.FallowSince.IsZero() {
		return 0, false
	}
	d := now.Sub(s.FallowSince)
	if d <= 0 {
		return 0, false
	}
	return d, true
}

// PodRef identifies a pod.
type PodRef struct {
	Namespace string
	Name      string
	UID       string
}

func (p PodRef) String() string { return p.Namespace + "/" + p.Name }

// PodView is a pod as a check sees it.
type PodView struct {
	Ref          PodRef
	Node         string
	Phase        string
	Accelerators int
	StartTime    *time.Time
	Restarts     int

	// WedgedReason is non-empty when the pod holds devices without running
	// work: CrashLoopBackOff, OOMKilled, Error, or a stalled init.
	WedgedReason string
	Terminated   *Termination
	Initialising bool

	// Pending is scheduling state, kept explicit so no check can mistake a pod
	// that holds nothing for one that does.
	Pending       bool
	Unschedulable bool

	// Evictable is false where something would block a node drain.
	Evictable   bool
	BlockReason string
	IsDaemonSet bool

	Provenance api.Provenance
	Owner      api.Owner
	Labels     map[string]string
}

// Termination describes the last container exit.
type Termination struct {
	Reason     string
	ExitCode   int
	FinishedAt time.Time
}

// NodeView is a node as a check sees it.
type NodeView struct {
	Name     string
	Pool     string
	Provider string
	Model    string
	Vendor   string

	Accelerators int
	Allocation   string
	TDPWatts     float64

	Ready         bool
	Unschedulable bool
	Age           time.Duration
	Initialising  bool

	// ScaleDownDisabled records the explicit annotation. An operator who has
	// pinned a node is making a decision, not a mistake.
	ScaleDownDisabled bool
}

// AutoscalerView exposes the one thing only the autoscaler knows: the minimum
// size an operator deliberately set.
type AutoscalerView struct {
	Floors map[string]int
}

// Floor returns a pool's minimum size and whether one is set.
//
// Matching is exact first, then longest suffix, because cloud providers
// decorate node group names (an AKS pool "gpu" appears as
// "aks-gpu-12345678-vmss"). A substring match over a map would be
// nondeterministic — pool "gpu" could match "gpu-big" or "gpu-small" depending
// on iteration order — and this decision gates whether ullage calls deliberate
// reserved capacity waste.
func (a *AutoscalerView) Floor(pool string) (int, bool) {
	if a == nil || pool == "" {
		return 0, false
	}
	if v, ok := a.Floors[pool]; ok {
		return v, true
	}
	best, bestLen, found := 0, -1, false
	names := make([]string, 0, len(a.Floors))
	for name := range a.Floors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !containsPool(name, pool) {
			continue
		}
		if len(name) > bestLen {
			best, bestLen, found = a.Floors[name], len(name), true
		}
	}
	return best, found
}

func containsPool(groupName, pool string) bool {
	if groupName == pool {
		return true
	}
	// "aks-gpu-12345678-vmss" contains "-gpu-"; require a delimiter so "gpu"
	// does not match "gpubig".
	for _, pattern := range []string{"-" + pool + "-", "-" + pool, pool + "-"} {
		if len(pattern) <= len(groupName) && indexOf(groupName, pattern) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// DevicesOf returns the devices held by a pod.
func (c *Cluster) DevicesOf(ref PodRef) []Device {
	var out []Device
	for _, d := range c.Devices {
		if d.Holder != nil && d.Holder.Namespace == ref.Namespace && d.Holder.Name == ref.Name {
			out = append(out, d)
		}
	}
	return out
}

// NodeByName looks up a node view.
func (c *Cluster) NodeByName(name string) *NodeView {
	for i := range c.Nodes {
		if c.Nodes[i].Name == name {
			return &c.Nodes[i]
		}
	}
	return nil
}

// PodsOnNode returns every pod scheduled to a node.
func (c *Cluster) PodsOnNode(node string) []PodView {
	var out []PodView
	for _, p := range c.Pods {
		if p.Node == node {
			out = append(out, p)
		}
	}
	return out
}
