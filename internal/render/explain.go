package render

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// Explain renders the full evidence screen for one finding.
//
// Section order is fixed and never adapts to the finding, because a reader who
// has seen it once should know where to look the second time. The order is also
// an argument: what was measured, then what it means, then who owns it, then
// what to do, then what could go wrong. Putting the command last is deliberate
// — a tool that leads with an action invites the reader to run it without
// reading the evidence.
func Explain(w io.Writer, f *api.Finding, res *api.Result, o Options) error {
	p := newPrinter(w, o)

	p.line("")
	p.bold("  %s", f.Workload.Ref())
	p.dim("  %s · %s", f.Check, f.Summary)
	p.line("")

	p.section("Evidence")
	p.field("Window", "%s ending %s",
		Human(f.Evidence.Window.Duration()), res.Scan.Started.Format("2 Jan 2006 15:04 MST"))
	p.field("Fallow for", "%s", Human(f.Evidence.FallowDuration.Duration()))
	if f.Evidence.LastNonZeroUtilization != nil {
		p.field("Last GPU work", "%s", f.Evidence.LastNonZeroUtilization.Format("2 Jan 2006 15:04 MST"))
	} else {
		p.field("Last GPU work", "none within the window")
	}
	// The peak is over the whole window; the zero claim is over the trailing
	// fallow run. Printing "peak 100%" three lines above "read exactly zero"
	// with no qualifier reads as a contradiction, and a reader who catches a
	// tool contradicting itself stops believing the rest of the screen — which
	// is the entire cost of getting this wording wrong.
	if f.Evidence.UtilizationMax > 0 && f.Evidence.LastNonZeroUtilization != nil {
		p.field("Peak utilization", "%.0f%% — reached before the fallow period began, not during it",
			f.Evidence.UtilizationMax)
	} else {
		p.field("Peak utilization", "%.0f%% across the whole window", f.Evidence.UtilizationMax)
	}
	if f.Evidence.PowerDrawWatts > 0 {
		if f.Evidence.PowerDrawTDPRatio > 0 && len(f.Accelerators) > 0 {
			p.field("Power draw", "%.0f W mean (%.0f%% of %.0f W TDP)",
				f.Evidence.PowerDrawWatts, f.Evidence.PowerDrawTDPRatio*100, f.Accelerators[0].TDPWatts)
		} else if f.Evidence.PowerDrawTDPRatio > 0 {
			// No accelerator entry to read a TDP from (should not happen for a
			// real finding, but indexing Accelerators[0] unconditionally used
			// to panic the whole explain screen if it ever did).
			p.field("Power draw", "%.0f W mean (%.0f%% of TDP)",
				f.Evidence.PowerDrawWatts, f.Evidence.PowerDrawTDPRatio*100)
		} else {
			p.field("Power draw", "%.0f W mean", f.Evidence.PowerDrawWatts)
		}
	}
	p.field("Sample coverage", "%.0f%% of expected samples present", f.Evidence.SampleCompleteness*100)

	if len(f.Evidence.Sparkline) > 0 {
		p.line("")
		p.field("Utilization", "%s", sparkline(f.Evidence.Sparkline))
		p.dim("                 %s", sparkAxis(f.Evidence.Sparkline, f.Evidence.Window.Duration()))
	}
	for _, n := range f.Evidence.Notes {
		p.dim("    note: %s", wrapIndent(n, 10, p.width()))
	}
	p.line("")

	p.section("What this means")
	p.para(meaning(f))
	p.line("")

	p.section("Accelerators")
	for _, a := range f.Accelerators {
		p.field("Held", "%d × %s (%s)", a.Count, a.Model, a.Allocation)
	}
	p.field("Fallow", "%s accelerator-hours", hours(f.Impact.GPUHoursFallow))
	if f.Impact.WindowCost != nil && len(f.Accelerators) > 0 {
		p.field("Cost", "~%s%s over the window",
			currencySymbol(f.Impact.Currency), money(*f.Impact.WindowCost))
		p.dim("                 %s rate for %s; ullage never blends rates across models",
			f.Impact.PricingSource, f.Accelerators[0].Model)
	} else if f.Impact.WindowCost != nil {
		// A cost with no accelerator to name the model of: still show the
		// figure rather than crash, since indexing Accelerators[0]
		// unconditionally used to panic the whole explain screen.
		p.field("Cost", "~%s%s over the window",
			currencySymbol(f.Impact.Currency), money(*f.Impact.WindowCost))
	} else if len(f.Accelerators) > 1 {
		p.dim("    No cost shown: this finding spans more than one accelerator model,")
		p.dim("    and a blended rate would be a fabricated number.")
	}
	p.line("")

	// Provenance is a first-class section rather than a footnote, because it is
	// what determines whether the command below is real or a no-op.
	p.section("Managed by")
	if !f.Provenance.Controlled {
		p.para("Nothing. These pods have no controller, so deleting them is final and sufficient.")
	} else {
		p.field("Root owner", "%s/%s", f.Provenance.RootKind, f.Provenance.RootName)
		if len(f.Provenance.Chain) > 1 {
			p.field("Chain", "pod → %s", chainString(f.Provenance.Chain))
		}
		if !f.Provenance.Recognized {
			p.dim("    %s is not a workload kind ullage understands.", f.Provenance.RootKind)
		}
	}
	p.line("")

	p.section("Owner")
	if f.Owner.Identity == "" {
		p.field("Owner", "unowned")
		p.dim("    No owner label or annotation was found on the pod, its controller,")
		p.dim("    or its namespace. An accelerator nobody claims is itself worth knowing about.")
	} else {
		p.field("Owner", "%s", f.Owner.Identity)
		p.field("Resolved via", "%s", f.Owner.ResolvedVia)
		if f.Owner.Detail != "" {
			p.dim("               %s", f.Owner.Detail)
		}
	}
	p.line("")

	p.section("What to do")
	p.para(f.Fix.Rationale)
	if f.Fix.Command != "" {
		p.line("")
		for _, l := range strings.Split(f.Fix.Command, "\n") {
			p.bold("    %s", l)
		}
	}
	if len(f.Fix.Blockers) > 0 {
		p.line("")
		p.dim("    Blocked by:")
		for _, b := range f.Fix.Blockers {
			p.dim("      · %s — %s", b.Object, b.Reason)
		}
	}
	if f.Fix.ConfirmWith != "" {
		p.line("")
		p.dim("    Confirm with %s before running this.", f.Fix.ConfirmWith)
	} else if f.Fix.Targets != api.FixTargetNone && f.Owner.Identity == "" {
		p.line("")
		p.dim("    No owner is recorded, so there is nobody to confirm with. Check with")
		p.dim("    whoever runs this namespace before acting.")
	}
	p.line("")

	if f.Risk != "" {
		p.section("Before you do")
		p.para(f.Risk)
		p.line("")
	}

	p.section("Stop it happening again")
	p.para(prevention(f))
	p.line("")

	// The finding id, not the workload reference. Suppressions match on the
	// id, so printing the reference here produced an entry that could never
	// match — following the tool's own printed advice silently did nothing.
	// The example expiry is relative to today. A hardcoded date is guaranteed to
	// become a date in the past, and copying it produces a suppression that
	// expired before it was written.
	p.dim("  Suppress: ullage ignore %s --reason \"...\" --until %s",
		f.ID, time.Now().AddDate(0, 3, 0).Format("2006-01-02"))
	p.dim("  Docs:     %s", f.Docs)
	p.line("")
	return p.err
}

func meaning(f *api.Finding) string {
	switch f.Check {
	case api.CheckIdlePod:
		// "the whole period above" would name the window, and the window is not
		// what was zero — the trailing fallow run is. The Evidence block
		// directly above prints both, and one of them is often a non-zero peak.
		s := fmt.Sprintf(
			"Every utilization sample for these accelerators read exactly zero for the last %s. "+
				"ullage does not claim the workload is unimportant, and it does not estimate how "+
				"efficiently it ran — GPU utilization is a poor measure of that. It claims only "+
				"what the metric can prove: no CUDA kernel was resident on these devices at any "+
				"sampled moment in that time.",
			Human(f.Evidence.FallowDuration.Duration()))
		if f.Evidence.PowerDrawTDPRatio > 0 && f.Evidence.PowerDrawTDPRatio < 0.20 {
			s += " Power draw independently agrees: the devices are drawing near-idle wattage."
		}
		return s
	case api.CheckStuckPod:
		return "These pods are scheduled and holding allocated accelerators, but their containers " +
			"are not running work. The devices are unavailable to anything else for as long as this " +
			"continues. This is not a utilization judgement — a wedged pod holds its device whatever " +
			"the metrics say."
	case api.CheckUnusedNode:
		if f.ByDesign {
			return f.Because
		}
		return fmt.Sprintf("These nodes advertise accelerators and are Ready and schedulable, but no "+
			"pod holding an accelerator — by extended resource, MIG profile, time-sliced replica or "+
			"DRA claim — has been placed on them, and no accelerator on them has reported work in "+
			"the last %s. They are not being drained, they are not cordoned, and they are past the "+
			"initialisation grace period.", Human(f.Evidence.FallowDuration.Duration()))
	}
	return f.Summary
}

func prevention(f *api.Finding) string {
	switch f.Check {
	case api.CheckIdlePod:
		return "Interactive GPU sessions are the most common source of this finding. A TTL controller, " +
			"an activity-based idle culler, or a scheduled scale-to-zero for notebook workloads removes " +
			"the class of problem rather than this instance of it."
	case api.CheckStuckPod:
		return "Set a backoffLimit on Jobs and an appropriate restartPolicy so a failing workload " +
			"eventually stops holding its device instead of crash-looping indefinitely."
	case api.CheckUnusedNode:
		return "Confirm the autoscaler's minimum size for this pool matches the floor you actually " +
			"want, and that scale-down is not being blocked by pods with no eviction toleration."
	}
	return ""
}

func (p *printer) section(title string) {
	p.bold("  %s", title)
}

func (p *printer) field(label, format string, args ...any) {
	p.line("    %-16s %s", label, fmt.Sprintf(format, args...))
}

func (p *printer) para(s string) {
	p.line("    %s", wrapIndent(s, 4, p.width()))
}

// sparkline renders normalised buckets as block characters.
//
// Each bucket is the peak of its period, not the mean: the chart exists to
// answer "did anything at all happen here", and a mean would hide a burst.
func sparkline(b []float64) string {
	blocks := []rune("▁▂▃▄▅▆▇█")
	max := 0.0
	for _, v := range b {
		if v > max {
			max = v
		}
	}
	var sb strings.Builder
	for _, v := range b {
		if max <= 0 {
			sb.WriteRune('▁')
			continue
		}
		i := int(v / max * float64(len(blocks)-1))
		if i < 0 {
			i = 0
		}
		if i >= len(blocks) {
			i = len(blocks) - 1
		}
		sb.WriteRune(blocks[i])
	}
	if max == 0 {
		return sb.String() + "  all zero"
	}
	return sb.String() + fmt.Sprintf("  peak %.0f%%", max)
}

func sparkAxis(b []float64, window interface{ Hours() float64 }) string {
	n := len(b)
	if n < 4 {
		return ""
	}
	left := fmt.Sprintf("%.0fd ago", window.Hours()/24)
	right := "now"
	pad := n - len(left) - len(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}
