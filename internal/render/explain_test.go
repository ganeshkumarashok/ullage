package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// sparkline exists to answer "did anything at all happen here", and each
// bucket is a peak, not a mean, so a short burst must never be smoothed away.
// These are the boundary shapes a bucketiser can hand it.

func TestSparklineOnAnEmptySliceReportsAllZeroNotAPanic(t *testing.T) {
	if got := sparkline(nil); got != "  all zero" {
		t.Fatalf("sparkline(nil) = %q, want %q", got, "  all zero")
	}
	if got := sparkline([]float64{}); got != "  all zero" {
		t.Fatalf("sparkline([]float64{}) = %q, want %q", got, "  all zero")
	}
}

func TestSparklineOnAllZeroBucketsReportsAllZero(t *testing.T) {
	got := sparkline([]float64{0, 0, 0, 0})
	if !strings.HasSuffix(got, "all zero") {
		t.Fatalf("sparkline(all zero) = %q, want it to say all zero", got)
	}
	if strings.Contains(got, "peak") {
		t.Fatalf("sparkline(all zero) = %q, should not also claim a peak", got)
	}
}

func TestSparklineOnAllMaxBucketsFillsEveryBlockAtTheTopLevel(t *testing.T) {
	got := sparkline([]float64{100, 100, 100})
	if !strings.HasPrefix(got, "███") {
		t.Fatalf("sparkline(all max) = %q, want three full blocks", got)
	}
	if !strings.Contains(got, "peak 100%") {
		t.Fatalf("sparkline(all max) = %q, want it to report the peak", got)
	}
}

func TestSparklineOnASingleElementDoesNotPanic(t *testing.T) {
	got := sparkline([]float64{42})
	if !strings.Contains(got, "peak 42%") {
		t.Fatalf("sparkline([42]) = %q, want the single value reported as the peak", got)
	}
}

// A burst inside an otherwise idle window must reach the top block, because
// the whole point of the chart is not to hide it behind a mean.
func TestSparklinePreservesABurstAgainstOtherwiseLowValues(t *testing.T) {
	got := sparkline([]float64{2, 3, 88, 1})
	runes := []rune(strings.SplitN(got, " ", 2)[0])
	if len(runes) != 4 {
		t.Fatalf("sparkline produced %d blocks for 4 buckets: %q", len(runes), got)
	}
	if runes[2] != '█' {
		t.Fatalf("sparkline(%v)[2] = %q, want the burst bucket at the top block '█'", []float64{2, 3, 88, 1}, string(runes[2]))
	}
	if !strings.Contains(got, "peak 88%") {
		t.Fatalf("sparkline = %q, want peak 88%%", got)
	}
}

// Values outside the nominal 0-100 range must not panic or index out of
// bounds: negative readings clamp to the lowest block instead of underflowing.
func TestSparklineClampsValuesOutsideTheExpectedRangeWithoutPanicking(t *testing.T) {
	got := sparkline([]float64{-5, 10, 3})
	if !strings.Contains(got, "peak 10%") {
		t.Fatalf("sparkline with a negative value = %q, want the real positive peak reported", got)
	}
	// A negative value must not wrap or index a block below the first one; it
	// must be visually indistinguishable from a genuine zero.
	runes := []rune(strings.SplitN(got, " ", 2)[0])
	if runes[0] != '▁' {
		t.Fatalf("sparkline clamps a negative bucket to %q, want the lowest block ▁", string(runes[0]))
	}
}

// An all-negative series has no positive max, so the internal normalisation
// never engages, and it renders identically to a genuine all-zero series —
// this is a display simplification (GPU utilization is never negative in
// practice), not a rounding bug.
func TestSparklineOnAllNegativeValuesRendersAsAllZero(t *testing.T) {
	got := sparkline([]float64{-5, -10, -1})
	if !strings.HasSuffix(got, "all zero") {
		t.Fatalf("sparkline(all negative) = %q, want it to render the same as all zero", got)
	}
}

func TestSparkAxisIsBlankBelowFourBuckets(t *testing.T) {
	if got := sparkAxis([]float64{1, 2, 3}, 336*time.Hour); got != "" {
		t.Fatalf("sparkAxis with 3 buckets = %q, want empty (too narrow to label meaningfully)", got)
	}
}

func TestSparkAxisLabelsTheWindowStartAndNow(t *testing.T) {
	got := sparkAxis(make([]float64, 40), 336*time.Hour)
	if !strings.HasPrefix(got, "14d ago") {
		t.Fatalf("sparkAxis = %q, want it to start with the window length in days", got)
	}
	if !strings.HasSuffix(got, "now") {
		t.Fatalf("sparkAxis = %q, want it to end with 'now'", got)
	}
}

// meaning() and prevention() are what "What this means" and "Stop it
// happening again" show; each check must produce prose that actually mentions
// its own evidence, not a generic fallback that could apply to any check.

func TestMeaningForIdlePodCitesTheMeasuredFallowDuration(t *testing.T) {
	f := &api.Finding{
		Check:    api.CheckIdlePod,
		Evidence: api.Evidence{FallowDuration: api.ISODuration(96 * time.Hour)},
	}
	got := meaning(f)
	if !strings.Contains(got, "4d") {
		t.Fatalf("meaning(idle pod) = %q, want it to cite the 4-day fallow duration", got)
	}
	if strings.Contains(got, "Power draw independently agrees") {
		t.Fatalf("meaning(idle pod) claimed power agreement with no power evidence present: %q", got)
	}
}

func TestMeaningForIdlePodAddsPowerCorroborationOnlyWhenItIsLow(t *testing.T) {
	low := &api.Finding{Check: api.CheckIdlePod, Evidence: api.Evidence{PowerDrawTDPRatio: 0.1}}
	if got := meaning(low); !strings.Contains(got, "Power draw independently agrees") {
		t.Fatalf("meaning(idle pod, low power ratio) = %q, want the power corroboration sentence", got)
	}

	high := &api.Finding{Check: api.CheckIdlePod, Evidence: api.Evidence{PowerDrawTDPRatio: 0.9}}
	if got := meaning(high); strings.Contains(got, "Power draw independently agrees") {
		t.Fatalf("meaning(idle pod, high power ratio) = %q, should not claim near-idle wattage when the ratio is 90%%", got)
	}
}

func TestMeaningForStuckPodDoesNotMakeAUtilizationClaim(t *testing.T) {
	f := &api.Finding{Check: api.CheckStuckPod}
	got := meaning(f)
	if !strings.Contains(got, "not a utilization judgement") {
		t.Fatalf("meaning(stuck pod) = %q, want it to explicitly disclaim being a utilization judgement", got)
	}
}

// A by-design unused node must explain itself with its own recorded reason,
// not the generic idle-node prose — printing the generic text would
// contradict the entire point of ByDesign, which is that this capacity was
// deliberately reserved.
func TestMeaningForByDesignUnusedNodeUsesItsOwnRecordedReason(t *testing.T) {
	f := &api.Finding{
		Check:    api.CheckUnusedNode,
		ByDesign: true,
		Because:  "kept warm for weekend incident response",
	}
	if got := meaning(f); got != "kept warm for weekend incident response" {
		t.Fatalf("meaning(by-design unused node) = %q, want the recorded Because verbatim", got)
	}
}

func TestMeaningForOrdinaryUnusedNodeCitesTheFallowDuration(t *testing.T) {
	f := &api.Finding{
		Check:    api.CheckUnusedNode,
		Evidence: api.Evidence{FallowDuration: api.ISODuration(240 * time.Hour)},
	}
	got := meaning(f)
	if !strings.Contains(got, "10d") {
		t.Fatalf("meaning(unused node) = %q, want it to cite the 10-day fallow duration", got)
	}
}

func TestMeaningFallsBackToTheSummaryForAnUnknownCheck(t *testing.T) {
	f := &api.Finding{Check: "some-future-check", Summary: "a summary from a check this renderer does not know"}
	if got := meaning(f); got != f.Summary {
		t.Fatalf("meaning(unknown check) = %q, want the raw Summary as a fallback", got)
	}
}

func TestPreventionNamesAConcreteMitigationPerCheck(t *testing.T) {
	cases := []struct {
		check string
		want  string
	}{
		{api.CheckIdlePod, "idle culler"},
		{api.CheckStuckPod, "backoffLimit"},
		{api.CheckUnusedNode, "autoscaler's minimum size"},
	}
	for _, tc := range cases {
		f := &api.Finding{Check: tc.check}
		if got := prevention(f); !strings.Contains(got, tc.want) {
			t.Fatalf("prevention(%s) = %q, want it to mention %q", tc.check, got, tc.want)
		}
	}
}

func TestPreventionOnAnUnknownCheckIsEmptyNotAGuess(t *testing.T) {
	f := &api.Finding{Check: "some-future-check"}
	if got := prevention(f); got != "" {
		t.Fatalf("prevention(unknown check) = %q, want empty rather than a fabricated suggestion", got)
	}
}

// --- Explain() end-to-end ---

func explainFixture() (*api.Finding, *api.Result) {
	cost := 96.0
	f := &api.Finding{
		ID:      "idle-notebook-42",
		Check:   api.CheckIdlePod,
		Summary: "notebook-42 has done no GPU work",
		Workload: api.Workload{
			Namespace: "ml", Kind: "Pod", Name: "notebook-42", Grouped: 1,
		},
		Accelerators: []api.Accelerator{
			{Model: "NVIDIA-A100-SXM4-80GB", Vendor: "nvidia", Count: 1, Allocation: api.AllocExclusive, TDPWatts: 400},
		},
		Evidence: api.Evidence{
			Window:             api.ISODuration(14 * 24 * time.Hour),
			FallowDuration:     api.ISODuration(96 * time.Hour),
			UtilizationMax:     0,
			SampleCompleteness: 1,
			Sparkline:          []float64{0, 0, 0, 0, 0},
		},
		Impact: api.Impact{
			GPUHoursFallow: 96,
			WindowCost:     &cost,
			Currency:       "USD",
			PricingSource:  "test rates",
		},
		Owner:      api.Owner{},
		Provenance: api.Provenance{Controlled: false, Recognized: true, RootKind: "Pod", RootName: "notebook-42"},
		Fix: api.Fix{
			Targets:   api.FixTargetPod,
			Command:   "kubectl delete pod -n ml notebook-42",
			Rationale: "no controller, deletion is final and sufficient",
		},
		Docs: "https://example.invalid/docs/idle-pod",
	}
	res := &api.Result{
		Scan: api.ScanMeta{
			Context: "prod-cluster",
			Started: time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC),
			Window:  api.ISODuration(14 * 24 * time.Hour),
		},
	}
	return f, res
}

func TestExplainShowsTheWorkloadEvidenceAndFixCommand(t *testing.T) {
	f, res := explainFixture()
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"ml/notebook-42",
		"idle-pod",
		"4d",             // the fallow duration, humanised
		"kubectl delete", // the fix command
		"unowned",        // no owner recorded
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Explain output missing %q:\n%s", want, out)
		}
	}
}

// The suppression hint must use the finding's ID, not its workload reference:
// suppressions match on ID, so printing the reference produces an entry a
// user could paste that would never actually match anything.
func TestExplainSuppressHintUsesTheFindingIDNotTheWorkloadRef(t *testing.T) {
	f, res := explainFixture()
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "ullage ignore idle-notebook-42") {
		t.Fatalf("Explain output does not suppress by ID:\n%s", out)
	}
}

func TestExplainShowsRiskSectionOnlyWhenPresent(t *testing.T) {
	f, res := explainFixture()
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Before you do") {
		t.Fatalf("Explain printed a risk section with no Risk set:\n%s", buf.String())
	}

	f.Risk = "This node also serves a shared inference endpoint."
	buf.Reset()
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Before you do") || !strings.Contains(buf.String(), "shared inference endpoint") {
		t.Fatalf("Explain did not print the risk section once Risk was set:\n%s", buf.String())
	}
}

func TestExplainWithNoOwnerExplainsWhyAndSkipsAConfirmationPrompt(t *testing.T) {
	f, res := explainFixture()
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "An accelerator nobody claims") {
		t.Fatalf("Explain does not explain the unowned case:\n%s", out)
	}
	// With no owner and no ConfirmWith, it must not fabricate a named person
	// to confirm with — it should say there is nobody to ask instead.
	if strings.Contains(out, "Confirm with ") {
		t.Fatalf("Explain named someone to confirm with despite no recorded owner:\n%s", out)
	}
	if !strings.Contains(out, "nobody to confirm with") {
		t.Fatalf("Explain does not say there is nobody to confirm with for an unowned finding:\n%s", out)
	}
}

func TestExplainWithAnOwnerRequiringConfirmationNamesWhoToAsk(t *testing.T) {
	f, res := explainFixture()
	f.Owner = api.Owner{Identity: "alice@example.com", ResolvedVia: "namespace annotation"}
	f.Fix.ConfirmWith = "alice@example.com"
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "alice@example.com") {
		t.Fatalf("Explain does not show the owner:\n%s", out)
	}
	if !strings.Contains(out, "Confirm with alice@example.com") {
		t.Fatalf("Explain does not name who to confirm with:\n%s", out)
	}
}

// A controlled workload's ownership chain must be shown so a reader can tell
// whether the fix command targets a pod that will just be recreated by its
// controller.
func TestExplainShowsTheOwnershipChainForAControlledWorkload(t *testing.T) {
	f, res := explainFixture()
	f.Provenance = api.Provenance{
		Controlled: true, Recognized: true, RootKind: "Deployment", RootName: "notebook",
		Chain: []api.OwnerRef{
			{Kind: "ReplicaSet", Name: "notebook-abc123"},
			{Kind: "Deployment", Name: "notebook"},
		},
	}
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Deployment/notebook") {
		t.Fatalf("Explain does not name the root owner:\n%s", out)
	}
	if !strings.Contains(out, "ReplicaSet/notebook-abc123 → Deployment/notebook") {
		t.Fatalf("Explain does not show the ownership chain:\n%s", out)
	}
}

// This is the crash this test suite caught: a finding with power-draw
// evidence but no accelerator entries used to panic with an index-out-of-range
// on Accelerators[0] (see the fix in explain.go). Rendering must degrade
// gracefully instead of taking the whole `ullage explain` command down.
func TestExplainDoesNotPanicOnPowerEvidenceWithNoAccelerators(t *testing.T) {
	f, res := explainFixture()
	f.Accelerators = nil
	f.Evidence.PowerDrawWatts = 50
	f.Evidence.PowerDrawTDPRatio = 0.1
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Explain panicked with power evidence and no accelerators: %v", r)
		}
	}()
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "50 W mean") {
		t.Fatalf("Explain dropped the power reading entirely instead of degrading gracefully:\n%s", buf.String())
	}
}

// The same class of crash, on the cost line: a WindowCost with no accelerator
// to name the model of.
func TestExplainDoesNotPanicOnACostWithNoAccelerators(t *testing.T) {
	f, res := explainFixture()
	f.Accelerators = nil
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Explain panicked with a cost and no accelerators: %v", r)
		}
	}()
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "$96") {
		t.Fatalf("Explain dropped the cost entirely instead of degrading gracefully:\n%s", buf.String())
	}
}

// Long, unicode and zero-width workload names must render without panicking,
// the same guarantee the table view has to provide.
func TestExplainHandlesLongUnicodeNamesWithoutPanicking(t *testing.T) {
	f, res := explainFixture()
	f.Workload.Name = strings.Repeat("研究チームのトレーニングジョブ🔥", 5) + "\u200b\u200b"
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Explain panicked on a long unicode name: %v", r)
		}
	}()
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestExplainRendersTheSparklineWhenPresent(t *testing.T) {
	f, res := explainFixture()
	f.Evidence.Sparkline = []float64{0, 0, 0, 88, 0, 0}
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "peak 88%") {
		t.Fatalf("Explain did not render the sparkline's peak:\n%s", buf.String())
	}
}

func TestExplainOmitsTheSparklineWhenThereIsNoData(t *testing.T) {
	f, res := explainFixture()
	f.Evidence.Sparkline = nil
	var buf bytes.Buffer
	if err := Explain(&buf, f, res, Options{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Utilization") {
		t.Fatalf("Explain printed a Utilization sparkline field with no sparkline data:\n%s", buf.String())
	}
}
