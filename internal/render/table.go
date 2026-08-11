package render

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// Table renders the default scan output.
//
// The design constraint is that the first screen must be readable in about ten
// seconds and must survive being pasted into Slack, so it is 80 columns wide,
// has no box drawing, and puts the three things that decide whether a row
// matters — how much, for how long, and who — before anything else.
func Table(w io.Writer, res *api.Result, o Options) error {
	p := newPrinter(w, o)

	p.line("")
	p.line("ullage %s  %s  window %s",
		o.Version, res.Scan.Context, Human(res.Scan.Window.Duration()))
	p.line("")

	// The headline. Percentage first because a bare hour count means nothing
	// without the denominator.
	pct := res.FallowPercent()
	p.bold("  %s of %s accelerator-hours fallow (%.0f%%)",
		hours(res.Scan.GPUHoursFallow), hours(res.Scan.GPUHoursPaid), pct)

	// Analysed vs observed deliberately disagree when they should. Silence here
	// would let a user believe a partial scan was a whole one.
	if res.Scan.AcceleratorsAnalyzed < res.Scan.AcceleratorsObserved {
		p.dim("  %d of %d accelerators analysed  (%d excluded, see below)",
			res.Scan.AcceleratorsAnalyzed, res.Scan.AcceleratorsObserved,
			res.Scan.AcceleratorsObserved-res.Scan.AcceleratorsAnalyzed)
	} else {
		p.dim("  %d accelerators analysed", res.Scan.AcceleratorsAnalyzed)
	}
	p.line("")

	if len(res.Recommendations) == 0 {
		p.line("  No recommendations. Nothing in this cluster held an accelerator")
		p.line("  without using it for the whole window.")
		p.line("")
		p.renderContext(res)
		p.footerNoFindings(res)
		return p.err
	}

	p.renderRows(res)
	p.line("")
	p.renderByDesign(res)
	p.renderContext(res)
	p.renderFooter(res)
	return p.err
}

func (p *printer) renderRows(res *api.Result) {
	rows := res.Recommendations
	limit := len(rows)
	if p.o.Top > 0 && p.o.Top < limit {
		limit = p.o.Top
	}

	// Column widths adapt to content so a cluster with short names does not
	// waste half the terminal on padding.
	wWorkload, wOwner := 8, 5
	for _, f := range rows[:limit] {
		if n := len(f.Workload.Ref()); n > wWorkload {
			wWorkload = n
		}
		if n := len(p.ownerCell(f)); n > wOwner {
			wOwner = n
		}
	}
	if wWorkload > 34 {
		wWorkload = 34
	}
	if wOwner > 16 {
		wOwner = 16
	}

	head := fmt.Sprintf("  %-3s %-*s %5s %8s %6s %-*s",
		"", wWorkload, "WORKLOAD", "GPUS", "ULLAGE", "FOR", wOwner, "OWNER")
	p.dim("%s", head)

	for i, f := range rows[:limit] {
		marker := fmt.Sprintf("%d.", i+1)
		p.line("  %-3s %-*s %5d %8s %6s %-*s",
			marker,
			wWorkload, truncate(f.Workload.Ref(), wWorkload),
			f.TotalAccelerators(),
			hours(f.Impact.GPUHoursFallow),
			HumanShort(f.Evidence.FallowDuration.Duration()),
			wOwner, truncate(p.ownerCell(f), wOwner))

		// The second line is the one that makes the row actionable. Without the
		// reason, a row is an accusation; with it, it is a diagnosis.
		p.dim("      %s", p.reasonFor(f))

		// The money and the hardware belong on one short line. The provenance
		// of the rate matters, but repeating it on every row buries the number
		// it is supposed to qualify, so it is stated once in the footer.
		if f.Impact.WindowCost != nil {
			p.dim("      ~%s%s  ·  %s", currencySymbol(f.Impact.Currency),
				money(*f.Impact.WindowCost), AcceleratorSummary(f))
		} else if len(f.Accelerators) > 0 {
			p.dim("      %s", AcceleratorSummary(f))
		}
		if f.EvidenceConfidence != api.EvidenceHigh {
			p.dim("      confidence: %s — %s", f.EvidenceConfidence, firstNote(f))
		}
		if i < limit-1 {
			p.line("")
		}
	}

	if hidden := len(rows) - limit; hidden > 0 {
		p.line("")
		p.dim("  %d more recommendations. Use --top %d to see them all.", hidden, len(rows))
	}
}

// reasonFor states the finding in one line, in the terms the reader thinks in.
func (p *printer) reasonFor(f api.Finding) string {
	switch f.Check {
	case api.CheckIdlePod:
		s := fmt.Sprintf("no GPU work since %s", lastSeen(f))
		if f.Workload.Grouped > 1 {
			s = fmt.Sprintf("%d pods, %s", f.Workload.Grouped, s)
		}
		if f.Fix.Targets == api.FixTargetController {
			s += fmt.Sprintf(" · owned by %s", f.Provenance.RootKind)
		} else if f.Fix.Targets == api.FixTargetNone {
			s += fmt.Sprintf(" · owned by %s (no safe automatic fix)", f.Provenance.RootKind)
		}
		return s
	case api.CheckStuckPod:
		return strings.TrimSuffix(strings.SplitN(f.Summary, " while ", 2)[len(strings.SplitN(f.Summary, " while ", 2))-1], ".")
	case api.CheckUnusedNode:
		n := f.Workload.Grouped
		s := fmt.Sprintf("%s, nothing scheduled", plural(n, "node"))
		if len(f.Fix.Blockers) > 0 {
			s += fmt.Sprintf(" · %s block scale-down", plural(len(f.Fix.Blockers), "pod"))
		}
		return s
	}
	return f.Summary
}

func lastSeen(f api.Finding) string {
	if f.Evidence.LastNonZeroUtilization == nil {
		return "the window began"
	}
	return f.Evidence.LastNonZeroUtilization.Format("2 Jan 15:04")
}

func (p *printer) ownerCell(f api.Finding) string {
	if f.Owner.Identity == "" {
		return "unowned"
	}
	// Addresses are shortened from the domain, not the local part. Truncating
	// alice@example.com to "…ple.com" identifies nobody, while "alice@…"
	// identifies exactly one person to anyone who works there. The full value
	// is always in `explain` and in the JSON.
	id := f.Owner.Identity
	if at := strings.IndexByte(id, '@'); at > 0 && len(id) > 14 {
		return id[:at] + "@…"
	}
	return id
}

// renderByDesign is the section that separates ullage from a cost dashboard.
//
// Capacity held on purpose is not waste. Printing it in the same list as waste,
// with a removal command attached, is the fastest way for a tool to be
// dismissed as not understanding the business.
func (p *printer) renderByDesign(res *api.Result) {
	if len(res.ByDesign) == 0 {
		return
	}
	total := 0.0
	devices := 0
	for _, f := range res.ByDesign {
		total += f.Impact.GPUHoursFallow
		devices += f.TotalAccelerators()
	}
	p.line("  Fallow by design")
	p.dim("  %d accelerators, %s — held empty on purpose, not counted as waste",
		devices, hours(total))
	for _, f := range res.ByDesign {
		p.line("    · %s", f.Summary)
		p.dim("      %s", wrapIndent(f.Because, 6, p.width()))
	}
	p.line("")
}

func (p *printer) renderContext(res *api.Result) {
	if len(res.NotAnalyzed) > 0 {
		p.line("  Not analysed")
		for _, e := range res.NotAnalyzed {
			p.dim("    · %s — %s", plural(e.Accelerators, "accelerator"), e.Reason)
			if e.Detail != "" {
				p.dim("      %s", wrapIndent(e.Detail, 6, p.width()))
			}
		}
		p.line("")
	}

	// Unmet demand is context, never a finding: a Pending pod holds nothing.
	// But printing it beside the idle capacity is the single most persuasive
	// thing in the output, because it shows the two halves of the same problem.
	if res.UnmetDemand != nil {
		p.line("  Unmet demand")
		p.dim("    %d pods are waiting for %d accelerators — %s",
			res.UnmetDemand.Pods, res.UnmetDemand.Accelerators, res.UnmetDemand.Detail)
		p.dim("    This is context, not a finding: pending pods hold no devices.")
		p.line("")
	}

	for _, warn := range res.Warnings {
		p.dim("  ! %s", wrapIndent(warn, 4, p.width()))
	}
	if len(res.Warnings) > 0 {
		p.line("")
	}
}

func (p *printer) renderFooter(res *api.Result) {
	// Stated once, where it qualifies every number above it. A reader deciding
	// whether to act on a cost figure needs to know where the rate came from,
	// and a reader who already knows should not have to read it seven times.
	if res.Pricing != nil && !p.o.NoCost && res.Pricing.Source != "" {
		p.dim("  Costs use %s.", res.Pricing.Source)
		p.line("")
	}
	if res.BelowThreshold > 0 {
		p.dim("  %d findings below --min-confidence %s were not shown.",
			res.BelowThreshold, p.o.MinConfidence)
		p.line("")
	}

	// One suggested next command, personalised to the top finding the reader can
	// actually act on. A menu of six commands is a menu; one command is a next
	// step.
	top := actionable(res.Recommendations)
	if top == nil {
		p.dim("  Next: ullage explain %s", res.Recommendations[0].Workload.Ref())
		return
	}
	p.line("  Next: ullage explain %s", top.Workload.Ref())
	p.dim("        shows the evidence, the owner, and the exact command to fix it")
}

func (p *printer) footerNoFindings(res *api.Result) {
	if res.BelowThreshold > 0 {
		p.dim("  %d findings were below --min-confidence %s.", res.BelowThreshold, p.o.MinConfidence)
		p.dim("  Run with --min-confidence low to see them.")
		return
	}
	p.dim("  Run `ullage doctor` to confirm the scan saw what you expected.")
}

func actionable(f []api.Finding) *api.Finding {
	for i := range f {
		if f[i].Fix.Command != "" && f[i].Fix.Targets != api.FixTargetNone {
			return &f[i]
		}
	}
	if len(f) > 0 {
		return &f[0]
	}
	return nil
}

func firstNote(f api.Finding) string {
	if len(f.Evidence.Notes) > 0 {
		return f.Evidence.Notes[0]
	}
	return "evidence is weaker than usual"
}

// money formats a cost with thousands separators and no false precision. A
// figure like $2,016.37 implies the rate behind it is exact, and it never is.
func money(v float64) string {
	s := fmt.Sprintf("%.0f", v)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// plural renders a count with its noun, agreeing in number.
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func hours(h float64) string {
	switch {
	case h >= 10000:
		return fmt.Sprintf("%.0fk", h/1000)
	case h >= 1000:
		return fmt.Sprintf("%.1fk", h/1000)
	default:
		return fmt.Sprintf("%.0f", h)
	}
}

func currencySymbol(c string) string {
	switch strings.ToUpper(c) {
	case "USD":
		return "$"
	case "EUR":
		return "€"
	case "GBP":
		return "£"
	case "":
		return "$"
	default:
		return c + " "
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	// Elide from the left: the tail of a workload reference is more
	// identifying than its head.
	return "…" + s[len(s)-n+1:]
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
