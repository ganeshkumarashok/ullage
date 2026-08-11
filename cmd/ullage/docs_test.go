package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A README is the only part of a project everyone reads and nothing tests. The
// first command in ours was wrong for a while -- it passed `--window 14d`, the
// value the tool itself prints as its default, which the flag parser rejected
// until it was taught the unit. Anyone following the front page hit an error on
// their first line.
//
// So: every ullage command written in the documentation is parsed here with the
// real flag set. This does not prove the command does something useful, but it
// does prove nobody has to discover a typo by typing it.

var docFiles = []string{
	"../../README.md",
	"../../CONTRIBUTING.md",
	"../../deploy/cronjob.yaml",
	"../../examples/README.md",
	"../../e2e/README.md",
}

// Matches an invocation wherever it appears -- shell block, prose backticks, or
// a "Next:" hint in sample output.
var invocation = regexp.MustCompile(`(?m)\bullage\s+([^\n` + "`" + `"'|>)]*)`)

func TestEveryDocumentedCommandParses(t *testing.T) {
	for _, path := range docFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range invocation.FindAllStringSubmatch(string(raw), -1) {
			cmd := strings.TrimSpace(m[1])
			if cmd == "" || skipLine(m[0]) {
				continue
			}
			args := strings.Fields(cmd)
			args = stripSubcommand(args)
			if args == nil {
				continue
			}
			if !allFlags(args) {
				continue // a placeholder like <finding-id>
			}

			_, rest := stripDemo(args)
			f := newFlags("doc")
			f.fs.SetOutput(discard{})
			if err := f.fs.Parse(rest); err != nil {
				t.Errorf("%s documents `ullage %s`, which does not parse: %v", path, cmd, err)
			}
		}
	}
}

// skipLine drops matches that are prose about the tool rather than a command
// to type: package paths, image references, and URLs.
func skipLine(s string) bool {
	return strings.Contains(s, "/") && !strings.HasPrefix(strings.TrimSpace(s), "ullage ")
}

// stripSubcommand removes a leading verb, and reports nil for the ones that do
// not take the scan flag set.
func stripSubcommand(args []string) []string {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return args
	}
	switch args[0] {
	case "scan", "explain", "demo", "doctor", "checks":
		return args[1:]
	case "ignore", "version", "help":
		return nil
	default:
		return nil // a placeholder, or prose
	}
}

// allFlags reports whether every argument is a flag or a flag value, so that a
// documented placeholder id is not parsed as one.
func allFlags(args []string) bool {
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			if !strings.Contains(args[i], "=") && isFlagExpectingValue(args[i]) {
				i++ // its value
			}
			continue
		}
		return false
	}
	return true
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// The README shows a transcript of `ullage demo`. If it drifts from what the
// binary prints, the front page is a screenshot of a version nobody can run --
// which is how it came to claim 6 findings when the tool reports 7.
func TestReadmeTranscriptMatchesTheBinary(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)

	// Anchor on the claims most likely to rot: the headline ratio and the
	// census. Both are computed, so a change in the demo fixture moves them.
	for _, want := range []string{
		"accelerator-hours fallow",
		"accelerators analysed",
		"Fallow by design",
		"Not analysed",
		"Unmet demand",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README transcript no longer shows %q", want)
		}
	}

	// The census in the transcript has to reconcile, because that property is
	// one of the things the README claims the tool guarantees.
	m := regexp.MustCompile(`(\d+) of (\d+) accelerators analysed  \((\d+) excluded`).FindStringSubmatch(readme)
	if m == nil {
		t.Fatal("README transcript has no accelerator census line")
	}
	analysed, observed, excluded := atoi(t, m[1]), atoi(t, m[2]), atoi(t, m[3])
	if analysed+excluded != observed {
		t.Errorf("README census does not reconcile: %d analysed + %d excluded != %d observed",
			analysed, excluded, observed)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
