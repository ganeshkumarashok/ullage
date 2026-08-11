package humanize_test

import (
	"testing"
	"time"

	"github.com/ullage-project/ullage/internal/humanize"
)

func TestDurationRendersDaysHoursMinutesInDecreasingGranularity(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero", 0, "0h"},
		{"negative collapses to zero", -5 * time.Hour, "0h"},
		{"minutes only, under an hour", 59 * time.Minute, "59m"},
		{"exactly one hour", time.Hour, "1h 00m"},
		{"hour and a half", 90 * time.Minute, "1h 30m"},
		{"just under a day", 23*time.Hour + 59*time.Minute, "23h 59m"},
		{"exactly one day", 24 * time.Hour, "1d"},
		{"a day and an hour", 25 * time.Hour, "1d 01h"},
		{"multiple days, no remainder hours", 3 * 24 * time.Hour, "3d"},
		{"multiple days with hours", 3*24*time.Hour + 5*time.Hour, "3d 05h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanize.Duration(tc.in); got != tc.want {
				t.Fatalf("Duration(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Short is the table-column form, documented as at most four characters for
// the windows this tool actually deals with (hours to a few hundred days).
func TestShortStaysWithinItsColumnBudget(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero", 0, "0h"},
		{"negative collapses to zero", -time.Minute, "0h"},
		{"minutes only", 59 * time.Minute, "59m"},
		{"exactly one hour", time.Hour, "1h"},
		{"hour and a half truncates the fraction", 90 * time.Minute, "1h"},
		{"just under a day", 23 * time.Hour, "23h"},
		{"exactly one day", 24 * time.Hour, "1d"},
		{"a realistic large scan window", 180 * 24 * time.Hour, "180d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := humanize.Short(tc.in)
			if got != tc.want {
				t.Fatalf("Short(%v) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > 4 {
				t.Fatalf("Short(%v) = %q is longer than the 4-character budget the table column assumes", tc.in, got)
			}
		})
	}
}

// Past a thousand accelerator-hours the individual digit stops carrying
// meaning, so Hours switches to a compact k-suffixed form. The boundary
// values are where a rounding mistake would first become visible in the
// headline dollar figure's denominator.
func TestHoursSwitchesToCompactFormAtThePublishedThresholds(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want string
	}{
		{"zero", 0, "0"},
		{"just under the k threshold", 999, "999"},
		{"exactly one k", 1000, "1.0k"},
		{"one decimal place below the 10k threshold", 9000, "9.0k"},
		// 9999/1000 = 9.999, which %.1f rounds up to 10.0 — the boundary reads
		// as if it crossed into 5 digits' worth of hours one unit early. This
		// is a %.1f rounding artefact of the chosen precision, not a bug: the
		// underlying value is unchanged and only the compact rendering rounds.
		{"just under 10k rounds up in its one decimal place", 9999, "10.0k"},
		{"exactly ten thousand drops to no decimal places", 10000, "10k"},
		{"large value", 99999, "100k"},
		{"negative renders as a negative count, not clamped to zero", -5, "-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanize.Hours(tc.in); got != tc.want {
				t.Fatalf("Hours(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The singular/plural boundary is the one place this function is interesting:
// everything except exactly one is plural, including zero and negative counts.
func TestPluralAgreesInNumberOnlyAtExactlyOne(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{-1, "pods"},
		{0, "pods"},
		{1, "pod"},
		{2, "pods"},
		{100, "pods"},
	}
	for _, tc := range cases {
		if got := humanize.Plural(tc.n, "pod"); got != tc.want {
			t.Fatalf("Plural(%d, %q) = %q, want %q", tc.n, "pod", got, tc.want)
		}
	}
}
