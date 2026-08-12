package main

import (
	"bytes"
	"strings"
	"testing"
)

// The scan asks the same question of every device, so the raw query list is
// mostly repetition. Printing it verbatim buried nine real queries in eighty-odd
// lines, on the one surface whose entire purpose is letting a sceptic check the
// work.
func TestExplainQueriesPrintsEachQueryOnce(t *testing.T) {
	queries := []string{
		"max_over_time(DCGM_FI_DEV_GPU_UTIL[1d])",
		"max_over_time(DCGM_FI_DEV_GPU_UTIL[1d])",
		"max_over_time(DCGM_FI_DEV_GPU_UTIL[1d])",
		"avg_over_time(DCGM_FI_DEV_POWER_USAGE[1d])",
		"max_over_time(DCGM_FI_DEV_GPU_UTIL[1d])",
		"avg_over_time(DCGM_FI_DEV_POWER_USAGE[1d])",
	}

	var buf bytes.Buffer
	explainQueries(&buf, queries)
	out := buf.String()

	for _, q := range []string{
		"max_over_time(DCGM_FI_DEV_GPU_UTIL[1d])",
		"avg_over_time(DCGM_FI_DEV_POWER_USAGE[1d])",
	} {
		if got := strings.Count(out, q+"\n"); got != 1 {
			t.Errorf("query %q printed %d times, want exactly once:\n%s", q, got, out)
		}
	}
	if !strings.Contains(out, "2 distinct queries") {
		t.Errorf("output does not report the distinct count:\n%s", out)
	}
}

// A bare PromQL string is only evaluable by someone who already knows the DCGM
// field names, which is not the person who needs to be convinced.
func TestEveryQueryTheScanIssuesIsExplained(t *testing.T) {
	t.Setenv("ULLAGE_DEMO_NOW", readmeDemoNow)
	out := captureStdout(t, func() {
		if err := run([]string{"demo", "--explain-queries"}); err != nil {
			t.Fatalf("running the documented command: %v", err)
		}
	})

	var (
		queries   []string
		explained int
		prev      string
	)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "DCGM_") {
			prev = line
			continue
		}
		queries = append(queries, line)
		if strings.HasPrefix(prev, "#") {
			explained++
		} else {
			t.Errorf("query is printed with no explanation of what it answers: %s", line)
		}
		prev = line
	}

	if len(queries) == 0 {
		t.Fatal("--explain-queries printed no queries at all")
	}
	if explained != len(queries) {
		t.Errorf("%d of %d queries explained", explained, len(queries))
	}

	seen := map[string]bool{}
	for _, q := range queries {
		if seen[q] {
			t.Errorf("query printed more than once: %s", q)
		}
		seen[q] = true
	}
}
