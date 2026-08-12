package scan

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ganeshkumarashok/ullage/internal/check"
	"github.com/ganeshkumarashok/ullage/internal/config"
	"github.com/ganeshkumarashok/ullage/internal/humanize"
	"github.com/ganeshkumarashok/ullage/internal/inventory"
	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// Options controls a scan.
type Options struct {
	Window         time.Duration
	IdleThreshold  time.Duration
	StuckThreshold time.Duration
	InitGrace      time.Duration
	Step           time.Duration
	MinConfidence  string
	Namespaces     []string
	Checks         []string
	Pricing        *api.Pricing
	Suppressions   *config.Suppressions
	Now            time.Time
	Trace          bool
	Version        string
	Progress       func(string)
}

// Defaults fills in the documented default values.
func (o *Options) Defaults() {
	if o.Window == 0 {
		o.Window = 14 * 24 * time.Hour
	}
	if o.IdleThreshold == 0 {
		o.IdleThreshold = 24 * time.Hour
	}
	if o.StuckThreshold == 0 {
		o.StuckThreshold = time.Hour
	}
	if o.InitGrace == 0 {
		// Pulling a multi-gigabyte image or downloading forty gigabytes of
		// weights in an init container routinely takes more than an hour while
		// legitimately holding the device.
		o.InitGrace = 6 * time.Hour
	}
	if o.Step == 0 {
		// One sample per hour over a fortnight is 336 points per device: enough
		// to show shape, cheap enough not to melt a shared query frontend.
		o.Step = time.Hour
	}
	if o.MinConfidence == "" {
		o.MinConfidence = api.EvidenceMedium
	}
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}
	if o.Progress == nil {
		o.Progress = func(string) {}
	}
}

// EffectiveParams reports the settings a scan would actually run with, so that
// every code path emitting a Result describes itself the same way — including
// the early return for a cluster with no accelerators at all.
func EffectiveParams(opts Options) api.Params {
	opts.Defaults()
	p := api.Params{
		IdleThreshold:  api.ISODuration(opts.IdleThreshold),
		StuckThreshold: api.ISODuration(opts.StuckThreshold),
		MinConfidence:  opts.MinConfidence,
		Step:           api.ISODuration(opts.Step),
		Checks:         []string{},
	}
	if checks, err := check.Selected(opts.Checks); err == nil {
		for _, c := range checks {
			p.Checks = append(p.Checks, c.Describe().ID)
		}
		sort.Strings(p.Checks)
	}
	return p
}

// Analyse runs the checks over a cluster fact view and enriches what they find.
//
// Checks detect; this function decides. Ownership, provenance, the fix command,
// grouping, ranking and pricing all happen here, once, so that every check gets
// them identically and a new check inherits the whole pipeline for free.
func Analyse(ctx context.Context, cl *inventory.Cluster, inv *inventory.Inventory, opts Options) (*api.Result, error) {
	opts.Defaults()

	// Resolve the check set before the result is built so params can record
	// what actually ran. An empty list means "all of them", so recording the
	// request verbatim wrote `"checks": []` on the default invocation -- which
	// tells a consumer nothing, and silently changes meaning the day a fourth
	// check ships, making two results incomparable across versions.
	checks, err := check.Selected(opts.Checks)
	if err != nil {
		return nil, err
	}
	effectiveParams := EffectiveParams(opts)

	res := &api.Result{
		APIVersion: api.Version,
		Scan: api.ScanMeta{
			Tool:    api.Tool{Name: "ullage", Version: opts.Version},
			Context: cl.Context,
			Started: cl.Now,
			Window:  api.ISODuration(cl.Window),
			// Never nil, and never the request verbatim: an empty --checks
			// means "all of them", so a consumer reading `[]` learns nothing
			// and two results stop being comparable the day a check is added.
			Params:               effectiveParams,
			PodLabelSchema:       cl.PodLabelSchema,
			AcceleratorsObserved: inv.Observed,
			AcceleratorsAnalyzed: inv.Analyzed,
			AllocationModels:     inv.Counts,
			GPUHoursPaid:         inv.PaidHours,
			GPUHoursNotAnalysed:  inv.NotAnalysedHours,
			ProfilingMetrics:     cl.ProfilingMetrics,
			EnginesChecked:       cl.EnginesChecked,
		},
		Recommendations: []api.Finding{},
		Suppressed:      []api.Finding{},
		NotAnalyzed:     inv.Exclusions,
		Warnings:        []string{},
		Pricing:         opts.Pricing,
	}
	if res.NotAnalyzed == nil {
		res.NotAnalyzed = []api.Exclusion{}
	}
	// Belt and braces for the ordering above: exclusions are assembled by
	// several packages, and a stable document is worth more than the order any
	// one of them happened to append in.
	sort.SliceStable(res.NotAnalyzed, func(i, j int) bool {
		if res.NotAnalyzed[i].Code != res.NotAnalyzed[j].Code {
			return res.NotAnalyzed[i].Code < res.NotAnalyzed[j].Code
		}
		return res.NotAnalyzed[i].Detail < res.NotAnalyzed[j].Detail
	})

	params := check.Params{
		IdleThreshold:  opts.IdleThreshold,
		StuckThreshold: opts.StuckThreshold,
		InitGrace:      opts.InitGrace,
	}

	var raw []check.RawFinding
	for _, c := range checks {
		opts.Progress("Running check " + c.Describe().ID)
		found, err := c.Run(ctx, cl, params)
		if err != nil {
			// One failing check must not lose the others' findings. A partial
			// result that says so is more useful than no result.
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("check %s failed and was skipped: %v", c.Describe().ID, err))
			continue
		}
		raw = append(raw, found...)
	}

	var ranked, byDesign, suppressed []api.Finding
	below := 0
	for _, rf := range raw {
		if !opts.inScope(rf.Subject.Namespace) {
			continue
		}
		f := enrich(cl, rf, opts)

		// Checked before confidence and before by-design, so that a matching
		// entry is recorded as used even when the finding would not have been
		// shown anyway. Otherwise suppressing a finding would make the tool
		// report the suppression as dead the moment it started working.
		if reason, ok := opts.Suppressions.Match(f.ID); ok {
			f.Suppressed = true
			f.SuppressedReason = reason
			suppressed = append(suppressed, f)
			continue
		}

		switch {
		case f.ByDesign:
			byDesign = append(byDesign, f)
		case !meetsConfidence(f.EvidenceConfidence, opts.MinConfidence):
			below++
		default:
			ranked = append(ranked, f)
		}
	}

	sortFindings(ranked)
	sortFindings(byDesign)
	sortFindings(suppressed)
	res.Warnings = append(res.Warnings, opts.Suppressions.Warnings()...)

	total := 0.0
	for i := range ranked {
		ranked[i].Rank = i + 1
		total += ranked[i].Impact.GPUHoursUnused
	}
	for i := range byDesign {
		byDesign[i].Rank = i + 1
	}
	for i := range suppressed {
		suppressed[i].Rank = i + 1
	}

	// Assigned only when non-empty, so the empty slices set on Result above
	// survive. A field declared as a list must never serialise as null: every
	// consumer that iterates it breaks on the first cluster that has none, and
	// "no suppressions" is by far the most common case.
	if len(ranked) > 0 {
		res.Recommendations = ranked
	}
	if len(byDesign) > 0 {
		res.ByDesign = byDesign
	}
	if len(suppressed) > 0 {
		res.Suppressed = suppressed
	}
	res.BelowThreshold = below
	res.Scan.GPUHoursUnused = total
	res.UnmetDemand = unmetDemand(cl)
	return res, nil
}

// enrich turns a raw detection into a recommendation.
func enrich(cl *inventory.Cluster, rf check.RawFinding, opts Options) api.Finding {
	desc := descriptorFor(rf.Check)

	f := api.Finding{
		ID:                 check.FindingID(rf.Check, rf.Subject),
		Check:              rf.Check,
		EvidenceConfidence: rf.Confidence,
		Summary:            subjectRef(rf.Subject) + ": " + rf.Summary,
		Evidence:           rf.Evidence,
		Risk:               desc.Risk,
		Docs:               desc.Docs,
		ByDesign:           rf.ByDesign,
		Because:            rf.Because,
	}

	switch rf.Subject.Kind {
	case "node-pool":
		f.Workload = api.Workload{
			Kind: "NodePool", Name: rf.Subject.Name,
			Grouped: len(rf.Subject.Nodes), Members: rf.Subject.Nodes,
		}
		f.Owner = api.Owner{
			Identity: "platform", ResolvedVia: "node-pool",
			Detail: "node pools are owned by whoever runs the cluster",
		}
		f.OwnershipConfidence = api.OwnerResolved
		f.Provenance = api.Provenance{
			Controlled: true, RootKind: "NodePool",
			RootName: rf.Subject.Pool, Recognized: true,
		}
		f.Accelerators = acceleratorsForNodes(cl, rf.Subject.Nodes)
		f.Fix = poolFix(cl, rf, desc)

	default:
		pod := firstPod(cl, rf.Subject.Pods)
		names := make([]string, 0, len(rf.Subject.Pods))
		for _, r := range rf.Subject.Pods {
			names = append(names, r.Name)
		}
		f.Workload = api.Workload{
			Namespace: rf.Subject.Namespace,
			Kind:      workloadKind(pod),
			Name:      rf.Subject.Name,
			Grouped:   len(names),
			Members:   names,
		}
		f.Owner = pod.Owner
		f.OwnershipConfidence = OwnershipConfidence(pod.Owner)
		f.Provenance = pod.Provenance
		f.Accelerators = acceleratorsForPods(cl, rf.Subject.Pods, rf.Devices)
		f.Fix = SynthesiseFix(pod.Provenance, rf.Subject.Namespace, names, pod.Owner, desc.Prevention, rf.Subject.PartialOwner)
		if rf.Check == api.CheckStuckPod {
			f.Fix = stuckFix(pod, rf, desc)
		}
	}

	f.Fix.Blockers = rf.Blockers
	if rf.ByDesign {
		f.Fix = api.Fix{
			Targets: api.FixTargetNone,
			Rationale: "No action recommended. Review the reservation if the workload it was held " +
				"for has already shipped.",
		}
	}

	// Unused hours are device-hours, so a finding covering four devices for a
	// day is four times one covering one. This is the only ranking signal, and
	// it is the one a reader can verify by hand.
	// Every device's own idle time, added up. Falling back to the product is
	// only correct when the devices really were all unused together.
	f.Impact.GPUHoursUnused = rf.UnusedHours
	if f.Impact.GPUHoursUnused == 0 {
		f.Impact.GPUHoursUnused = float64(f.TotalAccelerators()) * rf.Unused.Hours()
	}
	priceFinding(&f, opts.Pricing)
	return f
}

// priceFinding attaches money only where a real rate exists for a single model.
//
// A rate blended across mixed models — T4 and H100 differ by roughly tenfold —
// is a fabricated number wearing a decimal point, and a fabricated dollar figure
// that a FinOps team can disprove destroys credibility across every other
// number in the output.
func priceFinding(f *api.Finding, p *api.Pricing) {
	if p == nil || len(f.Accelerators) != 1 {
		return
	}
	rate, ok := p.Rate(f.Accelerators[0].Model)
	if !ok {
		return
	}
	cost := f.Impact.GPUHoursUnused * rate
	f.Impact.WindowCost = &cost
	f.Impact.Currency = p.Currency
	f.Impact.PricingSource = p.Source
	f.Impact.PricingScope = "single-sku"
}

func descriptorFor(id string) check.Descriptor {
	if c, ok := check.Lookup(id); ok {
		return c.Describe()
	}
	return check.Descriptor{ID: id}
}

func acceleratorsForPods(cl *inventory.Cluster, pods []inventory.PodRef, deviceIDs []string) []api.Accelerator {
	byModel := map[string]*api.Accelerator{}
	counted := 0
	for _, ref := range pods {
		for _, d := range cl.DevicesOf(ref) {
			a, ok := byModel[d.Model]
			if !ok {
				a = &api.Accelerator{Model: d.Model, Vendor: d.Vendor, Allocation: d.Allocation, TDPWatts: d.TDPWatts}
				byModel[d.Model] = a
			}
			a.Count++
			counted++
		}
	}
	// A stuck pod often has no metrics at all — a container that never started
	// never produced any — so fall back to what the pod requested rather than
	// reporting zero devices for the most clear-cut findings the tool has.
	if counted == 0 {
		requested, model, vendor, alloc, tdp := 0, "unknown", "unknown", api.AllocExclusive, 0.0
		for _, ref := range pods {
			for _, p := range cl.Pods {
				if p.Ref == ref {
					requested += p.Accelerators
					if n := cl.NodeByName(p.Node); n != nil {
						model, vendor, alloc, tdp = n.Model, n.Vendor, n.Allocation, n.TDPWatts
					}
				}
			}
		}
		if requested == 0 {
			return nil
		}
		return []api.Accelerator{{Model: model, Vendor: vendor, Count: requested, Allocation: alloc, TDPWatts: tdp}}
	}
	return flatten(byModel)
}

func acceleratorsForNodes(cl *inventory.Cluster, nodes []string) []api.Accelerator {
	byModel := map[string]*api.Accelerator{}
	for _, name := range nodes {
		n := cl.NodeByName(name)
		if n == nil {
			continue
		}
		a, ok := byModel[n.Model]
		if !ok {
			a = &api.Accelerator{Model: n.Model, Vendor: n.Vendor, Allocation: n.Allocation, TDPWatts: n.TDPWatts}
			byModel[n.Model] = a
		}
		a.Count += n.Accelerators
	}
	return flatten(byModel)
}

func flatten(m map[string]*api.Accelerator) []api.Accelerator {
	out := make([]api.Accelerator, 0, len(m))
	for _, a := range m {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func firstPod(cl *inventory.Cluster, refs []inventory.PodRef) inventory.PodView {
	for _, ref := range refs {
		for _, p := range cl.Pods {
			if p.Ref == ref {
				return p
			}
		}
	}
	if len(refs) > 0 {
		return inventory.PodView{Ref: refs[0]}
	}
	return inventory.PodView{}
}

func workloadKind(p inventory.PodView) string {
	if p.Provenance.Controlled && p.Provenance.RootKind != "" {
		return p.Provenance.RootKind
	}
	return "Pod"
}

func (o *Options) inScope(ns string) bool {
	if len(o.Namespaces) == 0 || ns == "" {
		return true
	}
	for _, n := range o.Namespaces {
		if n == ns {
			return true
		}
	}
	return false
}

var confidenceRank = map[string]int{
	api.EvidenceLow:    0,
	api.EvidenceMedium: 1,
	api.EvidenceHigh:   2,
}

// meetsConfidence reports whether a finding clears the operator's bar.
//
// Both sides fail closed on an unrecognised level, and they fail closed in
// opposite directions on purpose. An unknown level on the `have` side means a
// check emitted something this build does not understand, so it must not be
// published. An unknown level on the `min` side means the caller asked for a
// bar that does not exist -- read as the zero value that would once have been
// "low", it silently *lowered* the threshold, which is the one direction a
// tool that recommends deleting hardware must never fail in. The CLI rejects
// such a value outright; this guard covers callers of pkg/ullage, who have no
// flag parser between them and this function.
func meetsConfidence(have, min string) bool {
	h, ok := confidenceRank[have]
	if !ok {
		return false
	}
	m, ok := confidenceRank[min]
	if !ok {
		m = confidenceRank[api.EvidenceHigh]
	}
	return h >= m
}

// sortFindings orders by money where money is known, and by accelerator-hours
// where it is not.
//
// Deliberately not a composite score involving confidence or ease of fix: a
// reader must be able to look at the list and understand instantly why row one
// is above row two. Confidence filters before ranking; it never acts as a
// multiplier within it. Ties break deterministically so output is stable
// between runs on an unchanged cluster.
//
// Hours alone are the wrong ranking for a tool that prints a dollar figure on
// every row: 2,700 hours of L4 is a third of the value of 1,000 hours of A100,
// so an hours-ranked list puts the cheapest finding at the top and tells the
// reader to start there. Whoever reads this has limited attention, and the
// first row is the only one some of them will act on.
//
// Findings whose cost is unknown are not silently sorted to the bottom — a
// missing price is not a small number. They are ranked among themselves by
// hours and placed after the priced ones, because a number you cannot compare
// cannot be claimed to be larger.
func sortFindings(f []api.Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		ci, cj := f[i].Impact.WindowCost, f[j].Impact.WindowCost
		if (ci != nil) != (cj != nil) {
			return ci != nil
		}
		if ci != nil && *ci != *cj {
			return *ci > *cj
		}
		if f[i].Impact.GPUHoursUnused != f[j].Impact.GPUHoursUnused {
			return f[i].Impact.GPUHoursUnused > f[j].Impact.GPUHoursUnused
		}
		if f[i].Evidence.UnusedDuration != f[j].Evidence.UnusedDuration {
			return f[i].Evidence.UnusedDuration > f[j].Evidence.UnusedDuration
		}
		return f[i].ID < f[j].ID
	})
}

// unmetDemand counts Pending pods that want accelerators.
//
// This is context, never a finding. A Pending pod has not been scheduled and
// holds no device — it is the victim of the waste, frequently blocked by
// exactly the workloads the checks found. Reporting recoverable hours against
// it would be arithmetically zero and conceptually backwards. Printing it
// beside the idle capacity is the most persuasive thing in the output, because
// it shows both halves of the same problem.
func unmetDemand(cl *inventory.Cluster) *api.UnmetDemand {
	count, gpus, unschedulable := 0, 0, 0
	for _, p := range cl.Pods {
		if !p.Pending || p.Accelerators == 0 {
			continue
		}
		count++
		gpus += p.Accelerators
		if p.Unschedulable {
			unschedulable++
		}
	}
	if count == 0 {
		return nil
	}
	detail := "waiting for accelerators that are not available"
	if unschedulable > 0 {
		detail = fmt.Sprintf("%d reported Unschedulable by the scheduler", unschedulable)
	}
	return &api.UnmetDemand{Pods: count, Accelerators: gpus, Detail: detail}
}

// Ref renders a subject the way a person types it.
func subjectRef(s check.Subject) string {
	if s.Namespace == "" {
		return s.Name
	}
	return s.Namespace + "/" + s.Name
}

var _ = humanize.Duration
