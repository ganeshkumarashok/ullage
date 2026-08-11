package render

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// DetectTTY decides whether escape codes are safe to emit. Getting this wrong
// in the direction of "on" means a piped or CI log fills with raw \033[1m
// sequences; getting it wrong in the direction of "off" just costs some
// polish, so the asymmetry means every disabling condition needs its own test.

func TestDetectTTYDisablesColorWhenNoColorIsSet(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "xterm-256color")
	var o Options
	o.DetectTTY(&bytes.Buffer{})
	if o.Color {
		t.Fatal("Color = true with NO_COLOR set; escape codes would leak into whatever respected that convention")
	}
}

func TestDetectTTYDisablesColorWhenTermIsDumb(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	var o Options
	o.DetectTTY(&bytes.Buffer{})
	if o.Color {
		t.Fatal("Color = true with TERM=dumb")
	}
}

// An empty TERM is what a lot of non-interactive launchers (cron, some CI
// runners) leave behind, and is indistinguishable here from "no terminal
// capability is known", so it must disable color the same as TERM=dumb.
func TestDetectTTYDisablesColorWhenTermIsUnset(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "")
	var o Options
	o.DetectTTY(&bytes.Buffer{})
	if o.Color {
		t.Fatal("Color = true with TERM unset")
	}
}

// A writer that is not an *os.File (a bytes.Buffer, e.g. captured output in a
// test or library caller) can never be a terminal, so color must be off
// regardless of what the environment claims.
func TestDetectTTYDisablesColorForANonFileWriter(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	var o Options
	o.DetectTTY(&bytes.Buffer{})
	if o.Color {
		t.Fatal("Color = true for a bytes.Buffer; a buffer is never a terminal")
	}
}

// Piped stdout is an *os.File, but not a character device, and is the single
// most common way `ullage | tee scan.log` would otherwise end up with raw
// escape codes baked into a saved log file.
func TestDetectTTYDisablesColorForAPipedFile(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	var o Options
	o.DetectTTY(w)
	if o.Color {
		t.Fatal("Color = true for a pipe; `ullage | tee log` would write raw escape codes into the log")
	}
}

func TestEnvWidthFallsBackTo80WhenColumnsIsAbsentOrInvalid(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  int
	}{
		{"unset", "", 80},
		{"not a number", "wide", 80},
		{"zero is not a usable width", "0", 80},
		{"negative is not a usable width", "-10", 80},
		{"a real terminal width", "120", 120},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COLUMNS", tc.value)
			if got := envWidth(); got != tc.want {
				t.Fatalf("envWidth() with COLUMNS=%q = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

// The printer clamps its working width to [60, 100] regardless of what the
// terminal reports, so a very narrow or very wide window does not distort the
// fixed-column layout the table depends on.
func TestPrinterWidthClampsToItsSupportedRange(t *testing.T) {
	cases := []struct {
		name  string
		width int
		want  int
	}{
		{"unset defaults to 80", 0, 80},
		{"narrower than the minimum clamps up", 40, 60},
		{"exactly the minimum", 60, 60},
		{"within range passes through", 90, 90},
		{"exactly the maximum", 100, 100},
		{"wider than the maximum clamps down", 500, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPrinter(&bytes.Buffer{}, Options{Width: tc.width})
			if got := p.width(); got != tc.want {
				t.Fatalf("width() with Options.Width=%d = %d, want %d", tc.width, got, tc.want)
			}
		})
	}
}

func TestWrapIndentReturnsEmptyForEmptyInput(t *testing.T) {
	if got := wrapIndent("", 4, 80); got != "" {
		t.Fatalf("wrapIndent(\"\") = %q, want empty", got)
	}
	if got := wrapIndent("   ", 4, 80); got != "" {
		t.Fatalf("wrapIndent of only whitespace = %q, want empty", got)
	}
}

// available width = width - indent = 34 - 4 = 30. "alpha bravo charlie delta
// echo" is exactly 30 characters, so "foxtrot" is the first word that would
// push past the limit and must start a new, indented line.
const wrapWords = "alpha bravo charlie delta echo foxtrot golf hotel"

func TestWrapIndentBreaksBeforeExceedingTheAvailableWidth(t *testing.T) {
	got := wrapIndent(wrapWords, 4, 34)
	want := "alpha bravo charlie delta echo\n    foxtrot golf hotel"
	if got != want {
		t.Fatalf("wrapIndent = %q, want %q", got, want)
	}
}

// Continuation lines are padded to the given indent so wrapped prose lines up
// under the label that introduced it, not flush against the left margin.
func TestWrapIndentPadsContinuationLinesToTheIndent(t *testing.T) {
	got := wrapIndent(wrapWords, 4, 34)
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("wrapIndent did not wrap at all: %q", got)
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "    ") {
			t.Fatalf("continuation line %q is not padded to the 4-space indent", l)
		}
	}
}

// A width narrower than what any word could fit in must not spin or panic:
// the function clamps its internal limit to 30 rather than wrapping every
// single character onto its own line. With indent=0 it must wrap at exactly
// the same word as the explicit-limit-30 case above.
func TestWrapIndentClampsAnUnreasonablyNarrowWidth(t *testing.T) {
	got := wrapIndent(wrapWords, 0, 1)
	want := "alpha bravo charlie delta echo\nfoxtrot golf hotel"
	if got != want {
		t.Fatalf("wrapIndent with an unusably narrow width = %q, want the same wrap point as a clamped 30-column limit %q", got, want)
	}
}
