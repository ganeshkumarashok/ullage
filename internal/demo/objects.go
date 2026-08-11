package demo

import (
	"fmt"
	"strings"
	"time"
)

// Kubernetes object rendering.
//
// These produce the same JSON shapes a real API server does, because the client
// under test parses them with its production decoder. Anything the fixture gets
// wrong shows up as a decode failure rather than as a passing test.

func (c *Cluster) nodeItems() []map[string]any {
	items := make([]map[string]any, 0, len(c.nodes))
	for _, n := range c.nodes {
		allocatable := map[string]string{"cpu": "96", "memory": "900Gi"}
		capacity := map[string]string{"cpu": "96", "memory": "900Gi"}
		if n.gpus > 0 {
			allocatable["nvidia.com/gpu"] = fmt.Sprintf("%d", n.gpus)
			capacity["nvidia.com/gpu"] = fmt.Sprintf("%d", n.gpus)
		}
		conditions := []map[string]any{{
			"type": "Ready", "status": boolStatus(n.ready),
			"lastTransitionTime": n.created.Format(time.RFC3339),
		}}
		items = append(items, map[string]any{
			"metadata": map[string]any{
				"name":              n.name,
				"labels":            n.labels,
				"annotations":       n.annotations,
				"creationTimestamp": n.created.Format(time.RFC3339),
			},
			"spec": map[string]any{"unschedulable": n.unsched},
			"status": map[string]any{
				"allocatable": allocatable,
				"capacity":    capacity,
				"conditions":  conditions,
			},
		})
	}
	return items
}

func boolStatus(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func (c *Cluster) podItems() []map[string]any {
	items := make([]map[string]any, 0, len(c.pods))
	for _, p := range c.pods {
		containers := []map[string]any{{
			"name":  "main",
			"image": "example/workload:latest",
		}}
		if p.gpus > 0 {
			containers[0]["resources"] = map[string]any{
				"limits": map[string]string{
					"nvidia.com/gpu": fmt.Sprintf("%d", p.gpus),
				},
			}
		}
		if p.migSlices > 0 {
			containers[0]["resources"] = map[string]any{
				"limits": map[string]string{
					"nvidia.com/mig-" + p.migProfile: fmt.Sprintf("%d", p.migSlices),
				},
			}
		}

		status := map[string]any{"phase": p.phase}
		if !p.started.IsZero() {
			status["startTime"] = p.started.Format(time.RFC3339)
		}

		cs := map[string]any{
			"name":         "main",
			"restartCount": p.restarts,
			"ready":        p.waiting == "",
		}
		switch {
		case p.waiting != "":
			cs["state"] = map[string]any{
				"waiting": map[string]any{
					"reason":  p.waiting,
					"message": "back-off 5m0s restarting failed container",
				},
			}
		case p.phase == "Running":
			cs["state"] = map[string]any{
				"running": map[string]any{"startedAt": p.started.Format(time.RFC3339)},
			}
		default:
			cs["state"] = map[string]any{"waiting": map[string]any{"reason": "PodScheduled"}}
		}
		if p.terminated != nil {
			cs["lastState"] = map[string]any{
				"terminated": map[string]any{
					"reason":     p.terminated.reason,
					"exitCode":   p.terminated.exitCode,
					"finishedAt": p.terminated.finished.Format(time.RFC3339),
				},
			}
		}
		status["containerStatuses"] = []map[string]any{cs}

		if p.phase == "Pending" {
			status["conditions"] = []map[string]any{{
				"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
				"message": "0/14 nodes are available: 14 Insufficient nvidia.com/gpu",
			}}
		}

		meta := map[string]any{
			"name":              p.name,
			"namespace":         p.namespace,
			"uid":               p.uid,
			"labels":            p.labels,
			"annotations":       p.annotations,
			"creationTimestamp": p.started.Format(time.RFC3339),
		}
		if p.owner != nil {
			meta["ownerReferences"] = []map[string]any{{
				"kind":       p.owner.kind,
				"name":       p.owner.name,
				"apiVersion": p.owner.apiVersion,
				"controller": true,
				"uid":        "uid-" + p.owner.name,
			}}
		}

		items = append(items, map[string]any{
			"metadata": meta,
			"spec": map[string]any{
				"nodeName":   p.node,
				"containers": containers,
			},
			"status": status,
		})
	}
	return items
}

func (c *Cluster) namespaceItems() []map[string]any {
	seen := map[string]bool{}
	var items []map[string]any
	// Namespace-level ownership is the last resort in the attribution chain,
	// and it is what rescues the many workloads nobody labelled.
	owners := map[string]string{
		"research":    "research-platform@example.com",
		"training":    "training-team@example.com",
		"serving":     "serving-team@example.com",
		"ml-platform": "ml-platform@example.com",
		"inference":   "inference-team@example.com",
	}
	for _, p := range c.pods {
		if seen[p.namespace] {
			continue
		}
		seen[p.namespace] = true
		labels := map[string]string{}
		if o, ok := owners[p.namespace]; ok {
			labels["owner"] = o
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": p.namespace, "labels": labels},
		})
	}
	return items
}

func (c *Cluster) pdbItems() []map[string]any {
	return []map[string]any{}
}

// autoscalerConfigMap renders the status the cluster autoscaler publishes.
//
// This ConfigMap is the only place a node group's minimum size is visible from
// inside the cluster, and that number is what separates deliberately reserved
// warm capacity from waste.
func (c *Cluster) autoscalerConfigMap() map[string]any {
	var b strings.Builder
	b.WriteString("time: " + c.Now.Format(time.RFC3339) + "\n")
	b.WriteString("autoscalerStatus: Running\n")
	b.WriteString("nodeGroups:\n")

	pools := map[string]int{}
	for _, n := range c.nodes {
		pools[n.pool]++
	}
	names := make([]string, 0, len(pools))
	for p := range pools {
		names = append(names, p)
	}
	sortStrings(names)

	for _, pool := range names {
		min := c.floors[pool]
		fmt.Fprintf(&b, "  - name: aks-%s-41234567-vmss\n", pool)
		b.WriteString("    health:\n      status: Healthy\n")
		fmt.Fprintf(&b, "      nodeCounts:\n        registered:\n          total: %d\n          ready: %d\n",
			pools[pool], pools[pool])
		fmt.Fprintf(&b, "      minSize: %d\n      maxSize: 20\n", min)
	}

	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "cluster-autoscaler-status",
			"namespace": "kube-system",
		},
		"data": map[string]string{"status": b.String()},
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// claimItems renders DRA ResourceClaims.
//
// Under DRA a pod's spec carries no extended resource at all, so the claim is
// the only record that the pod holds any device. A tool that reads only
// nvidia.com/gpu sees an empty node here and recommends deleting it.
func (c *Cluster) claimItems() []map[string]any {
	var items []map[string]any
	for _, p := range c.pods {
		if p.draDevices == 0 {
			continue
		}
		results := make([]map[string]any, 0, p.draDevices)
		for i := 0; i < p.draDevices; i++ {
			results = append(results, map[string]any{
				"device": fmt.Sprintf("gpu-%d", i),
				"driver": "gpu.nvidia.com",
				"pool":   p.node,
			})
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{
				"name":      p.name + "-gpus",
				"namespace": p.namespace,
			},
			"status": map[string]any{
				"allocation": map[string]any{
					"devices": map[string]any{"results": results},
				},
				"reservedFor": []map[string]any{{
					"resource": "pods", "name": p.name, "uid": p.uid,
				}},
			},
		})
	}
	return items
}
