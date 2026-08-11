package ullage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ullage-project/ullage/internal/humanize"
	"github.com/ullage-project/ullage/internal/kube"
	"github.com/ullage-project/ullage/internal/promql"
)

// DoctorCheck is one prerequisite and its verdict.
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail,omitempty"`
	Remedy string `json:"remedy,omitempty"`
}

// DoctorReport is the result of a prerequisite check.
type DoctorReport struct {
	Checks []DoctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// Doctor verifies that a scan could succeed, and says precisely what is missing
// when it could not.
//
// The failure mode this prevents is the one that kills adoption: a first run
// that returns an empty table, leaving the reader unable to tell whether their
// cluster is efficient or their setup is broken. Those two outcomes look
// identical and mean opposite things.
func Doctor(ctx context.Context, opts Options) (*DoctorReport, error) {
	r := &DoctorReport{OK: true}
	add := func(c DoctorCheck) {
		if c.Status == "fail" {
			r.OK = false
		}
		r.Checks = append(r.Checks, c)
	}

	kc, err := kube.New(kube.Config{
		Kubeconfig: opts.Kubeconfig,
		Context:    opts.Context,
		APIServer:  opts.APIServer,
		Insecure:   opts.Prometheus.Insecure,
	})
	if err != nil {
		add(DoctorCheck{
			Name: "kubernetes credentials", Status: "fail", Detail: err.Error(),
			Remedy: "set --kubeconfig, or run inside the cluster",
		})
		return r, nil
	}

	ver, err := kc.ServerVersion(ctx)
	if err != nil {
		add(DoctorCheck{
			Name: "kubernetes API reachable", Status: "fail", Detail: err.Error(),
			Remedy: "check that `kubectl get nodes` works with the same context",
		})
		return r, nil
	}
	add(DoctorCheck{Name: "kubernetes API reachable", Status: "ok",
		Detail: fmt.Sprintf("%s (%s)", ver, kc.Server())})

	nodes, err := kc.Nodes(ctx)
	if err != nil {
		add(DoctorCheck{Name: "list nodes", Status: "fail", Detail: err.Error(),
			Remedy: "ullage needs cluster-wide read on nodes, pods, namespaces and poddisruptionbudgets"})
		return r, nil
	}
	gpuNodes, devices := 0, 0
	models := map[string]int{}
	for _, n := range nodes {
		if q, ok := n.Status.Allocatable["nvidia.com/gpu"]; ok && q != "0" {
			gpuNodes++
			var c int
			fmt.Sscanf(q, "%d", &c)
			devices += c
			models[n.Metadata.Labels["nvidia.com/gpu.product"]] += c
		}
	}
	switch {
	case gpuNodes == 0:
		add(DoctorCheck{Name: "accelerator nodes", Status: "warn",
			Detail: fmt.Sprintf("no node advertises nvidia.com/gpu across %d nodes", len(nodes)),
			Remedy: "if this cluster uses DRA, that is expected — ullage reads ResourceSlices too"})
	default:
		add(DoctorCheck{Name: "accelerator nodes", Status: "ok",
			Detail: fmt.Sprintf("%d nodes, %d devices, %s", gpuNodes, devices, summarise(models))})
	}

	if _, err := kc.Pods(ctx); err != nil {
		add(DoctorCheck{Name: "list pods", Status: "fail", Detail: err.Error(),
			Remedy: "grant get/list on pods in all namespaces"})
	} else {
		add(DoctorCheck{Name: "list pods", Status: "ok"})
	}

	if _, err := kc.PodDisruptionBudgets(ctx); err != nil {
		add(DoctorCheck{Name: "list poddisruptionbudgets", Status: "warn", Detail: err.Error(),
			Remedy: "without this, scale-down blockers cannot be diagnosed and node findings drop to medium confidence"})
	} else {
		add(DoctorCheck{Name: "list poddisruptionbudgets", Status: "ok"})
	}

	// Either autoscaler is enough. Only a cluster where neither can be read has
	// a problem, because then a deliberately reserved pool and a wasted one
	// look identical.
	caStatus, caErr := kc.ClusterAutoscalerStatus(ctx)
	kp, kpErr := kc.KarpenterNodePools(ctx)
	switch {
	case caErr == nil && caStatus != nil:
		add(DoctorCheck{Name: "autoscaler", Status: "ok",
			Detail: fmt.Sprintf("cluster-autoscaler, %d node groups", len(caStatus.Groups))})
	case kpErr == nil && kp != nil:
		add(DoctorCheck{Name: "autoscaler", Status: "ok",
			Detail: fmt.Sprintf("Karpenter, %d NodePools", len(kp.NodePools))})
	default:
		add(DoctorCheck{Name: "autoscaler", Status: "warn",
			Detail: "no cluster-autoscaler-status ConfigMap in kube-system and no Karpenter NodePools",
			Remedy: "without one, a node pool held open deliberately is indistinguishable from waste, so ullage will say so rather than guess"})
	}

	if opts.Prometheus.URL == "" {
		add(DoctorCheck{Name: "metrics endpoint", Status: "fail",
			Detail: "no --prometheus given",
			Remedy: "pass --prometheus, or set ULLAGE_PROMETHEUS"})
		return r, nil
	}

	pc := promql.New(promql.Config{
		URL: opts.Prometheus.URL, Auth: opts.Prometheus.Auth,
		Token: opts.Prometheus.Token, Username: opts.Prometheus.Username,
		Password: opts.Prometheus.Password, Headers: opts.Prometheus.Headers,
		Timeout: opts.Prometheus.Timeout, Insecure: opts.Prometheus.Insecure,
	})
	if err := pc.Ping(ctx); err != nil {
		add(DoctorCheck{Name: "metrics endpoint reachable", Status: "fail", Detail: err.Error(),
			Remedy: "check the URL and credentials; the path should not include /api/v1"})
		return r, nil
	}
	add(DoctorCheck{Name: "metrics endpoint reachable", Status: "ok", Detail: pc.URL()})

	now := time.Now()
	util, err := pc.Query(ctx, "DCGM_FI_DEV_GPU_UTIL", now)
	switch {
	case err != nil:
		add(DoctorCheck{Name: "DCGM_FI_DEV_GPU_UTIL", Status: "fail", Detail: err.Error()})
	case len(util) == 0:
		add(DoctorCheck{Name: "DCGM_FI_DEV_GPU_UTIL", Status: "fail",
			Detail: "the metric exists nowhere in this endpoint",
			Remedy: "install dcgm-exporter (it ships with the NVIDIA GPU Operator) and confirm Prometheus scrapes it"})
	default:
		add(DoctorCheck{Name: "DCGM_FI_DEV_GPU_UTIL", Status: "ok",
			Detail: fmt.Sprintf("%d series", len(util))})
	}

	schema, err := pc.DetectLabelSchema(ctx, "DCGM_FI_DEV_GPU_UTIL", now)
	switch {
	case err != nil || schema.Pod == "":
		add(DoctorCheck{Name: "pod attribution labels", Status: "warn",
			Detail: "dcgm-exporter series carry no pod label",
			Remedy: "set DCGM_EXPORTER_KUBERNETES=true so per-pod claims can be made; without it only node-level findings are possible"})
	default:
		add(DoctorCheck{Name: "pod attribution labels", Status: "ok",
			Detail: fmt.Sprintf("using %s / %s", schema.Pod, schema.Namespace)})
	}

	// A fourteen-day window over a seven-day retention silently analyses seven
	// days and reports the wrong denominator, so retention is checked directly.
	window := opts.Window
	if window == 0 {
		window = 336 * time.Hour
	}
	old, err := pc.Query(ctx, "DCGM_FI_DEV_GPU_UTIL", now.Add(-window+time.Hour))
	switch {
	case err != nil:
		add(DoctorCheck{Name: "retention covers the window", Status: "warn", Detail: err.Error()})
	case len(old) == 0:
		add(DoctorCheck{Name: "retention covers the window", Status: "warn",
			Detail: fmt.Sprintf("no samples %s ago", humanize.Duration(window)),
			Remedy: "shorten --window, or raise Prometheus retention; ullage will report the window it actually had"})
	default:
		add(DoctorCheck{Name: "retention covers the window", Status: "ok",
			Detail: fmt.Sprintf("samples present %s ago", humanize.Duration(window))})
	}

	prof, err := pc.Query(ctx, "DCGM_FI_PROF_SM_ACTIVE", now)
	if err == nil && len(prof) > 0 {
		add(DoctorCheck{Name: "profiling metrics", Status: "ok",
			Detail: "DCGM_FI_PROF_SM_ACTIVE present — findings will distinguish idle from underused"})
	} else {
		add(DoctorCheck{Name: "profiling metrics", Status: "warn",
			Detail: "DCGM_FI_PROF_SM_ACTIVE absent",
			Remedy: "optional; without it ullage reports only unambiguous idleness, never low efficiency"})
	}

	return r, nil
}

func summarise(models map[string]int) string {
	if len(models) == 0 {
		return ""
	}
	parts := make([]string, 0, len(models))
	for m, c := range models {
		if m == "" {
			m = "unknown"
		}
		parts = append(parts, fmt.Sprintf("%d×%s", c, m))
	}
	return strings.Join(parts, ", ")
}
