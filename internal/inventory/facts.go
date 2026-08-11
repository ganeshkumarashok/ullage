package inventory

import (
	"fmt"
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

	// Step is the sample interval the measurements were gathered at, needed to
	// turn a sample count back into a coverage fraction over any span.
	Step time.Duration

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
	// Samples counts the samples of this record's own series — this holder's
	// tenure on the device. A GPU reused inside the window produces one record
	// per holder, and each counts only its own.
	Samples int

	Max  float64
	Mean float64

	// Completeness is coverage of the physical accelerator across the whole
	// window. It answers "was this card being watched", which is the right
	// question for a claim about a node. It is the wrong question for a claim
	// about a pod that has not existed for the whole window — use
	// CoverageOver for that.
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

// CoverageOver reports what fraction of a span this record's own series covers.
//
// It exists because coverage measured against the scan window is the wrong
// denominator for anything shorter-lived than the window. A pod that has
// existed for two days of a fortnight can never exceed roughly 14% coverage by
// that measure, so a fixed threshold silently discards every young pod — and a
// GPU pod someone started last week and forgot is the single most actionable
// thing this tool can find, as well as the first thing an evaluator will try.
func (s Stats) CoverageOver(span, step time.Duration) float64 {
	if step <= 0 || span <= 0 {
		return 0
	}
	expected := span.Seconds() / step.Seconds()
	if expected < 1 {
		expected = 1
	}
	c := float64(s.Samples) / expected
	if c > 1 {
		return 1
	}
	return c
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

	// Slices counts partitioned accelerator requests: MIG profiles under the
	// mixed strategy, or time-sliced replicas. These are not whole devices and
	// are deliberately kept out of Accelerators, which would otherwise report a
	// 1g.5gb slice as a whole A100.
	Slices   int
	SliceRes string

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

// Occupies reports whether the pod holds accelerator hardware of any kind:
// whole devices, DRA claims, MIG profiles, or time-sliced replicas.
//
// This is a method rather than a field on purpose. It is the question every
// "is this node in use?" check must ask, and getting it wrong means
// recommending the deletion of hardware that is at capacity. A field can be
// left unset by a caller that adds a new allocation model and forgets; a
// method derived from the counts cannot.
func (p PodView) Occupies() bool { return p.Accelerators > 0 || p.Slices > 0 }

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

	// KarpenterPool is set when the node carries karpenter.sh/nodepool.
	//
	// Read from the node rather than inferred from which autoscaler was
	// discovered, because the two disagree in the common cases. A cluster can
	// run cluster-autoscaler for its system pool and Karpenter for its GPU
	// pool, and RBAC can deny karpenter.sh reads while the nodes themselves
	// stay perfectly readable. In both, autoscaler-level detection says "not
	// Karpenter" and the tool emits `eksctl scale nodegroup` for a node group
	// that does not exist — or, worse, for a similarly named ASG that does.
	KarpenterPool string
}

// Karpenter reports whether this node is managed by Karpenter.
func (n NodeView) Karpenter() bool { return n.KarpenterPool != "" }

// AutoscalerView exposes the one thing only the autoscaler knows: whether an
// operator deliberately decided this capacity should stay.
//
// The two autoscalers express that differently. cluster-autoscaler publishes a
// per-node-group minimum size. Karpenter has no minimum size at all — it
// consolidates empty nodes by itself — and instead expresses intent through
// disruption budgets and do-not-disrupt annotations. Both are represented here
// so a check can ask "is this deliberate?" without knowing which is installed.
type AutoscalerView struct {
	// Kind is the autoscaler that produced this view, for wording.
	Kind string

	// Floors are cluster-autoscaler minimum node-group sizes.
	Floors map[string]int

	// Pinned are Karpenter NodePools whose disruption budget allows zero nodes.
	Pinned map[string]bool

	// Scheduled are Karpenter NodePools pinned only during a cron window, which
	// ullage does not evaluate. Reported as context, never as a hold.
	Scheduled map[string]bool

	// Pools are every pool name in the cluster, which is what makes it possible
	// to decide that node group "aks-gpu-big-1234-vmss" belongs to pool
	// "gpu-big" and not to pool "gpu".
	Pools []string
}

// Reclaims reports whether this autoscaler removes empty nodes on its own
// without an operator-set minimum standing in the way.
//
// This is the difference between "no minimum size was readable, so a deliberate
// reservation cannot be ruled out" — correct for cluster-autoscaler — and the
// same sentence on a Karpenter cluster, where it would be printed on every
// finding and be wrong every time, because Karpenter has no minimum size to
// read.
func (a *AutoscalerView) Reclaims() bool {
	return a != nil && a.Kind == "karpenter"
}

// Held reports whether a pool is deliberately kept, with the reason.
// It answers for whichever autoscaler is installed.
func (a *AutoscalerView) Held(pool string) (string, bool) {
	if a == nil {
		return "", false
	}
	if min, ok := a.Floor(pool); ok && min > 0 {
		return fmt.Sprintf("held at a minimum of %d nodes by the cluster autoscaler", min), true
	}
	if a.Pinned[pool] {
		return "held by a Karpenter disruption budget that allows zero nodes to be disrupted", true
	}
	return "", false
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
	// Two problems at once, and they have to be solved together.
	//
	// First, one Kubernetes pool is routinely several autoscaler node groups:
	// AKS creates one VMSS per availability zone and EKS one ASG per zone, and
	// GPU capacity is zone-constrained often enough that this is the normal
	// case. So floors must be summed — with zone minimums of {0, 0, 2} the
	// pool's real floor is 2, and picking any single group returns 0 (calling
	// reserved capacity waste) or 2 (overstating the reservation threefold).
	//
	// Second, pool names nest. "aks-gpu-big-1234-vmss" contains "gpu", so a
	// naive match sums the floors of pool "gpu-big" into pool "gpu". Summing
	// makes that worse than picking one did, and it fails silently in the
	// direction of hiding real waste.
	//
	// So each node group is assigned to exactly one pool — the longest pool
	// name that matches it, which is the one it actually belongs to — and only
	// then are the floors summed. Ownership is decided globally rather than
	// per-query, because "does this group belong to me?" cannot be answered
	// without knowing who else is asking.
	total, found := 0, false
	names := make([]string, 0, len(a.Floors))
	for name := range a.Floors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// With no pool list there is nothing to disambiguate against, so fall
		// back to matching directly. Callers that can supply Pools get the
		// stronger answer; callers that cannot are no worse off than before.
		if len(a.Pools) == 0 {
			if !containsPool(name, pool) {
				continue
			}
		} else if a.owner(name) != pool {
			continue
		}
		total += a.Floors[name]
		found = true
	}
	return total, found
}

// owner returns the pool a node group belongs to: the longest pool name that
// matches it. Ties are broken by name so the answer is stable.
func (a *AutoscalerView) owner(group string) string {
	best := ""
	for _, pool := range a.Pools {
		if !containsPool(group, pool) {
			continue
		}
		if len(pool) > len(best) || (len(pool) == len(best) && pool < best) {
			best = pool
		}
	}
	return best
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
