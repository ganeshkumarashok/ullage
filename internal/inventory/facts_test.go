package inventory_test

import (
	"testing"
	"time"

	"github.com/ganeshkumarashok/ullage/internal/inventory"
	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

var now = time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Floor
//
// Floor decides whether a pool's empty nodes are deliberate reserved capacity
// or waste. Getting it wrong in one direction tells someone to delete nodes
// their autoscaler will immediately recreate; in the other it hides real spend
// behind a reservation that does not exist.
// ---------------------------------------------------------------------------

func TestFloorPrefersAnExactPoolName(t *testing.T) {
	a := &inventory.AutoscalerView{
		Pools:  []string{"gpu"},
		Floors: map[string]int{"gpu": 3, "aks-gpu-12345678-vmss": 99},
	}
	got, ok := a.Floor("gpu")
	if !ok || got != 3 {
		t.Fatalf("Floor(gpu) = %d, %v; an exact name is the autoscaler talking about "+
			"the pool itself and must win over any decorated node group", got, ok)
	}
}

// One Kubernetes pool is routinely several autoscaler node groups: AKS creates
// a VMSS per availability zone and EKS an ASG per zone. GPU capacity is
// zone-constrained often enough that this is the normal case, not an edge one.
func TestFloorSumsTheZonalNodeGroupsOfOnePool(t *testing.T) {
	a := &inventory.AutoscalerView{
		Pools: []string{"gpu"},
		Floors: map[string]int{
			"aks-gpu-1111-vmss": 0,
			"aks-gpu-2222-vmss": 0,
			"aks-gpu-3333-vmss": 2,
		},
	}
	got, ok := a.Floor("gpu")
	if !ok {
		t.Fatal("Floor(gpu) found nothing across three matching zonal groups")
	}
	if got != 2 {
		t.Fatalf("Floor(gpu) = %d, want 2. Picking a single group returns 0 — which calls "+
			"reserved capacity waste — or overstates the reservation threefold", got)
	}
}

// Pool names nest. "aks-gpu-big-1234-vmss" contains "gpu", so a naive match
// sums pool gpu-big's floor into pool gpu, and it fails in the direction of
// hiding real waste.
func TestFloorDoesNotStealANestedPoolsNodeGroups(t *testing.T) {
	a := &inventory.AutoscalerView{
		Pools: []string{"gpu", "gpu-big"},
		Floors: map[string]int{
			"aks-gpu-1111-vmss":     1,
			"aks-gpu-big-2222-vmss": 5,
		},
	}
	small, ok := a.Floor("gpu")
	if !ok || small != 1 {
		t.Fatalf("Floor(gpu) = %d, %v, want 1: gpu-big's five nodes belong to gpu-big", small, ok)
	}
	big, ok := a.Floor("gpu-big")
	if !ok || big != 5 {
		t.Fatalf("Floor(gpu-big) = %d, %v, want 5", big, ok)
	}
}

// Ownership is decided globally: the longest pool name matching a group is the
// one it belongs to. Without the pool list there is nothing to disambiguate
// against, so matching falls back to direct containment.
func TestFloorWithoutAPoolListStillMatchesDirectly(t *testing.T) {
	a := &inventory.AutoscalerView{
		Floors: map[string]int{"aks-gpu-1111-vmss": 4},
	}
	got, ok := a.Floor("gpu")
	if !ok || got != 4 {
		t.Fatalf("Floor(gpu) = %d, %v, want 4: a caller that cannot supply Pools must be "+
			"no worse off than a direct match", got, ok)
	}
}

func TestFloorIsStableAcrossRepeatedCalls(t *testing.T) {
	a := &inventory.AutoscalerView{
		Pools: []string{"gpu", "gpu-big", "cpu"},
		Floors: map[string]int{
			"aks-gpu-1-vmss": 1, "aks-gpu-2-vmss": 1,
			"aks-gpu-big-1-vmss": 3, "aks-cpu-1-vmss": 7,
		},
	}
	// Map iteration order varies per run; the answer must not.
	first, _ := a.Floor("gpu")
	for i := 0; i < 200; i++ {
		if got, _ := a.Floor("gpu"); got != first {
			t.Fatalf("Floor(gpu) returned %d then %d; a nondeterministic floor makes ullage "+
				"call the same pool deliberate on one run and wasteful on the next", first, got)
		}
	}
	if first != 2 {
		t.Fatalf("Floor(gpu) = %d, want 2", first)
	}
}

func TestFloorRefusesTheEmptyPoolName(t *testing.T) {
	a := &inventory.AutoscalerView{Floors: map[string]int{"": 5, "gpu": 1}}
	if got, ok := a.Floor(""); ok {
		t.Fatalf("Floor(\"\") = %d, %v; an unnamed pool matches everything and must not "+
			"claim a floor", got, ok)
	}
}

func TestFloorOnANilAutoscalerIsNotAFloor(t *testing.T) {
	var a *inventory.AutoscalerView
	if _, ok := a.Floor("gpu"); ok {
		t.Fatal("a cluster with no autoscaler reported a floor; no autoscaler means nothing " +
			"is holding nodes deliberately")
	}
	if _, ok := a.Held("gpu"); ok {
		t.Fatal("Held reported a reason with no autoscaler installed")
	}
	if a.Reclaims() {
		t.Fatal("Reclaims() true with no autoscaler")
	}
}

// A pool name must match on a delimiter boundary, or "gpu" claims "gpubig".
func TestPoolMatchingRespectsNameBoundaries(t *testing.T) {
	cases := []struct {
		group string
		match bool
		why   string
	}{
		{"gpu", true, "an exact name is the pool itself"},
		{"aks-gpu-12345678-vmss", true, "AKS decorates both sides"},
		{"gpu-nodes", true, "a trailing delimiter still names the pool"},
		{"eks-gpu", true, "a leading delimiter still names the pool"},
		{"gpubig", false, "gpubig is a different pool that merely starts the same"},
		{"biggpu", false, "biggpu is a different pool that merely ends the same"},
		{"aks-gpubig-1-vmss", false, "a decorated unrelated pool must not match either"},
	}
	for _, tc := range cases {
		a := &inventory.AutoscalerView{Floors: map[string]int{tc.group: 2}}
		_, ok := a.Floor("gpu")
		if ok != tc.match {
			t.Errorf("group %q matched pool \"gpu\" = %v, want %v: %s", tc.group, ok, tc.match, tc.why)
		}
	}
}

func TestHeldPrefersAConcreteFloorOverADisruptionBudget(t *testing.T) {
	a := &inventory.AutoscalerView{
		Floors: map[string]int{"gpu": 2},
		Pinned: map[string]bool{"gpu": true},
	}
	reason, ok := a.Held("gpu")
	if !ok {
		t.Fatal("Held(gpu) found no reason despite both a floor and a pin")
	}
	if want := "minimum of 2 nodes"; !contains(reason, want) {
		t.Fatalf("Held(gpu) = %q, want it to name the concrete floor (%q): a number the "+
			"reader can check beats a general statement", reason, want)
	}
}

func TestAZeroFloorIsNotAReasonToKeepNodes(t *testing.T) {
	a := &inventory.AutoscalerView{Floors: map[string]int{"gpu": 0}}
	if reason, ok := a.Held("gpu"); ok {
		t.Fatalf("Held(gpu) = %q, true; a floor of zero is the autoscaler saying it may "+
			"remove every node, which is the opposite of holding them", reason)
	}
}

func TestKarpenterPinIsReportedWhenNoFloorExists(t *testing.T) {
	a := &inventory.AutoscalerView{Kind: "karpenter", Pinned: map[string]bool{"gpu": true}}
	reason, ok := a.Held("gpu")
	if !ok || !contains(reason, "disruption budget") {
		t.Fatalf("Held(gpu) = %q, %v; a budget allowing zero disruption is why the nodes "+
			"are still there and the reader needs to be told which object to look at", reason, ok)
	}
	if !a.Reclaims() {
		t.Fatal("Karpenter reclaims empty nodes on its own; Reclaims() must say so")
	}
}

// ---------------------------------------------------------------------------
// DevicesOf
// ---------------------------------------------------------------------------

// dcgm-exporter stamps the holding pod onto the series, the series lingers in
// Prometheus after the holder goes away, and pod names repeat constantly under
// StatefulSets and Jobs. Matching on namespace and name alone attributes a
// finished job's device to a running namesake.
func TestDevicesOfRefusesADeviceOnAnotherNode(t *testing.T) {
	ref := inventory.PodRef{Namespace: "ml", Name: "train-0"}
	cl := &inventory.Cluster{
		Pods: []inventory.PodView{{Ref: ref, Node: "gpu-a", Phase: "Running", Accelerators: 1}},
		Devices: []inventory.Device{
			{ID: "a/0", Node: "gpu-a", Holder: &ref},
			{ID: "b/0", Node: "gpu-b", Holder: &ref},
		},
	}
	got := cl.DevicesOf(ref)
	if len(got) != 1 {
		t.Fatalf("DevicesOf returned %d devices for a pod running on one node; a namesake's "+
			"leftover series makes the pod look like it holds twice the hardware", len(got))
	}
	if got[0].Node != "gpu-a" {
		t.Fatalf("attributed the device on %s, want the pod's own node gpu-a", got[0].Node)
	}
}

// If the pod is not in the inventory there is no node to check against, and
// refusing everything would silently drop findings.
func TestDevicesOfWithoutAKnownNodeStillReturnsTheDevices(t *testing.T) {
	ref := inventory.PodRef{Namespace: "ml", Name: "ghost"}
	cl := &inventory.Cluster{
		Devices: []inventory.Device{{ID: "a/0", Node: "gpu-a", Holder: &ref}},
	}
	if got := cl.DevicesOf(ref); len(got) != 1 {
		t.Fatalf("DevicesOf returned %d for a pod absent from the inventory; with no node "+
			"to compare against there is nothing to contradict the metric", len(got))
	}
}

func TestDevicesOfIgnoresUnheldDevicesAndOtherPods(t *testing.T) {
	mine := inventory.PodRef{Namespace: "ml", Name: "mine"}
	other := inventory.PodRef{Namespace: "ml", Name: "other"}
	cl := &inventory.Cluster{
		Pods: []inventory.PodView{{Ref: mine, Node: "gpu-a", Accelerators: 1}},
		Devices: []inventory.Device{
			{ID: "a/0", Node: "gpu-a", Holder: &mine},
			{ID: "a/1", Node: "gpu-a", Holder: &other},
			{ID: "a/2", Node: "gpu-a", Holder: nil},
		},
	}
	got := cl.DevicesOf(mine)
	if len(got) != 1 || got[0].ID != "a/0" {
		t.Fatalf("DevicesOf = %v, want only a/0", ids(got))
	}
}

// Namespace is part of identity: two teams naming a pod "trainer" is normal.
func TestDevicesOfSeparatesIdenticalNamesInDifferentNamespaces(t *testing.T) {
	a := inventory.PodRef{Namespace: "team-a", Name: "trainer"}
	b := inventory.PodRef{Namespace: "team-b", Name: "trainer"}
	cl := &inventory.Cluster{
		Pods: []inventory.PodView{
			{Ref: a, Node: "gpu-a", Accelerators: 1},
			{Ref: b, Node: "gpu-a", Accelerators: 1},
		},
		Devices: []inventory.Device{
			{ID: "a/0", Node: "gpu-a", Holder: &a},
			{ID: "a/1", Node: "gpu-a", Holder: &b},
		},
	}
	if got := cl.DevicesOf(a); len(got) != 1 || got[0].ID != "a/0" {
		t.Fatalf("DevicesOf(team-a/trainer) = %v, want only a/0", ids(got))
	}
}

// ---------------------------------------------------------------------------
// CoverageOver
// ---------------------------------------------------------------------------

func TestCoverageOverMeasuresAgainstTheSpanNotTheWindow(t *testing.T) {
	// Four days of samples at 30s.
	s := inventory.Stats{Samples: int((4 * 24 * time.Hour) / (30 * time.Second))}

	if c := s.CoverageOver(4*24*time.Hour, 30*time.Second); c < 0.99 {
		t.Fatalf("coverage over the pod's own four-day life = %.3f, want ~1: it was watched "+
			"completely for as long as it existed", c)
	}
	if c := s.CoverageOver(14*24*time.Hour, 30*time.Second); c > 0.30 {
		t.Fatalf("coverage over a fortnight = %.3f, want ~0.29: the same samples are a small "+
			"part of a longer window, and conflating the two is what hid every young pod", c)
	}
}

func TestCoverageOverIsCappedAtOne(t *testing.T) {
	s := inventory.Stats{Samples: 100000}
	if c := s.CoverageOver(time.Hour, 30*time.Second); c != 1 {
		t.Fatalf("coverage = %.3f; a faster scrape than assumed means better coverage than "+
			"expected, never more than complete", c)
	}
}

func TestCoverageOverRefusesDegenerateSpans(t *testing.T) {
	s := inventory.Stats{Samples: 50}
	for _, tc := range []struct{ span, step time.Duration }{
		{0, 30 * time.Second},
		{-time.Hour, 30 * time.Second},
		{time.Hour, 0},
		{time.Hour, -time.Second},
	} {
		if c := s.CoverageOver(tc.span, tc.step); c != 0 {
			t.Errorf("CoverageOver(%v, %v) = %.3f, want 0; an unmeasurable span must read as "+
				"no coverage, which refuses the finding, not as full coverage", tc.span, tc.step, c)
		}
	}
}

func TestCoverageOverASpanShorterThanOneStep(t *testing.T) {
	s := inventory.Stats{Samples: 1}
	if c := s.CoverageOver(5*time.Second, 30*time.Second); c != 1 {
		t.Fatalf("coverage = %.3f; a single sample is complete coverage of a span too short "+
			"to hold two, and dividing by a fractional expectation would exceed 1", c)
	}
	empty := inventory.Stats{Samples: 0}
	if c := empty.CoverageOver(5*time.Second, 30*time.Second); c != 0 {
		t.Fatalf("coverage of no samples = %.3f, want 0", c)
	}
}

// ---------------------------------------------------------------------------
// FallowFor
// ---------------------------------------------------------------------------

func TestFallowForRefusesASeriesWithNoSamples(t *testing.T) {
	s := inventory.Stats{Samples: 0, FallowSince: now.Add(-9 * 24 * time.Hour)}
	if d, ok := s.FallowFor(now); ok {
		t.Fatalf("FallowFor = %v, true for a series that returned nothing. Unknown is not "+
			"idle: an exporter that crashed a week ago would otherwise recommend deleting "+
			"every GPU in the cluster", d)
	}
}

// Max comes from an aggregate query; LastNonZero from a chunked, downsampled
// range query. When the aggregate proves work happened and the shape cannot say
// when, only one of the two readings ends in a deletion.
func TestFallowForRefusesWhenTheAggregateContradictsTheShape(t *testing.T) {
	s := inventory.Stats{Samples: 100, Max: 0.78, LastNonZero: nil, FallowSince: now.Add(-9 * 24 * time.Hour)}
	if d, ok := s.FallowFor(now); ok {
		t.Fatalf("FallowFor = %v, true for a device the aggregate measured at 78%% peak. "+
			"The shape's silence is missing data, not proof the device was idle", d)
	}
}

func TestFallowForAcceptsAGenuinelySilentDevice(t *testing.T) {
	s := inventory.Stats{Samples: 100, Max: 0, LastNonZero: nil, FallowSince: now.Add(-9 * 24 * time.Hour)}
	d, ok := s.FallowFor(now)
	if !ok {
		t.Fatal("FallowFor refused a device with samples, a zero peak and a known start; " +
			"that is exactly the case this tool exists to report")
	}
	if d != 9*24*time.Hour {
		t.Fatalf("FallowFor = %v, want 9 days", d)
	}
}

func TestFallowForRefusesWorkAtOrAfterNow(t *testing.T) {
	for _, last := range []time.Time{now, now.Add(time.Hour)} {
		s := inventory.Stats{Samples: 100, LastNonZero: &last, FallowSince: now.Add(-time.Hour)}
		if d, ok := s.FallowFor(now); ok {
			t.Errorf("FallowFor = %v, true with work recorded at %v; a device that worked "+
				"at or after the scan instant has not been idle at all", d, last)
		}
	}
}

func TestFallowForRefusesAnUndatedStart(t *testing.T) {
	s := inventory.Stats{Samples: 100}
	if _, ok := s.FallowFor(now); ok {
		t.Fatal("FallowFor succeeded with a zero FallowSince; without a start instant there " +
			"is no duration to report and no claim to make")
	}
}

func TestFallowForRefusesAFutureStart(t *testing.T) {
	s := inventory.Stats{Samples: 100, FallowSince: now.Add(time.Hour)}
	if d, ok := s.FallowFor(now); ok {
		t.Fatalf("FallowFor = %v, true from a start in the future; clock skew must not "+
			"produce a negative or nonsense idle duration", d)
	}
}

// ---------------------------------------------------------------------------
// Small views
// ---------------------------------------------------------------------------

func TestOccupiesCountsSlicesAsWellAsWholeDevices(t *testing.T) {
	cases := []struct {
		name         string
		accelerators int
		slices       int
		want         bool
	}{
		{"holds nothing", 0, 0, false},
		{"holds a whole device", 1, 0, true},
		{"holds a MIG slice only", 0, 1, true},
		{"holds both", 2, 3, true},
	}
	for _, tc := range cases {
		p := inventory.PodView{Accelerators: tc.accelerators, Slices: tc.slices}
		if got := p.Occupies(); got != tc.want {
			t.Errorf("%s: Occupies() = %v, want %v; a pod holding a slice occupies capacity "+
				"just as surely as one holding a card", tc.name, got, tc.want)
		}
	}
}

func TestKarpenterNodeIsIdentifiedByItsNodePool(t *testing.T) {
	if (inventory.NodeView{}).Karpenter() {
		t.Error("a node with no Karpenter NodePool was reported as Karpenter-managed; the " +
			"fix command differs per provisioner, so this decides what the reader is told to run")
	}
	if !(inventory.NodeView{KarpenterPool: "gpu"}).Karpenter() {
		t.Error("a node carrying a Karpenter NodePool was not recognised")
	}
}

func TestPodRefPrintsAsNamespaceSlashName(t *testing.T) {
	ref := inventory.PodRef{Namespace: "ml", Name: "trainer", UID: "abc"}
	if got := ref.String(); got != "ml/trainer" {
		t.Fatalf("PodRef.String() = %q, want %q: this string is pasted into kubectl "+
			"commands and shown as the finding's identity", got, "ml/trainer")
	}
}

func TestNodeByNameFindsNodesAndReportsAbsenceAsNil(t *testing.T) {
	cl := &inventory.Cluster{Nodes: []inventory.NodeView{
		{Name: "gpu-a", Pool: "gpu"}, {Name: "gpu-b", Pool: "gpu"},
	}}
	n := cl.NodeByName("gpu-b")
	if n == nil || n.Name != "gpu-b" {
		t.Fatalf("NodeByName(gpu-b) = %v, want the node", n)
	}
	if got := cl.NodeByName("absent"); got != nil {
		t.Fatalf("NodeByName(absent) = %v, want nil so callers must handle the gap", got)
	}
}

func TestPodsOnNodeReturnsOnlyThatNodesPods(t *testing.T) {
	cl := &inventory.Cluster{Pods: []inventory.PodView{
		{Ref: inventory.PodRef{Namespace: "ml", Name: "a"}, Node: "gpu-a"},
		{Ref: inventory.PodRef{Namespace: "ml", Name: "b"}, Node: "gpu-b"},
		{Ref: inventory.PodRef{Namespace: "ml", Name: "c"}, Node: "gpu-a"},
	}}
	got := cl.PodsOnNode("gpu-a")
	if len(got) != 2 {
		t.Fatalf("PodsOnNode(gpu-a) returned %d pods, want 2", len(got))
	}
	for _, p := range got {
		if p.Node != "gpu-a" {
			t.Fatalf("PodsOnNode(gpu-a) included a pod on %s", p.Node)
		}
	}
	if got := cl.PodsOnNode("empty"); len(got) != 0 {
		t.Fatalf("PodsOnNode(empty) returned %d pods, want none", len(got))
	}
}

func TestAllocationModelsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range []string{
		api.AllocExclusive, api.AllocTimeSliced, api.AllocMIG, api.AllocDRA,
	} {
		if a == "" {
			t.Error("an allocation model serialises as the empty string, which is " +
				"indistinguishable from an unset field in the JSON contract")
		}
		if seen[a] {
			t.Errorf("allocation model %q is duplicated; the census counts would merge two "+
				"genuinely different sharing models", a)
		}
		seen[a] = true
	}
}

// ---------------------------------------------------------------------------

func ids(devs []inventory.Device) []string {
	out := make([]string, 0, len(devs))
	for _, d := range devs {
		out = append(out, d.ID)
	}
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
