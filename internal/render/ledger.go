package render

import (
	"sort"
	"strconv"

	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// Ledger sorts every accelerator-hour the cluster paid for into exactly one
// bucket, so that the buckets sum back to the total.
//
// This is the arithmetic behind the report's hero chart, and it is separated
// from the drawing because a chart that does not reconcile is worse than no
// chart: it invites the reader to trust a picture that quietly loses hours.
// Reconciles reports whether the sum held. When it does not, the renderer shows
// the ledger as plain rows and says so, rather than drawing a waterfall whose
// bars do not add up to the bar above them.
//
// The buckets are mutually exclusive by construction upstream, not by
// arithmetic here. A device held by a pod cannot also sit on a node that no pod
// holds a device on, which is what separates idle-pod from unused-node; shared
// devices are excluded from per-pod analysis entirely; and a device is
// allocated to at most one pod. Residual is what is left after the named
// buckets are removed, so it can only go negative if that upstream guarantee
// breaks — which is exactly when this chart must refuse to draw itself.
type Ledger struct {
	// Paid is every accelerator-hour in the window, taken from the scan rather
	// than recomputed, so the report cannot disagree with the terminal output.
	Paid float64

	Rows []LedgerRow

	// Fallow is the bucket the whole report is about: capacity with an owner
	// and, usually, a command.
	Fallow float64

	// Residual is Paid minus every other bucket. It is deliberately not called
	// "used": ullage measures fallow hours, not productive ones, and an hour in
	// here may be real work or may be idleness that did not clear the reporting
	// threshold. Naming it "utilized" would claim a measurement that was never
	// taken.
	Residual float64

	Reconciles  bool
	Discrepancy float64
}

// LedgerRow is one deduction, in the order the report subtracts them.
type LedgerRow struct {
	// Key is stable and machine-readable; consumers of the HTML should not have
	// to match on prose.
	Key   string
	Label string

	// Note says why these hours were set aside. Every deduction carries one:
	// an unexplained subtraction is the thing that makes a reader stop
	// believing the total.
	Note string

	Hours        float64
	Accelerators int

	// Deduction distinguishes hours removed from the total from the fallow
	// hours that remain at the end of it.
	Deduction bool

	// Geometry for the conservation bar, resolved here rather than in the
	// template so the document renders identically without scripting and the
	// numbers behind the picture can be asserted in a test.
	Width    float64 // span in viewBox units
	Next     float64 // x offset of the following segment
	SharePct string  // the same proportion, for the table
}

// BuildLedger reduces a result to the capacity conservation.
func BuildLedger(res *api.Result) Ledger {
	windowHours := res.Scan.Window.Duration().Hours()

	byDesignHours, byDesignAcc := sumFindings(res.ByDesign)
	suppressedHours, suppressedAcc := sumFindings(res.Suppressed)

	var notAnalysedAcc int
	for _, e := range res.NotAnalyzed {
		notAnalysedAcc += e.Accelerators
	}
	// An excluded accelerator was never measured, so no fallow duration exists
	// for it. It is charged for the time it existed, which is what the cluster
	// actually paid, and is the honest way to keep it visible rather than
	// letting unmeasurable capacity quietly leave the denominator.
	//
	// The scan reports this on the same per-node basis as GPUHoursPaid. The
	// fallback covers results built without it -- older JSON, and tests that
	// assemble a Result by hand -- where charging the whole window is the same
	// approximation the paid figure itself used to make.
	notAnalysedHours := res.Scan.GPUHoursNotAnalysed
	if notAnalysedHours == 0 && notAnalysedAcc > 0 {
		notAnalysedHours = float64(notAnalysedAcc) * windowHours
	}

	l := Ledger{
		Paid:   res.Scan.GPUHoursPaid,
		Fallow: res.Scan.GPUHoursFallow,
	}

	fallowAcc := 0
	for _, f := range res.Recommendations {
		fallowAcc += acceleratorCount(f)
	}

	l.Residual = l.Paid - notAnalysedHours - byDesignHours - suppressedHours - l.Fallow

	l.Rows = []LedgerRow{
		{
			Key: "residual",
			// Leading with "Worked" would assert the one thing this row
			// cannot support. The label states what is known — that nothing
			// flagged it — and the description carries the rest.
			Label:        "Unflagged",
			Note:         "Worked, or idle below the threshold. Not measured as productive.",
			Hours:        l.Residual,
			Accelerators: res.Scan.AcceleratorsAnalyzed - byDesignAcc - suppressedAcc - fallowAcc,
			Deduction:    true,
		},
		{
			Key:          "not-analysed",
			Label:        "No usable metric",
			Note:         "Shared, partitioned or still initialising. Counted, never judged.",
			Hours:        notAnalysedHours,
			Accelerators: notAnalysedAcc,
			Deduction:    true,
		},
		{
			Key:          "by-design",
			Label:        "Reserved by policy",
			Note:         "Held empty on purpose. Not waste.",
			Hours:        byDesignHours,
			Accelerators: byDesignAcc,
			Deduction:    true,
		},
		{
			Key:          "suppressed",
			Label:        "Suppressed",
			Note:         "Silenced in .ullage.yaml, and still counted here.",
			Hours:        suppressedHours,
			Accelerators: suppressedAcc,
			Deduction:    true,
		},
		{
			Key:          "fallow",
			Label:        "Fallow, with an owner",
			Note:         "Paid for, did no work, and someone can act on it.",
			Hours:        l.Fallow,
			Accelerators: fallowAcc,
		},
	}

	var sum float64
	for _, r := range l.Rows {
		sum += r.Hours
	}
	l.Discrepancy = sum - l.Paid

	var x float64
	shares := make([]float64, len(l.Rows))
	for i := range l.Rows {
		share := l.Share(l.Rows[i])
		shares[i] = share
		l.Rows[i].Width = share * ledgerBarWidth
		x += l.Rows[i].Width
		l.Rows[i].Next = x
	}
	for i, pct := range apportion(shares) {
		l.Rows[i].SharePct = pct
	}

	// A tolerance rather than an equality test: these are floating-point hour
	// counts summed in a different order from the one that produced the total.
	// One accelerator-second is far below anything the report displays and far
	// above the rounding error being allowed for.
	const tolerance = 1.0 / 3600.0
	l.Reconciles = l.Residual >= 0 && absFloat(l.Discrepancy) <= tolerance

	return l
}

// Share returns a row's share of paid capacity, 0-1, for bar geometry.
func (l Ledger) Share(r LedgerRow) float64 {
	if l.Paid <= 0 {
		return 0
	}
	s := r.Hours / l.Paid
	if s < 0 {
		return 0
	}
	if s > 1 {
		return 1
	}
	return s
}

// Deductions returns the rows subtracted from paid capacity.
func (l Ledger) Deductions() []LedgerRow {
	out := make([]LedgerRow, 0, len(l.Rows))
	for _, r := range l.Rows {
		if r.Deduction {
			out = append(out, r)
		}
	}
	return out
}

// ledgerBarWidth is the conservation bar's width in SVG viewBox units.
const ledgerBarWidth = 720.0

// apportion renders shares as whole percentages that add up to exactly 100.
//
// Rounding each share on its own is the obvious thing and it is wrong here.
// Five buckets that individually round up land on 101%, directly beneath a
// total row that says 100% — on the one table in the document whose entire
// purpose is to show that the headline figure equals the sum of its parts.
// A reader who spots that has been handed a reason to disbelieve everything
// else, and they would be right to look.
//
// So the whole percentages are allocated by largest remainder: floor
// everything, then hand the leftover points to the buckets that lost the most
// in the flooring. The result is off by at most one point on any single row
// and exact in the total, which is the right way round for a document that
// asks to be checked.
func apportion(shares []float64) []string {
	out := make([]string, len(shares))
	floors := make([]int, len(shares))
	type rem struct {
		i    int
		frac float64
	}
	var rems []rem
	allocated := 0

	for i, share := range shares {
		pct := share * 100
		// A bucket that exists but rounds to nothing is shown as "<1%" rather
		// than "0%", which beside a non-zero hour count reads as a
		// contradiction. It takes no share of the rounding, having already
		// been given more than its arithmetic due.
		if pct > 0 && pct < 0.5 {
			out[i] = "<1%"
			continue
		}
		floors[i] = int(pct)
		allocated += floors[i]
		rems = append(rems, rem{i: i, frac: pct - float64(floors[i])})
	}

	// Ties broken by position so the same input always renders identically:
	// the document is byte-compared in tests and read side by side by people.
	sort.SliceStable(rems, func(a, b int) bool { return rems[a].frac > rems[b].frac })
	for n := 0; n < 100-allocated && n < len(rems); n++ {
		floors[rems[n].i]++
	}

	for i := range out {
		if out[i] == "" {
			out[i] = strconv.Itoa(floors[i]) + "%"
		}
	}
	return out
}

func sumFindings(fs []api.Finding) (hours float64, accelerators int) {
	for _, f := range fs {
		hours += f.Impact.GPUHoursFallow
		accelerators += acceleratorCount(f)
	}
	return hours, accelerators
}

func acceleratorCount(f api.Finding) int {
	var n int
	for _, a := range f.Accelerators {
		n += a.Count
	}
	return n
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
