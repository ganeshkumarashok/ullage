package ullage_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ullage-project/ullage/internal/demo"
	"github.com/ullage-project/ullage/pkg/ullage"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// End-to-end coverage over the demo cluster, served as real HTTP by real
// handlers and read by the production clients. Anything the fixture gets wrong
// about the shape of a Kubernetes object or a Prometheus response surfaces here
// as a decode failure, which is the point: a fake that agrees with the client
// about a mistake tests nothing.

func scan(t *testing.T) *api.Result {
	t.Helper()
	now := time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)
	srv := demo.Start(now)
	t.Cleanup(srv.Close)

	res, err := ullage.Scan(context.Background(), ullage.Options{
		APIServer:  srv.Kube.URL,
		Prometheus: ullage.PrometheusOptions{URL: srv.Prometheus.URL},
		Now:        now,
		Version:    "test",
		Trace:      true,
	})
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	return res
}

func byID(res *api.Result, id string) *api.Finding {
	for i := range res.Recommendations {
		if res.Recommendations[i].Workload.Ref() == id {
			return &res.Recommendations[i]
		}
	}
	return nil
}

func TestScanFindsTheIntendedWorkloads(t *testing.T) {
	res := scan(t)
	for _, want := range []string{
		"research/jupyter-alice",
		"research/scratch-pod-bob",
		"ml-platform/finetune-carol",
		"research/dra-sandbox-erin",
		"serving/embed-v2",
		"pool/l4-serving",
	} {
		if byID(res, want) == nil {
			t.Errorf("no finding for %s", want)
		}
	}
}

// The negative cases matter more than the positive ones. A tool that reports
// everything is not useful, and every false positive costs the trust that makes
// the true ones actionable.
func TestScanStaysSilentWhereItShould(t *testing.T) {
	res := scan(t)
	for _, unwanted := range []string{
		"training/llama-pretrain", // genuinely busy
		"training/dataprep",       // bursty: ~4% mean, never idle
		"pool/h100-train",         // fully occupied
		"pool/h100-reserve",       // held by an autoscaler floor: by design, not waste
		"pool/l40s-dra",           // occupied through a ResourceClaim
		"pool/a10-new",            // driver still initialising
	} {
		if f := byID(res, unwanted); f != nil {
			t.Errorf("%s was reported as waste: %s", unwanted, f.Summary)
		}
	}
}

func TestAutoscalerFloorIsByDesignNotWaste(t *testing.T) {
	res := scan(t)
	if len(res.ByDesign) == 0 {
		t.Fatal("the reserved pool should appear as fallow by design")
	}
	found := false
	for _, f := range res.ByDesign {
		if f.Workload.Ref() == "pool/h100-reserve" {
			found = true
		}
	}
	if !found {
		t.Error("pool/h100-reserve is held at an autoscaler minimum and must be shown " +
			"as deliberate, separately from waste and with no removal command")
	}
}

func TestControllerOwnedWorkloadTargetsTheController(t *testing.T) {
	res := scan(t)
	f := byID(res, "research/jupyter-alice")
	if f == nil {
		t.Fatal("missing finding")
	}
	if f.Workload.Grouped != 3 {
		t.Errorf("grouped %d pods, want 3", f.Workload.Grouped)
	}
	if f.Fix.Targets != api.FixTargetController {
		t.Errorf("fix targets %q, want the controller: deleting the pods would be "+
			"undone by the StatefulSet within seconds", f.Fix.Targets)
	}
	if want := "kubectl scale statefulset -n research jupyter-alice --replicas=0"; f.Fix.Command != want {
		t.Errorf("command is %q, want %q", f.Fix.Command, want)
	}
}

func TestUnrecognisedOwnerGetsNoCommand(t *testing.T) {
	res := scan(t)
	f := byID(res, "ml-platform/finetune-carol")
	if f == nil {
		t.Fatal("missing finding")
	}
	if f.Provenance.Recognized {
		t.Error("a kubeflow.org Notebook is not a kind ullage knows how to remove")
	}
	if f.Fix.Command != "" {
		t.Errorf("emitted %q for an unrecognised owner; refusing to guess is the "+
			"stronger trust signal", f.Fix.Command)
	}
}

func TestCrashLoopResolvesToRootDeployment(t *testing.T) {
	res := scan(t)
	f := byID(res, "serving/embed-v2")
	if f == nil {
		t.Fatal("missing finding: the pod is owned by a ReplicaSet owned by a Deployment")
	}
	if f.Provenance.RootKind != "Deployment" {
		t.Errorf("root is %s/%s, want the Deployment: scaling the ReplicaSet would be "+
			"reverted by its Deployment on the next reconcile",
			f.Provenance.RootKind, f.Provenance.RootName)
	}
	if f.Fix.Command == "" || !contains(f.Fix.Command, "--previous") {
		t.Errorf("fix is %q; a crash loop needs the previous container's logs, never a deletion", f.Fix.Command)
	}
}

func TestBlockerDiagnosisLeadsTheNodeFix(t *testing.T) {
	res := scan(t)
	f := byID(res, "pool/l4-serving")
	if f == nil {
		t.Fatal("missing finding")
	}
	if len(f.Fix.Blockers) != 2 {
		t.Fatalf("named %d blockers, want 2", len(f.Fix.Blockers))
	}
	for _, b := range f.Fix.Blockers {
		if b.Object == "gpu-operator/dcgm-exporter-l4-serving-0" {
			t.Error("a DaemonSet pod was named as a blocker; the autoscaler ignores them")
		}
	}
	if !contains(f.Fix.Rationale, "prevent the autoscaler") {
		t.Errorf("rationale is %q; the blocker diagnosis is the finding, not a footnote", f.Fix.Rationale)
	}
}

func TestDRADevicesAreAnalysedNotExcluded(t *testing.T) {
	res := scan(t)
	if res.Scan.AllocationModels.DRA == 0 {
		t.Fatal("no DRA devices were discovered")
	}
	for _, e := range res.NotAnalyzed {
		if e.Code == api.ExclDRA {
			t.Error("DRA devices were excluded; a ResourceClaim reserves whole devices, " +
				"so exclusivity holds and an idleness claim is supportable")
		}
	}
	if byID(res, "research/dra-sandbox-erin") == nil {
		t.Error("the idle DRA workload was not found")
	}
}

// The census must reconcile. A headline percentage over a denominator the
// reader cannot account for turns a monitoring gap into a claim about
// efficiency.
// The demo cluster deliberately contains a GPU that was handed to a second pod
// during the window, so it returns two utilization series for one physical
// device. Counting series as accelerators produces "analysed 61 of 68" against
// 8 excluded — a total of 69 — and the honest denominator is the single claim
// the whole tool rests on.
func TestAcceleratorAccountingReconciles(t *testing.T) {
	res := scan(t)
	if res.Scan.AcceleratorsAnalyzed > res.Scan.AcceleratorsObserved {
		t.Fatalf("analysed %d of %d observed: more accelerators were analysed than exist, "+
			"which means metric series are being counted as hardware",
			res.Scan.AcceleratorsAnalyzed, res.Scan.AcceleratorsObserved)
	}
	excluded := 0
	for _, e := range res.NotAnalyzed {
		excluded += e.Accelerators
	}
	if got := res.Scan.AcceleratorsAnalyzed + excluded; got != res.Scan.AcceleratorsObserved {
		t.Errorf("analysed %d + excluded %d = %d, but observed %d",
			res.Scan.AcceleratorsAnalyzed, excluded, got, res.Scan.AcceleratorsObserved)
	}
	m := res.Scan.AllocationModels
	if got := m.DevicePluginExclusive + m.TimeSliced + m.MIG + m.DRA; got != res.Scan.AcceleratorsObserved {
		t.Errorf("allocation census sums to %d, observed %d", got, res.Scan.AcceleratorsObserved)
	}
}

func TestSharedDevicesAreRefusedWithAReason(t *testing.T) {
	res := scan(t)
	want := map[string]bool{api.ExclTimeSliced: false, api.ExclMIG: false, api.ExclInitialising: false}
	for _, e := range res.NotAnalyzed {
		if _, ok := want[e.Code]; ok {
			want[e.Code] = true
			if e.Detail == "" || e.Remedy == "" {
				t.Errorf("exclusion %s has no detail or remedy; an unexplained exclusion "+
					"is indistinguishable from a bug", e.Code)
			}
		}
	}
	for code, seen := range want {
		if !seen {
			t.Errorf("expected exclusion %s", code)
		}
	}
}

func TestPendingPodsAreContextNotFindings(t *testing.T) {
	res := scan(t)
	if res.UnmetDemand == nil || res.UnmetDemand.Pods == 0 {
		t.Fatal("pending GPU pods should be reported as unmet demand")
	}
	for _, f := range res.Recommendations {
		if f.Workload.Namespace == "research" && contains(f.Workload.Name, "sweep-worker") {
			t.Error("a Pending pod was reported as waste; it holds no devices")
		}
	}
}

// The trust argument for this tool is that its claims are checkable, which is
// empty unless the queries that produced them are visible.
func TestScanRecordsItsQueriesAndParameters(t *testing.T) {
	res := scan(t)
	if len(res.Scan.Queries) == 0 {
		t.Error("no queries were recorded despite Trace being set")
	}
	if res.Scan.Params.IdleThreshold.Duration() == 0 {
		t.Error("the effective idle threshold was not recorded, so the result cannot be reproduced")
	}
	if res.APIVersion != api.Version {
		t.Errorf("apiVersion is %q, want %q", res.APIVersion, api.Version)
	}
}

func TestFindingsAreRankedByFallowHours(t *testing.T) {
	res := scan(t)
	for i := 1; i < len(res.Recommendations); i++ {
		prev := res.Recommendations[i-1].Impact.GPUHoursFallow
		cur := res.Recommendations[i].Impact.GPUHoursFallow
		if cur > prev {
			t.Fatalf("finding %d (%.0f h) outranks %d (%.0f h)", i, cur, i-1, prev)
		}
	}
}

// Costs must never be blended across models: T4 and H100 differ roughly
// tenfold, so a single averaged rate produces a fabricated number wearing a
// decimal point.
func TestCostsAreSingleSKUOnly(t *testing.T) {
	res := scan(t)
	for _, f := range res.Recommendations {
		if f.Impact.WindowCost == nil {
			continue
		}
		if len(f.Accelerators) != 1 {
			t.Errorf("%s is priced across %d models", f.Workload.Ref(), len(f.Accelerators))
		}
		if f.Impact.PricingScope != "single-sku" {
			t.Errorf("%s has pricing scope %q", f.Workload.Ref(), f.Impact.PricingScope)
		}
	}
}

func TestDoctorReportsOnTheDemoCluster(t *testing.T) {
	now := time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)
	srv := demo.Start(now)
	t.Cleanup(srv.Close)

	rep, err := ullage.Doctor(context.Background(), ullage.Options{
		APIServer:  srv.Kube.URL,
		Prometheus: ullage.PrometheusOptions{URL: srv.Prometheus.URL},
	})
	if err != nil {
		t.Fatalf("doctor failed: %v", err)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("doctor reported nothing")
	}
	for _, c := range rep.Checks {
		if c.Status == "fail" {
			t.Errorf("doctor failed on %q: %s", c.Name, c.Detail)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// A field declared as a list must serialise as [], never as null. Every
// consumer that iterates one breaks on the first cluster that has none, and
// "no suppressions" or "no warnings" is the overwhelmingly common case — so the
// bug appears for the healthiest clusters and never in a demo.
func TestListFieldsAreNeverNull(t *testing.T) {
	res := scan(t)
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"recommendations", "suppressed", "notAnalyzed", "warnings"} {
		v, ok := m[k]
		if !ok {
			t.Errorf("%q is absent from the contract", k)
			continue
		}
		if string(v) == "null" {
			t.Errorf("%q serialised as null; a consumer iterating it panics on any "+
				"cluster where the list is empty", k)
		}
	}

	// Enumerating the top level by hand only catches the lists someone
	// remembered. Walk the whole document instead: `scan.params.checks`
	// shipped as null precisely because it is nested and appears only on the
	// default invocation, which is the one nobody runs in a test.
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, child := range t2 {
				p := k
				if path != "" {
					p = path + "." + k
				}
				walk(p, child)
			}
		case []any:
			for _, child := range t2 {
				walk(path+"[]", child)
			}
		}
	}
	walk("", doc)

	// A null anywhere is only detectable against the declared shape, so check
	// every declared slice field is present and non-null.
	for path, isSlice := range sliceFields(reflect.TypeOf(res), "") {
		if !isSlice {
			continue
		}
		if got := lookup(doc, path); got == nil {
			t.Errorf("%q serialised as null; every list in the contract must be [] when "+
				"empty, or a consumer that asks for its length crashes", path)
		}
	}
}

// sliceFields reports the JSON path of every slice-typed field in the contract.
func sliceFields(t reflect.Type, prefix string) map[string]bool {
	out := map[string]bool{}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Slice:
			out[path] = true
		case reflect.Struct:
			if ft.String() == "time.Time" {
				continue
			}
			for k, v := range sliceFields(ft, path) {
				out[k] = v
			}
		}
	}
	return out
}

// lookup walks a dotted path through decoded JSON, returning nil for a null.
func lookup(doc any, path string) any {
	cur := doc
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return struct{}{} // not reachable on this document; not a null
		}
		v, present := m[part]
		if !present {
			return struct{}{}
		}
		cur = v
	}
	return cur
}
