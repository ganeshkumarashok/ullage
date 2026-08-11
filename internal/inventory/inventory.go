package inventory

import (
	"strconv"
	"strings"

	"github.com/ullage-project/ullage/internal/kube"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// Device inventory and allocation-model detection.
//
// The claim "GPU utilization read zero, so no work happened" holds only when
// one pod exclusively holds one physical device. Three things break that, and
// each of them breaks it silently unless it is detected:
//
//   - Time-slicing and MPS inflate nvidia.com/gpu allocatable by the replica
//     factor, so a GPU count becomes a count of virtual devices, and an idle
//     pod sharing a physical device with a busy one reads as busy.
//   - MIG makes device-level utilization meaningless per instance.
//   - DRA removes nvidia.com/gpu from requests and allocatable entirely, so
//     every attribution query returns nothing at all.
//
// Detecting these is the difference between a tool that is narrow and a tool
// that is wrong. A silent zero on a DRA cluster is the worst possible failure:
// it reports a healthy cluster it never actually examined.

// NodeInventory is what ullage knows about one node's accelerators.
type NodeInventory struct {
	Node *kube.Node

	Pool     string
	Model    string
	Vendor   string
	Provider string
	TDPWatts float64

	// PhysicalUnknown marks a node whose real card count cannot be determined
	// from anything the cluster reports -- today, MIG under the single
	// strategy, where every label and every resource count has been rewritten
	// to describe instances. Such capacity is left out of the paid total
	// rather than guessed at, because a guess here multiplies straight into
	// the headline figure.
	PhysicalUnknown bool

	// Physical is the real device count. Advertised is what the device plugin
	// puts in allocatable, which is inflated under time-slicing.
	Physical   int
	Advertised int
	Replicas   int

	Allocation string // one of api.Alloc*
	Detail     string

	// Initialising marks a node whose GPU stack is not ready yet: it is Ready
	// as a node but its devices are not allocatable. Such a node is transiently
	// "unused" by construction and must not be reported as waste.
	Initialising bool

	// Unhealthy counts devices present in capacity but missing from
	// allocatable. The device plugin withdraws a device it cannot talk to, and
	// the cloud provider keeps charging for it: unusable and paid for at once.
	Unhealthy int
}

// Analyzable reports whether per-pod idleness claims are supportable here.
//
// DRA counts as analysable: a ResourceClaim reserves whole devices for a pod,
// so the exclusivity the claim depends on holds. That matters because DRA went
// GA in Kubernetes 1.34 and the largest, most waste-prone fleets are the ones
// moving to it — a tool that shrugs at DRA is obsolete on arrival. What DRA
// changes is discovery, not exclusivity: the devices are invisible to a
// nvidia.com/gpu census, which is why the inventory looks for them separately.
//
// Time-slicing and MIG are genuinely different. There the device-level metric
// reflects every co-tenant, so no per-pod claim is supportable at this
// telemetry tier and the devices are excluded out loud.
func (n NodeInventory) Analyzable() bool {
	return n.Allocation == api.AllocExclusive || n.Allocation == api.AllocDRA
}

// tdpByModel is a small table of device thermal design power, used to turn a
// raw power reading into "consistent with an idle device". Values are the
// vendor-published board TDP. When a model is unknown, ullage omits the power
// corroboration rather than guessing, which costs a confidence tier and never
// invents evidence.
var tdpByModel = map[string]float64{
	"NVIDIA-A100-SXM4-80GB": 400,
	"NVIDIA-A100-SXM4-40GB": 400,
	"NVIDIA-A100-PCIE-40GB": 250,
	"NVIDIA-H100-SXM5-80GB": 700,
	"NVIDIA-H100-PCIE-80GB": 350,
	"NVIDIA-H200":           700,
	"NVIDIA-L40S":           350,
	"NVIDIA-L4":             72,
	"NVIDIA-A10":            150,
	"NVIDIA-A10G":           150,
	"Tesla-T4":              70,
	"Tesla-V100-SXM2-16GB":  300,
	"AMD-Instinct-MI300X":   750,
	"AMD-Instinct-MI250X":   560,
}

// TDP returns the board power for a model, and whether it is known.
func TDP(deviceModel string) (float64, bool) {
	if v, ok := tdpByModel[deviceModel]; ok {
		return v, true
	}
	// Normalise "NVIDIA A100-SXM4-80GB" and similar spacings.
	norm := strings.ReplaceAll(deviceModel, " ", "-")
	if v, ok := tdpByModel[norm]; ok {
		return v, true
	}
	return 0, false
}

func labelInt(n *kube.Node, key string) int {
	v, ok := n.Metadata.Labels[key]
	if !ok {
		return 0
	}
	i, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return i
}

func labelIs(n *kube.Node, key, want string) bool {
	return strings.EqualFold(strings.TrimSpace(n.Metadata.Labels[key]), want)
}

// InventoryNode classifies a single node's accelerators.
//
// draClaimed is the number of devices on this node known to be allocated via
// DRA ResourceClaims, which is the only positive signal that a node with GPU
// hardware but no extended resource is a DRA node rather than a broken one.
func InventoryNode(n *kube.Node, draClaimed int) *NodeInventory {
	advertised, resource := n.Status.Allocatable.GPUs()
	capacity, _ := n.Status.Capacity.GPUs()

	// Feature discovery labels the hardware even when no extended resource is
	// advertised, which is exactly the DRA case.
	labelled := labelInt(n, "nvidia.com/gpu.count")
	migCapable := labelIs(n, "nvidia.com/mig.capable", "true")
	migStrategy := strings.TrimSpace(n.Metadata.Labels["nvidia.com/mig.strategy"])
	migConfig := strings.TrimSpace(n.Metadata.Labels["nvidia.com/mig.config"])
	sharing := strings.TrimSpace(n.Metadata.Labels["nvidia.com/gpu.sharing-strategy"])
	replicas := labelInt(n, "nvidia.com/gpu.replicas")
	if replicas == 0 {
		replicas = labelInt(n, "nvidia.com/gpu.count.replicas")
	}
	mpsCapable := labelIs(n, "nvidia.com/mps.capable", "true")

	// Under the "single" MIG strategy the device plugin advertises instances as
	// plain nvidia.com/gpu, so an 8-card node partitioned seven ways advertises
	// 56 -- indistinguishable by count from 56 whole cards, and priced like
	// them. Feature discovery is what gives it away: it rewrites the product
	// label to carry the profile, as in A100-SXM4-40GB-MIG-1g.5gb.
	migProduct := strings.Contains(strings.ToUpper(n.GPUModel()), "MIG-")

	// A MIG node also advertises resources of the form nvidia.com/mig-1g.10gb.
	migAdvertised := 0
	for k, v := range n.Status.Allocatable {
		if strings.HasPrefix(k, "nvidia.com/mig-") {
			if i, err := strconv.Atoi(v); err == nil {
				migAdvertised += i
			}
		}
	}

	inv := &NodeInventory{
		Node:       n,
		Pool:       n.Pool(),
		Model:      n.GPUModel(),
		Provider:   n.Provider(),
		Advertised: advertised,
		Replicas:   1,
		Allocation: api.AllocExclusive,
	}
	inv.Vendor = vendorOf(resource, inv.Model)
	if tdp, ok := TDP(inv.Model); ok {
		inv.TDPWatts = tdp
	}

	switch {
	case draClaimed > 0 || (labelled > 0 && advertised == 0 && migAdvertised == 0 && draClaimed >= 0 && hasDRAHint(n)):
		inv.Allocation = api.AllocDRA
		inv.Physical = maxInt(labelled, draClaimed)
		inv.Detail = "devices allocated through DRA ResourceClaims"

	case (migCapable || migProduct) && (migAdvertised > 0 || migProduct || (migConfig != "" && migConfig != "all-disabled")):
		inv.Allocation = api.AllocMIG
		inv.Physical = maxInt(labelled, capacity)
		if inv.Physical == 0 {
			inv.Physical = advertised
		}
		strategy := migStrategy
		if strategy == "" {
			if migProduct {
				strategy = "single"
			} else {
				strategy = "mixed"
			}
		}
		if strategy == "single" || migProduct {
			// Every advertised unit is an instance, not a card, and nothing in
			// the labels reports how many cards they came from: gpu.count is
			// rewritten to the instance count too. Time-slicing has a divisor
			// to undo -- advertised/replicas -- and MIG under this strategy
			// has none.
			//
			// So the card count is unknown, and saying so is the only honest
			// option. Carrying the instance count as a physical count billed
			// an eight-card node split seven ways as fifty-six cards: seven
			// times the capacity it has, in the paid total on the front page
			// and in every unused-node finding about the pool. Better to
			// exclude capacity we cannot count than to inflate the number the
			// whole tool is judged by.
			inv.PhysicalUnknown = true
			inv.Physical = 0
			inv.Detail = "MIG enabled (single strategy; " + strconv.Itoa(maxInt(advertised, labelled)) +
				" instances advertised, physical card count unknown)"
		} else {
			inv.Detail = "MIG enabled (" + strategy + " strategy)"
		}

	case replicas > 1 || sharing == "time-slicing" || (mpsCapable && sharing == "mps"):
		inv.Allocation = api.AllocTimeSliced
		if replicas < 1 {
			replicas = 1
		}
		inv.Replicas = replicas
		// The device plugin multiplies allocatable by the replica factor. The
		// physical count is what ullage reports and reasons about; counting
		// virtual replicas would corrupt every number derived from it.
		if replicas > 1 && advertised > 0 {
			inv.Physical = advertised / replicas
		}
		if inv.Physical == 0 {
			inv.Physical = maxInt(labelled, 0)
		}
		strategy := sharing
		if strategy == "" {
			strategy = "time-slicing"
		}
		inv.Detail = strategy + ", " + strconv.Itoa(replicas) + " replicas per device"

	default:
		// Capacity, not allocatable. Allocatable is what the scheduler may
		// place on; capacity is what the machine has and what the invoice is
		// for. The device plugin withdraws a device it cannot talk to -- an
		// Xid, a fallen-off-the-bus PCIe link, ECC retirement -- and an 8-GPU
		// node with two dead cards advertises six. Trusting allocatable makes
		// those two disappear from a tool whose entire subject is hardware that
		// is paid for and not doing work.
		inv.Physical = maxInt(advertised, capacity)
		if capacity > advertised && advertised > 0 {
			inv.Unhealthy = capacity - advertised
			inv.Detail = strconv.Itoa(inv.Unhealthy) + " of " + strconv.Itoa(capacity) +
				" devices are in capacity but not allocatable: the device plugin has " +
				"withdrawn them, and they are still being paid for"
		}
		if advertised == 0 && (labelled > 0 || capacity > 0) {
			// Hardware is present and labelled but nothing is allocatable: the
			// driver or device plugin has not finished initialising. A newly
			// scaled-up node looks exactly like an idle one from a distance.
			//
			// This is keyed on allocatable rather than on the physical count,
			// because the physical count now falls back to capacity: a node
			// still bringing its driver up has capacity for every card and
			// allocatable for none, and would otherwise both lose this
			// classification and be reported as a node full of dead hardware.
			inv.Physical = maxInt(labelled, capacity)
			inv.Initialising = true
			inv.Unhealthy = 0
			inv.Detail = "GPU hardware present but not yet allocatable"
		}
	}

	if inv.Physical < 0 {
		inv.Physical = 0
	}
	return inv
}

// hasDRAHint reports whether a node advertises a DRA driver. Feature discovery
// and the NVIDIA DRA driver both label nodes they manage.
func hasDRAHint(n *kube.Node) bool {
	for k := range n.Metadata.Labels {
		if strings.Contains(k, "dra.nvidia.com") || strings.Contains(k, "resource.k8s.io") {
			return true
		}
	}
	return labelIs(n, "nvidia.com/dra.controller", "true") ||
		labelIs(n, "nvidia.com/dra.kubelet-plugin", "true")
}

func vendorOf(resource, deviceModel string) string {
	switch {
	case strings.HasPrefix(resource, "nvidia.com"):
		return "nvidia"
	case strings.HasPrefix(resource, "amd.com"):
		return "amd"
	case strings.HasPrefix(resource, "gpu.intel.com"):
		return "intel"
	case strings.HasPrefix(resource, "habana.ai"):
		return "habana"
	}
	lower := strings.ToLower(deviceModel)
	switch {
	case strings.Contains(lower, "nvidia") || strings.Contains(lower, "tesla"):
		return "nvidia"
	case strings.Contains(lower, "amd") || strings.Contains(lower, "instinct"):
		return "amd"
	case strings.Contains(lower, "intel"):
		return "intel"
	}
	return "unknown"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Inventory is the whole cluster's accelerator census.
type Inventory struct {
	Nodes map[string]*NodeInventory

	Observed int
	Analyzed int
	Counts   api.AllocationCounts

	Exclusions []api.Exclusion
}

// Build classifies every node and produces the analysed/not-analysed
// accounting that the rest of the output depends on.
func Build(nodes []kube.Node, draByNode map[string]int) *Inventory {
	inv := &Inventory{Nodes: map[string]*NodeInventory{}}

	type bucket struct {
		devices int
		pools   map[string]bool
		detail  string
	}
	shared := map[string]*bucket{}
	add := func(kind, detail string, devices int, pool string) {
		b, ok := shared[kind]
		if !ok {
			b = &bucket{pools: map[string]bool{}}
			shared[kind] = b
		}
		b.devices += devices
		b.pools[pool] = true
		if b.detail == "" {
			b.detail = detail
		}
	}

	for i := range nodes {
		n := &nodes[i]
		ni := InventoryNode(n, draByNode[n.Metadata.Name])
		if ni.Physical == 0 && ni.Advertised == 0 {
			continue // not a GPU node
		}
		inv.Nodes[n.Metadata.Name] = ni
		inv.Observed += ni.Physical

		switch ni.Allocation {
		case api.AllocExclusive:
			inv.Counts.DevicePluginExclusive += ni.Physical
			if ni.Initialising {
				add("initialising", ni.Detail, ni.Physical, ni.Pool)
			} else {
				inv.Analyzed += ni.Physical
			}
		case api.AllocTimeSliced:
			inv.Counts.TimeSliced += ni.Physical
			add(api.AllocTimeSliced, ni.Detail, ni.Physical, ni.Pool)
		case api.AllocMIG:
			// Under the single strategy the card count is unknown, so the
			// instances are what there is to report. They are excluded from
			// analysis either way, and reporting nothing would make the
			// hardware disappear from the account entirely.
			counted := ni.Physical
			if ni.PhysicalUnknown {
				counted = ni.Advertised
			}
			inv.Counts.MIG += counted
			add(api.AllocMIG, ni.Detail, counted, ni.Pool)
		case api.AllocDRA:
			inv.Counts.DRA += ni.Physical
			// Not excluded: DRA allocation is exclusive, so idleness claims
			// hold. It is recorded separately only so the census is honest
			// about how the devices were found.
			if ni.Initialising {
				add("initialising", ni.Detail, ni.Physical, ni.Pool)
			} else {
				inv.Analyzed += ni.Physical
			}
		}
	}

	// Iterate in a fixed order. Ranging over the map directly meant two scans
	// of the same cluster emitted the exclusions in different orders, so the
	// output of a tool that asks to be trusted with reproducible numbers did
	// not reproduce -- and neither did its JSON, which people diff in CI.
	kinds := make([]string, 0, len(shared))
	for k := range shared {
		kinds = append(kinds, k)
	}
	sortStrings(kinds)

	for _, kind := range kinds {
		b := shared[kind]
		pools := make([]string, 0, len(b.pools))
		for p := range b.pools {
			pools = append(pools, p)
		}
		sortStrings(pools)
		poolList := strings.Join(pools, ", ")

		switch kind {
		case api.AllocTimeSliced:
			inv.Exclusions = append(inv.Exclusions, api.Exclusion{
				Code:         api.ExclTimeSliced,
				Reason:       "shared-device",
				Accelerators: b.devices,
				Detail: "pool " + poolList + " is " + b.detail +
					". Device-level utilization reflects every co-tenant, so an idle pod sharing a device with a busy one is invisible",
				Remedy: "Per-pod idleness on shared devices needs per-process accounting; node-level checks still apply",
			})
		case api.AllocMIG:
			inv.Exclusions = append(inv.Exclusions, api.Exclusion{
				Code:         api.ExclMIG,
				Reason:       "mig-instance",
				Accelerators: b.devices,
				Detail:       "pool " + poolList + " has " + b.detail + ". Device-level utilization is not meaningful per MIG instance",
				Remedy:       "Requires per-instance DCGM series (DCGM_FI_PROF_* with GPU_I_ID)",
			})
		case "initialising":
			inv.Exclusions = append(inv.Exclusions, api.Exclusion{
				Code:         api.ExclInitialising,
				Reason:       "driver-initialising",
				Accelerators: b.devices,
				Detail:       "pool " + poolList + " has GPU hardware that is not yet allocatable — the driver or device plugin is still starting",
				Remedy:       "Transient; re-run once the GPU operator reports ready",
			})
		}
	}
	return inv
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Analyzable is the number of devices whose allocation model permits a claim,
// before metrics availability is taken into account.
func (i *Inventory) Analyzable() int {
	n := 0
	for _, ni := range i.Nodes {
		if ni.Analyzable() && !ni.Initialising {
			n += ni.Physical
		}
	}
	return n
}
