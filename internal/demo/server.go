// Package demo runs the real ullage pipeline against a synthetic cluster.
//
// The servers below are httptest servers: they speak real HTTP and serve real
// Kubernetes and Prometheus response shapes, but they are not a kube-apiserver
// and not a Prometheus. What is real is everything downstream of them — the
// same client, the same queries, the same checks, the same renderer — so the
// demo exercises the production path rather than illustrating what it would
// print. `ullage demo --serve` exposes these endpoints so anyone can point the
// real CLI at them and confirm nothing between the wire and the output is
// staged.
package demo

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"
)

// Cluster is a synthetic cluster: a set of node pools, workloads and metric
// series designed so that each scenario exercises a different decision in the
// pipeline.
type Cluster struct {
	Now   time.Time
	nodes []node
	pods  []pod
	// series holds the utilization shapes over the window.
	//
	// A slice rather than a map keyed by "host/gpu", because one physical GPU
	// produces several series when it is handed to a second pod during the
	// window — which is what normal job churn looks like at 14d. A map keyed by
	// device made that unrepresentable, so the fixture stayed green on a bug
	// that fires on any real cluster with turnover.
	series []*seriesSpec
	floors map[string]int
}

type node struct {
	name        string
	pool        string
	model       string
	gpus        int
	labels      map[string]string
	annotations map[string]string
	created     time.Time
	ready       bool
	unsched     bool
}

type pod struct {
	namespace   string
	name        string
	node        string
	gpus        int
	phase       string
	started     time.Time
	labels      map[string]string
	annotations map[string]string
	owner       *ownerRef
	waiting     string
	terminated  *terminated
	restarts    int
	uid         string
	// draDevices is how many devices this pod holds through a ResourceClaim
	// rather than through the nvidia.com/gpu extended resource.
	draDevices int
	// migSlices is how many MIG profiles this pod holds. Under the mixed
	// strategy these appear as their own extended resource and never as
	// nvidia.com/gpu, so a pod holding one requests no whole device at all.
	migSlices  int
	migProfile string
}

type ownerRef struct {
	kind, name, apiVersion string
	// root, when set, is what this owner is itself owned by.
	root *ownerRef
}

type terminated struct {
	reason   string
	exitCode int
	finished time.Time
}

type seriesSpec struct {
	host  string
	gpu   string
	model string
	pod   string
	ns    string

	// busyUntil, busyFrom and duty describe the utilization shape.
	pattern func(t time.Time, now time.Time) float64
	// powerW is the mean board power the device reports.
	powerW float64
	// gapFrom/gapTo omit samples entirely, which is how a real scrape outage
	// looks: absent, not zero.
	gapFrom, gapTo time.Time
}

// Servers holds the running fakes.
type Servers struct {
	Kube       *httptest.Server
	Prometheus *httptest.Server
	Cluster    *Cluster
}

// Close shuts both servers down.
func (s *Servers) Close() {
	s.Kube.Close()
	s.Prometheus.Close()
}

// Start builds the scenario and serves it.
func Start(now time.Time) *Servers {
	c := Build(now)
	return &Servers{
		Kube:       httptest.NewServer(c.KubeHandler()),
		Prometheus: httptest.NewServer(c.PromHandler()),
		Cluster:    c,
	}
}

// KubeHandler serves the subset of the Kubernetes API that ullage reads.
func (c *Cluster) KubeHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"major": "1", "minor": "34", "gitVersion": "v1.34.1"})
	})
	mux.HandleFunc("/api/v1/pods", func(w http.ResponseWriter, r *http.Request) {
		writePage(w, r, "PodList", c.podItems())
	})
	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		writePage(w, r, "NodeList", c.nodeItems())
	})
	mux.HandleFunc("/api/v1/namespaces", func(w http.ResponseWriter, r *http.Request) {
		writePage(w, r, "NamespaceList", c.namespaceItems())
	})
	mux.HandleFunc("/api/v1/namespaces/kube-system/configmaps/cluster-autoscaler-status",
		func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, c.autoscalerConfigMap())
		})
	mux.HandleFunc("/apis/policy/v1/poddisruptionbudgets", func(w http.ResponseWriter, r *http.Request) {
		writePage(w, r, "PodDisruptionBudgetList", c.pdbItems())
	})

	// Discovery, so provenance resolution can walk into a CRD it has never
	// seen. This is what lets the tool name a Notebook owner correctly instead
	// of printing a command that would not work.
	mux.HandleFunc("/apis/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/apis/")
		parts := strings.Split(strings.Trim(path, "/"), "/")

		switch len(parts) {
		case 2: // discovery: /apis/{group}/{version}
			writeJSON(w, discoveryFor(parts[0], parts[1]))
			return
		case 3: // /apis/{group}/{version}/{resource} — cluster-scoped list
			if parts[0] == "resource.k8s.io" && parts[2] == "resourceclaims" {
				if parts[1] != "v1" {
					http.NotFound(w, r)
					return
				}
				writeJSON(w, map[string]any{"kind": "ResourceClaimList", "items": c.claimItems()})
				return
			}
			http.NotFound(w, r)
			return
		case 5: // /apis/{group}/{version}/namespaces/{ns}/{resource} — list
			http.NotFound(w, r)
			return
		case 6: // /apis/{group}/{version}/namespaces/{ns}/{resource}/{name}
			obj := c.controller(parts[5], parts[3])
			if obj == nil {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, obj)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return mux
}

func discoveryFor(group, version string) map[string]any {
	resources := []map[string]any{}
	add := func(kind, plural string) {
		resources = append(resources, map[string]any{
			"name": plural, "kind": kind, "namespaced": true,
		})
	}
	switch group {
	case "apps":
		add("Deployment", "deployments")
		add("StatefulSet", "statefulsets")
		add("ReplicaSet", "replicasets")
		add("DaemonSet", "daemonsets")
	case "batch":
		add("Job", "jobs")
		add("CronJob", "cronjobs")
	case "kubeflow.org":
		add("Notebook", "notebooks")
	}
	return map[string]any{
		"kind":         "APIResourceList",
		"groupVersion": group + "/" + version,
		"resources":    resources,
	}
}

// controller returns a controller object by plural resource name, so the
// provenance walk can find the root of an ownership chain.
func (c *Cluster) controller(name, namespace string) map[string]any {
	for _, p := range c.pods {
		if p.owner == nil || p.namespace != namespace {
			continue
		}
		// The walk asks for each level in turn, so a root above the pod's
		// direct owner has to be answerable too — otherwise the chain stops at
		// the ReplicaSet and the fix targets an object its Deployment will
		// immediately recreate.
		if p.owner.root != nil && p.owner.root.name == name {
			return map[string]any{
				"apiVersion": p.owner.root.apiVersion,
				"kind":       p.owner.root.kind,
				"metadata": map[string]any{
					"name": name, "namespace": namespace,
					"labels": map[string]string{"owner": "serving-team@example.com"},
				},
				"spec": map[string]any{"replicas": 1},
			}
		}
		if p.owner.name != name {
			continue
		}
		meta := map[string]any{
			"name":      p.owner.name,
			"namespace": namespace,
			"labels":    map[string]string{},
		}
		if p.owner.root != nil {
			meta["ownerReferences"] = []map[string]any{{
				"kind":       p.owner.root.kind,
				"name":       p.owner.root.name,
				"apiVersion": p.owner.root.apiVersion,
				"controller": true,
			}}
		}
		// Owner labels live on the controller for the Deployment scenario,
		// which exercises attribution falling through from pod to workload.
		if p.owner.kind == "Deployment" || p.owner.root != nil {
			meta["labels"] = map[string]string{"owner": "serving-team"}
		}
		return map[string]any{
			"apiVersion": p.owner.apiVersion,
			"kind":       p.owner.kind,
			"metadata":   meta,
		}
	}
	return nil
}

// writePage serves a list the way a real API server does: honouring ?limit and
// ?continue, and truncating with a continue token when there is more.
//
// The demo deliberately pages at a size far below the client's, so every demo
// run exercises the paging path. A client that ignored the continue token would
// silently report a cluster missing most of its pods, which is the failure mode
// most likely to produce a confidently wrong recommendation — and the one least
// likely to be noticed, since the output still looks perfectly plausible.
func writePage(w http.ResponseWriter, r *http.Request, kind string, items []map[string]any) {
	const demoPage = 3

	from := 0
	if tok := r.URL.Query().Get("continue"); tok != "" {
		if n, err := strconv.Atoi(tok); err == nil {
			from = n
		}
	}
	if from > len(items) {
		from = len(items)
	}
	to := from + demoPage
	if to > len(items) {
		to = len(items)
	}
	out := map[string]any{"kind": kind, "items": items[from:to]}
	if to < len(items) {
		out["metadata"] = map[string]any{"continue": strconv.Itoa(to)}
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// PromHandler serves the Prometheus HTTP API.
//
// It evaluates the aggregate functions ullage actually issues rather than
// returning canned answers, so the client's query construction is genuinely
// exercised: a wrong range selector or a wrong metric name produces no data
// here exactly as it would against a real Prometheus.
func (c *Cluster) PromHandler() http.Handler {
	mux := http.NewServeMux()

	handle := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		query := r.Form.Get("query")

		if strings.Contains(r.URL.Path, "query_range") {
			start := parseTime(r.Form.Get("start"))
			end := parseTime(r.Form.Get("end"))
			step, _ := strconv.Atoi(strings.TrimSuffix(r.Form.Get("step"), "s"))
			if step == 0 {
				step = 3600
			}
			writeJSON(w, c.rangeResult(query, start, end, time.Duration(step)*time.Second))
			return
		}
		writeJSON(w, c.instantResult(query, parseTime(r.Form.Get("time"))))
	}
	mux.HandleFunc("/api/v1/query", handle)
	mux.HandleFunc("/api/v1/query_range", handle)
	return mux
}

func parseTime(s string) time.Time {
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Now().UTC()
	}
	return time.Unix(int64(n), 0).UTC()
}

// instantResult evaluates max_over_time / avg_over_time / count_over_time.
func (c *Cluster) instantResult(query string, at time.Time) map[string]any {
	metric, fn, window := parseAggregate(query)
	if metric == "" {
		return promResponse("vector", nil)
	}
	if metric == "DCGM_FI_PROF_SM_ACTIVE" {
		// Profiling metrics are deliberately absent: the demo cluster is a
		// tier-1 cluster, which is the common case.
		return promResponse("vector", nil)
	}

	var results []map[string]any
	for _, s := range c.sortedSeries() {
		if metric == "DCGM_FI_DEV_POWER_USAGE" && s.powerW == 0 {
			continue
		}
		start := at.Add(-window)
		var vals []float64
		for t := start; !t.After(at); t = t.Add(30 * time.Second) {
			if s.absent(t) {
				continue
			}
			if metric == "DCGM_FI_DEV_POWER_USAGE" {
				vals = append(vals, s.power(t, c.Now))
			} else {
				vals = append(vals, s.pattern(t, c.Now))
			}
		}
		if len(vals) == 0 {
			continue
		}
		v := aggregate(fn, vals)
		results = append(results, map[string]any{
			"metric": s.labels(metric),
			"value":  []any{float64(at.Unix()), fmt.Sprintf("%g", v)},
		})
	}
	return promResponse("vector", results)
}

func (c *Cluster) rangeResult(query string, start, end time.Time, step time.Duration) map[string]any {
	metric, _, _ := parseAggregate(query)
	if metric == "" {
		metric = strings.TrimSpace(query)
	}
	var results []map[string]any
	for _, s := range c.sortedSeries() {
		if metric == "DCGM_FI_DEV_POWER_USAGE" && s.powerW == 0 {
			continue
		}
		var values []any
		for t := start; !t.After(end); t = t.Add(step) {
			if s.absent(t) {
				continue
			}
			v := s.pattern(t, c.Now)
			if metric == "DCGM_FI_DEV_POWER_USAGE" {
				v = s.power(t, c.Now)
			}
			values = append(values, []any{float64(t.Unix()), fmt.Sprintf("%g", v)})
		}
		if len(values) == 0 {
			continue
		}
		results = append(results, map[string]any{
			"metric": s.labels(metric),
			"values": values,
		})
	}
	return promResponse("matrix", results)
}

func promResponse(kind string, result []map[string]any) map[string]any {
	if result == nil {
		result = []map[string]any{}
	}
	return map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": kind, "result": result},
	}
}

// parseAggregate pulls the metric, function and range out of a query such as
// max_over_time(DCGM_FI_DEV_GPU_UTIL[14d]).
func parseAggregate(q string) (metric, fn string, window time.Duration) {
	q = strings.TrimSpace(q)
	open := strings.Index(q, "(")
	if open < 0 {
		return strings.Trim(q, "{} "), "last", 0
	}
	fn = q[:open]
	inner := strings.TrimSuffix(q[open+1:], ")")
	lb := strings.Index(inner, "[")
	if lb < 0 {
		return strings.TrimSpace(inner), fn, 0
	}
	metric = strings.TrimSpace(inner[:lb])
	rng := strings.Trim(inner[lb+1:], "]")
	window = parseRange(rng)
	return metric, fn, window
}

func parseRange(s string) time.Duration {
	if s == "" {
		return 0
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil {
		return 0
	}
	switch unit {
	case 'd':
		return time.Duration(n) * 24 * time.Hour
	case 'h':
		return time.Duration(n) * time.Hour
	case 'm':
		return time.Duration(n) * time.Minute
	}
	return 0
}

func aggregate(fn string, vals []float64) float64 {
	switch fn {
	case "max_over_time":
		m := math.Inf(-1)
		for _, v := range vals {
			if v > m {
				m = v
			}
		}
		return m
	case "avg_over_time":
		sum := 0.0
		for _, v := range vals {
			sum += v
		}
		return sum / float64(len(vals))
	case "count_over_time":
		return float64(len(vals))
	default:
		return vals[len(vals)-1]
	}
}

func (s *seriesSpec) absent(t time.Time) bool {
	return !s.gapFrom.IsZero() && !t.Before(s.gapFrom) && t.Before(s.gapTo)
}

func (s *seriesSpec) power(t, now time.Time) float64 {
	util := s.pattern(t, now)
	if util == 0 {
		// An idle device still draws standby power — around a seventh of board
		// TDP. Reporting zero would make the corroboration trivially true and
		// therefore worthless.
		return s.powerW * 0.14
	}
	return s.powerW * (0.35 + 0.6*util/100)
}

func (s *seriesSpec) labels(metric string) map[string]string {
	l := map[string]string{
		"__name__":  metric,
		"Hostname":  s.host,
		"gpu":       s.gpu,
		"modelName": s.model,
		"UUID":      "GPU-" + s.host + "-" + s.gpu,
	}
	if s.pod != "" {
		// Deliberately the exported_ schema: kube-prometheus-stack's default
		// relabelling renames DCGM's own pod labels, and a tool that only
		// understands the plain schema silently attributes nothing on the most
		// common Prometheus install in existence.
		l["exported_pod"] = s.pod
		l["exported_namespace"] = s.ns
		l["pod"] = "dcgm-exporter-" + s.host
		l["namespace"] = "gpu-operator"
	}
	return l
}
