package main

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The instant the README transcripts are generated at.
//
// The demo cluster floats with the wall clock, so without pinning it the
// documented output is stale an hour after it is written and there is no way
// to tell an intentional change from drift. Pinned, the transcript becomes an
// assertion: re-run the command, diff the result, fail if they differ.
const readmeDemoNow = "2026-08-11T07:00:00Z"

// TestREADMETranscriptMatchesTheTool re-runs the command the README claims to
// have run and compares the output byte for byte.
//
// This is the test that stops the front page lying. A README that shows output
// the tool no longer produces is the fastest way to lose a reader who is
// evaluating whether to trust the numbers, and it happens silently: nothing
// else in the build reads prose. Changing the explain renderer now fails here
// until the README is regenerated with it.
func TestREADMETranscriptMatchesTheTool(t *testing.T) {
	documented := readmeTranscript(t, "Stop it happening again")

	t.Setenv("ULLAGE_DEMO_NOW", readmeDemoNow)
	actual := captureStdout(t, func() {
		if err := run([]string{"explain", "research/jupyter-alice", "--demo"}); err != nil {
			t.Fatalf("running the documented command: %v", err)
		}
	})

	if got, want := normalise(actual), normalise(documented); got != want {
		t.Errorf("README transcript no longer matches `ullage explain`.\n\n"+
			"Regenerate it with:\n"+
			"  ULLAGE_DEMO_NOW=%s go run ./cmd/ullage explain research/jupyter-alice --demo\n\n"+
			"%s", readmeDemoNow, firstDifference(want, got))
	}
}

// readmeTranscript returns the fenced block holding the explain output.
//
// It is selected by a line only that output contains, not by position: the
// transcript lives in a <details> fence separate from the fence showing the
// command, and both of those are presentation choices the README should stay
// free to change without breaking this test.
func readmeTranscript(t *testing.T, marker string) string {
	t.Helper()
	src, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}

	var found []string
	for _, b := range regexp.MustCompile("(?s)```[a-z]*\n(.*?)\n```").FindAllStringSubmatch(string(src), -1) {
		if strings.Contains(b[1], marker) {
			found = append(found, b[1])
		}
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("no fenced block in the README contains %q — "+
			"if the transcript was removed, remove this test with it", marker)
	default:
		t.Fatalf("%d fenced blocks contain %q; the marker no longer identifies one transcript", len(found), marker)
	}
	return ""
}

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what it
// wrote. The renderer writes to os.Stdout directly, which is the right thing
// for a CLI and means a test has to redirect the real file descriptor.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		_ = r.Close()
		done <- buf.String()
	}()

	fn()

	os.Stdout = saved
	// The reader only sees EOF once the write end is closed, so this has to
	// happen before the receive or the test deadlocks.
	_ = w.Close()
	return <-done
}

// normalise trims leading and trailing blank lines and trailing whitespace on
// every line, so that Markdown fence padding is not mistaken for a change in
// the tool's output.
func normalise(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// firstDifference reports the first differing line with a little context, which
// is far more useful in CI than two sixty-line blocks.
func firstDifference(want, got string) string {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			var b strings.Builder
			b.WriteString("first difference at line ")
			b.WriteString(strconv.Itoa(i + 1))
			b.WriteString(" of the transcript:\n")
			b.WriteString("  README: " + wl + "\n")
			b.WriteString("  tool:   " + gl + "\n")
			return b.String()
		}
	}
	return "transcripts differ only in length"
}

// `--version` is what people type, and what every install script and package
// manager probe uses to confirm a binary works. Before this it reached the flag
// parser and produced "flag provided but not defined: -version" on exit 2 --
// a failed first command, and a failed automated check.
func TestVersionAndHelpAnswerToTheUsualSpellings(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-version", "-V"} {
		var err error
		out := captureStdout(t, func() { err = run([]string{arg}) })
		if err != nil {
			t.Errorf("ullage %s: %v", arg, err)
			continue
		}
		if !strings.HasPrefix(out, "ullage v") {
			t.Errorf("ullage %s printed %q, want a version line", arg, firstLine(out))
		}
	}
	for _, arg := range []string{"help", "--help", "-h", "-help"} {
		var err error
		out := captureStdout(t, func() { err = run([]string{arg}) })
		if err != nil {
			t.Errorf("ullage %s: %v", arg, err)
			continue
		}
		if !strings.Contains(out, "Usage") && !strings.Contains(out, "ullage") {
			t.Errorf("ullage %s printed %q, want the usage text", arg, firstLine(out))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// The `ullage demo` summary at the top of the README carried "5.9k of 23k
// accelerator-hours unused (26%)" for a while after the paid-hours arithmetic
// started charging nodes for the time they existed rather than for the whole
// window. Nothing caught it: the explain transcript was pinned, and `make
// check` only asserted that the demo exits cleanly, never that it printed what
// the front page claims.
//
// That headline is the first number a reader sees, immediately above the
// sentence "every number above is computed, not canned". It has to be true.
func TestREADMEHeadlineMatchesTheDemo(t *testing.T) {
	t.Setenv("ULLAGE_DEMO_NOW", readmeDemoNow)
	actual := captureStdout(t, func() {
		if err := run([]string{"demo", "--exit-zero"}); err != nil {
			t.Fatalf("running the demo: %v", err)
		}
	})

	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(raw)

	summary := regexp.MustCompile(`(?m)^\s*[\d.]+k? of [\d.]+k? accelerator-hours unused \(\d+%\)\s*$`)
	line := summary.FindString(actual)
	if line == "" {
		t.Fatalf("the demo no longer prints a unused summary line; this test can no "+
			"longer pin the README headline.\n\n%s", actual)
	}
	want := strings.TrimSpace(line)

	if !strings.Contains(readme, want) {
		t.Errorf("the demo prints %q, which appears nowhere in the README.\n\n"+
			"The headline and the alt text on the hero image both quote this number. "+
			"Regenerate them with:\n"+
			"  ULLAGE_DEMO_NOW=%s go run ./cmd/ullage demo --exit-zero",
			want, readmeDemoNow)
	}

	// The accelerator census sits directly under it and drifts for the same
	// reasons.
	census := regexp.MustCompile(`(?m)^\s*\d+ of \d+ accelerators analysed.*$`)
	if c := strings.TrimSpace(census.FindString(actual)); c != "" && !strings.Contains(readme, c) {
		t.Errorf("the demo prints %q, which appears nowhere in the README", c)
	}
}

// Every section heading the demo prints has to appear in the README transcript.
//
// Renaming a label in the renderer used to leave the front page quoting output
// the tool no longer produced. It happened with "Unused by design", which the
// code had started printing as "Reserved on purpose": the headline test pinned
// only the summary and census lines, so nothing noticed that the rest of the
// transcript had gone stale.
func TestREADMEShowsTheSectionsTheDemoActuallyPrints(t *testing.T) {
	t.Setenv("ULLAGE_DEMO_NOW", readmeDemoNow)
	actual := captureStdout(t, func() {
		if err := run([]string{"demo", "--exit-zero"}); err != nil {
			t.Fatalf("running the demo: %v", err)
		}
	})

	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(raw)

	heading := regexp.MustCompile(`(?m)^  [A-Z][a-z].*$`)
	found := heading.FindAllString(actual, -1)
	if len(found) == 0 {
		t.Fatal("the demo printed no section headings; this test can no longer pin anything")
	}

	for _, h := range found {
		if !strings.Contains(readme, strings.TrimRight(h, " ")) {
			t.Errorf("the demo prints %q, which appears nowhere in the README.\n"+
				"Regenerate the transcript with:\n"+
				"  ULLAGE_DEMO_NOW=%s go run ./cmd/ullage demo --exit-zero",
				strings.TrimSpace(h), readmeDemoNow)
		}
	}
}
