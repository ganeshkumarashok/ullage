package inventory

import (
	"strings"
	"testing"

	"github.com/ullage-project/ullage/internal/kube"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// node builds a GPU node with the given labels and allocatable resources.
func node(name string, labels map[string]string, alloc map[string]string) *kube.Node {
	n := &kube.Node{}
	n.Metadata.Name = name
	n.Metadata.Labels = labels
	n.Status.Allocatable = kube.ResourceList(alloc)
	n.Status.Capacity = kube.ResourceList(alloc)
	return n
}

// Under the "single" MIG strategy the device plugin advertises MIG instances as
// plain nvidia.com/gpu. An 8-card node partitioned seven ways therefore reports
// nvidia.com/gpu: 56, which is arithmetically identical to a node holding 56
// whole A100s. Classifying it as exclusive puts it into the analysis and prices
// every 1g.5gb slice at the rate of the card it was cut from: a seven-fold
// overstatement of both the hardware and the money.
func TestSingleStrategyMIGIsNotCountedAsWholeCards(t *testing.T) {
	n := node("mig-single", map[string]string{
		"nvidia.com/gpu.product":  "A100-SXM4-40GB-MIG-1g.5gb",
		"nvidia.com/gpu.count":    "56",
		"nvidia.com/mig.strategy": "single",
		"nvidia.com/mig.capable":  "true",
	}, map[string]string{"nvidia.com/gpu": "56"})

	inv := InventoryNode(n, 0)

	if inv.Allocation != api.AllocMIG {
		t.Fatalf("allocation = %q, want %q: the product label carries a MIG profile, so "+
			"every advertised unit is an instance. Calling it exclusive prices a 1g.5gb "+
			"slice as a whole A100 and inflates the reported waste sevenfold.",
			inv.Allocation, api.AllocMIG)
	}
	if !strings.Contains(inv.Detail, "single") {
		t.Errorf("detail = %q, want it to name the single strategy: an operator who sees a "+
			"MIG exclusion needs to know which partitioning produced it.", inv.Detail)
	}
}

// The exclusion is the point of the classification: MIG instances have no
// meaningful per-instance device utilization, so the node must leave the
// analysis rather than be measured with a metric that cannot see it.
func TestSingleStrategyMIGNodeIsExcludedFromAnalysis(t *testing.T) {
	n := node("mig-single", map[string]string{
		"nvidia.com/gpu.product": "H100-SXM5-80GB-MIG-2g.20gb",
		"nvidia.com/gpu.count":   "21",
	}, map[string]string{"nvidia.com/gpu": "21"})

	inv := Build([]kube.Node{*n}, nil)

	if inv.Counts.MIG != 21 {
		t.Fatalf("MIG count = %d, want 21: the instances were not attributed to MIG.", inv.Counts.MIG)
	}
	if inv.Counts.DevicePluginExclusive != 0 {
		t.Fatalf("device-plugin exclusive count = %d, want 0: MIG instances were counted as whole "+
			"devices, which is what puts them into the priced analysis.", inv.Counts.DevicePluginExclusive)
	}
	var found bool
	for _, e := range inv.Exclusions {
		if e.Code == api.ExclMIG {
			found = true
		}
	}
	if !found {
		t.Errorf("no MIG exclusion was recorded, so the node stays in the analysis and is "+
			"measured with a device-level metric that cannot see an instance. "+
			"Exclusions: %+v", inv.Exclusions)
	}
}

// A MIG-capable node with MIG switched off is an ordinary whole-card node, and
// it carries mig.capable and mig.strategy labels regardless. Excluding it would
// silently drop real, analysable hardware -- the opposite failure, and a
// quieter one, because nothing in the output says the node went missing.
func TestMIGCapableButDisabledNodeStaysExclusive(t *testing.T) {
	n := node("a100-whole", map[string]string{
		"nvidia.com/gpu.product":  "A100-SXM4-40GB",
		"nvidia.com/gpu.count":    "8",
		"nvidia.com/mig.strategy": "single",
		"nvidia.com/mig.capable":  "true",
		"nvidia.com/mig.config":   "all-disabled",
	}, map[string]string{"nvidia.com/gpu": "8"})

	inv := InventoryNode(n, 0)

	if inv.Allocation != api.AllocExclusive {
		t.Fatalf("allocation = %q, want %q: MIG is capable but disabled, so these are eight "+
			"whole A100s. Excluding them drops real hardware from the analysis without "+
			"saying so.", inv.Allocation, api.AllocExclusive)
	}
	if inv.Physical != 8 {
		t.Errorf("physical = %d, want 8", inv.Physical)
	}
}

// The mixed strategy advertises instances under their own resource names and
// keeps nvidia.com/gpu for unpartitioned cards, so it must keep reporting the
// physical card count rather than the instance count.
func TestMixedStrategyMIGStillReportsPhysicalCards(t *testing.T) {
	n := node("mig-mixed", map[string]string{
		"nvidia.com/gpu.product":  "A100-SXM4-40GB",
		"nvidia.com/gpu.count":    "8",
		"nvidia.com/mig.strategy": "mixed",
		"nvidia.com/mig.capable":  "true",
	}, map[string]string{
		"nvidia.com/gpu":         "4",
		"nvidia.com/mig-1g.5gb":  "21",
		"nvidia.com/mig-2g.10gb": "3",
	})

	inv := InventoryNode(n, 0)

	if inv.Allocation != api.AllocMIG {
		t.Fatalf("allocation = %q, want %q", inv.Allocation, api.AllocMIG)
	}
	if inv.Physical != 8 {
		t.Errorf("physical = %d, want 8: under the mixed strategy the card count is still "+
			"reported truthfully by gpu.count, and summing instances would overstate it.",
			inv.Physical)
	}
}
