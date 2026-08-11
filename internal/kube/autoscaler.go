package kube

import (
	"context"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// AutoscalerStatus is the cluster-autoscaler's published view of each node
// group, read from the cluster-autoscaler-status ConfigMap in kube-system.
//
// This is the only place a node group's *minimum size* is visible from inside
// the cluster, and it is the difference between two very different findings:
// "these nodes are idle and the autoscaler cannot remove them" and "these nodes
// are idle because someone deliberately pinned a floor". Recommending
// --min-count 0 against a deliberately reserved pool is the fastest way for a
// tool to be classified as not understanding the business.
type AutoscalerStatus struct {
	Groups map[string]NodeGroupStatus
}

type NodeGroupStatus struct {
	Name    string
	MinSize int
	MaxSize int
	Ready   int
}

type configMap struct {
	Data map[string]string `json:"data"`
}

// caStatusDoc is the structured status published by cluster-autoscaler 1.30+.
type caStatusDoc struct {
	NodeGroups []struct {
		Name    string `yaml:"name"`
		MinSize int    `yaml:"minSize"`
		MaxSize int    `yaml:"maxSize"`
		Health  struct {
			NodeCounts struct {
				Registered struct {
					Total int `yaml:"total"`
					Ready int `yaml:"ready"`
				} `yaml:"registered"`
			} `yaml:"nodeCounts"`
			MinSize int `yaml:"minSize"`
			MaxSize int `yaml:"maxSize"`
		} `yaml:"health"`
	} `yaml:"nodeGroups"`
}

// ClusterAutoscalerStatus reads and parses the autoscaler status ConfigMap.
// Its absence is normal — many clusters do not run the autoscaler — so a
// missing ConfigMap returns nil without an error.
func (c *Client) ClusterAutoscalerStatus(ctx context.Context) (*AutoscalerStatus, error) {
	var cm configMap
	err := c.get(ctx, "/api/v1/namespaces/kube-system/configmaps/cluster-autoscaler-status", &cm)
	if err != nil {
		if _, ok := err.(*NotFound); ok {
			return nil, nil
		}
		if _, ok := err.(*Forbidden); ok {
			return nil, nil
		}
		return nil, err
	}

	out := &AutoscalerStatus{Groups: map[string]NodeGroupStatus{}}
	for _, key := range []string{"status", "clusterAutoscalerStatus"} {
		raw, ok := cm.Data[key]
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		var doc caStatusDoc
		if err := yaml.Unmarshal([]byte(raw), &doc); err == nil && len(doc.NodeGroups) > 0 {
			for _, g := range doc.NodeGroups {
				min, max := g.MinSize, g.MaxSize
				if min == 0 {
					min = g.Health.MinSize
				}
				if max == 0 {
					max = g.Health.MaxSize
				}
				out.Groups[g.Name] = NodeGroupStatus{
					Name:    g.Name,
					MinSize: min,
					MaxSize: max,
					Ready:   g.Health.NodeCounts.Registered.Ready,
				}
			}
			return out, nil
		}
		// Older autoscalers publish a free-text blob. Parse it leniently rather
		// than not at all: the minimum size is worth having either way.
		parseLegacyCAStatus(raw, out)
	}
	if len(out.Groups) == 0 {
		return nil, nil
	}
	return out, nil
}

// parseLegacyCAStatus handles lines of the shape:
//
//	Name:        aks-h100reserve-1234-vmss
//	Health:      Healthy (ready=2 ... minSize=2 maxSize=8)
func parseLegacyCAStatus(raw string, out *AutoscalerStatus) {
	var current string
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Name:") {
			current = strings.TrimSpace(strings.TrimPrefix(t, "Name:"))
			continue
		}
		if current == "" || !strings.Contains(t, "minSize=") {
			continue
		}
		g := NodeGroupStatus{Name: current}
		for _, field := range strings.FieldsFunc(t, func(r rune) bool {
			return r == ' ' || r == '(' || r == ')' || r == ','
		}) {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				continue
			}
			switch k {
			case "minSize":
				g.MinSize = n
			case "maxSize":
				g.MaxSize = n
			case "ready":
				g.Ready = n
			}
		}
		out.Groups[current] = g
		current = ""
	}
}
