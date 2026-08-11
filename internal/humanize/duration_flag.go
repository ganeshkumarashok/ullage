package humanize

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseDuration accepts what an operator will actually type.
//
// Go's time.ParseDuration stops at hours, so `14d` — the value this tool's own
// help text advertises as the default window — is a parse error. Prometheus,
// kubectl and every dashboard the user came from accept days and weeks, so
// rejecting them makes the first command someone copies out of the README fail
// for a reason that looks like their mistake.
//
// Days and weeks are treated as fixed 24h and 168h spans. This is a
// measurement window, not a calendar appointment, so no DST or leap-second
// reasoning applies.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	orig := s
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		// A bare sign with nothing after it (`-` or `+`) is not a duration.
		// Without this check it fell through to the loop below untouched,
		// which never runs and never errors, so it silently returned a zero
		// duration instead of rejecting the input.
		return 0, fmt.Errorf("invalid duration %q", orig)
	}

	var total time.Duration
	var matched bool
	rest := s
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 {
			break
		}
		unitEnd := i
		for unitEnd < len(rest) && !(rest[unitEnd] >= '0' && rest[unitEnd] <= '9') {
			unitEnd++
		}
		unit := rest[i:unitEnd]
		if unit != "d" && unit != "w" {
			break
		}
		n, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		span := 24 * time.Hour
		if unit == "w" {
			span = 7 * 24 * time.Hour
		}
		total += time.Duration(n * float64(span))
		matched = true
		rest = rest[unitEnd:]
	}

	if rest != "" {
		// Whatever is left is a duration Go already understands, so `1d12h`
		// works as well as `1d` and `12h`.
		d, err := time.ParseDuration(rest)
		if err != nil {
			if matched {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			return 0, err
		}
		total += d
	}
	if neg {
		total = -total
	}
	return total, nil
}

// DurationFlag adapts ParseDuration to the flag package.
type DurationFlag struct {
	Value time.Duration
	// Present distinguishes "not given" from "given as zero", so a default can
	// be applied without a sentinel value.
	Present bool
}

func (d *DurationFlag) String() string {
	if d == nil || d.Value == 0 {
		return ""
	}
	return Duration(d.Value)
}

// Set implements flag.Value.
func (d *DurationFlag) Set(s string) error {
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	d.Value, d.Present = v, true
	return nil
}
