package render

import (
	"fmt"
	"testing"
	"time"

	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// ledgerResult builds a result whose numbers are chosen so the arithmetic can
// be checked by hand: 68 accelerators over a 14-day window is 22,848 paid
// hours, which is the demo cluster's shape.
func ledgerResult() *api.Result {
	const window = 14 * 24 * time.Hour
	return &api.Result{
		Scan: api.ScanMeta{
			Window:               api.ISODuration(window),
			AcceleratorsObserved: 68,
			AcceleratorsAnalyzed: 60,
			GPUHoursPaid:         68 * 336,
			GPUHoursFallow:       5856,
		},
		Recommendations: []api.Finding{
			{
				Accelerators: []api.Accelerator{{Model: "NVIDIA-A100-SXM4-80GB", Count: 3}},
				Impact:       api.Impact{GPUHoursFallow: 1008},
			},
			{
				Accelerators: []api.Accelerator{{Model: "NVIDIA-L4", Count: 8}},
				Impact:       api.Impact{GPUHoursFallow: 2688},
			},
			{
				Accelerators: []api.Accelerator{{Model: "NVIDIA-L40S", Count: 4}},
				Impact:       api.Impact{GPUHoursFallow: 1056},
			},
			{
				Accelerators: []api.Accelerator{{Model: "NVIDIA-A100-SXM4-80GB", Count: 5}},
				Impact:       api.Impact{GPUHoursFallow: 1104},
			},
		},
		ByDesign: []api.Finding{
			{
				Accelerators: []api.Accelerator{{Model: "NVIDIA-H100-SXM5-80GB", Count: 16}},
				Impact:       api.Impact{GPUHoursFallow: 5376},
			},
		},
		Suppressed: []api.Finding{},
		NotAnalyzed: []api.Exclusion{
			{Code: api.ExclTimeSliced, Accelerators: 2},
			{Code: api.ExclMIG, Accelerators: 2},
			{Code: api.ExclInitialising, Accelerators: 4},
		},
	}
}

func TestLedgerReconcilesToPaidCapacity(t *testing.T) {
	l := BuildLedger(ledgerResult())

	if !l.Reconciles {
		t.Fatalf("ledger did not reconcile: discrepancy %.4f, residual %.1f", l.Discrepancy, l.Residual)
	}

	// Every bucket, summed, must be the paid total. This is the whole point of
	// the type: a reader must be able to add the bars up.
	var sum float64
	for _, r := range l.Rows {
		sum += r.Hours
	}
	if sum != l.Paid {
		t.Errorf("rows sum to %.1f, paid is %.1f", sum, l.Paid)
	}

	// Hand-computed: 22,848 paid − 2,688 unmeasurable − 5,376 by design
	// − 0 suppressed − 5,856 fallow = 8,928.
	if l.Residual != 8928 {
		t.Errorf("residual = %.1f, want 8928", l.Residual)
	}

	want := map[string]float64{
		"not-analysed": 2688,
		"by-design":    5376,
		"suppressed":   0,
		"fallow":       5856,
		"residual":     8928,
	}
	for _, r := range l.Rows {
		if w, ok := want[r.Key]; ok && r.Hours != w {
			t.Errorf("%s = %.1f hours, want %.1f", r.Key, r.Hours, w)
		}
	}
}

// The residual is the difference between "we did not flag this" and "this was
// productive". Naming it after work would claim a measurement ullage never
// takes, so the label is pinned by a test.
func TestLedgerResidualDoesNotClaimTheCapacityWasUsed(t *testing.T) {
	l := BuildLedger(ledgerResult())

	var row LedgerRow
	for _, r := range l.Rows {
		if r.Key == "residual" {
			row = r
		}
	}
	if row.Key == "" {
		t.Fatal("no residual row")
	}
	// The label is the headline claim, and it is read on its own — in the
	// chart legend, in the accessible description, and in the table. It may
	// not assert productivity, because ullage measures fallow hours and never
	// measures productive ones.
	for _, banned := range []string{"used", "utilized", "utilised", "productive", "busy", "worked", "active"} {
		if containsFold(row.Label, banned) {
			t.Errorf("residual label %q claims the capacity was %q; ullage does not measure that", row.Label, banned)
		}
	}
	// The note may explain the nuance, but only if it carries the disclaimer
	// with it.
	if !containsFold(row.Note, "not measured as productive") {
		t.Errorf("residual note %q does not say what it is not", row.Note)
	}
}

// If two checks ever report the same device, the deductions exceed what was
// paid and the residual goes negative. The chart must refuse to draw rather
// than show bars that do not add up.
func TestLedgerRefusesToReconcileWhenFindingsOverlap(t *testing.T) {
	res := ledgerResult()
	// Double-count: claim the same hours again as fallow.
	res.Scan.GPUHoursFallow = 20000

	l := BuildLedger(res)

	if l.Reconciles {
		t.Fatal("ledger reconciled despite overlapping findings; the report would draw a waterfall that does not add up")
	}
	if l.Residual >= 0 {
		t.Errorf("residual = %.1f, expected negative to signal the overlap", l.Residual)
	}
}

// A cluster with no accelerators at all must not divide by zero.
func TestLedgerHandlesAnEmptyCluster(t *testing.T) {
	res := &api.Result{
		Scan: api.ScanMeta{
			Window: api.ISODuration(14 * 24 * time.Hour),
		},
		Recommendations: []api.Finding{},
		ByDesign:        []api.Finding{},
		Suppressed:      []api.Finding{},
		NotAnalyzed:     []api.Exclusion{},
	}

	l := BuildLedger(res)

	if !l.Reconciles {
		t.Errorf("empty cluster did not reconcile: discrepancy %.4f", l.Discrepancy)
	}
	for _, r := range l.Rows {
		if got := l.Share(r); got != 0 {
			t.Errorf("share of %s = %v on an empty cluster, want 0", r.Key, got)
		}
	}
}

// Share drives bar geometry, so it must stay inside the box even if upstream
// hands it something impossible.
func TestLedgerShareIsAlwaysAProportion(t *testing.T) {
	l := Ledger{Paid: 100}
	cases := []struct {
		hours float64
		want  float64
	}{
		{hours: 50, want: 0.5},
		{hours: 0, want: 0},
		{hours: -10, want: 0},
		{hours: 250, want: 1},
	}
	for _, c := range cases {
		if got := l.Share(LedgerRow{Hours: c.hours}); got != c.want {
			t.Errorf("Share(%.0f of 100) = %v, want %v", c.hours, got, c.want)
		}
	}
}

func containsFold(haystack, needle string) bool {
	h, n := []rune(lower(haystack)), []rune(lower(needle))
	if len(n) > len(h) {
		return false
	}
	for i := 0; i+len(n) <= len(h); i++ {
		match := true
		for j := range n {
			if h[i+j] != n[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}

// The ledger is the one table in the document whose whole purpose is to show
// that the headline equals the sum of its parts. Rounding each share on its
// own puts 39 + 12 + 24 + 0 + 26 = 101% directly above a total row reading
// 100%, and a reader who notices has been handed a reason to disbelieve
// everything else on the page.
func TestLedgerSharesAddUpToExactlyOneHundred(t *testing.T) {
	// The proportions the demo cluster actually produces, which is where the
	// 101% was found.
	for _, shares := range [][]float64{
		{8928.0 / 22848, 2688.0 / 22848, 5376.0 / 22848, 0, 5856.0 / 22848},
		{1.0 / 3, 1.0 / 3, 1.0 / 3},
		{0.5, 0.5},
		{1},
		{0.985, 0.005, 0.005, 0.005},
	} {
		total, approx := 0, false
		var displayed []string
		for _, pct := range apportion(shares) {
			displayed = append(displayed, pct)
			if pct == "<1%" {
				// Deliberately not whole, and already rounded up: the row
				// takes no part in the exact total.
				approx = true
				continue
			}
			var n int
			if _, err := fmt.Sscanf(pct, "%d%%", &n); err != nil {
				t.Fatalf("share %q is not a whole percentage", pct)
			}
			total += n
		}
		switch {
		case approx && total > 100:
			t.Errorf("shares %v render as %v, summing to %d%% — above a total row that "+
				"says 100%%.", shares, displayed, total)
		case !approx && total != 100:
			t.Errorf("shares %v render as %v, summing to %d%% under a total row that says "+
				"100%%. The parts must add up on the one table that exists to prove the "+
				"headline equals the sum of its parts — in either direction, since a "+
				"reader checking the arithmetic finds 99%% exactly as fast as 101%%.",
				shares, displayed, total)
		}
	}
}

// A bucket holding real hours must never render as "0%": beside a non-zero
// hour count in the next column it reads as a contradiction, and the fix for
// the sum is not allowed to reintroduce it.
func TestATinyButRealBucketIsNeverShownAsZero(t *testing.T) {
	got := apportion([]float64{0.9994, 0.0006})
	if got[1] != "<1%" {
		t.Fatalf("share of 0.06%% rendered as %q, want \"<1%%\": rounding a bucket that "+
			"holds real accelerator-hours down to nothing invites the reader to "+
			"ignore it.", got[1])
	}
}
