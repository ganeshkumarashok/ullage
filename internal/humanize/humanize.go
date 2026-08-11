// Package humanize formats values for people.
//
// It sits outside the contract package deliberately: how "13d 21h" should look
// to a reader is a presentation decision, and a data contract that carries
// presentation decisions leaks. It sits outside the renderer because finding
// summaries are prose composed during analysis, so both layers need it.
package humanize

import (
	"fmt"
	"time"
)

// Duration renders a duration the way a person reads it: 13d 21h.
func Duration(d time.Duration) string {
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

// Short is the table-column form: at most four characters.
func Short(d time.Duration) string {
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

// Hours renders an accelerator-hour count compactly. Past a thousand hours the
// individual digits stop carrying meaning and the magnitude is the point.
func Hours(h float64) string {
	switch {
	case h >= 10000:
		return fmt.Sprintf("%.0fk", h/1000)
	case h >= 1000:
		return fmt.Sprintf("%.1fk", h/1000)
	default:
		return fmt.Sprintf("%.0f", h)
	}
}

// Plural returns the singular or plural form of a noun.
func Plural(n int, singular string) string {
	if n == 1 {
		return singular
	}
	return singular + "s"
}
