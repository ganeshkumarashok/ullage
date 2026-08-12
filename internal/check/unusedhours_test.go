package check_test

import (
	"testing"
	"time"

	"github.com/ganeshkumarashok/ullage/internal/check"
	"github.com/ganeshkumarashok/ullage/internal/inventory"
	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// The overstatement all three independent reviews found, and the one that
// shows up on the front page.
//
// A Deployment with ten replicas: one has been idle a fortnight, nine have
// been idle four days. That is 14 + 36 = 50 device-days of waste. Reporting
// the group's longest idle run against every device in it gives 10 x 14 = 140
// -- nearly three times the real figure.
//
// It is not a rounding error. Findings are ranked by unused hours and priced
// from them, so the inflated group outranks genuinely worse waste, and the
// cluster total that the tool exists to state is wrong in the direction that
// makes the tool look good. The first person to check the number by hand stops
// believing the rest of it.
func TestGroupedUnusedHoursAreSummedNotMultiplied(t *testing.T) {
	var devices []inventory.Device
	var pods []inventory.PodView

	add := func(name string, idle time.Duration) {
		p := runningPod("research", name, "node-a", 1)
		p.Provenance = api.Provenance{
			Controlled: true, Recognized: true,
			RootKind: "Deployment", RootName: "featurizer",
		}
		start := now.Add(-idle)
		p.StartTime = &start
		pods = append(pods, p)
		ref := p.Ref
		devices = append(devices, device("node-a/"+name, "node-a", "gpu", &ref, idleStats(idle, false)))
	}

	add("old", 14*24*time.Hour)
	for i := 0; i < 9; i++ {
		add(string(rune('a'+i)), 4*24*time.Hour)
	}

	cl := cluster(devices, pods, nil)
	found := find(t, check.IdlePod{}, cl)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want the ten replicas grouped into 1", len(found))
	}
	f := found[0]

	const want = 14*24 + 9*4*24 // device-hours
	if f.UnusedHours != want {
		t.Fatalf("UnusedHours = %.0f, want %d.\n"+
			"One accelerator idle a fortnight beside nine idle four days is %d device-hours. "+
			"%.0f is the group's longest idle run billed against all ten devices, which "+
			"invents waste that never happened and ranks this group above real findings.",
			f.UnusedHours, want, want, f.UnusedHours)
	}

	// The headline stays the longest, because "this has been going on for two
	// weeks" is the true and useful thing to say about the group.
	if f.Unused != 14*24*time.Hour {
		t.Fatalf("Unused = %s, want the group's longest run of 336h", f.Unused)
	}
}

// The complement: when every device in the group really was idle for the same
// stretch, the sum and the product agree, and nothing about the fix may shrink
// a total that was correct.
func TestUniformlyIdleGroupIsUnchanged(t *testing.T) {
	var devices []inventory.Device
	var pods []inventory.PodView
	for i := 0; i < 4; i++ {
		name := string(rune('a' + i))
		p := runningPod("research", name, "node-a", 1)
		p.Provenance = api.Provenance{
			Controlled: true, Recognized: true,
			RootKind: "Deployment", RootName: "featurizer",
		}
		pods = append(pods, p)
		ref := p.Ref
		devices = append(devices, device("node-a/"+name, "node-a", "gpu", &ref,
			idleStats(7*24*time.Hour, false)))
	}

	found := find(t, check.IdlePod{}, cluster(devices, pods, nil))
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1", len(found))
	}
	if got, want := found[0].UnusedHours, float64(4*7*24); got != want {
		t.Fatalf("UnusedHours = %.0f, want %.0f: four devices idle a week each is %.0f "+
			"device-hours, and the fix must not shrink a total that was already right",
			got, want, want)
	}
}

// The same arithmetic on the node check, where the stakes are higher because
// the devices per node multiply it again.
//
// A pool of two nodes: one 8-GPU node empty a fortnight, one 8-GPU node empty
// four days. Real waste is 8*336 + 8*96 = 3456 device-hours. Billing the
// pool's longest against every device gives 16*336 = 5376.
func TestPoolUnusedHoursFollowEachNodesOwnEmptyTime(t *testing.T) {
	var devices []inventory.Device
	mk := func(node string, idle time.Duration) inventory.NodeView {
		for i := 0; i < 8; i++ {
			devices = append(devices, device(node+"/"+string(rune('0'+i)), node, "gpu", nil,
				idleStats(idle, false)))
		}
		return inventory.NodeView{
			Name: node, Pool: "gpu", Ready: true, Accelerators: 8,
			Age: idle, Allocation: api.AllocExclusive,
		}
	}
	nodes := []inventory.NodeView{
		mk("old", 14*24*time.Hour),
		mk("new", 4*24*time.Hour),
	}

	found := find(t, check.UnusedNode{}, cluster(devices, nil, nodes))
	if len(found) != 1 {
		t.Fatalf("got %d findings, want the pool grouped into 1", len(found))
	}

	const want = 8*14*24 + 8*4*24
	if found[0].UnusedHours != want {
		t.Fatalf("UnusedHours = %.0f, want %d.\n"+
			"A node empty for a fortnight beside one empty four days is %d device-hours "+
			"of waste. Charging the whole pool the longest node's duration is how the "+
			"headline cluster total ends up nearly double what actually happened.",
			found[0].UnusedHours, want, want)
	}
}
