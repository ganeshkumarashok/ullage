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
			_, rest = stripServe(rest)
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

// The front page now carries a nav bar, and section links are the first thing
// to rot when a heading is reworded: nothing breaks loudly, the link just
// stops going anywhere.
var (
	internalLink = regexp.MustCompile(`\]\(#([a-z0-9-]+)\)`)
	heading      = regexp.MustCompile(`(?m)^#{2,4} (.+)$`)
)

func TestInternalLinksResolve(t *testing.T) {
	for _, path := range []string{"../../README.md", "../../CONTRIBUTING.md", "../../examples/README.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		doc := string(raw)

		anchors := map[string]bool{}
		for _, h := range heading.FindAllStringSubmatch(doc, -1) {
			anchors[slug(h[1])] = true
		}
		for _, l := range internalLink.FindAllStringSubmatch(doc, -1) {
			if !anchors[l[1]] {
				t.Errorf("%s links to #%s, which no heading produces", path, l[1])
			}
		}
	}
}

// slug reproduces the anchor GitHub derives from a heading: lowercased,
// punctuation dropped, spaces hyphenated.
func slug(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(h)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// --help is the first thing many people run, and every flag it lists is a
// promise the parser has to keep. It listed "--demo" as a common flag: that
// works on `ullage explain`, where it is stripped by hand, but the top-level
// scan rejects it with "flag provided but not defined: -demo" -- a stranger's
// very first command failing on something the help just told them to use.
//
// The shipped ci-gate.sh example made exactly this mistake, which is a good
// sign the help text taught it.
func TestEveryFlagInTheHelpTextIsRealAtTheTopLevel(t *testing.T) {
	// Every mention counts, not just the flag column. The --demo that caused
	// this appeared mid-sentence in another flag's description, which is
	// exactly where a reader picks a flag up and exactly where a check
	// looking only at the left-hand column will not find it.
	re := regexp.MustCompile(`(--[a-z0-9-]+)`)

	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(usage, -1) {
		name := strings.TrimPrefix(m[1], "--")
		if seen[name] {
			continue
		}
		seen[name] = true

		f := newFlags("help-check")
		f.fs.SetOutput(discard{})
		if f.fs.Lookup(name) == nil {
			t.Errorf("--help mentions %q, but the top-level flag set "+
				"has no such flag. Someone's first command will fail with "+
				"\"flag provided but not defined\" on something the help told them to type. "+
				"A flag that only works on a subcommand does not belong in the top-level "+
				"help: name the subcommand instead.", m[1])
		}
	}
	if len(seen) == 0 {
		t.Fatal("no flags were found in the usage text, so this test is checking nothing")
	}
}
