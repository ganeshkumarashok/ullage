package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// Duration formatting lives in the renderer, not in the contract package.
// How long "13d 21h" should look to a person is a presentation decision, and a
// data contract that carries presentation decisions leaks.

// Human renders a duration the way a person reads it: 13d 21h.
func Human(d time.Duration) string {
	if d <= 0 {
		return "0h"
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %02dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	default:
		mins := int((d % time.Hour) / time.Minute)
		if hours == 0 {
			return fmt.Sprintf("%dm", mins)
		}
		return fmt.Sprintf("%dh %02dm", hours, mins)
	}
}

// ThresholdLabel renders a configured threshold exactly and compactly.
//
// Human pads to "1h 00m" so that durations line up in a table column, which is
// wrong for a setting quoted in a sentence; HumanShort would round 90m to "1h",
// which misstates what the scan actually did. Round values print as one unit and
// anything else falls back to the padded form rather than lying.
func ThresholdLabel(d time.Duration) string {
	switch {
	case d <= 0:
		return "0"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d < time.Hour && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		// Sub-hour remainders read better as "1h 30m" than as "90m", which
		// stops being legible the moment the threshold is a few hours.
		return Human(d)
	}
}

// HumanShort is the table-column form: at most four characters.
func HumanShort(d time.Duration) string {
	switch {
	case d <= 0:
		return "0h"
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}

// AcceleratorSummary renders "3 × A100-80GB".
func AcceleratorSummary(f api.Finding) string {
	parts := make([]string, 0, len(f.Accelerators))
	for _, a := range f.Accelerators {
		parts = append(parts, fmt.Sprintf("%d × %s", a.Count, a.Model))
	}
	return strings.Join(parts, ", ")
}

func chainString(chain []api.OwnerRef) string {
	parts := make([]string, 0, len(chain))
	for _, c := range chain {
		parts = append(parts, c.String())
	}
	return strings.Join(parts, " → ")
}
