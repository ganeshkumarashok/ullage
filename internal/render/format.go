package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/ullage-project/ullage/pkg/ullage/api"
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
