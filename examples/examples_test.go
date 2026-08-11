// Package examples_test runs the shipped example scripts against a stub binary
// and checks the arguments they build.
//
// These scripts are the first thing a stranger runs, and until this test they
// were broken in the one mode nobody exercised. Both built their argument list
// by interpolating an unquoted `${PROMETHEUS:+--prometheus "$PROMETHEUS"}`
// followed by `${PROMETHEUS:-demo}`, which is `demo` when the variable is unset
// and *the URL again* when it is set. So against a real Prometheus the command
// became:
//
//	ullage --prometheus http://prom:9090 http://prom:9090 --output json ...
//
// Go's flag package stops parsing at the first non-flag argument. The stray URL
// was not rejected; it made `--output json` vanish, and jq was handed a page of
// human-readable text. The scripts worked perfectly in demo mode, which is the
// only mode a contributor ever tries.
//
// Nothing here needs a cluster: a stub on PATH records what it was asked to do.
package examples_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stub writes a fake ullage that appends its argv to a file, one line per
// invocation, then emits the given stdout so the script can proceed.
func stub(t *testing.T, stdout string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "argv")
	bin = filepath.Join(dir, "ullage")

	script := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> " + log + "\ncat <<'JSON'\n" + stdout + "\nJSON\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// scanHeader is every field the example scripts read out of `scan`. It is
// spelled out rather than trimmed to the minimum so that a script reaching for
// a field nobody thought about fails here instead of in somebody's pipeline.
const scanHeader = `"scan": {
    "tool": {"name": "ullage", "version": "v0.0.0-test"},
    "context": "test-cluster",
    "started": "2026-08-11T07:00:00Z",
    "window": "P14D",
    "acceleratorsObserved": 8,
    "acceleratorsAnalyzed": 8,
    "gpuHoursPaid": 2688,
    "gpuHoursFallow": 0
  }`

// emptyReport is a well-formed scan that found nothing, so a gate script runs
// all the way to its exit decision rather than bailing early.
const emptyReport = `{
  "apiVersion": "ullage.dev/v0.1",
  ` + scanHeader + `,
  "recommendations": [],
  "byDesign": [],
  "suppressed": [],
  "notAnalyzed": [],
  "warnings": [],
  "belowThreshold": [],
  "unmetDemand": [],
  "pricing": {}
}`

func run(t *testing.T, script, bin string, env ...string) {
	t.Helper()
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "ULLAGE="+bin)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	// Exit 1 is a budget failure, which is a legitimate outcome; only a crash
	// or a usage error matters here.
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() > 1 {
			t.Fatalf("%s exited %v\n%s", filepath.Base(script), err, out)
		}
	}
}

func firstInvocation(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the script never invoked ullage at all: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 {
		t.Fatal("the script never invoked ullage at all")
	}
	return lines[0]
}

func TestExamplesPassTheirFlagsThroughAgainstARealPrometheus(t *testing.T) {
	const url = "http://prom.example:9090"

	for _, script := range []string{"ci-gate.sh", "weekly-digest.sh"} {
		t.Run(script, func(t *testing.T) {
			bin, log := stub(t, emptyReport)
			run(t, script, bin, "PROMETHEUS="+url, "BUDGET_USD=100000")

			argv := firstInvocation(t, log)

			if !strings.Contains(argv, "--output json") {
				t.Fatalf("ullage was invoked as:\n  ullage %s\n"+
					"--output json never reached it, so the script is parsing human text as JSON", argv)
			}
			if n := strings.Count(argv, url); n != 1 {
				t.Fatalf("ullage was invoked as:\n  ullage %s\n"+
					"the Prometheus URL appears %d times; a bare one is a positional argument "+
					"and Go's flag package stops parsing there", argv, n)
			}
			if strings.Contains(argv, "demo") {
				t.Fatalf("ullage was invoked as:\n  ullage %s\n"+
					"a real --prometheus URL was given but the script still asked for the demo cluster", argv)
			}
		})
	}
}

// The demo path is what a contributor runs first, so it has to keep working.
func TestExamplesRunTheDemoWhenNoPrometheusIsSet(t *testing.T) {
	for _, script := range []string{"ci-gate.sh", "weekly-digest.sh"} {
		t.Run(script, func(t *testing.T) {
			bin, log := stub(t, emptyReport)
			run(t, script, bin, "PROMETHEUS=", "BUDGET_USD=100000")

			argv := firstInvocation(t, log)
			if !strings.Contains(argv, "demo") {
				t.Fatalf("ullage was invoked as:\n  ullage %s\nwant the demo subcommand", argv)
			}
			if strings.Contains(argv, "--prometheus") {
				t.Fatalf("ullage was invoked as:\n  ullage %s\n"+
					"no PROMETHEUS was set but the script asked for one anyway", argv)
			}
			if !strings.Contains(argv, "--output json") {
				t.Fatalf("ullage was invoked as:\n  ullage %s\nwant --output json", argv)
			}
		})
	}
}

// A finding ullage could not price is not a finding worth nothing. The gate
// sums `windowCost`, and `// 0` in jq turns an unknown price into zero, so the
// single most expensive thing in the cluster -- an accelerator so new the price
// book has never heard of it -- was the one guaranteed to pass a dollar budget.
func TestTheBudgetGateWillNotWaveThroughAFindingItCannotPrice(t *testing.T) {
	const unpriced = `{
  "apiVersion": "ullage.dev/v0.1",
  ` + scanHeader + `,
  "recommendations": [
    {"id": "idle-pod/research/big",
     "summary": "3 pods, no GPU work since the window began",
     "impact": {"gpuHoursFallow": 3000},
     "owner": {"identity": "alice@example.com"},
     "fix": {"command": "kubectl scale statefulset -n research big --replicas=0"}}
  ],
  "byDesign": [], "suppressed": [], "notAnalyzed": [], "warnings": [],
  "belowThreshold": [], "unmetDemand": [], "pricing": {}
}`
	bin, _ := stub(t, unpriced)

	cmd := exec.Command("bash", "ci-gate.sh")
	cmd.Env = append(os.Environ(), "ULLAGE="+bin, "BUDGET_USD=500", "PROMETHEUS=http://prom.example:9090")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("the gate passed a finding it could not price:\n%s\n"+
			"3000 unpriced accelerator-hours summed to $0 and slipped under a $500 budget", out)
	}
	if !strings.Contains(string(out), "no price") {
		t.Fatalf("the gate failed, but not for a reason anyone could act on:\n%s", out)
	}
}
