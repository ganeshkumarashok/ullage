package render

import (
	"testing"
	"time"

	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

func TestHumanRendersDaysHoursMinutesInDecreasingGranularity(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero", 0, "0h"},
		{"negative collapses to zero", -time.Hour, "0h"},
		{"minutes only", 45 * time.Minute, "45m"},
		{"exactly one hour", time.Hour, "1h 00m"},
		{"just under a day", 23*time.Hour + 59*time.Minute, "23h 59m"},
		{"exactly one day", 24 * time.Hour, "1d"},
		{"a day and some hours", 24*time.Hour + 5*time.Hour, "1d 05h"},
		{"many days", 13*24*time.Hour + 21*time.Hour, "13d 21h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Human(tc.in); got != tc.want {
				t.Fatalf("Human(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHumanShortStaysCompactForTheTableColumn(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0h"},
		{-time.Hour, "0h"},
		{45 * time.Minute, "45m"},
		{90 * time.Minute, "1h"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{9 * 24 * time.Hour, "9d"},
	}
	for _, tc := range cases {
		if got := HumanShort(tc.in); got != tc.want {
			t.Fatalf("HumanShort(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// AcceleratorSummary is the "3 × A100-80GB" fragment printed on every row and
// in the explain screen. An empty accelerator list (which should never happen
// for a real finding, but a caller could construct one) must render as an
// empty string, not panic or print a stray separator.
func TestAcceleratorSummaryJoinsCountAndModel(t *testing.T) {
	cases := []struct {
		name string
		in   []api.Accelerator
		want string
	}{
		{"none", nil, ""},
		{"one model", []api.Accelerator{{Model: "NVIDIA-A100-SXM4-80GB", Count: 3}}, "3 × NVIDIA-A100-SXM4-80GB"},
		{
			"mixed models",
			[]api.Accelerator{
				{Model: "NVIDIA-A100-SXM4-80GB", Count: 2},
				{Model: "Tesla-T4", Count: 1},
			},
			"2 × NVIDIA-A100-SXM4-80GB, 1 × Tesla-T4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := api.Finding{Accelerators: tc.in}
			if got := AcceleratorSummary(f); got != tc.want {
				t.Fatalf("AcceleratorSummary(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// chainString renders an ownership chain as "pod → ReplicaSet/foo →
// Deployment/foo" in the explain screen's "Managed by" section. Getting the
// arrow direction or separator wrong there silently misrepresents who owns
// the accelerator.
func TestChainStringJoinsOwnersWithAnArrow(t *testing.T) {
	cases := []struct {
		name string
		in   []api.OwnerRef
		want string
	}{
		{"empty chain", nil, ""},
		{"single link", []api.OwnerRef{{Kind: "Pod", Name: "trainer-0"}}, "Pod/trainer-0"},
		{
			"multiple links",
			[]api.OwnerRef{
				{Kind: "ReplicaSet", Name: "trainer-rs"},
				{Kind: "Deployment", Name: "trainer"},
			},
			"ReplicaSet/trainer-rs → Deployment/trainer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chainString(tc.in); got != tc.want {
				t.Fatalf("chainString(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A threshold is quoted in a sentence, where "1h 00m" reads as padding and
// rounding 90m to "1h" would misstate what the scan was told to do.
func TestThresholdLabelIsExactAndCompact(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{24 * time.Hour, "1d"},
		{14 * 24 * time.Hour, "14d"},
		{time.Hour, "1h"},
		{6 * time.Hour, "6h"},
		{30 * time.Minute, "30m"},
		{90 * time.Minute, "1h 30m"},
		{0, "0"},
	} {
		if got := ThresholdLabel(tc.in); got != tc.want {
			t.Errorf("ThresholdLabel(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
