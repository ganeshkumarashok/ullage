package scan

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ullage-project/ullage/internal/check"
	"github.com/ullage-project/ullage/internal/inventory"
	"github.com/ullage-project/ullage/internal/kube"
	"github.com/ullage-project/ullage/internal/promql"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// Metric names. DCGM_FI_DEV_GPU_UTIL is asked only the one question it can
// answer honestly — was this ever non-zero — and power draw is the independent
// corroboration from a different sensor.
const (
	MetricGPUUtil = "DCGM_FI_DEV_GPU_UTIL"
	MetricPower   = "DCGM_FI_DEV_POWER_USAGE"
	MetricProfSM  = "DCGM_FI_PROF_SM_ACTIVE"
)

// otherEngines are the accelerator engines that DCGM_FI_DEV_GPU_UTIL does not
// see. It reports SM activity, so a device can read exactly zero across an
// entire fortnight while doing continuous, expensive, entirely real work:
//
//   - a video pipeline living on NVENC and NVDEC, which is most of what
//     inference-on-video looks like;
//   - a data-loading or checkpointing stage saturating the copy engines;
//   - a model parked in framebuffer between requests, which is the whole point
//     of keeping a warm replica and is exactly what someone would be furious to
//     have scaled to zero.
//
// All four are in dcgm-exporter's default counter set, so this asks for
// nothing an ordinary installation does not already export. A device where any
// of them was ever non-zero is not idle, whatever the SM gauge said.
var otherEngines = []struct {
	metric string
	what   string
}{
	{"DCGM_FI_DEV_ENC_UTIL", "the video encoder"},
	{"DCGM_FI_DEV_DEC_UTIL", "the video decoder"},
	{"DCGM_FI_DEV_MEM_COPY_UTIL", "the memory copy engines"},
	{"DCGM_FI_DEV_FB_USED", "framebuffer memory"},
}

// Gatherer turns a live cluster into the normalized fact layer.
//
// Everything backend-specific lives here. Past this point nothing knows that
// Kubernetes or Prometheus exist, which is what lets checks be tested from
// literals and lets a different metrics source be substituted without touching
// a single check.
type Gatherer struct {
	Kube *kube.Client
	Prom *promql.Client

	// Trace records the exact queries issued, when requested. The trust
	// argument for this tool is that its claims are checkable, and that is
	// empty unless the reader can see and re-run the queries.
	Trace bool

	// Selector is a PromQL label matcher applied to every metric read, for the
	// case where the endpoint holds more than this one cluster.
	Selector string

	queries []string
}

// Gather builds the cluster fact view.
func (g *Gatherer) Gather(ctx context.Context, opts Options) (*inventory.Cluster, *inventory.Inventory, []string, error) {
	var warnings []string
	end := opts.Now
	start := end.Add(-opts.Window)

	opts.Progress("Reading cluster state")
	pods, err := g.Kube.Pods(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing pods: %w", err)
	}
	nodes, err := g.Kube.Nodes(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing nodes: %w", err)
	}
	namespaces, err := g.Kube.Namespaces(ctx)
	if err != nil {
		warnings = append(warnings, "namespaces are not readable, so owner attribution will be weaker")
	}
	nsByName := map[string]*kube.ObjectMeta{}
	for i := range namespaces {
		nsByName[namespaces[i].Name] = &namespaces[i]
	}

	// A DRA pod holds no extended resource, so the *only* record that it is
	// sitting on four H100s is its ResourceClaim. Swallowing this error, which
	// is what a bare `_` did, made an occupied node read as an empty one and
	// attached "delete this pool" to hardware at capacity. RBAC that has not
	// been updated for resource.k8s.io is the ordinary way to get here.
	//
	// A cluster with no DRA at all is not an error: ResourceClaims returns
	// nil, nil when the API group is absent.
	claims, claimsErr := g.Kube.ResourceClaims(ctx)
	draByPod := draDevicesByPod(claims)
	draByNode := draDevicesByNode(claims, pods)

	// Which nodes are we now unable to speak about? Only those hosting a pod
	// that declares a claim: the pod spec is readable even when the claim is
	// not, so the blast radius is exactly known rather than assumed to be the
	// whole cluster. Nodes with no DRA pods on them are still fully analysable.
	draOpaqueNodes := map[string]bool{}
	if claimsErr != nil {
		for i := range pods {
			if pods[i].UsesDRA() && pods[i].Spec.NodeName != "" {
				draOpaqueNodes[pods[i].Spec.NodeName] = true
			}
		}
		warnings = append(warnings, fmt.Sprintf(
			"ResourceClaims are not readable (%v), so devices held through DRA are invisible; "+
				"%d node(s) running DRA pods are excluded rather than reported as empty",
			claimsErr, len(draOpaqueNodes)))
	}

	opts.Progress("Classifying accelerator allocation")
	inv := inventory.Build(nodes, draByNode)
	if inv.Observed == 0 {
		return nil, inv, append(warnings, "no accelerators were found on any node"), nil
	}

	opts.Progress(fmt.Sprintf("Querying GPU metrics over %s", opts.Window.Round(time.Hour)))
	if strings.TrimSpace(g.Selector) == "" {
		if label, values := g.detectMergedClusters(ctx, end); label != "" {
			shown := values
			if len(shown) > 4 {
				shown = append(append([]string{}, shown[:4]...),
					fmt.Sprintf("and %d more", len(values)-4))
			}
			warnings = append(warnings, fmt.Sprintf(
				"the metrics endpoint holds more than one cluster (label %q has values %s), "+
					"and the accelerators of this cluster cannot be told apart from theirs; "+
					"node names are not unique across clusters, so a busy device elsewhere "+
					"can answer for an idle one here. Pass --metrics-selector '%s=\"...\"' "+
					"to name this cluster",
				label, strings.Join(shown, ", "), label))
		}
	}
	schema, err := g.Prom.DetectLabelSchema(ctx, g.sel(MetricGPUUtil), end)
	if err != nil {
		return nil, inv, warnings, err
	}
	if !schema.Found {
		warnings = append(warnings,
			"GPU metrics carry no pod or namespace labels under either the `pod` or `exported_pod` "+
				"schema, so per-pod checks cannot run; node-level checks still apply")
	}

	devices, powerless, engines, scrape, err := g.devices(ctx, schema, inv, start, end, opts.Step)
	if err != nil {
		return nil, inv, warnings, err
	}
	// Saying which engines were consulted is not a detail. "No GPU work" from a
	// tool that only looked at the SMs is a different statement from one that
	// checked the encoder, the decoder, the copy engines and framebuffer too,
	// and the operator deciding whether to run the command needs to know which
	// of those they were given.
	if len(engines) == 0 {
		warnings = append(warnings,
			"only DCGM_FI_DEV_GPU_UTIL was available, which reports SM activity alone; work "+
				"on the video encoder or decoder, the copy engines, or a model held resident "+
				"in framebuffer reads as zero here and cannot be ruled out")
	}
	if powerless {
		warnings = append(warnings,
			"no power draw series were returned, so idle findings cannot be corroborated "+
				"and are reported at medium confidence")
	}

	cl := &inventory.Cluster{
		Context:           g.Kube.Context(),
		Now:               end,
		Window:            opts.Window,
		Step:              scrape,
		Devices:           devices,
		MetricsAttributed: schema.Found,
		PodLabelSchema:    schema.Pod,
		EnginesChecked:    engines,
	}

	// Who decides whether an empty node stays? nil means nobody could be
	// identified, and that is unknown, not permission to recommend removing
	// capacity.
	//
	// cluster-autoscaler is asked first because its status ConfigMap carries a
	// minimum size, which is the strongest signal available. Karpenter has no
	// minimum size to read, so a cluster running only Karpenter would otherwise
	// look like a cluster with no autoscaler at all — and would collect the
	// "a deliberate minimum cannot be ruled out" caveat on every single
	// finding, which is both noisy and false.
	if status, err := g.Kube.ClusterAutoscalerStatus(ctx); err == nil && status != nil {
		floors := map[string]int{}
		for _, grp := range status.Groups {
			floors[grp.Name] = grp.MinSize
		}
		cl.Autoscaler = &inventory.AutoscalerView{
			Kind: "cluster-autoscaler", Floors: floors,
		}
	} else if kp, err := g.Kube.KarpenterNodePools(ctx); err == nil && kp != nil {
		pinned, scheduled := map[string]bool{}, map[string]bool{}
		for name, np := range kp.NodePools {
			pinned[name] = np.Pinned
			scheduled[name] = np.ScheduledHold
		}
		cl.Autoscaler = &inventory.AutoscalerView{
			Kind:      "karpenter",
			Pinned:    pinned,
			Scheduled: scheduled,
		}
	} else {
		warnings = append(warnings,
			"no cluster-autoscaler status ConfigMap and no Karpenter NodePools were readable, so "+
				"deliberately reserved capacity could not be distinguished from unused capacity")
	}

	// Whether a PodDisruptionBudget forbids evicting a pod is the difference
	// between "this node can be reclaimed" and a command that will hang. An
	// unreadable list is not an empty list, and treating it as one produces
	// the confident version of the wrong answer.
	pdbs, pdbErr := g.Kube.PodDisruptionBudgets(ctx)
	if pdbErr != nil {
		warnings = append(warnings, fmt.Sprintf(
			"PodDisruptionBudgets are not readable (%v), so pods are reported as possibly "+
				"blocking scale-down rather than assumed safe to evict", pdbErr))
	}

	opts.Progress("Resolving workload owners")
	resolver := NewResolver(g.Kube)
	for i := range pods {
		cl.Pods = append(cl.Pods, g.podView(ctx, resolver, &pods[i], nsByName, pdbs, pdbErr == nil, draByPod, end))
	}
	for name, ni := range inv.Nodes {
		kpool, _ := kube.NodePoolOf(ni.Node)
		cl.Nodes = append(cl.Nodes, inventory.NodeView{
			KarpenterPool:     kpool,
			Name:              name,
			Pool:              ni.Pool,
			Provider:          ni.Provider,
			Model:             ni.Model,
			Vendor:            ni.Vendor,
			Accelerators:      ni.Physical,
			Allocation:        ni.Allocation,
			TDPWatts:          ni.TDPWatts,
			Ready:             ni.Node.Ready(),
			Unschedulable:     ni.Node.Spec.Unschedulable,
			Age:               end.Sub(ni.Node.Metadata.CreationTimestamp),
			Initialising:      ni.Initialising,
			OccupancyUnknown:  draOpaqueNodes[name],
			ScaleDownDisabled: strings.EqualFold(ni.Node.Metadata.Annotations["cluster-autoscaler.kubernetes.io/scale-down-disabled"], "true"),
		})
	}
	sort.Slice(cl.Nodes, func(i, j int) bool { return cl.Nodes[i].Name < cl.Nodes[j].Name })

	// Deciding which pool an autoscaler node group belongs to needs the full
	// list of pool names, so it happens here rather than where the autoscaler
	// was read — the nodes did not exist yet at that point.
	if cl.Autoscaler != nil {
		cl.Autoscaler.Pools = poolNames(cl.Nodes)
	}

	// Attribution is counted after the join, not assumed from the census: a
	// device whose metrics exist but carry no pod is not an analysed device.
	// Counted over distinct device IDs, not over records. cl.Devices holds one
	// record per metric series, and a GPU handed to a second pod during the
	// window produces a second series for the same physical card. Counting
	// records would report more accelerators analysed than the cluster has —
	// "analysed 80 of 68 observed" — which discredits the one number the whole
	// tool rests on. Per-series records are kept deliberately, because
	// per-pod attribution needs them; only the census collapses them.
	seen := map[string]bool{}
	for _, d := range cl.Devices {
		if d.Analyzable {
			seen[d.ID] = true
		}
	}
	analyzed := len(seen)
	inv.Analyzed = analyzed

	// Every device the census saw but the metrics did not must be named. The
	// alternative is a headline that quietly divides by a denominator the
	// reader cannot see, which turns a monitoring gap into a claim about
	// efficiency — the exact failure this tool exists to avoid making.
	if missing := inv.Analyzable() - analyzed; missing > 0 {
		inv.Exclusions = append(inv.Exclusions, api.Exclusion{
			Code:         api.ExclNoMetrics,
			Reason:       "no-metrics",
			Accelerators: missing,
			Detail: "the census found these accelerators on nodes, but no DCGM series " +
				"was returned for them over the window",
			Remedy: "check that dcgm-exporter is running on every GPU node and that Prometheus scrapes it; " +
				"until then these devices are counted as paid for but not judged",
		})
	}

	if len(cl.Devices) > 0 && analyzed == 0 {
		warnings = append(warnings,
			"no accelerator could be analysed: metrics were found but none could be matched to a node")
	}

	if prof, err := g.Prom.Query(ctx, MetricProfSM, end); err == nil && len(prof) > 0 {
		cl.ProfilingMetrics = true
	}
	return cl, inv, warnings, nil
}

// ScrapeInterval is the fallback dcgm-exporter scrape period, used only when
// the real one cannot be measured. It is the default in the exporter's own
// chart.
//
// It was previously assumed unconditionally, and the direction of the error is
// not safe. Sample counts come from count_over_time, which reports points, so
// coverage is samples divided by window/interval. Assume 30s against an
// exporter scraping at 15s and the expected count is halved, so seven days of
// data across a fourteen-day window divides out to 100% coverage -- a full
// fortnight of confident observation conjured from half a window of data, on
// the one number that decides whether a recommendation is trustworthy enough
// to print.
const ScrapeInterval = 30 * time.Second

// measureScrapeInterval asks the data how often it arrives instead of assuming.
//
// One instant query over a recent hour: the number of points a series produced
// in an hour divides into an hour to give the period. The busiest series is
// used because a series that started mid-hour would understate it, and
// overstating the interval overstates coverage, which is the failure this
// exists to prevent.
//
// A cluster that cannot answer keeps the documented default. Guessing wrong is
// survivable; guessing silently in the flattering direction is not, so the
// measured value is recorded and reported.
func (g *Gatherer) measureScrapeInterval(ctx context.Context, end time.Time) (time.Duration, bool) {
	const probe = time.Hour
	samples, err := g.instant(ctx,
		fmt.Sprintf("count_over_time(%s[%s])", g.sel(MetricGPUUtil), promql.Range(probe)), end)
	if err != nil || len(samples) == 0 {
		return ScrapeInterval, false
	}

	best := 0.0
	for _, s := range samples {
		if s.Value > best {
			best = s.Value
		}
	}
	if best <= 1 {
		return ScrapeInterval, false
	}

	raw := probe.Seconds() / best

	// Snapped to a configured interval rather than used raw. count_over_time
	// counts both endpoints, so an hour at 30s returns 121 points and divides
	// out to 29.75s -- and a scrape period slightly *longer* than the truth
	// inflates every coverage figure derived from it.
	//
	// Scrape intervals are configured, not continuous, so the nearest
	// plausible value is the honest reading. The candidates are far enough
	// apart that a few percent of probe error cannot move between them.
	candidates := []time.Duration{
		time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second,
		15 * time.Second, 20 * time.Second, 30 * time.Second,
		time.Minute, 2 * time.Minute, 5 * time.Minute,
	}
	best2, bestErr := time.Duration(0), math.MaxFloat64
	for _, c := range candidates {
		if e := math.Abs(c.Seconds()-raw) / c.Seconds(); e < bestErr {
			best2, bestErr = c, e
		}
	}
	// Nothing within a quarter of a plausible interval is not a scrape period,
	// it is a broken probe: a recording rule, a partial final hour, or a
	// series that started moments ago.
	if best2 == 0 || bestErr > 0.25 {
		return ScrapeInterval, false
	}
	return best2, true
}

// chunk is the sub-window each range aggregate is evaluated over.
//
// Aggregate push-down makes the *response* small, but it does not make the
// query cheap: Prometheus still loads every raw sample in the range to evaluate
// max_over_time, and that counts against --query.max-samples, which defaults to
// 50,000,000. At a 30s scrape a 14d range is 40,320 samples per series, so a
// single whole-window query over one series per GPU exceeds the limit at about
// 1,240 GPUs — inside the range of cluster this tool is built for, where it
// fails outright rather than degrading.
//
// Evaluating in 24h chunks and combining in Go bounds each query at 2,880
// samples per series whatever the window length, which puts the ceiling around
// 17,000 GPUs. max and count both decompose over sub-windows exactly, so this
// costs nothing in accuracy — only a handful of extra round trips.
const chunk = 24 * time.Hour

// devices runs the metric queries and joins them into device facts.
//
// The queries are aggregate push-downs rather than raw sample streams. The
// question "was this ever non-zero in the window" is an aggregate, and asking
// Prometheus for a fortnight of raw samples for every device to answer it in Go
// would move tens of millions of samples for a few hundred booleans.
func (g *Gatherer) devices(ctx context.Context, schema promql.LabelSchema, inv *inventory.Inventory, start, end time.Time, step time.Duration) ([]inventory.Device, bool, []string, time.Duration, error) {
	window := end.Sub(start)

	// maxCovered is the fraction of the window the "was it ever non-zero"
	// question was actually answered for. It is the most important number in
	// this function: an interval with no answer is an interval where the device
	// might have been at 100%, and treating it as zero is precisely how a tool
	// recommends deleting a busy GPU.
	maxUtil, maxCovered, err := g.chunked(ctx, MetricGPUUtil, "max_over_time", schema, start, end, maxCombine)
	if err != nil {
		return nil, false, nil, 0, err
	}
	if len(maxUtil) == 0 {
		return nil, false, nil, 0, promql.ErrNoSeries
	}
	// Power is averaged over a recent window rather than the whole one. The
	// claim being corroborated is that the device is doing nothing *now*, and a
	// fourteen-day mean over a device that worked for the first three days
	// reports a healthy power draw for a device that has been dark ever since —
	// which reads as a contradiction and quietly downgrades a correct finding.
	powerWindow := 24 * time.Hour
	if window < powerWindow {
		powerWindow = window
	}
	// Every other engine DCGM can see. A device busy on any of them is not
	// idle, so the answers are merged into one "was this ever non-zero" map and
	// consulted alongside the SM gauge.
	//
	// A metric that is simply not exported comes back empty, which disqualifies
	// nothing — dcgm-exporter can be configured down to a handful of counters,
	// and refusing to run against those installations would be worse than
	// running with the SM gauge alone, provided the report says which engines
	// were actually checked. Which engines answered is recorded for exactly
	// that reason.
	busyElsewhere := map[string]string{}
	var checkedEngines []string

	// How often samples actually arrive, rather than how often the exporter's
	// default chart says they do.
	scrape, _ := g.measureScrapeInterval(ctx, end)
	for _, e := range otherEngines {
		vals, covered, err := g.chunked(ctx, e.metric, "max_over_time", schema, start, end, maxCombine)
		if err != nil || covered <= 0 || len(vals) == 0 {
			continue
		}
		checkedEngines = append(checkedEngines, e.metric)
		for _, v := range vals {
			if v.Value <= 0 {
				continue
			}
			k := deviceKey(v.Labels)
			if k == "" {
				continue
			}
			if _, seen := busyElsewhere[k]; !seen {
				busyElsewhere[k] = e.what
			}
		}
	}

	avgPower, _ := g.instant(ctx, fmt.Sprintf("avg_over_time(%s[%s])", g.sel(MetricPower), promql.Range(powerWindow)), end)
	samples, _, err := g.chunked(ctx, MetricGPUUtil, "count_over_time", schema, start, end, sumCombine)
	if err != nil && ctx.Err() != nil {
		return nil, false, nil, 0, ctx.Err()
	}

	// The sparkline and "when did work last happen" need shape, not precision,
	// so they use one coarse range query at an hourly step: 336 points per
	// series over a fortnight rather than four thousand.
	shape, err := g.Prom.QueryRange(ctx, g.sel(MetricGPUUtil), start, end, step)
	if err != nil {
		shape = nil
	}
	if g.Trace {
		// The traced query has to be the query. A trace that omits the selector
		// is one a reader can paste and get a different answer from, which
		// makes the evidence unverifiable in exactly the setup where the
		// selector mattered.
		g.queries = append(g.queries, fmt.Sprintf("%s @ range %s..%s step %s",
			g.sel(MetricGPUUtil), start.Format(time.RFC3339), end.Format(time.RFC3339), step))
	}
	// Keyed by series, not by device. A GPU held by two pods inside the window
	// returns a series per holder, so a device-keyed map keeps whichever one
	// happened to be decoded last and then hands that shape to *every* record
	// for the card. Observed live: a finished job's stale series overwrote the
	// running job's, and a GPU sitting at 78% was reported as having done no
	// work since the window began.
	shapeBy := map[string]promql.Series{}
	for _, s := range shape {
		shapeBy[seriesKey(s.Labels, schema)] = s
	}
	powerBy := map[string]float64{}
	for _, s := range avgPower {
		powerBy[deviceKey(s.Labels)] = s.Value
	}
	// Summed, not assigned: dcgm-exporter labels utilization with the pod that
	// held the device, so a GPU reused by three pods over a fortnight returns
	// three series. Last-write-wins would report the coverage of whichever one
	// happened to sort last, understating completeness for exactly the busy
	// devices whose coverage matters most.
	samplesBy := map[string]float64{}
	seriesSamplesBy := map[string]float64{}
	for _, s := range samples {
		samplesBy[deviceKey(s.Labels)] += s.Value
		seriesSamplesBy[seriesKey(s.Labels, schema)] += s.Value
	}

	// Expected sample count, used to tell a scrape gap from a genuine zero. An
	// absent sample is not an idle sample, and conflating them is how a tool
	// confidently reports that a device nobody was watching did nothing.
	expected := window.Seconds() / scrape.Seconds()
	if expected < 1 {
		expected = 1
	}

	var out []inventory.Device
	for _, s := range maxUtil {
		key := deviceKey(s.Labels)
		node := labelOf(s.Labels, "Hostname", "hostname", "instance", "node")
		ni := inv.Nodes[node]

		d := inventory.Device{
			ID:     key,
			Node:   node,
			Model:  labelOf(s.Labels, "modelName", "model", "device_model"),
			Vendor: "nvidia",
		}
		if ni != nil {
			d.Pool = ni.Pool
			d.Allocation = ni.Allocation
			d.TDPWatts = ni.TDPWatts
			d.Vendor = ni.Vendor
			if d.Model == "" {
				d.Model = ni.Model
			}
			d.Analyzable = ni.Analyzable() && !ni.Initialising
		}
		if schema.Found {
			if pod := s.Labels[schema.Pod]; pod != "" {
				d.Holder = &inventory.PodRef{Namespace: s.Labels[schema.Namespace], Name: pod}
			}
		}

		// The SM gauge said nothing ran; another engine says something did.
		// The second is evidence of work and the first is only the absence of
		// one kind of it, so the second wins.
		busyOn := busyElsewhere[key]

		d.Util = inventory.Stats{
			Max:            s.Value,
			ZeroThroughout: s.Value == 0 && busyOn == "",
			BusyEngine:     busyOn,
			FallowSince:    start,
			// Capped by how much of the window the max query answered for.
			// Sample counts come from a separate query, so without this cap a
			// device whose max chunks half failed still reports full coverage
			// and its zero is believed. The checks gate on completeness, so
			// this turns a partial read into a rejected finding rather than a
			// confident wrong one.
			Completeness: clamp(samplesBy[key]/expected, 0, 1) * maxCovered,
			Answered:     maxCovered,
			Samples:      int(seriesSamplesBy[seriesKey(s.Labels, schema)]),
		}
		if series, ok := shapeBy[seriesKey(s.Labels, schema)]; ok {
			refine(&d.Util, series, start, end, step)
		}
		if watts, ok := powerBy[key]; ok {
			d.Power = inventory.Stats{Samples: 1, Mean: watts, Max: watts}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	return out, len(avgPower) == 0, checkedEngines, scrape, nil
}

// refine fills in the parts of a device's statistics that need the shape of the
// series rather than a single aggregate.
func refine(st *inventory.Stats, s promql.Series, start, end time.Time, step time.Duration) {
	sum := promql.Summarise(s, start, end, step, 14)
	if n := len(s.Samples); n > 0 {
		last := s.Samples[n-1].T
		st.LastSample = &last
		// Three resolutions of slack absorbs a delayed scrape and the
		// alignment of the range grid; beyond that the series has stopped,
		// and a stopped series says nothing about the present.
		st.Stale = end.Sub(last) > 3*step
	}
	st.Buckets = sum.Buckets
	st.Mean = sum.Mean
	st.LastNonZero = sum.LastNonZero
	if sum.FallowSince.After(st.FallowSince) {
		st.FallowSince = sum.FallowSince
	}
	// A series that never read non-zero has no FallowSince of its own, so it
	// would otherwise claim to have been idle since the window opened — even if
	// its first sample landed yesterday because the node joined then. A device
	// cannot give evidence about time before it was being watched.
	if sum.LastNonZero == nil && len(s.Samples) > 0 {
		if first := s.Samples[0].T; first.After(st.FallowSince) {
			st.FallowSince = first
		}
	}
	// The coarse series is a downsample, so it can only ever contradict a zero
	// by finding work the aggregate missed — never the other way round.
	if sum.Max > st.Max {
		st.Max = sum.Max
		st.ZeroThroughout = false
	}
}

// chunked evaluates a range aggregate in sub-windows and combines the results.
//
// Sub-window failures are tolerated as long as one succeeds: a Prometheus that
// has lost older blocks, or a retention shorter than the window, should shorten
// the evidence rather than abort the scan. What is not tolerated is silence —
// a chunk that fails reduces sample completeness, which the checks already
// treat as a reason to lower confidence rather than to make a claim.
// clusterLabels are the labels a metrics backend adds when it holds more than
// one cluster. Prometheus itself writes none of them: they arrive from the
// external_labels of a federated setup, or from the remote_write of a central
// Thanos, Mimir or Grafana Cloud tenant.
var clusterLabels = []string{"cluster", "cluster_name", "k8s_cluster_name", "prometheus", "source_cluster"}

// sel applies the configured metric selector.
//
// Against a central store holding forty clusters, an unqualified metric name
// returns all forty. Node names are not globally unique -- kubeadm and kind
// both produce "gpu-worker-0" -- so a busy device in one cluster can answer for
// an idle device of the same name in another, and the merge is silent.
func (g *Gatherer) sel(metric string) string {
	if strings.TrimSpace(g.Selector) == "" {
		return metric
	}
	return metric + "{" + strings.TrimSpace(g.Selector) + "}"
}

// detectMergedClusters reports the cluster-identifying label, if any, that
// carries more than one value in the metric being read.
//
// This is the only chance to notice. Once the samples are joined to nodes, a
// foreign device either vanishes -- taking its evidence with it -- or answers
// for a local node of the same name, and neither leaves a trace.
func (g *Gatherer) detectMergedClusters(ctx context.Context, at time.Time) (label string, values []string) {
	samples, err := g.Prom.QueryExpr(ctx, g.sel(MetricGPUUtil), at)
	if err != nil || len(samples) == 0 {
		return "", nil
	}
	for _, l := range clusterLabels {
		seen := map[string]bool{}
		for _, s := range samples {
			if v := strings.TrimSpace(s.Labels[l]); v != "" {
				seen[v] = true
			}
		}
		if len(seen) > 1 {
			for v := range seen {
				values = append(values, v)
			}
			sort.Strings(values)
			return l, values
		}
	}
	return "", nil
}

func (g *Gatherer) chunked(
	ctx context.Context, metric, fn string, schema promql.LabelSchema, start, end time.Time,
	combine func(a, b float64) float64,
) ([]promql.VectorSample, float64, error) {
	acc := map[string]*promql.VectorSample{}
	var order []string
	var lastErr error
	ok := false
	var answered, total time.Duration

	for at := end; at.After(start); at = at.Add(-chunk) {
		span := chunk
		if rem := at.Sub(start); rem < span {
			span = rem
		}
		total += span
		q := fmt.Sprintf("%s(%s[%s])", fn, g.sel(metric), promql.Range(span))
		got, err := g.instant(ctx, q, at)
		if err != nil {
			// Cancellation is not a gap in the data, it is the end of the
			// scan. Continuing would spend the remaining chunks failing and
			// then report the partial result as if it were the whole window.
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			lastErr = err
			continue
		}
		ok = true
		answered += span
		for _, s := range got {
			// seriesKey, not an ad-hoc key: identity here has to mean exactly
			// what it means everywhere else. Concatenating the pod labels
			// without the namespace merged team-a/trainer and team-b/trainer
			// on one card into a single result, summing one team's sample
			// count into the other's coverage.
			k := seriesKey(s.Labels, schema)
			if cur, seen := acc[k]; seen {
				cur.Value = combine(cur.Value, s.Value)
				continue
			}
			cp := s
			acc[k] = &cp
			order = append(order, k)
		}
	}
	if !ok {
		if lastErr == nil {
			lastErr = promql.ErrNoSeries
		}
		return nil, 0, lastErr
	}
	covered := 1.0
	if total > 0 {
		covered = float64(answered) / float64(total)
	}
	out := make([]promql.VectorSample, 0, len(order))
	for _, k := range order {
		out = append(out, *acc[k])
	}
	return out, covered, nil
}

func maxCombine(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func sumCombine(a, b float64) float64 { return a + b }

func (g *Gatherer) instant(ctx context.Context, query string, at time.Time) ([]promql.VectorSample, error) {
	if g.Trace {
		g.queries = append(g.queries, query)
	}
	return g.Prom.QueryExpr(ctx, query, at)
}

// Queries returns the recorded query trace.
func (g *Gatherer) Queries() []string { return g.queries }

// seriesKey identifies one series, which is one holder's tenure on one device.
// Records differ from one another only by holder, so the device key plus the
// pod label is the whole identity.
func seriesKey(labels map[string]string, schema promql.LabelSchema) string {
	pod := ""
	if schema.Found {
		pod = labels[schema.Pod] + "/" + labels[schema.Namespace]
	}
	return deviceKey(labels) + "|" + pod
}

func deviceKey(labels map[string]string) string {
	return labelOf(labels, "Hostname", "hostname", "instance", "node") + "/" +
		labelOf(labels, "gpu", "GPU_I_ID", "device", "UUID")
}

func labelOf(labels map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := labels[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// podView normalizes one pod, resolving provenance and ownership up front so no
// check ever has to.
func (g *Gatherer) podView(ctx context.Context, r *Resolver, p *kube.Pod, nsByName map[string]*kube.ObjectMeta, pdbs []kube.PodDisruptionBudget, pdbsReadable bool, draByPod map[string]int, now time.Time) inventory.PodView {
	gpus, _ := p.GPURequest()
	// Under DRA the extended resource is absent entirely, so a pod holding four
	// L40S through a ResourceClaim requests literally nothing countable. Adding
	// the claimed devices here is what stops such a pod reading as "holds no
	// accelerators" — which would in turn make its fully occupied node look
	// empty and get it recommended for deletion.
	if n := draByPod[p.Metadata.UID]; n > 0 {
		gpus += n
	}
	// The same trap in a second shape. Under the MIG mixed strategy a pod asks
	// for nvidia.com/mig-1g.5gb, never nvidia.com/gpu, so it holds no whole
	// device — and a whole-device count reads a MIG pool running at capacity as
	// completely empty.
	slices, sliceRes := p.SliceRequest()
	reason, term := p.WedgedReason()

	prov := r.Resolve(ctx, p)
	var ctrl *kube.Controller
	if prov.Controlled {
		ctrl, _ = r.get(ctx, prov.APIVersion, prov.RootKind, p.Metadata.Namespace, prov.RootName)
	}

	view := inventory.PodView{
		Ref:          inventory.PodRef{Namespace: p.Metadata.Namespace, Name: p.Metadata.Name, UID: p.Metadata.UID},
		Node:         p.Spec.NodeName,
		Phase:        p.Status.Phase,
		Accelerators: gpus,
		Slices:       slices,
		SliceRes:     sliceRes,
		Restarts:     p.RestartCount(),
		WedgedReason: reason,
		Initialising: initialising(p),
		// Not scheduled -- which is not the same as phase Pending.
		//
		// A pod bound to a node and stuck in ImagePullBackOff is phase
		// Pending, and the scheduler has already committed that node's
		// accelerator to it: nothing else can be placed there. Treating it as
		// pending made the node look empty to unused-node and made the pod
		// invisible to stuck-pod, so a node wedged on a bad image tag was
		// reported as reclaimable -- and the pod holding it, which is exactly
		// the waste this tool exists to find, was reported as nothing at all.
		//
		// A pod with no nodeName genuinely holds nothing. That, and only that,
		// is what this means.
		Pending:    p.Spec.NodeName == "",
		Provenance: prov,
		Owner:      AttributeOwner(p, ctrl, nsByName[p.Metadata.Namespace]),
		Labels:     p.Metadata.Labels,
	}
	if p.Status.StartTime != nil {
		view.StartTime = p.Status.StartTime
	}
	if term != nil {
		view.Terminated = &inventory.Termination{
			Reason: term.Reason, ExitCode: term.ExitCode, FinishedAt: term.FinishedAt,
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" && c.Reason == "Unschedulable" {
			view.Unschedulable = true
		}
	}
	view.IsDaemonSet = prov.RootKind == "DaemonSet"
	view.Evictable, view.BlockReason = evictability(p, prov, pdbs, pdbsReadable)
	return view
}

// evictability decides whether a pod would block a node drain, and says why.
//
// This is the substance of the unused-node check: an empty node is visible in
// any dashboard, but the reason the autoscaler cannot reclaim it is not visible
// anywhere.
func evictability(p *kube.Pod, prov api.Provenance, pdbs []kube.PodDisruptionBudget, pdbsReadable bool) (bool, string) {
	// Infrastructure DaemonSets are supposed to be there. dcgm-exporter, the
	// device plugin, CNI and CSI pods on an empty node are normal, and the
	// autoscaler ignores them, so calling them blockers would be a false alarm
	// on every node in the cluster.
	if prov.RootKind == "DaemonSet" {
		return true, ""
	}
	if strings.EqualFold(p.Metadata.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"], "false") {
		return false, "annotated cluster-autoscaler.kubernetes.io/safe-to-evict: false"
	}
	// Karpenter's equivalent. A pod carrying it pins the node under it, which
	// is exactly the kind of blocker worth naming.
	if kube.DoNotDisrupt(p.Metadata) {
		return false, "annotated karpenter.sh/do-not-disrupt: true"
	}
	if _, ok := p.Metadata.Annotations["kubernetes.io/config.mirror"]; ok {
		return false, "static pod, cannot be evicted"
	}
	if !prov.Controlled {
		// The autoscaler will not evict a pod no controller would recreate.
		return false, "not managed by a controller, so the autoscaler will not evict it"
	}
	// Said after the checks above, not before: a DaemonSet pod or an annotated
	// one has a definite answer that no budget changes, and reporting "we could
	// not check" for every dcgm-exporter in the cluster would bury the cases
	// where the doubt is real.
	if !pdbsReadable {
		return false, "PodDisruptionBudgets could not be read, so whether this pod can be evicted is unknown"
	}
	for _, pdb := range pdbs {
		if pdb.Metadata.Namespace != p.Metadata.Namespace || pdb.Status.DisruptionsAllowed != 0 {
			continue
		}
		matched, certain := pdb.Spec.Selector.Matches(p.Metadata.Labels)
		if matched && certain {
			return false, "PodDisruptionBudget " + pdb.Metadata.Name + " allows no disruptions"
		}
		if matched && !certain {
			return false, "PodDisruptionBudget " + pdb.Metadata.Name +
				" may apply (its selector uses an operator ullage does not recognise)"
		}
	}
	return true, ""
}

func initialising(p *kube.Pod) bool {
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.State.Running != nil || (cs.State.Waiting != nil && !cs.Ready) {
			return true
		}
	}
	return false
}

// draDevicesByNode counts DRA-allocated devices per node.
//
// Claims carry the device pool and the reserving pod carries the node, so the
// two together give a per-node count without interpreting the DRA API in depth.
// This is what stops a DRA cluster looking like a cluster with no accelerators
// at all — the worst possible failure, because it reports a healthy cluster the
// tool never examined.
// acceleratorDrivers are the DRA drivers known to hand out accelerators.
//
// DRA is not a GPU feature. The same ResourceClaim machinery allocates NICs,
// FPGAs and any other device a vendor writes a driver for, and every one of
// them appears in exactly the shape a GPU claim does. Counting results without
// reading the driver turns a pod holding two RDMA NICs into a pod holding two
// idle H100s, priced accordingly.
var acceleratorDrivers = map[string]bool{
	"gpu.nvidia.com":              true,
	"gpu.amd.com":                 true,
	"gpu.intel.com":               true,
	"tpu.google.com":              true,
	"gaudi.intel.com":             true,
	"accelerator.tenstorrent.com": true,
}

// isAcceleratorDriver reports whether a DRA driver hands out accelerators.
//
// The allowlist cannot be complete -- anyone may publish a driver -- so an
// unknown driver whose name identifies it as an accelerator is accepted, and
// anything else is not counted. Guessing wrong in the permissive direction
// invents hardware and bills for it; guessing wrong in the conservative
// direction under-reports, which the scan already says out loud when DRA
// devices go unread.
func isAcceleratorDriver(driver string) bool {
	d := strings.ToLower(strings.TrimSpace(driver))
	if d == "" {
		// Older allocations, and fakes, omit it. A claim that reached this far
		// was found on a node ullage already believes has accelerators.
		return true
	}
	if acceleratorDrivers[d] {
		return true
	}
	first, _, _ := strings.Cut(d, ".")
	switch first {
	case "gpu", "tpu", "npu", "accelerator", "gaudi":
		return true
	}
	return false
}

// deviceIdentity names the physical device an allocation result points at, so
// the same device claimed twice is counted once.
func deviceIdentity(driver, pool, device string) string {
	return driver + "/" + pool + "/" + device
}

// acceleratorsIn returns the distinct accelerators a claim allocates.
//
// A device partitioned between claims carries a shareID and appears in each of
// them; the identity deliberately excludes the shareID so those collapse to the
// one card they are cut from.
func acceleratorsIn(c *kube.ResourceClaim) []string {
	if c.Status.Allocation == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range c.Status.Allocation.Devices.Results {
		if !isAcceleratorDriver(r.Driver) {
			continue
		}
		id := deviceIdentity(r.Driver, r.Pool, r.Device)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// draDevicesByPod counts the accelerators each pod holds through a
// ResourceClaim.
//
// A claim may be reserved for several pods at once. The devices are shared
// between them, not duplicated for each, so billing the full count to every pod
// would report three times the hardware the cluster actually has -- and the
// census reconciliation that is supposed to catch exactly that kind of error
// would see the invented devices as real.
func draDevicesByPod(claims []kube.ResourceClaim) map[string]int {
	out := map[string]int{}
	for i := range claims {
		c := &claims[i]
		devices := acceleratorsIn(c)
		if len(devices) == 0 {
			continue
		}
		var holders []string
		for _, rf := range c.Status.ReservedFor {
			if rf.UID != "" {
				holders = append(holders, rf.UID)
			}
		}
		if len(holders) == 0 {
			continue
		}
		// The count is summed into the pod's accelerator request, so the totals
		// across pods have to add up to the devices that exist. One device
		// shared by three pods is one device: handing each of them a whole one
		// would report three, and a cluster that appears to own hardware it
		// never bought is the failure the census reconciliation exists to
		// catch. Devices are dealt out round-robin over a stable order, so the
		// remainder lands somewhere rather than being invented or dropped.
		sort.Strings(holders)
		for i := range devices {
			out[holders[i%len(holders)]]++
		}
	}
	return out
}

func draDevicesByNode(claims []kube.ResourceClaim, pods []kube.Pod) map[string]int {
	if len(claims) == 0 {
		return nil
	}
	nodeOfPod := map[string]string{}
	for i := range pods {
		nodeOfPod[pods[i].Metadata.UID] = pods[i].Spec.NodeName
	}
	out := map[string]int{}
	// A device shared between pods on the same node is one device on that node,
	// and the identity is what makes that true regardless of how many claims
	// mention it.
	seenByNode := map[string]map[string]bool{}
	for i := range claims {
		c := &claims[i]
		devices := acceleratorsIn(c)
		if len(devices) == 0 {
			continue
		}
		for _, rf := range c.Status.ReservedFor {
			if node := nodeOfPod[rf.UID]; node != "" {
				if seenByNode[node] == nil {
					seenByNode[node] = map[string]bool{}
				}
				for _, id := range devices {
					if seenByNode[node][id] {
						continue
					}
					seenByNode[node][id] = true
					out[node]++
				}
				break
			}
		}
	}
	return out
}

var _ = check.Params{}

// poolNames lists the distinct pool names in the cluster, sorted.
func poolNames(nodes []inventory.NodeView) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		if n.Pool == "" || seen[n.Pool] {
			continue
		}
		seen[n.Pool] = true
		out = append(out, n.Pool)
	}
	sort.Strings(out)
	return out
}
