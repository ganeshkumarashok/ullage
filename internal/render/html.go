package render

import (
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

//go:embed assets/report.css
var reportCSS string

//go:embed assets/report.js
var reportJS string

//go:embed assets/report.html.tmpl
var reportTmpl string

// HTMLOptions controls the report document.
type HTMLOptions struct {
	Options

	// Redact removes cluster context, Prometheus URL and owner identities. A
	// report is written to be forwarded, and the person forwarding it should
	// not have to choose between explaining the finding and leaking an
	// internal hostname.
	Redact bool

	// Now is the generation timestamp. Injectable so the golden test is not a
	// clock test.
	Now time.Time
}

// HTML writes a self-contained report: one file, no external stylesheet, no
// script from a CDN, no web font, and no request of any kind once it is
// written. It has to open from a file:// URL on the laptop of somebody who has
// no access to the cluster it describes, because that is who decides whether
// the capacity gets released.
func HTML(w io.Writer, res *api.Result, o HTMLOptions) error {
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}

	t, err := template.New("report").Funcs(htmlFuncs()).Parse(reportTmpl)
	if err != nil {
		return fmt.Errorf("parse report template: %w", err)
	}

	doc := buildReport(res, o)
	if o.Redact {
		// One sweep over the finished document, rather than a redact call at
		// each site that looked sensitive to whoever wrote it. See redact.go.
		newRedactor(res).scrub(reflect.ValueOf(&doc).Elem())
	}
	return t.Execute(w, doc)
}

// report is the whole document, resolved before rendering so the template
// contains no arithmetic and the numbers can be tested without parsing HTML.
type report struct {
	// CSS and JS are the only values marked trusted, and both are compiled in.
	// Everything originating from a cluster goes through ordinary escaping.
	CSS template.CSS
	JS  template.JS

	Tool        string
	Version     string
	Context     string
	Generated   string
	WindowLabel string
	Started     string
	Prometheus  string
	Redacted    bool

	Ledger Ledger

	Headline    string
	HeadlineSub string
	Cost        string
	HasCost     bool
	FallowPct   string

	Observed  int
	Analyzed  int
	Excluded  int
	Coverage  string
	HasCovers bool

	Findings   []findingView
	ByDesign   []findingView
	Suppressed []findingView

	Owners        []ownerBar
	OwnersOmitted int
	OwnerMeasure  string

	Exclusions  []api.Exclusion
	Warnings    []string
	UnmetDemand *api.UnmetDemand

	PricingSource string
	Params        api.Params
	MinConfidence string

	// Thresholds are pre-formatted because api.ISODuration has no String
	// method, so a template that prints one directly renders the underlying
	// nanosecond count -- "idle >= 86400000000000", which is how the footer
	// read until this section put the numbers somewhere people look.
	IdleLabel  string
	StuckLabel string
	StepLabel  string

	Signals []signalView
}

// signalView is one metric the scan consulted, described by the question it
// answers rather than by its DCGM field name.
//
// The report is read by people deciding whether to delete something expensive,
// and "DCGM_FI_DEV_FB_USED" tells them nothing about whether they should. What
// makes the number trustworthy is seeing that a warm model in framebuffer was
// something the scan actively looked for -- and, when a series was missing,
// that it knows it looked with one eye closed.
type signalView struct {
	Metric    string
	Answers   string
	Consulted bool
}

// signalCatalogue is every signal an idle claim can rest on, in the order a
// reader meets them: compute first, then the engines an SM-only gauge misses,
// then the independent corroboration.
var signalCatalogue = []signalView{
	{Metric: "DCGM_FI_DEV_GPU_UTIL", Answers: "compute on the SMs"},
	{Metric: "DCGM_FI_DEV_ENC_UTIL", Answers: "video encoding"},
	{Metric: "DCGM_FI_DEV_DEC_UTIL", Answers: "video decoding"},
	{Metric: "DCGM_FI_DEV_MEM_COPY_UTIL", Answers: "host↔device copies: data loading, checkpointing"},
	{Metric: "DCGM_FI_DEV_FB_USED", Answers: "a model held resident in framebuffer"},
	{Metric: "DCGM_FI_DEV_POWER_USAGE", Answers: "board power, corroborating idle from a second sensor"},
}

// signalsFor marks which of the catalogue this scan actually had.
func signalsFor(engines []string) []signalView {
	have := make(map[string]bool, len(engines))
	for _, e := range engines {
		have[e] = true
	}
	out := make([]signalView, 0, len(signalCatalogue))
	for _, s := range signalCatalogue {
		// The SM gauge is the one series a scan cannot run without, and power
		// is reported through its own warning rather than the engine list.
		s.Consulted = have[s.Metric] ||
			s.Metric == "DCGM_FI_DEV_GPU_UTIL" ||
			s.Metric == "DCGM_FI_DEV_POWER_USAGE"
		out = append(out, s)
	}
	return out
}

type findingView struct {
	Rank         int
	ID           string
	Anchor       string
	Check        string
	CheckLabel   string
	Subject      string
	Kind         string
	SubjectSub   string
	Summary      string
	Because      string
	Hours        string
	Cost         string
	HasCost      bool
	Accelerators string
	Owner        string
	OwnerVia     string
	Confidence   string
	ConfClass    string
	Ownership    string
	Window       string
	Fallow       string
	LastSeen     string
	UtilMax      string
	Power        string
	Completeness string
	Notes        []string
	Chain        []string
	ShowChain    bool
	Controller   string
	Command      string
	CommandID    string
	Rationale    string
	Frees        string
	Targets      string
	Blockers     []api.Blocker
	Risk         string
	Docs         string
	Share        float64
}

type ownerBar struct {
	Name    string
	Hours   string
	Cost    string
	HasCost bool
	Count   int
	Share   float64
}

func buildReport(res *api.Result, o HTMLOptions) report {
	l := BuildLedger(res)

	r := report{
		CSS:           template.CSS(reportCSS),
		JS:            template.JS(reportJS),
		Tool:          "ullage",
		Version:       firstNonEmpty(res.Scan.Tool.Version, o.Version),
		Context:       res.Scan.Context,
		Generated:     o.Now.UTC().Format("2006-01-02 15:04 MST"),
		WindowLabel:   Human(res.Scan.Window.Duration()),
		Started:       res.Scan.Started.UTC().Format("2006-01-02 15:04 MST"),
		Prometheus:    res.Scan.PrometheusURL,
		Redacted:      o.Redact,
		Ledger:        l,
		Observed:      res.Scan.AcceleratorsObserved,
		Analyzed:      res.Scan.AcceleratorsAnalyzed,
		Excluded:      res.Scan.AcceleratorsObserved - res.Scan.AcceleratorsAnalyzed,
		Exclusions:    res.NotAnalyzed,
		Warnings:      res.Warnings,
		PricingSource: pricingSource(res),
		Params:        res.Scan.Params,
		MinConfidence: res.Scan.Params.MinConfidence,
		Signals:       signalsFor(res.Scan.EnginesChecked),
		IdleLabel:     ThresholdLabel(res.Scan.Params.IdleThreshold.Duration()),
		StuckLabel:    ThresholdLabel(res.Scan.Params.StuckThreshold.Duration()),
		StepLabel:     ThresholdLabel(res.Scan.Params.Step.Duration()),
	}

	if o.Redact {
		r.Context = "redacted"
		r.Prometheus = ""
	}

	if res.UnmetDemand != nil && res.UnmetDemand.Accelerators > 0 {
		r.UnmetDemand = res.UnmetDemand
	}

	r.Headline = hours(res.Scan.GPUHoursFallow)
	r.FallowPct = fmt.Sprintf("%.0f", res.FallowPercent())
	r.HeadlineSub = fmt.Sprintf("of %s accelerator-hours paid for in the last %s",
		hours(res.Scan.GPUHoursPaid), Human(res.Scan.Window.Duration()))

	if c, ok := totalCost(res.Recommendations); ok && !o.NoCost {
		r.Cost = costLabel(c, currencyOf(res))
		r.HasCost = true
	}

	// Sample completeness is the honest denominator for everything above it:
	// a report drawn from three-quarters of the samples is not the same
	// document as one drawn from all of them, and the reader has to be told.
	if worst, ok := worstCompleteness(res.Recommendations); ok && worst < 0.995 {
		r.Coverage = fmt.Sprintf("%.0f%%", worst*100)
		r.HasCovers = true
	}

	for i, f := range res.Recommendations {
		r.Findings = append(r.Findings, newFindingView(f, res, o, l, anchorFor(f, o, "f", i)))
	}
	for i, f := range res.ByDesign {
		r.ByDesign = append(r.ByDesign, newFindingView(f, res, o, l, anchorFor(f, o, "d", i)))
	}
	for i, f := range res.Suppressed {
		r.Suppressed = append(r.Suppressed, newFindingView(f, res, o, l, anchorFor(f, o, "s", i)))
	}

	r.Owners, r.OwnersOmitted, r.OwnerMeasure = ownerBars(res, o)

	return r
}

// ownerTop bounds the owner chart. Past this the bars stop being a ranking and
// start being a wall, and the findings table is the better instrument.
const ownerTop = 12

func ownerBars(res *api.Result, o HTMLOptions) ([]ownerBar, int, string) {
	type agg struct {
		hours float64
		cost  float64
		count int
		cost0 bool
	}
	byOwner := map[string]*agg{}
	for _, f := range res.Recommendations {
		key := ownerKey(f)
		a := byOwner[key]
		if a == nil {
			a = &agg{}
			byOwner[key] = a
		}
		a.hours += f.Impact.GPUHoursFallow
		a.count++
		if f.Impact.WindowCost != nil {
			a.cost += *f.Impact.WindowCost
			a.cost0 = true
		}
	}
	if len(byOwner) == 0 {
		return nil, 0, ""
	}

	keys := make([]string, 0, len(byOwner))
	// Rank by money when every owner has a price. Accelerators do not cost the
	// same per hour, so ordering by hours while printing money puts the most
	// expensive owner third and makes the reader distrust the whole section.
	// If any owner is unpriced, ranking by cost would silently sink them to
	// the bottom at zero, so hours it is.
	byCost := !o.NoCost
	for k, a := range byOwner {
		keys = append(keys, k)
		if !a.cost0 {
			byCost = false
		}
	}
	measure := func(a *agg) float64 {
		if byCost {
			return a.cost
		}
		return a.hours
	}
	// Name breaks ties so the document is byte-identical between two runs over
	// the same data. A report that reshuffles itself cannot be diffed, and
	// diffing two weeks is the main way anybody proves progress.
	sort.Slice(keys, func(i, j int) bool {
		a, b := measure(byOwner[keys[i]]), measure(byOwner[keys[j]])
		if a != b {
			return a > b
		}
		return keys[i] < keys[j]
	})

	var max float64
	for _, k := range keys {
		if m := measure(byOwner[k]); m > max {
			max = m
		}
	}

	cur := currencyOf(res)
	out := make([]ownerBar, 0, len(keys))
	for _, k := range keys {
		a := byOwner[k]
		b := ownerBar{Name: k, Hours: hours(a.hours), Count: a.count}
		// The bar's length encodes whatever the list was ranked by, so that
		// the order and the picture cannot tell different stories.
		if max > 0 {
			b.Share = measure(a) / max
		}
		if a.cost0 && !o.NoCost {
			b.Cost = costLabel(a.cost, cur)
			b.HasCost = true
		}
		out = append(out, b)
	}
	rankedBy := "fallow accelerator-hours"
	if byCost {
		rankedBy = "cost"
	}
	if len(out) > ownerTop {
		return out[:ownerTop], len(out) - ownerTop, rankedBy
	}
	return out, 0, rankedBy
}

// ownerKey groups by the real identity even when the report will be redacted:
// grouping first and masking afterwards keeps the groups faithful to the
// cluster, which a mask applied beforehand would not.
func ownerKey(f api.Finding) string {
	if f.Owner.Identity != "" {
		return f.Owner.Identity
	}
	if f.Workload.Namespace != "" {
		return f.Workload.Namespace
	}
	return "unattributed"
}

// anchorFor names the element a reader can link to. Normally that is the
// finding ID, which makes a shared link self-describing. A redacted report
// cannot afford that: joining names with hyphens produces a single opaque
// token that no whole-token replacement can take apart, so the anchor would
// republish exactly what the flag was asked to remove. There it falls back to
// a position, which is stable for a given report and says nothing.
func anchorFor(f api.Finding, o HTMLOptions, prefix string, i int) string {
	if o.Redact {
		return fmt.Sprintf("%s%d", prefix, i+1)
	}
	return slug(f.ID)
}

func newFindingView(f api.Finding, res *api.Result, o HTMLOptions, l Ledger, anchor string) findingView {
	v := findingView{
		Rank:         f.Rank,
		ID:           f.ID,
		Check:        f.Check,
		CheckLabel:   checkLabel(f.Check),
		Kind:         f.Workload.Kind,
		Summary:      f.Summary,
		Because:      f.Because,
		Hours:        hours(f.Impact.GPUHoursFallow),
		Accelerators: AcceleratorSummary(f),
		Confidence:   f.EvidenceConfidence,
		ConfClass:    confClass(f.EvidenceConfidence),
		Ownership:    f.OwnershipConfidence,
		Window:       Human(f.Evidence.Window.Duration()),
		Fallow:       Human(f.Evidence.FallowDuration.Duration()),
		UtilMax:      fmt.Sprintf("%.0f%%", f.Evidence.UtilizationMax),
		Notes:        f.Evidence.Notes,
		Risk:         f.Risk,
		Docs:         f.Docs,
		Targets:      f.Fix.Targets,
		Rationale:    f.Fix.Rationale,
		Frees:        f.Fix.Frees,
		Blockers:     f.Fix.Blockers,
		Command:      f.Fix.Command,
		Anchor:       anchor,
		CommandID:    "cmd-" + anchor,
	}

	v.Subject = f.Workload.Ref()
	if f.Workload.Grouped > 1 {
		v.SubjectSub = fmt.Sprintf("%s across %d pods", f.Summary, f.Workload.Grouped)
	} else {
		v.SubjectSub = f.Summary
	}

	if f.Owner.Identity != "" {
		v.Owner = f.Owner.Identity
		v.OwnerVia = f.Owner.ResolvedVia
	}

	if f.Evidence.LastNonZeroUtilization != nil {
		v.LastSeen = f.Evidence.LastNonZeroUtilization.UTC().Format("2 Jan 15:04 MST")
	} else {
		// Absent means "not in this window", never "never". The report says so
		// in words because the difference is the whole basis of the claim.
		v.LastSeen = "no work at any point in the window"
	}

	if f.Evidence.PowerDrawWatts > 0 {
		v.Power = fmt.Sprintf("%.0f W", f.Evidence.PowerDrawWatts)
		if f.Evidence.PowerDrawTDPRatio > 0 {
			v.Power += fmt.Sprintf(" (%.0f%% of TDP)", f.Evidence.PowerDrawTDPRatio*100)
		}
	}

	v.Completeness = fmt.Sprintf("%.0f%% of expected samples", f.Evidence.SampleCompleteness*100)

	if f.Provenance.Controlled {
		v.Controller = f.Provenance.RootKind + "/" + f.Provenance.RootName
	}
	for _, c := range f.Provenance.Chain {
		v.Chain = append(v.Chain, c.String())
	}
	// A one-link chain that repeats the controller above it is noise, and a
	// reader who sees the same value twice starts looking for the difference.
	v.ShowChain = len(v.Chain) > 1 || (len(v.Chain) == 1 && v.Chain[0] != v.Controller)

	if f.Impact.WindowCost != nil && !o.NoCost {
		v.Cost = costLabel(*f.Impact.WindowCost, firstNonEmpty(f.Impact.Currency, currencyOf(res)))
		v.HasCost = true
	}

	if l.Fallow > 0 {
		v.Share = f.Impact.GPUHoursFallow / l.Fallow
	}

	return v
}

func htmlFuncs() template.FuncMap {
	return template.FuncMap{
		// pct renders a 0-1 share as a CSS percentage width. Geometry is
		// computed in Go so the document is complete without scripting.
		"pct": func(f float64) string {
			if f < 0 {
				f = 0
			}
			if f > 1 {
				f = 1
			}
			return fmt.Sprintf("%.4f%%", f*100)
		},
		"hours":    hours,
		"rowClass": rowClass,
		"swatch":   swatchClass,
		"lower":    strings.ToLower,
	}
}

// rowClass maps a ledger bucket to its fill. The colours are assigned by
// meaning, and the meaning is written next to them: orange is capacity that
// was paid for and did nothing, blue is capacity held empty on purpose, grey
// is capacity nothing could be proven about, green is everything left over.
// GPU dashboards disagree about whether green means hot or good, so no colour
// here carries meaning on its own.
func rowClass(key string) string {
	switch key {
	case "fallow":
		return "bar-fallow"
	case "by-design":
		return "hatch-design"
	case "not-analysed":
		return "bar-neutral"
	case "suppressed":
		// Suppressed hours are fallow hours somebody silenced, not leftovers.
		// Sharing the residual colour would file them under "probably fine",
		// which is the opposite of what a suppression means.
		return "bar-suppressed"
	default:
		return "bar-residual"
	}
}

func swatchClass(key string) string {
	switch key {
	case "fallow":
		return "sw-fallow"
	case "by-design":
		return "sw-design"
	case "not-analysed":
		return "sw-neutral"
	case "suppressed":
		return "sw-suppressed"
	default:
		return "sw-residual"
	}
}

func confClass(c string) string {
	switch strings.ToLower(c) {
	case "high":
		return "conf-high"
	case "medium":
		return "conf-medium"
	default:
		return "conf-low"
	}
}

func checkLabel(id string) string {
	switch id {
	case "idle-pod":
		return "idle pod"
	case "unused-node":
		return "unused node"
	case "stuck-pod":
		return "stuck pod"
	default:
		return strings.ReplaceAll(id, "-", " ")
	}
}

func totalCost(fs []api.Finding) (float64, bool) {
	var sum float64
	var any bool
	for _, f := range fs {
		if f.Impact.WindowCost != nil {
			sum += *f.Impact.WindowCost
			any = true
		}
	}
	return sum, any
}

func worstCompleteness(fs []api.Finding) (float64, bool) {
	worst := 1.0
	var any bool
	for _, f := range fs {
		if f.Evidence.SampleCompleteness > 0 && f.Evidence.SampleCompleteness < worst {
			worst = f.Evidence.SampleCompleteness
			any = true
		}
	}
	return worst, any
}

// Pricing is nil whenever costing was disabled or no rate was known, which is
// an ordinary outcome rather than an error. Both accessors treat it that way.
func currencyOf(res *api.Result) string {
	if res.Pricing != nil && res.Pricing.Currency != "" {
		return res.Pricing.Currency
	}
	return "USD"
}

func pricingSource(res *api.Result) string {
	if res.Pricing == nil {
		return ""
	}
	return res.Pricing.Source
}

// costLabel formats a figure the same way the terminal does, with the
// approximation marker kept. Two renderings of one scan that disagree about a
// dollar amount would be worse than either of them being slightly wrong.
func costLabel(v float64, currency string) string {
	return "≈" + currencySymbol(currency) + money(v)
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
