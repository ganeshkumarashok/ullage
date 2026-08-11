package render

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// These tests build a *api.Result from literals, the same way check_test.go
// builds clusters from literals: a renderer's behaviour is decided entirely by
// the struct it is given, so no fixture files or golden output are needed.

func baseResult() *api.Result {
	return &api.Result{
		APIVersion: api.Version,
		Scan: api.ScanMeta{
			Context:              "prod-cluster",
			Window:               api.ISODuration(14 * 24 * time.Hour),
			GPUHoursPaid:         1000,
			GPUHoursFallow:       0,
			AcceleratorsAnalyzed: 40,
			AcceleratorsObserved: 40,
		},
		Recommendations: []api.Finding{},
		Suppressed:      []api.Finding{},
		NotAnalyzed:     []api.Exclusion{},
		Warnings:        []string{},
	}
}

func idleFinding(ns, name string, gpus int, owner string) api.Finding {
	cost := 42.0
	return api.Finding{
		ID:                  "idle-" + name,
		Check:               api.CheckIdlePod,
		EvidenceConfidence:  api.EvidenceHigh,
		OwnershipConfidence: api.OwnerResolved,
		Summary:             name + " has done no GPU work",
		Workload:            api.Workload{Namespace: ns, Kind: "Pod", Name: name, Grouped: 1},
		Accelerators: []api.Accelerator{
			{Model: "NVIDIA-A100-SXM4-80GB", Vendor: "nvidia", Count: gpus, Allocation: api.AllocExclusive},
		},
		Evidence: api.Evidence{
			Window:         api.ISODuration(14 * 24 * time.Hour),
			FallowDuration: api.ISODuration(96 * time.Hour),
		},
		Impact: api.Impact{
			GPUHoursFallow: float64(gpus) * 96,
			WindowCost:     &cost,
			Currency:       "USD",
			PricingSource:  "test rates",
		},
		Owner:      api.Owner{Identity: owner, ResolvedVia: "pod label"},
		Provenance: api.Provenance{Controlled: false, Recognized: true, RootKind: "Pod", RootName: name},
		Fix:        api.Fix{Targets: api.FixTargetPod, Command: "kubectl delete pod " + name, Rationale: "no controller, deletion is final"},
		Docs:       "https://example.invalid/docs/idle-pod",
	}
}

// A cluster with nothing to report must not print the table header, a
// separator, or any row — those would look like a table with zero-width rows
// rather than the absence of a problem, and a reader skimming for the header
// alone could mistake it for "the scan is still loading".
func TestTableWithZeroFindingsPrintsNoRecommendationsNotAnEmptyTable(t *testing.T) {
	res := baseResult()
	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "WORKLOAD") {
		t.Fatalf("output contains the table header with no rows under it:\n%s", out)
	}
	if !strings.Contains(out, "No recommendations") {
		t.Fatalf("output does not say there were no recommendations:\n%s", out)
	}
}

func TestTableWithFindingsShowsWorkloadGPUCountAndOwner(t *testing.T) {
	res := baseResult()
	f := idleFinding("ml", "notebook-42", 2, "alice@example.com")
	res.Recommendations = []api.Finding{f}
	res.Scan.GPUHoursFallow = f.Impact.GPUHoursFallow

	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "ml/notebook-42") {
		t.Fatalf("output does not name the workload:\n%s", out)
	}
	if !strings.Contains(out, "2") {
		t.Fatalf("output does not show the accelerator count 2:\n%s", out)
	}
	// The owner cell shortens a long email from the domain, not the local
	// part (see ownerCell's doc comment), so only the prefix is guaranteed.
	if !strings.Contains(out, "alice@") {
		t.Fatalf("output does not identify the owner:\n%s", out)
	}
}

func TestTableRendersEachFindingWithARankMarker(t *testing.T) {
	res := baseResult()
	a := idleFinding("ml", "notebook-a", 1, "a@x.com")
	b := idleFinding("ml", "notebook-b", 1, "b@x.com")
	res.Recommendations = []api.Finding{a, b}

	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	ia := strings.Index(out, "notebook-a")
	ib := strings.Index(out, "notebook-b")
	if ia == -1 || ib == -1 {
		t.Fatalf("both findings must appear:\n%s", out)
	}
	if ia > ib {
		t.Fatalf("finding order was not preserved: notebook-b printed before notebook-a")
	}
	if !strings.Contains(out, "1.") || !strings.Contains(out, "2.") {
		t.Fatalf("output does not carry rank markers 1. and 2.:\n%s", out)
	}
}

// --top truncates the visible list but must say so, rather than silently
// dropping findings a reader would otherwise assume were the whole result.
func TestTableWithMoreFindingsThanTopSaysHowManyAreHidden(t *testing.T) {
	res := baseResult()
	for i := 0; i < 5; i++ {
		res.Recommendations = append(res.Recommendations, idleFinding("ml", "job-"+string(rune('a'+i)), 1, ""))
	}
	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test", Top: 2}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "3 more recommendations") {
		t.Fatalf("output does not report the 3 hidden findings:\n%s", out)
	}
	if strings.Contains(out, "job-e") {
		t.Fatalf("a finding beyond --top 2 was printed anyway:\n%s", out)
	}
}

// A workload name far longer than the column budget must be shortened, not
// left to blow out the table's fixed-width layout, and must not panic. The
// full reference is still allowed to appear once, in the "Next: ullage
// explain ..." line, since that is a copy-pasteable command and truncating it
// there would make it unusable.
func TestTableTruncatesAnExtremelyLongWorkloadNameWithoutPanicking(t *testing.T) {
	res := baseResult()
	longName := strings.Repeat("very-long-training-job-name-", 10)
	res.Recommendations = []api.Finding{idleFinding("ml", longName, 1, "")}

	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "1.") && strings.Contains(line, longName) {
			t.Fatalf("the table row was not shortened and blows out the fixed-width layout:\n%s", line)
		}
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("a truncated name should carry the elision marker:\n%s", out)
	}
}

// Unicode workload names must render as valid UTF-8 and must not panic, even
// when they are long enough to require truncation. truncate() used to slice
// by byte offset, which could split a multi-byte rune and emit invalid UTF-8
// into the table (see the fix in table.go).
func TestTableHandlesUnicodeWorkloadNamesWithoutCorruptingOutput(t *testing.T) {
	res := baseResult()
	name := strings.Repeat("研究チームのトレーニングジョブ", 3)
	res.Recommendations = []api.Finding{idleFinding("ml", name, 1, "")}

	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test"}); err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(buf.Bytes()) {
		t.Fatalf("output is not valid UTF-8:\n%q", buf.String())
	}
}

// Zero-width characters (e.g. a stray U+200B in a copy-pasted label) must not
// panic the fixed-width column formatting.
func TestTableHandlesZeroWidthCharactersWithoutPanicking(t *testing.T) {
	res := baseResult()
	name := "job\u200b\u200b\u200bwith\u200bzero\u200bwidth\u200bspaces\u200bthat\u200bis\u200bpretty\u200blong"
	res.Recommendations = []api.Finding{idleFinding("ml", name, 1, "")}

	var buf bytes.Buffer
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Table panicked on a zero-width-character name: %v", r)
		}
	}()
	if err := Table(&buf, res, Options{Version: "v-test"}); err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(buf.Bytes()) {
		t.Fatal("output is not valid UTF-8")
	}
}

// ByDesign findings are how a "this capacity is idle on purpose" decision
// stays visible (see Finding.ByDesign's doc comment). They must show up
// alongside actionable recommendations, not only when there happen to be some.
func TestTableShowsByDesignFindingsAlongsideRecommendations(t *testing.T) {
	res := baseResult()
	res.Recommendations = []api.Finding{idleFinding("ml", "notebook", 1, "")}
	res.ByDesign = []api.Finding{{
		Summary:  "reserved capacity for the on-call GPU pool",
		Because:  "kept warm for incident response, not counted as waste",
		ByDesign: true,
		Accelerators: []api.Accelerator{
			{Model: "NVIDIA-A100-SXM4-80GB", Count: 4},
		},
		Impact: api.Impact{GPUHoursFallow: 200},
	}}

	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Fallow by design") {
		t.Fatalf("output does not show the by-design section:\n%s", out)
	}
	if !strings.Contains(out, "reserved capacity for the on-call GPU pool") {
		t.Fatalf("output does not name the by-design finding:\n%s", out)
	}
}

// This is the case the by-design section previously dropped silently: zero
// actionable recommendations but at least one recorded by-design decision.
// Before the fix, Table took the "No recommendations" early return and never
// called renderByDesign at all, so a cluster that was entirely idle-by-design
// looked indistinguishable from one with no idle capacity whatsoever — the
// exact transparency ByDesign exists to provide was lost.
func TestTableShowsByDesignFindingsEvenWithZeroRecommendations(t *testing.T) {
	res := baseResult()
	res.ByDesign = []api.Finding{{
		Summary:      "reserved capacity for the on-call GPU pool",
		Because:      "kept warm for incident response, not counted as waste",
		ByDesign:     true,
		Accelerators: []api.Accelerator{{Model: "NVIDIA-A100-SXM4-80GB", Count: 4}},
		Impact:       api.Impact{GPUHoursFallow: 200},
	}}

	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "No recommendations") {
		t.Fatalf("expected the no-recommendations headline:\n%s", out)
	}
	if !strings.Contains(out, "Fallow by design") {
		t.Fatalf("a by-design finding with zero recommendations was not shown at all; "+
			"the decision it records became invisible instead of staying visible:\n%s", out)
	}
}

func TestTableShowsSuppressedTotalWithItsSize(t *testing.T) {
	res := baseResult()
	res.Recommendations = []api.Finding{idleFinding("ml", "notebook", 1, "")}
	cost := 10.0
	res.Suppressed = []api.Finding{{
		Summary: "suppressed job",
		Impact:  api.Impact{GPUHoursFallow: 50, WindowCost: &cost, Currency: "USD"},
	}}

	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test", ConfigFile: ".ullage.yaml"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "1 finding suppressed by .ullage.yaml") {
		t.Fatalf("output does not report the suppressed finding with its config file and count:\n%s", out)
	}
	if !strings.Contains(out, "50") {
		t.Fatalf("output does not report the suppressed accelerator-hours:\n%s", out)
	}
}

func TestTableRendersNotAnalyzedAndUnmetDemandAsContextNotFindings(t *testing.T) {
	res := baseResult()
	res.Recommendations = []api.Finding{idleFinding("ml", "notebook", 1, "")}
	res.NotAnalyzed = []api.Exclusion{
		{Code: api.ExclMIG, Reason: "MIG-partitioned devices", Accelerators: 8},
	}
	res.UnmetDemand = &api.UnmetDemand{Pods: 3, Accelerators: 6, Detail: "waiting for H100s"}

	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Not analysed") {
		t.Fatalf("output does not show the Not analysed section:\n%s", out)
	}
	if !strings.Contains(out, "Unmet demand") {
		t.Fatalf("output does not show the Unmet demand section:\n%s", out)
	}
	if !strings.Contains(out, "not a finding") {
		t.Fatalf("unmet demand must be explicitly labelled context, not a finding:\n%s", out)
	}
}

// --- unit tests for the unexported helpers renderRows and friends depend on ---

func TestOwnerCellShortensALongEmailFromTheDomainNotTheLocalPart(t *testing.T) {
	p := newPrinter(&bytes.Buffer{}, Options{})
	cases := []struct {
		name string
		id   string
		want string
	}{
		{"no owner", "", "unowned"},
		{"short email is shown in full", "a@b.com", "a@b.com"},
		{
			"long email is shortened after the @",
			"alice@example.com",
			"alice@…",
		},
		{"non-email identity is shown in full even if long", "svc-account-ml-training-pipeline", "svc-account-ml-training-pipeline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := api.Finding{Owner: api.Owner{Identity: tc.id}}
			if got := p.ownerCell(f); got != tc.want {
				t.Fatalf("ownerCell(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func TestReasonForIdlePodNamesWhenWorkWasLastSeen(t *testing.T) {
	p := newPrinter(&bytes.Buffer{}, Options{})
	f := idleFinding("ml", "notebook", 1, "")
	got := p.reasonFor(f)
	if !strings.Contains(got, "no GPU work since") {
		t.Fatalf("reasonFor(idle pod) = %q, does not explain what was measured", got)
	}
}

func TestReasonForIdlePodWithNoRecordedActivityBlamesTheWindowNotAFakeTimestamp(t *testing.T) {
	p := newPrinter(&bytes.Buffer{}, Options{})
	f := idleFinding("ml", "notebook", 1, "")
	f.Evidence.LastNonZeroUtilization = nil
	got := p.reasonFor(f)
	if !strings.Contains(got, "the window began") {
		t.Fatalf("reasonFor with no LastNonZeroUtilization = %q, want it to say activity was never seen rather than printing a zero time", got)
	}
}

func TestReasonForUnusedNodeReportsBlockingPods(t *testing.T) {
	p := newPrinter(&bytes.Buffer{}, Options{})
	f := api.Finding{
		Check:    api.CheckUnusedNode,
		Workload: api.Workload{Grouped: 2},
		Fix:      api.Fix{Blockers: []api.Blocker{{Object: "pod/x", Reason: "no eviction toleration"}}},
	}
	got := p.reasonFor(f)
	if !strings.Contains(got, "2 nodes") {
		t.Fatalf("reasonFor(unused node) = %q, does not report the node count", got)
	}
	if !strings.Contains(got, "block scale-down") {
		t.Fatalf("reasonFor(unused node) = %q, does not mention the blocker", got)
	}
}

func TestActionablePrefersAFindingWithARealCommand(t *testing.T) {
	noCommand := api.Finding{Workload: api.Workload{Name: "a"}, Fix: api.Fix{}}
	blocked := api.Finding{Workload: api.Workload{Name: "b"}, Fix: api.Fix{Command: "kubectl delete", Targets: api.FixTargetNone}}
	real := api.Finding{Workload: api.Workload{Name: "c"}, Fix: api.Fix{Command: "kubectl delete pod c", Targets: api.FixTargetPod}}

	got := actionable([]api.Finding{noCommand, blocked, real})
	if got == nil || got.Workload.Name != "c" {
		t.Fatalf("actionable() = %+v, want the finding with a real, unblocked command", got)
	}
}

func TestActionableFallsBackToTheFirstFindingWhenNoneHaveACommand(t *testing.T) {
	a := api.Finding{Workload: api.Workload{Name: "a"}}
	b := api.Finding{Workload: api.Workload{Name: "b"}}
	got := actionable([]api.Finding{a, b})
	if got == nil || got.Workload.Name != "a" {
		t.Fatalf("actionable() = %+v, want the first finding as a fallback", got)
	}
}

func TestActionableOnEmptyListReturnsNilNotAPanic(t *testing.T) {
	if got := actionable(nil); got != nil {
		t.Fatalf("actionable(nil) = %+v, want nil", got)
	}
}

func TestFirstNoteFallsBackToAGenericExplanationWhenThereAreNone(t *testing.T) {
	f := api.Finding{}
	if got := firstNote(f); got == "" {
		t.Fatal("firstNote with no notes returned empty; a low-confidence row would show a blank reason")
	}
	f.Evidence.Notes = []string{"only 40% of samples present"}
	if got := firstNote(f); got != "only 40% of samples present" {
		t.Fatalf("firstNote = %q, want the first recorded note", got)
	}
}

func TestMoneyGroupsThousandsWithoutFalsePrecision(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1234, "1,234"},
		{1234567, "1,234,567"},
		{999.6, "1,000"}, // no decimal places: the rate behind it is never exact
	}
	for _, tc := range cases {
		if got := money(tc.in); got != tc.want {
			t.Fatalf("money(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPluralOnlyAgreesInNumberAtExactlyOne(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0 pods"},
		{1, "1 pod"},
		{2, "2 pods"},
		{-1, "-1 pods"},
	}
	for _, tc := range cases {
		if got := plural(tc.n, "pod"); got != tc.want {
			t.Fatalf("plural(%d, pod) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestHoursCompactsAtThousandBoundaries(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{10000, "10k"},
		{-5, "-5"},
	}
	for _, tc := range cases {
		if got := hours(tc.in); got != tc.want {
			t.Fatalf("hours(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCurrencySymbolCoversKnownCurrenciesAndFallsBackToTheCode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"USD", "$"},
		{"usd", "$"}, // case-insensitive
		{"EUR", "€"},
		{"GBP", "£"},
		{"", "$"}, // an empty currency defaults to USD's symbol
		{"JPY", "JPY "},
	}
	for _, tc := range cases {
		if got := currencySymbol(tc.in); got != tc.want {
			t.Fatalf("currencySymbol(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncateLeavesShortStringsUntouched(t *testing.T) {
	if got := truncate("short", 34); got != "short" {
		t.Fatalf("truncate did not leave a short string alone: %q", got)
	}
}

func TestTruncateElidesFromTheLeftKeepingTheIdentifyingTail(t *testing.T) {
	// The tail of a workload reference (the pod/job name) is more identifying
	// than its head (often just a shared namespace prefix).
	got := truncate("very-long-namespace/actual-workload-name", 20)
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("truncate(%q) = %q, want it to elide from the left", "very-long-namespace/actual-workload-name", got)
	}
	if !strings.HasSuffix(got, "workload-name") {
		t.Fatalf("truncate(%q) = %q, want the identifying tail preserved", "very-long-namespace/actual-workload-name", got)
	}
}

func TestTruncateHandlesDegenerateWidthsWithoutPanicking(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
	}{
		{"n is zero", "hello", 0},
		{"n is negative", "hello", -5},
		{"n is one", "hello", 1},
		{"empty string", "", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("truncate(%q, %d) panicked: %v", tc.s, tc.n, r)
				}
			}()
			_ = truncate(tc.s, tc.n)
		})
	}
}

// truncate used to slice by byte offset, which can land inside a multi-byte
// rune and produce invalid UTF-8 in the printed table. It now counts and
// slices in runes.
func TestTruncateNeverEmitsInvalidUTF8ForMultiByteNames(t *testing.T) {
	names := []string{
		strings.Repeat("研究チームのトレーニングジョブ名前空間テスト", 1),
		"🔥🔥🔥🔥🔥🔥🔥🔥🔥🔥workload-with-emoji-prefix",
		"a\u200bb\u200bc\u200bd\u200be\u200bf\u200bg\u200bh\u200bi\u200bj",
	}
	for _, name := range names {
		for _, width := range []int{1, 5, 10, 15, 20, 34} {
			got := truncate(name, width)
			if !utf8.ValidString(got) {
				t.Fatalf("truncate(%q, %d) = %q, which is not valid UTF-8", name, width, got)
			}
		}
	}
}

func TestSortedKeysReturnsNamesInAscendingOrder(t *testing.T) {
	m := map[string]int{"gamma": 1, "alpha": 2, "beta": 3}
	got := sortedKeys(m)
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("sortedKeys returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedKeys returned %v, want %v", got, want)
		}
	}
}

func TestSortedKeysOnEmptyMapReturnsEmptyNotNil(t *testing.T) {
	got := sortedKeys(map[string]int{})
	if got == nil {
		t.Fatal("sortedKeys(empty map) returned nil; a caller ranging over it is fine either way, but this differs from make([]string, 0)")
	}
	if len(got) != 0 {
		t.Fatalf("sortedKeys(empty map) = %v, want empty", got)
	}
}

// A zero-findings run where every finding was filtered by --min-confidence
// must say so and tell the reader how to see them — silently reporting
// "no recommendations" would look identical to a genuinely clean cluster,
// which is the one distinction this footer exists to preserve.
func TestZeroFindingsBelowConfidenceThresholdSaysSoAndHowToSeeThem(t *testing.T) {
	res := baseResult()
	res.BelowThreshold = 3
	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test", MinConfidence: "medium"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "3 findings were below --min-confidence medium") {
		t.Fatalf("output does not report the suppressed-by-confidence count:\n%s", out)
	}
	if !strings.Contains(out, "--min-confidence low") {
		t.Fatalf("output does not tell the reader how to see the filtered findings:\n%s", out)
	}
}

// A genuinely clean cluster (BelowThreshold == 0) gets a different, doctor
// pointing footer instead of a fabricated confidence-filter explanation.
func TestZeroFindingsWithNothingFilteredPointsAtDoctorNotAFabricatedReason(t *testing.T) {
	res := baseResult()
	var buf bytes.Buffer
	if err := Table(&buf, res, Options{Version: "v-test"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "ullage doctor") {
		t.Fatalf("output does not suggest `ullage doctor` for a genuinely clean run:\n%s", out)
	}
	if strings.Contains(out, "--min-confidence") {
		t.Fatalf("output fabricated a confidence-filter explanation with BelowThreshold == 0:\n%s", out)
	}
}

// Colour is opt-in via Options.Color, independent of TTY detection (which is
// exercised separately in printer_test.go). Section headers and command lines
// must carry the ANSI codes when it is on, and the plain text must still be
// present underneath them so a reader piping through a colour-stripping tool
// loses nothing.
func TestTableWrapsHeadersAndCommandsInANSIWhenColorIsRequested(t *testing.T) {
	res := baseResult()
	f := idleFinding("ml", "notebook-42", 1, "alice@example.com")
	res.Recommendations = []api.Finding{f}
	res.Scan.GPUHoursFallow = f.Impact.GPUHoursFallow

	var plain, colored bytes.Buffer
	if err := Table(&plain, res, Options{Version: "v-test", Color: false}); err != nil {
		t.Fatal(err)
	}
	if err := Table(&colored, res, Options{Version: "v-test", Color: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(colored.String(), "\033[") {
		t.Fatalf("Color: true produced no ANSI escape codes at all:\n%s", colored.String())
	}
	if strings.Contains(plain.String(), "\033[") {
		t.Fatalf("Color: false leaked an ANSI escape code into piped output:\n%s", plain.String())
	}
	if !strings.Contains(colored.String(), "ml/notebook-42") {
		t.Fatalf("colored output lost the workload name underneath the escape codes:\n%s", colored.String())
	}
}
