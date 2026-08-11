package scan

import (
	"testing"

	"github.com/ullage-project/ullage/internal/kube"
)

// claim builds an allocated ResourceClaim reserved for the given pod UIDs.
func claim(devices []struct{ driver, pool, device, share string }, holders ...string) kube.ResourceClaim {
	var c kube.ResourceClaim
	c.Status.Allocation = &struct {
		Devices struct {
			Results []struct {
				Device  string `json:"device"`
				Driver  string `json:"driver"`
				Pool    string `json:"pool"`
				ShareID string `json:"shareID,omitempty"`
			} `json:"results"`
		} `json:"devices"`
	}{}
	for _, d := range devices {
		c.Status.Allocation.Devices.Results = append(c.Status.Allocation.Devices.Results, struct {
			Device  string `json:"device"`
			Driver  string `json:"driver"`
			Pool    string `json:"pool"`
			ShareID string `json:"shareID,omitempty"`
		}{Device: d.device, Driver: d.driver, Pool: d.pool, ShareID: d.share})
	}
	for _, h := range holders {
		c.Status.ReservedFor = append(c.Status.ReservedFor, struct {
			Resource string `json:"resource"`
			Name     string `json:"name"`
			UID      string `json:"uid"`
		}{Resource: "pods", Name: h, UID: h})
	}
	return c
}

type dev = struct{ driver, pool, device, share string }

// DRA allocates any device a vendor writes a driver for. A pod holding two RDMA
// NICs through dra.net has the same claim shape as a pod holding two H100s, and
// counting results without reading the driver turns the NICs into idle GPUs --
// then prices them at H100 rates and recommends deleting the workload.
func TestNonAcceleratorDRAClaimsAreNotGPUs(t *testing.T) {
	claims := []kube.ResourceClaim{
		claim([]dev{{"dra.net", "nics", "nic-0", ""}, {"dra.net", "nics", "nic-1", ""}}, "pod-net"),
		claim([]dev{{"gpu.nvidia.com", "node-a", "gpu-0", ""}}, "pod-gpu"),
	}

	got := draDevicesByPod(claims)

	if got["pod-net"] != 0 {
		t.Errorf("pod-net holds %d accelerators, want 0: its claim is for network devices, "+
			"and calling them GPUs invents hardware that was never bought and bills for it.",
			got["pod-net"])
	}
	if got["pod-gpu"] != 1 {
		t.Errorf("pod-gpu holds %d accelerators, want 1: filtering by driver must not drop "+
			"real GPU claims, which would hide the devices DRA exists to allocate.",
			got["pod-gpu"])
	}
}

// A claim reserved for several pods holds its devices once and shares them.
// Billing the full count to each pod reports three times the hardware the
// cluster has, and the census reconciliation meant to catch that would see the
// invented devices as real.
func TestSharedClaimIsNotBilledToEveryPod(t *testing.T) {
	claims := []kube.ResourceClaim{
		claim([]dev{{"gpu.nvidia.com", "node-a", "gpu-0", ""}}, "pod-a", "pod-b", "pod-c"),
	}

	got := draDevicesByPod(claims)

	total := got["pod-a"] + got["pod-b"] + got["pod-c"]
	if total != 1 {
		t.Fatalf("the three pods hold %d accelerators between them, but the claim allocates "+
			"exactly one device. This count is summed into each pod's request, so a total "+
			"above the devices that exist reports hardware the cluster never bought: %v",
			total, got)
	}
}

// Devices are dealt out rather than divided away: a claim holding four GPUs
// shared by two pods is four GPUs, and the totals have to say four.
func TestSharedClaimDevicesAreAllAccountedFor(t *testing.T) {
	claims := []kube.ResourceClaim{
		claim([]dev{
			{"gpu.nvidia.com", "node-a", "gpu-0", ""},
			{"gpu.nvidia.com", "node-a", "gpu-1", ""},
			{"gpu.nvidia.com", "node-a", "gpu-2", ""},
			{"gpu.nvidia.com", "node-a", "gpu-3", ""},
		}, "pod-a", "pod-b"),
	}

	got := draDevicesByPod(claims)

	if total := got["pod-a"] + got["pod-b"]; total != 4 {
		t.Fatalf("the two pods hold %d accelerators between them, want 4: dividing devices "+
			"away loses real hardware from the census just as surely as duplicating it "+
			"invents some. %v", total, got)
	}
}

// An odd remainder has to land on a pod rather than evaporate.
func TestSharedClaimRemainderIsNotLost(t *testing.T) {
	claims := []kube.ResourceClaim{
		claim([]dev{
			{"gpu.nvidia.com", "node-a", "gpu-0", ""},
			{"gpu.nvidia.com", "node-a", "gpu-1", ""},
			{"gpu.nvidia.com", "node-a", "gpu-2", ""},
		}, "pod-a", "pod-b"),
	}

	got := draDevicesByPod(claims)

	if total := got["pod-a"] + got["pod-b"]; total != 3 {
		t.Fatalf("three devices between two pods totalled %d: %v", total, got)
	}
}

// A partitioned device carries a shareID and appears in one allocation per
// share. It is still one card, and the node census must say so or the cluster
// appears to own hardware it does not.
func TestSharedDeviceCountsOnceOnTheNode(t *testing.T) {
	pods := []kube.Pod{{}, {}}
	pods[0].Metadata.UID = "pod-a"
	pods[0].Spec.NodeName = "node-a"
	pods[1].Metadata.UID = "pod-b"
	pods[1].Spec.NodeName = "node-a"

	claims := []kube.ResourceClaim{
		claim([]dev{{"gpu.nvidia.com", "node-a", "gpu-0", "share-1"}}, "pod-a"),
		claim([]dev{{"gpu.nvidia.com", "node-a", "gpu-0", "share-2"}}, "pod-b"),
	}

	got := draDevicesByNode(claims, pods)

	if got["node-a"] != 1 {
		t.Fatalf("node-a holds %d accelerators, want 1: gpu-0 was claimed twice with "+
			"different share IDs, but it is one physical card. Counting it twice makes "+
			"the node look like it owns hardware nobody bought.", got["node-a"])
	}
}

// Two genuinely different devices on one node must still both be counted, or
// the conservative direction silently halves a real node's capacity.
func TestDistinctDevicesOnANodeAreBothCounted(t *testing.T) {
	pods := []kube.Pod{{}}
	pods[0].Metadata.UID = "pod-a"
	pods[0].Spec.NodeName = "node-a"

	claims := []kube.ResourceClaim{
		claim([]dev{
			{"gpu.nvidia.com", "node-a", "gpu-0", ""},
			{"gpu.nvidia.com", "node-a", "gpu-1", ""},
		}, "pod-a"),
	}

	if got := draDevicesByNode(claims, pods); got["node-a"] != 2 {
		t.Fatalf("node-a holds %d accelerators, want 2", got["node-a"])
	}
}

func TestAcceleratorDriverRecognition(t *testing.T) {
	for _, tc := range []struct {
		driver string
		want   bool
		why    string
	}{
		{"gpu.nvidia.com", true, "the reference NVIDIA GPU driver"},
		{"gpu.amd.com", true, "AMD GPUs"},
		{"gpu.intel.com", true, "Intel GPUs"},
		{"tpu.google.com", true, "TPUs are accelerators"},
		{"gpu.example.vendor", true, "an unknown vendor whose driver still names a GPU"},
		{"", true, "an omitted driver on a node already believed to have accelerators"},
		{"dra.net", false, "network devices are not accelerators"},
		{"net.nvidia.com", false, "NVIDIA also ships a network driver, and it is still a NIC"},
		{"fpga.intel.com", false, "an FPGA is not priced like a GPU"},
	} {
		if got := isAcceleratorDriver(tc.driver); got != tc.want {
			t.Errorf("isAcceleratorDriver(%q) = %v, want %v: %s", tc.driver, got, tc.want, tc.why)
		}
	}
}
