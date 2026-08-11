package humanize_test

import (
	"testing"
	"time"

	"github.com/ullage-project/ullage/internal/humanize"
)

// The bug this package exists to fix: `--window 14d` is the tool's own
// documented default, and time.ParseDuration rejects it because it has no `d`
// or `w` unit. A parse error on the first command someone copies out of the
// README reads as their mistake, not the tool's.
func TestDocumentedDefaultWindowParses(t *testing.T) {
	got, err := humanize.ParseDuration("14d")
	if err != nil {
		t.Fatalf("ParseDuration(%q) = %v; the tool's own --help default would fail on the first run", "14d", err)
	}
	if want := 14 * 24 * time.Hour; got != want {
		t.Fatalf("ParseDuration(14d) = %v, want %v", got, want)
	}
}

func TestParseDurationAcceptsWhatAnOperatorTypes(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"14d", 14 * 24 * time.Hour},
		{"2w", 2 * 7 * 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"90m", 90 * time.Minute},
		{"24h", 24 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"0s", 0},
		{"1d", 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"  14d  ", 14 * 24 * time.Hour}, // surrounding whitespace is trimmed
		{"3.5d", 84 * time.Hour},         // fractional days are allowed
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := humanize.ParseDuration(tc.in)
			if err != nil {
				t.Fatalf("ParseDuration(%q) returned %v, want %v with no error", tc.in, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("ParseDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// These three are the exact identities the doc comment promises: a day is a
// fixed 24h span and a week is a fixed 168h span, with no calendar or DST
// reasoning applied. If a future change made these anything but exact, every
// dashboard comparing a `14d` window against wall-clock time would drift.
func TestDayAndWeekAreFixedSpansNotCalendarUnits(t *testing.T) {
	one, err := humanize.ParseDuration("1d")
	if err != nil {
		t.Fatal(err)
	}
	if one != 24*time.Hour {
		t.Fatalf("1d = %v, want exactly 24h", one)
	}

	week, err := humanize.ParseDuration("1w")
	if err != nil {
		t.Fatal(err)
	}
	if week != 168*time.Hour {
		t.Fatalf("1w = %v, want exactly 168h", week)
	}

	compound, err := humanize.ParseDuration("1d12h")
	if err != nil {
		t.Fatal(err)
	}
	if compound != 36*time.Hour {
		t.Fatalf("1d12h = %v, want exactly 36h", compound)
	}
}

// A malformed --window must fail loudly. Every one of these silently
// succeeding would turn a typo into a scan over the wrong span, and a `0s`
// window in particular would make every device look freshly measured rather
// than not measured at all.
func TestParseDurationRejectsGarbageRatherThanGuessing(t *testing.T) {
	for _, in := range []string{"bogus", "", "14", "d", "14dd", "d1", "14D", "1W", "."} {
		t.Run(in, func(t *testing.T) {
			got, err := humanize.ParseDuration(in)
			if err == nil {
				t.Fatalf("ParseDuration(%q) = %v with no error; a typo in --window would silently change the scan span", in, got)
			}
		})
	}
}

// A bare sign with nothing after it is not a duration. Before the fix this
// fell through the parser untouched and returned (0, nil): a --window of "-"
// (e.g. from a broken shell substitution) would silently scan a zero-length
// window instead of failing, and a zero-length window makes every device look
// like it was never observed doing anything — the exact shape of "idle".
func TestBareSignWithNoDigitsIsRejectedNotZero(t *testing.T) {
	for _, in := range []string{"-", "+"} {
		t.Run(in, func(t *testing.T) {
			got, err := humanize.ParseDuration(in)
			if err == nil {
				t.Fatalf("ParseDuration(%q) = %v with no error; a bare sign silently became a zero window", in, got)
			}
		})
	}
}

// Negative durations are supported deliberately: the sign is stripped and
// reapplied at the end for any well-formed magnitude, matching how
// time.ParseDuration itself treats a leading `-`. This is not a window a scan
// would ever be given, but the function is general-purpose and must not treat
// a valid negative magnitude as an error while accepting a meaningless bare
// sign as zero.
func TestNegativeDurationsAreNegatedNotRejected(t *testing.T) {
	got, err := humanize.ParseDuration("-3d")
	if err != nil {
		t.Fatalf("ParseDuration(-3d) returned %v, want -72h with no error", err)
	}
	if want := -72 * time.Hour; got != want {
		t.Fatalf("ParseDuration(-3d) = %v, want %v", got, want)
	}

	pos, err := humanize.ParseDuration("+3d")
	if err != nil {
		t.Fatal(err)
	}
	if pos != 72*time.Hour {
		t.Fatalf("ParseDuration(+3d) = %v, want 72h", pos)
	}
}
