package main

import (
	"reflect"
	"strings"
	"testing"
)

// `explain --demo <id>` used to take "--demo" as the finding id and then report,
// confusingly, that no such finding existed. Both orders are things people type.
func TestSplitPositionalFindsTheIDWhereverItIs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantID  string
		wantRes []string
	}{
		{"id first", []string{"idle-pod/ml/a", "--demo"}, "idle-pod/ml/a", []string{"--demo"}},
		{"flag first", []string{"--demo", "idle-pod/ml/a"}, "idle-pod/ml/a", []string{"--demo"}},
		{"id alone", []string{"idle-pod/ml/a"}, "idle-pod/ml/a", []string{}},
		{"no id", []string{"--demo"}, "", []string{"--demo"}},
		{"nothing", []string{}, "", []string{}},
		{
			"boolean flags do not swallow the id",
			[]string{"--demo", "--no-cost", "idle-pod/ml/a"},
			"idle-pod/ml/a", []string{"--demo", "--no-cost"},
		},
		{
			// --config takes a value, so the word after it is that value and
			// must not be mistaken for the finding id.
			"a flag's value is not the id",
			[]string{"--config", "team.yaml", "idle-pod/ml/a"},
			"idle-pod/ml/a", []string{"--config", "team.yaml"},
		},
		{
			"attached form still leaves the id findable",
			[]string{"--config=team.yaml", "idle-pod/ml/a"},
			"idle-pod/ml/a", []string{"--config=team.yaml"},
		},
		{
			// The id can begin with a digit or contain slashes; neither makes
			// it look like a flag.
			"ids with slashes",
			[]string{"unused-node/pool/gpu-1"},
			"unused-node/pool/gpu-1", []string{},
		},
		{
			"only the first bare word is taken",
			[]string{"idle-pod/ml/a", "stray"},
			"idle-pod/ml/a", []string{"stray"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, rest := splitPositional(tc.args)
			if id != tc.wantID {
				t.Errorf("id = %q, want %q", id, tc.wantID)
			}
			if !reflect.DeepEqual(rest, tc.wantRes) {
				t.Errorf("rest = %v, want %v", rest, tc.wantRes)
			}
		})
	}
}

func TestStripDemo(t *testing.T) {
	for _, form := range []string{"--demo", "-demo"} {
		on, rest := stripDemo([]string{"--top", "3", form})
		if !on {
			t.Errorf("%s not recognised", form)
		}
		if !reflect.DeepEqual(rest, []string{"--top", "3"}) {
			t.Errorf("rest = %v", rest)
		}
	}
	if on, _ := stripDemo([]string{"--top", "3"}); on {
		t.Error("demo reported without the flag")
	}
}

// The tool tells people to run flags that --help never mentioned, which is the
// same as not having them. Every flag named in an error message, a remedy, or
// the README has to be findable in the one place people look first.
func TestHelpDocumentsTheFlagsWeTellPeopleToUse(t *testing.T) {
	must := []string{
		"--prometheus", "--window", "--output", "--top", "--namespace",
		"--checks", "--min-confidence", "--config", "--explain-queries",
		"--no-cost", "--kubeconfig", "--api-server", "--pricing",
		"--exit-zero", "--quiet", "--no-color", "--prometheus-token-file",
		"--prometheus-auth", "--insecure-skip-tls-verify", "--context",
		"--idle-threshold", "--stuck-threshold", "--step", "--timeout", "--trace",
	}
	for _, flag := range must {
		if !strings.Contains(usage, flag) {
			t.Errorf("%s is accepted but absent from --help", flag)
		}
	}
	for _, cmd := range []string{"explain", "demo", "doctor", "ignore", "checks", "version"} {
		if !strings.Contains(usage, "ullage "+cmd) {
			t.Errorf("subcommand %q absent from --help", cmd)
		}
	}
}

// A flag that --help documents but the parser rejects is worse than an
// undocumented one: it fails at the moment someone follows instructions.
func TestEveryDocumentedFlagParses(t *testing.T) {
	for _, line := range strings.Split(usage, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "--") {
			continue
		}
		name := strings.TrimPrefix(strings.Fields(trimmed)[0], "--")
		if name == "demo" {
			continue // a mode, handled before flag parsing
		}
		f := newFlags("test")
		if f.fs.Lookup(name) == nil {
			t.Errorf("--help documents --%s, but the parser does not accept it", name)
		}
	}
}

// --min-confidence used to accept anything. An unrecognised level was read as
// the zero value, which is the most permissive bar there is, so a typo -- or
// the entirely reasonable "Medium" -- silently published the low-confidence
// findings the operator believed they had just filtered out. Nothing else in
// the tool fails in that direction, and this one recommends deleting nodes.
func TestAnUnknownMinConfidenceIsRejectedRatherThanFailingOpen(t *testing.T) {
	f := &flags{minConfidence: "bogus", noCost: true}
	_, err := f.options()
	if err == nil {
		t.Fatal("--min-confidence bogus was accepted; an unrecognised bar must be " +
			"rejected, never read as the most permissive setting")
	}
	for _, want := range []string{"bogus", "high", "medium", "low"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; it must name what was rejected "+
				"and what the valid levels are", err, want)
		}
	}
}

// Rejecting "Medium" would be safe but needlessly hostile, so it is accepted
// and normalised. This is the case a user is most likely to actually type.
func TestMinConfidenceIsCaseInsensitive(t *testing.T) {
	for _, in := range []string{"Medium", "MEDIUM", " medium "} {
		f := &flags{minConfidence: in, noCost: true}
		o, err := f.options()
		if err != nil {
			t.Fatalf("--min-confidence %q was rejected: %v", in, err)
		}
		if o.MinConfidence != "medium" {
			t.Errorf("--min-confidence %q became %q, want %q", in, o.MinConfidence, "medium")
		}
	}
}
