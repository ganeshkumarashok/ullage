package inventory

import (
	"testing"
	"time"

	"github.com/ganeshkumarashok/ullage/internal/kube"
)

// Paid hours used to be "accelerators now x the whole window", which bills a
// node created this morning for the entire fortnight. GPU clusters autoscale,
// and this number is the denominator of the headline percentage and the base
// of the whole ledger, so the error lands directly on the one figure the tool
// asks to be trusted on.
func TestAYoungNodeIsBilledOnlyForTheTimeItExisted(t *testing.T) {
	const window = 14 * 24 * time.Hour
	now := time.Date(2026, 8, 11, 7, 0, 0, 0, time.UTC)

	n := node("fresh", map[string]string{
		"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB",
		"nvidia.com/gpu.count":   "8",
	}, map[string]string{"nvidia.com/gpu": "8"})
	n.Metadata.CreationTimestamp = now.Add(-24 * time.Hour)

	inv := Build([]kube.Node{*n}, nil, now, window)

	const want = 8 * 24.0
	if got := inv.PaidHours; got != want {
		t.Errorf("PaidHours = %v, want %v: a node one day old must be charged for "+
			"one day, not for the %v window it did not exist through",
			got, want, window)
	}
}

// The complement: a node older than the window is charged for the window, not
// for its whole life. Without this, a six-month-old node would report more
// paid hours than the window contains and the ledger could never reconcile.
func TestAnOldNodeIsBilledOnlyForTheWindow(t *testing.T) {
	const window = 14 * 24 * time.Hour
	now := time.Date(2026, 8, 11, 7, 0, 0, 0, time.UTC)

	n := node("veteran", map[string]string{
		"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB",
		"nvidia.com/gpu.count":   "8",
	}, map[string]string{"nvidia.com/gpu": "8"})
	n.Metadata.CreationTimestamp = now.Add(-180 * 24 * time.Hour)

	inv := Build([]kube.Node{*n}, nil, now, window)

	const want = 8 * 336.0
	if got := inv.PaidHours; got != want {
		t.Errorf("PaidHours = %v, want %v: capacity outside the window was never "+
			"in scope and must not enter the denominator", got, want)
	}
}

// Clock skew puts node creation timestamps in the future often enough to
// matter. A negative duration would subtract capacity from the denominator
// and could drive the reported unused share above 100%.
func TestANodeCreatedInTheFutureContributesNothingRatherThanNegativeTime(t *testing.T) {
	const window = 14 * 24 * time.Hour
	now := time.Date(2026, 8, 11, 7, 0, 0, 0, time.UTC)

	n := node("skewed", map[string]string{
		"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB",
		"nvidia.com/gpu.count":   "8",
	}, map[string]string{"nvidia.com/gpu": "8"})
	n.Metadata.CreationTimestamp = now.Add(2 * time.Hour)

	inv := Build([]kube.Node{*n}, nil, now, window)

	if inv.PaidHours != 0 {
		t.Errorf("PaidHours = %v, want 0: a clock-skewed timestamp must clamp to zero, "+
			"never to negative capacity", inv.PaidHours)
	}
}

// Unmeasurable capacity is charged on the same per-node basis as paid
// capacity. Derived independently -- one age-aware, one not -- the "no usable
// metric" bucket can exceed the capacity it is a subset of, and the ledger
// residual goes negative on a cluster that simply scaled up recently.
func TestNotAnalysedHoursUseTheSameBasisAsPaidHours(t *testing.T) {
	const window = 14 * 24 * time.Hour
	now := time.Date(2026, 8, 11, 7, 0, 0, 0, time.UTC)

	mig := node("mig-fresh", map[string]string{
		"nvidia.com/gpu.product": "H100-SXM5-80GB-MIG-2g.20gb",
		"nvidia.com/gpu.count":   "21",
	}, map[string]string{"nvidia.com/gpu": "21"})
	mig.Metadata.CreationTimestamp = now.Add(-24 * time.Hour)

	inv := Build([]kube.Node{*mig}, nil, now, window)

	if inv.NotAnalysedHours > inv.PaidHours && inv.PaidHours > 0 {
		t.Errorf("NotAnalysedHours %v exceeds PaidHours %v: a subset of capacity "+
			"cannot be larger than the capacity itself", inv.NotAnalysedHours, inv.PaidHours)
	}
	const want = 21 * 24.0
	if got := inv.NotAnalysedHours; got != want {
		t.Errorf("NotAnalysedHours = %v, want %v: excluded hardware must be charged "+
			"for the time it existed, exactly as paid capacity is", got, want)
	}
}
