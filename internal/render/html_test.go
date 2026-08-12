package render

import (
	"bytes"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// TestCapacityBarIsTheLedger is the assertion that makes the picture worth
// printing. A chart whose segments do not add up to the total it claims to
// divide is worse than no chart: it looks like evidence while being decoration.
func TestCapacityBarIsTheLedger(t *testing.T) {
	res := htmlFixture()

	var buf bytes.Buffer
	if err := HTML(&buf, res, HTMLOptions{}); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	segs := barSegments(t, out)
	if len(segs) < 2 {
		t.Fatalf("expected several segments, got %d; the test would prove nothing", len(segs))
	}

	// Every segment begins exactly where the last one ended. A gap or an
	// overlap would be visible as capacity invented or lost.
	var x float64
	for i, s := range segs {
		if math.Abs(s.x-x) > 0.01 {
			t.Errorf("segment %d starts at %.2f, want %.2f: the bar has a gap or an overlap", i, s.x, x)
		}
		x += s.w
	}
	if math.Abs(x-ledgerBarWidth) > 0.01 {
		t.Errorf("segments span %.2f of %.0f units: the bar does not fill the capacity it claims to divide", x, ledgerBarWidth)
	}
}

// TestLedgerNumbersReachTheDocument guards against a chart drawn from one
// calculation and a table written from another.
func TestLedgerNumbersReachTheDocument(t *testing.T) {
	res := htmlFixture()
	l := BuildLedger(res)

	var buf bytes.Buffer
	if err := HTML(&buf, res, HTMLOptions{}); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	for _, row := range l.Rows {
		if row.Hours == 0 {
			continue
		}
		if want := hours(row.Hours); !strings.Contains(out, want) {
			t.Errorf("row %q: %s accelerator-hours is not in the document", row.Label, want)
		}
	}
	if want := hours(l.Paid); !strings.Contains(out, want) {
		t.Errorf("the paid total %s is not in the document", want)
	}
}

// TestReportRefusesToChartAnUnreconciledLedger pins the fail-closed path. If
// the buckets ever stop adding up, the report must say so rather than draw a
// bar that quietly misrepresents the shortfall.
func TestReportRefusesToChartAnUnreconciledLedger(t *testing.T) {
	res := htmlFixture()
	// Claim more unused hours than were ever paid for. Nothing can divide a
	// total into parts larger than itself.
	res.Scan.GPUHoursUnused = res.Scan.GPUHoursPaid * 3

	var buf bytes.Buffer
	if err := HTML(&buf, res, HTMLOptions{}); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	if segs := barSegments(t, out); len(segs) != 0 {
		t.Errorf("drew %d segments for a ledger that does not reconcile", len(segs))
	}
	// Silence would be the worst outcome: a reader who sees findings and no
	// chart must be told the total could not be divided, not left guessing.
	if !strings.Contains(out, "Capacity reconciliation unavailable") {
		t.Error("the report omitted the chart but did not tell the reader why")
	}
	if !strings.Contains(out, `role="alert"`) {
		t.Error("the reconciliation notice is not announced to assistive technology")
	}
	// The findings themselves must survive; the chart is the only casualty.
	if !strings.Contains(out, "team/trainer") {
		t.Error("an unreconciled ledger suppressed the findings as well as the chart")
	}
}

// TestReportMakesNoNetworkRequests is the offline guarantee. The report is
// meant to be opened from a file:// URL, mailed around, and attached to a
// ticket, none of which may phone home or leak the fact that it was read.
func TestReportMakesNoNetworkRequests(t *testing.T) {
	res := htmlFixture()

	var buf bytes.Buffer
	if err := HTML(&buf, res, HTMLOptions{}); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	// Any element that fetches a subresource, and the CSS equivalents.
	for _, bad := range []string{"<link", "<script src", "<img", "<iframe", "<object", "<embed", "@import", "url(http"} {
		if strings.Contains(strings.ToLower(out), bad) {
			t.Errorf("the report fetches a subresource: found %q", bad)
		}
	}
}

// TestReportIsDeterministic matters because these reports get committed,
// diffed and attached to tickets. Two runs over the same scan must produce
// the same bytes, or every diff is noise.
func TestReportIsDeterministic(t *testing.T) {
	res := htmlFixture()
	o := HTMLOptions{Now: res.Scan.Started}

	var a, b bytes.Buffer
	if err := HTML(&a, res, o); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	if err := HTML(&b, res, o); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	if a.String() != b.String() {
		t.Error("two renders of one scan differ")
	}
}

// TestReportWorksWithoutJavaScript is the accessibility floor: the script is
// a copy button and nothing else, so no content may depend on it.
func TestReportWorksWithoutJavaScript(t *testing.T) {
	res := htmlFixture()

	var buf bytes.Buffer
	if err := HTML(&buf, res, HTMLOptions{}); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	body := out[strings.Index(out, "<body"):]
	if i := strings.Index(body, "<script"); i >= 0 {
		// Whatever the script does, the command text must already be present
		// as document content rather than written in by it.
		if !strings.Contains(body[:i], res.Recommendations[0].Fix.Command) {
			t.Error("the fix command is not in the document before the script runs")
		}
	}
	// The copy button is the one control that does nothing without scripting,
	// so it ships hidden and the script reveals it.
	if strings.Contains(out, `class="copy"`) && !strings.Contains(out, "hidden>copy") {
		t.Error("the copy button is visible without the script that makes it work")
	}
}

// TestOwnerBarsRankByWhatTheyDisplay guards a contradiction that is easy to
// introduce and hard to unsee: accelerators do not cost the same per hour, so
// a list ordered by hours but labelled with money puts the most expensive
// owner in third place. A reader who spots that stops trusting the document.
func TestOwnerBarsRankByWhatTheyDisplay(t *testing.T) {
	res := htmlFixture()
	cheapButLong := 100.0
	dearButShort := 900.0
	res.Recommendations[0].Impact.WindowCost = &cheapButLong
	res.Recommendations[0].Impact.GPUHoursUnused = 800
	res.Recommendations = append(res.Recommendations, api.Finding{
		Rank:     2,
		ID:       "idle-pod/team/inference",
		Check:    "idle-pod",
		Summary:  "team/inference: 1 accelerator held with no work",
		Workload: api.Workload{Namespace: "team", Kind: "Deployment", Name: "inference", Grouped: 1},
		Owner:    api.Owner{Identity: "expensive-team", ResolvedVia: "label"},
		Impact:   api.Impact{GPUHoursUnused: 40, WindowCost: &dearButShort, Currency: "USD"},
		Evidence: api.Evidence{Window: res.Scan.Window, UnusedDuration: res.Scan.Window, SampleCompleteness: 0.99},
		Fix:      api.Fix{Targets: "team/inference"},
	})

	bars, _, measure := ownerBars(res, HTMLOptions{})
	if len(bars) < 2 {
		t.Fatalf("got %d bars, want at least 2", len(bars))
	}
	if measure != "cost" {
		t.Fatalf("measure = %q, want %q when every owner is priced", measure, "cost")
	}
	// expensive-team has a fifth of the hours and nine times the cost.
	if bars[0].Name != "expensive-team" {
		t.Errorf("first bar is %q; the most expensive owner should lead a list ranked by cost", bars[0].Name)
	}
	// The bar length has to agree with the order, or the picture contradicts
	// the ranking it illustrates.
	for i := 1; i < len(bars); i++ {
		if bars[i].Share > bars[i-1].Share+1e-9 {
			t.Errorf("bar %d is longer than bar %d: length and order disagree", i, i-1)
		}
	}
}

// TestOwnerBarsFallBackToHours pins the other half: cost ranking must not be
// used when it would silently sink an unpriced owner to the bottom at zero.
func TestOwnerBarsFallBackToHours(t *testing.T) {
	res := htmlFixture()
	res.Recommendations = append(res.Recommendations, api.Finding{
		Rank:     2,
		ID:       "idle-pod/team/unpriced",
		Check:    "idle-pod",
		Workload: api.Workload{Namespace: "team", Kind: "Deployment", Name: "unpriced", Grouped: 1},
		Owner:    api.Owner{Identity: "no-price-team", ResolvedVia: "label"},
		Impact:   api.Impact{GPUHoursUnused: 5000}, // deliberately no WindowCost
		Evidence: api.Evidence{Window: res.Scan.Window, UnusedDuration: res.Scan.Window},
		Fix:      api.Fix{Targets: "team/unpriced"},
	})

	bars, _, measure := ownerBars(res, HTMLOptions{})
	if measure != "unused accelerator-hours" {
		t.Fatalf("measure = %q, want the hours fallback when an owner has no price", measure)
	}
	if bars[0].Name != "no-price-team" {
		t.Errorf("first bar is %q; the owner with the most unused hours should lead", bars[0].Name)
	}
}

type segment struct{ x, w float64 }

var rectRe = regexp.MustCompile(`<rect\s+class="(bar-[^"]*|hatch-[^"]*)"\s+x="([0-9.]+)"\s+y="[0-9]+"\s+width="([0-9.]+)"`)

func barSegments(t *testing.T, doc string) []segment {
	t.Helper()

	var out []segment
	for _, m := range rectRe.FindAllStringSubmatch(doc, -1) {
		x, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("x=%q: %v", m[2], err)
		}
		w, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			t.Fatalf("width=%q: %v", m[3], err)
		}
		out = append(out, segment{x: x, w: w})
	}
	return out
}

// htmlFixture is a small cluster whose ledger reconciles exactly:
// 2688 paid = 336 not-analysable + 672 by-design + 840 unused + 840 residual.
func htmlFixture() *api.Result {
	const window = api.ISODuration(336 * 3600 * 1e9)
	cost := 840.0

	return &api.Result{
		APIVersion: "ullage.dev/v1alpha1",
		Scan: api.ScanMeta{
			Tool:                 api.Tool{Name: "ullage", Version: "test"},
			Context:              "prod",
			Window:               window,
			AcceleratorsObserved: 8,
			AcceleratorsAnalyzed: 7,
			GPUHoursPaid:         2688,
			GPUHoursUnused:       840,
		},
		Recommendations: []api.Finding{{
			Rank:     1,
			ID:       "idle-pod/team/trainer",
			Check:    "idle-pod",
			Summary:  "team/trainer: 2 accelerators held with no work for 14d",
			Because:  "held 2 accelerators and did no work on them",
			Workload: api.Workload{Namespace: "team", Kind: "Deployment", Name: "trainer", Grouped: 1},
			Owner:    api.Owner{Identity: "team-a", ResolvedVia: "label"},
			Impact:   api.Impact{GPUHoursUnused: 840, WindowCost: &cost, Currency: "USD"},
			Evidence: api.Evidence{Window: window, UnusedDuration: window, SampleCompleteness: 0.99},
			Fix: api.Fix{
				Targets: "team/trainer",
				Command: "kubectl scale deployment -n team trainer --replicas=0",
			},
			EvidenceConfidence:  "high",
			OwnershipConfidence: "high",
		}},
		ByDesign: []api.Finding{{
			ID:       "unused-node/spare",
			Check:    "unused-node",
			Summary:  "spare capacity held deliberately",
			Workload: api.Workload{Kind: "Node", Name: "spare"},
			Impact:   api.Impact{GPUHoursUnused: 672},
			Evidence: api.Evidence{Window: window, UnusedDuration: window, SampleCompleteness: 1},
		}},
		NotAnalyzed: []api.Exclusion{{
			Code:         "mig-mixed",
			Reason:       "MIG devices are not analysed per pod",
			Accelerators: 1,
			Detail:       "one card exposes MIG profiles",
		}},
		Warnings: []string{},
	}
}
