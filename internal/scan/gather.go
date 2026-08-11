package scan

import (
	"context"
	"fmt"
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

	claims, _ := g.Kube.ResourceClaims(ctx)
	draByPod := draDevicesByPod(claims)
	draByNode := draDevicesByNode(claims, pods)

	opts.Progress("Classifying accelerator allocation")
	inv := inventory.Build(nodes, draByNode)
	if inv.Observed == 0 {
		return nil, inv, append(warnings, "no accelerators were found on any node"), nil
	}

	opts.Progress(fmt.Sprintf("Querying GPU metrics over %s", opts.Window.Round(time.Hour)))
	schema, err := g.Prom.DetectLabelSchema(ctx, MetricGPUUtil, end)
	if err != nil {
		return nil, inv, warnings, err
	}
	if !schema.Found {
		warnings = append(warnings,
			"GPU metrics carry no pod or namespace labels under either the `pod` or `exported_pod` "+
				"schema, so per-pod checks cannot run; node-level checks still apply")
	}

	devices, powerless, err := g.devices(ctx, schema, inv, start, end, opts.Step)
	if err != nil {
		return nil, inv, warnings, err
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
		Devices:           devices,
		MetricsAttributed: schema.Found,
	}

	// Autoscaler status is the only place a node group's minimum size is
	// visible from inside the cluster. nil means unknown, and unknown is not
	// permission to recommend removing capacity.
	if status, err := g.Kube.ClusterAutoscalerStatus(ctx); err == nil && status != nil {
		floors := map[string]int{}
		for _, grp := range status.Groups {
			floors[grp.Name] = grp.MinSize
		}
		cl.Autoscaler = &inventory.AutoscalerView{Floors: floors}
	} else {
		warnings = append(warnings,
			"the cluster-autoscaler status ConfigMap was not readable, so deliberate minimum pool "+
				"sizes could not be distinguished from unused capacity")
	}

	pdbs, _ := g.Kube.PodDisruptionBudgets(ctx)

	opts.Progress("Resolving workload owners")
	resolver := NewResolver(g.Kube)
	for i := range pods {
		cl.Pods = append(cl.Pods, g.podView(ctx, resolver, &pods[i], nsByName, pdbs, draByPod, end))
	}
	for name, ni := range inv.Nodes {
		cl.Nodes = append(cl.Nodes, inventory.NodeView{
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
			ScaleDownDisabled: strings.EqualFold(ni.Node.Metadata.Annotations["cluster-autoscaler.kubernetes.io/scale-down-disabled"], "true"),
		})
	}
	sort.Slice(cl.Nodes, func(i, j int) bool { return cl.Nodes[i].Name < cl.Nodes[j].Name })

	// Attribution is counted after the join, not assumed from the census: a
	// device whose metrics exist but carry no pod is not an analysed device.
	analyzed := 0
	for _, d := range cl.Devices {
		if d.Analyzable {
			analyzed++
		}
	}
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

// devices runs the metric queries and joins them into device facts.
//
// The queries are aggregate push-downs rather than raw sample streams. The
// question "was this ever non-zero in the window" is an aggregate, and asking
// Prometheus for a fortnight of raw samples for every device to answer it in Go
// would move tens of millions of samples for a few hundred booleans — it hits
// the query-frontend sample limit long before it hits a real cluster's size.
// max_over_time answers it in one value per series.
func (g *Gatherer) devices(ctx context.Context, schema promql.LabelSchema, inv *inventory.Inventory, start, end time.Time, step time.Duration) ([]inventory.Device, bool, error) {
	window := end.Sub(start)

	maxUtil, err := g.instant(ctx, fmt.Sprintf("max_over_time(%s[%s])", MetricGPUUtil, promql.Range(window)), end)
	if err != nil {
		return nil, false, err
	}
	if len(maxUtil) == 0 {
		return nil, false, promql.ErrNoSeries
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
	avgPower, _ := g.instant(ctx, fmt.Sprintf("avg_over_time(%s[%s])", MetricPower, promql.Range(powerWindow)), end)
	samples, _ := g.instant(ctx, fmt.Sprintf("count_over_time(%s[%s])", MetricGPUUtil, promql.Range(window)), end)

	// The sparkline and "when did work last happen" need shape, not precision,
	// so they use one coarse range query at an hourly step: 336 points per
	// series over a fortnight rather than four thousand.
	shape, err := g.Prom.QueryRange(ctx, MetricGPUUtil, start, end, step)
	if err != nil {
		shape = nil
	}
	if g.Trace {
		g.queries = append(g.queries, fmt.Sprintf("%s @ range %s..%s step %s",
			MetricGPUUtil, start.Format(time.RFC3339), end.Format(time.RFC3339), step))
	}
	shapeBy := map[string]promql.Series{}
	for _, s := range shape {
		shapeBy[deviceKey(s.Labels)] = s
	}
	powerBy := map[string]float64{}
	for _, s := range avgPower {
		powerBy[deviceKey(s.Labels)] = s.Value
	}
	samplesBy := map[string]float64{}
	for _, s := range samples {
		samplesBy[deviceKey(s.Labels)] = s.Value
	}

	// Expected sample count, used to tell a scrape gap from a genuine zero. An
	// absent sample is not an idle sample, and conflating them is how a tool
	// confidently reports that a device nobody was watching did nothing.
	expected := window.Seconds() / 30
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

		d.Util = inventory.Stats{
			Max:            s.Value,
			ZeroThroughout: s.Value == 0,
			FallowSince:    start,
			Completeness:   clamp(samplesBy[key]/expected, 0, 1),
			Samples:        int(samplesBy[key]),
		}
		if series, ok := shapeBy[key]; ok {
			refine(&d.Util, series, start, end, step)
		}
		if watts, ok := powerBy[key]; ok {
			d.Power = inventory.Stats{Samples: 1, Mean: watts, Max: watts}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	return out, len(avgPower) == 0, nil
}

// refine fills in the parts of a device's statistics that need the shape of the
// series rather than a single aggregate.
func refine(st *inventory.Stats, s promql.Series, start, end time.Time, step time.Duration) {
	sum := promql.Summarise(s, start, end, step, 14)
	st.Buckets = sum.Buckets
	st.Mean = sum.Mean
	st.LastNonZero = sum.LastNonZero
	if sum.FallowSince.After(st.FallowSince) {
		st.FallowSince = sum.FallowSince
	}
	// The coarse series is a downsample, so it can only ever contradict a zero
	// by finding work the aggregate missed — never the other way round.
	if sum.Max > st.Max {
		st.Max = sum.Max
		st.ZeroThroughout = false
	}
}

func (g *Gatherer) instant(ctx context.Context, query string, at time.Time) ([]promql.VectorSample, error) {
	if g.Trace {
		g.queries = append(g.queries, query)
	}
	return g.Prom.QueryExpr(ctx, query, at)
}

// Queries returns the recorded query trace.
func (g *Gatherer) Queries() []string { return g.queries }

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
func (g *Gatherer) podView(ctx context.Context, r *Resolver, p *kube.Pod, nsByName map[string]*kube.ObjectMeta, pdbs []kube.PodDisruptionBudget, draByPod map[string]int, now time.Time) inventory.PodView {
	gpus, _ := p.GPURequest()
	// Under DRA the extended resource is absent entirely, so a pod holding four
	// L40S through a ResourceClaim requests literally nothing countable. Adding
	// the claimed devices here is what stops such a pod reading as "holds no
	// accelerators" — which would in turn make its fully occupied node look
	// empty and get it recommended for deletion.
	if n := draByPod[p.Metadata.UID]; n > 0 {
		gpus += n
	}
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
		Restarts:     p.RestartCount(),
		WedgedReason: reason,
		Initialising: initialising(p),
		Pending:      p.Status.Phase == "Pending" || p.Spec.NodeName == "",
		Provenance:   prov,
		Owner:        AttributeOwner(p, ctrl, nsByName[p.Metadata.Namespace]),
		Labels:       p.Metadata.Labels,
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
	view.Evictable, view.BlockReason = evictability(p, prov, pdbs)
	return view
}

// evictability decides whether a pod would block a node drain, and says why.
//
// This is the substance of the unused-node check: an empty node is visible in
// any dashboard, but the reason the autoscaler cannot reclaim it is not visible
// anywhere.
func evictability(p *kube.Pod, prov api.Provenance, pdbs []kube.PodDisruptionBudget) (bool, string) {
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
	if _, ok := p.Metadata.Annotations["kubernetes.io/config.mirror"]; ok {
		return false, "static pod, cannot be evicted"
	}
	if !prov.Controlled {
		// The autoscaler will not evict a pod no controller would recreate.
		return false, "not managed by a controller, so the autoscaler will not evict it"
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
				" may apply (its selector uses matchExpressions, which ullage does not evaluate)"
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
// draDevicesByPod counts the devices each pod holds through a ResourceClaim.
func draDevicesByPod(claims []kube.ResourceClaim) map[string]int {
	out := map[string]int{}
	for _, c := range claims {
		if c.Status.Allocation == nil {
			continue
		}
		n := len(c.Status.Allocation.Devices.Results)
		if n == 0 {
			continue
		}
		for _, rf := range c.Status.ReservedFor {
			if rf.UID != "" {
				out[rf.UID] += n
			}
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
	for _, c := range claims {
		if c.Status.Allocation == nil {
			continue
		}
		n := len(c.Status.Allocation.Devices.Results)
		if n == 0 {
			continue
		}
		for _, rf := range c.Status.ReservedFor {
			if node := nodeOfPod[rf.UID]; node != "" {
				out[node] += n
				break
			}
		}
	}
	return out
}

var _ = check.Params{}
