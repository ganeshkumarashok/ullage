package inventory

import (
	"strings"
	"testing"
	"time"

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

	inv := Build([]kube.Node{*n}, nil, time.Now(), 14*24*time.Hour)

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

// Allocatable is what the scheduler may place on. Capacity is what the machine
// has, and what the invoice is for. The device plugin withdraws a device it
// cannot talk to -- an Xid, a PCIe link that fell off the bus, ECC retirement
// -- so an 8-GPU node with two dead cards advertises six.
//
// Those two are the purest form of the thing this tool exists to find: paid
// for, and incapable of doing work. Counting allocatable makes them vanish.
func TestUnhealthyDevicesAreStillPaidFor(t *testing.T) {
	n := node("a100-degraded", map[string]string{
		"nvidia.com/gpu.product": "A100-SXM4-40GB",
		"nvidia.com/gpu.count":   "8",
	}, nil)
	n.Status.Capacity = kube.ResourceList{"nvidia.com/gpu": "8"}
	n.Status.Allocatable = kube.ResourceList{"nvidia.com/gpu": "6"}

	inv := InventoryNode(n, 0)

	if inv.Physical != 8 {
		t.Fatalf("physical = %d, want 8: two devices are in capacity but not allocatable. "+
			"The cloud provider bills for eight, so a tool measuring hardware that is paid "+
			"for and idle cannot report six.", inv.Physical)
	}
	if inv.Unhealthy != 2 {
		t.Errorf("unhealthy = %d, want 2", inv.Unhealthy)
	}
	if !strings.Contains(inv.Detail, "still being paid for") {
		t.Errorf("detail = %q, want it to say the withdrawn devices are still charged: "+
			"an operator seeing 8 counted and 6 schedulable needs the reason.", inv.Detail)
	}
}

// A healthy node must not acquire a phantom fault: capacity equals allocatable
// there, and any difference reported would be an invented incident.
func TestHealthyNodeReportsNoUnhealthyDevices(t *testing.T) {
	n := node("a100-healthy", map[string]string{
		"nvidia.com/gpu.product": "A100-SXM4-40GB",
	}, map[string]string{"nvidia.com/gpu": "8"})

	inv := InventoryNode(n, 0)

	if inv.Unhealthy != 0 {
		t.Fatalf("unhealthy = %d on a node whose capacity and allocatable agree", inv.Unhealthy)
	}
	if inv.Physical != 8 {
		t.Fatalf("physical = %d, want 8", inv.Physical)
	}
	if strings.Contains(inv.Detail, "paid for") {
		t.Errorf("detail = %q on a healthy node: telling an operator that devices have "+
			"been withdrawn when none have sends them hunting a fault that does not "+
			"exist, and teaches them to ignore the message when one does.", inv.Detail)
	}
}

// A node whose plugin has not started yet advertises nothing allocatable and
// is already handled as initialising. It must keep that classification rather
// than be reported as eight simultaneous hardware faults.
func TestInitialisingNodeIsNotReportedAsUnhealthy(t *testing.T) {
	n := node("a100-booting", map[string]string{
		"nvidia.com/gpu.product": "A100-SXM4-40GB",
		"nvidia.com/gpu.count":   "8",
	}, nil)
	n.Status.Capacity = kube.ResourceList{"nvidia.com/gpu": "8"}
	n.Status.Allocatable = kube.ResourceList{}

	inv := InventoryNode(n, 0)

	if inv.Unhealthy != 0 {
		t.Errorf("unhealthy = %d: a node still bringing its driver up has not failed, and "+
			"reporting eight faults would bury the nodes that really did.", inv.Unhealthy)
	}
	if !inv.Initialising {
		t.Errorf("the node lost its initialising classification, so a node that is " +
			"transiently unused by construction can be reported as waste")
	}
}

// The paid total is the denominator of every share the report prints and the
// basis of every dollar figure. Under the single strategy nothing in the
// cluster reports how many cards the instances were cut from -- gpu.count is
// rewritten to the instance count along with everything else -- so counting
// instances there bills an 8-card node as 56 cards.
//
// Time-slicing has a divisor to undo and MIG under this strategy has none, so
// the capacity is carried as unknown and left out of the total rather than
// guessed at. The instances stay visible in the MIG exclusion, which is where
// an operator looks to find out what was not measured and why.
func TestSingleStrategyMIGStaysOutOfThePaidTotal(t *testing.T) {
	mig := node("mig-single", map[string]string{
		"nvidia.com/gpu.product": "A100-SXM4-40GB-MIG-1g.5gb",
		"nvidia.com/gpu.count":   "56",
	}, map[string]string{"nvidia.com/gpu": "56"})
	plain := node("exclusive", map[string]string{
		"nvidia.com/gpu.product": "A100-SXM4-40GB",
		"nvidia.com/gpu.count":   "8",
	}, map[string]string{"nvidia.com/gpu": "8"})

	inv := Build([]kube.Node{*mig, *plain}, nil, time.Now(), 14*24*time.Hour)

	if inv.Observed != 8 {
		t.Fatalf("observed accelerators = %d, want 8 (the exclusive node alone). The MIG node's "+
			"56 instances came from an unknown number of cards; adding them to the paid total "+
			"multiplies straight into the headline hours and the headline dollars.", inv.Observed)
	}
	if got := inv.Nodes["mig-single"]; !got.PhysicalUnknown || got.Physical != 0 {
		t.Fatalf("mig node physical = %d, unknown = %v; want 0 and true, so that no check can "+
			"bill hours against a card count nobody knows.", got.Physical, got.PhysicalUnknown)
	}
	if inv.Counts.MIG != 56 {
		t.Errorf("MIG count = %d, want 56: the instances must stay visible in the exclusion even "+
			"though the cards behind them cannot be counted. Reporting nothing would make the "+
			"hardware vanish from the account entirely.", inv.Counts.MIG)
	}
}
