package render

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// TestRedactRemovesEveryIdentifier is the assertion the feature actually owes
// its user. Rather than checking the handful of fields someone thought of, it
// harvests every identifier out of the input and requires that none of them
// survives anywhere in the document.
//
// If a future field carries a cluster name into the report and nobody
// remembers to redact it, this fails.
func TestRedactRemovesEveryIdentifier(t *testing.T) {
	res := redactFixture()

	var buf bytes.Buffer
	if err := HTML(&buf, res, HTMLOptions{Redact: true}); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	for _, name := range identifiersIn(t, res) {
		if strings.Contains(out, name) {
			t.Errorf("redacted report still contains %q", name)
		}
	}
}

// TestRedactKeepsTheReportUsable guards the other half: a redacted report that
// has had its structure or its arithmetic destroyed is not a redacted report,
// it is a broken one.
func TestRedactKeepsTheReportUsable(t *testing.T) {
	res := redactFixture()

	var buf bytes.Buffer
	if err := HTML(&buf, res, HTMLOptions{Redact: true}); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"color-scheme: light dark", // the stylesheet survived the sweep
		"</html>",
		"idle pod", // check names are ours, not the cluster's
		"accelerator-hours",
		"14d", // durations are not identifiers
	} {
		if !strings.Contains(out, want) {
			t.Errorf("redacted report lost %q", want)
		}
	}

	// The unused total is a number, not a name, and removing it would defeat
	// the purpose of sharing the report at all.
	if !strings.Contains(out, hours(res.Scan.GPUHoursUnused)) {
		t.Errorf("redacted report lost the unused total %q", hours(res.Scan.GPUHoursUnused))
	}
}

// TestRedactIsOffOtherwise proves the flag is doing the work, so that a
// passing redaction test cannot be satisfied by a renderer that omits the
// names in both modes.
func TestRedactIsOffOtherwise(t *testing.T) {
	res := redactFixture()

	var buf bytes.Buffer
	if err := HTML(&buf, res, HTMLOptions{}); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"ml-platform", "finetune-carol", "alice@example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("unredacted report is missing %q; the redaction test would pass vacuously", want)
		}
	}
}

// TestRedactSurvivesNamesThatCollideWithProse pins the boundary behaviour. A
// namespace called "gpu" must be removed where it is the namespace, and must
// not shred the surrounding English.
func TestRedactSurvivesNamesThatCollideWithProse(t *testing.T) {
	res := redactFixture()
	res.Recommendations[0].Workload.Namespace = "gpu"
	res.Recommendations[0].Summary = "gpu/thing holds a gpu-enabled accelerator"

	var buf bytes.Buffer
	if err := HTML(&buf, res, HTMLOptions{Redact: true}); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	// "gpu-enabled" is a different token and must be left alone, as must the
	// word inside "GPU hours" prose elsewhere in the document.
	if !strings.Contains(out, "gpu-enabled") {
		t.Error("whole-token matching failed: 'gpu-enabled' was rewritten")
	}
	if strings.Contains(out, "gpu/thing") {
		t.Error("the namespace 'gpu' survived where it was an identifier")
	}
}

// TestRedactLeavesTheStylesheetAlone covers a failure that is invisible until
// somebody unlucky runs it: the sweep walks the whole document, and the
// document carries our own stylesheet and script. A workload named after any
// CSS keyword would otherwise punch holes in the styling of every report that
// mentioned it, and the reader would see a broken page rather than a redacted
// one.
func TestRedactLeavesTheStylesheetAlone(t *testing.T) {
	res := redactFixture()
	res.Recommendations[0].Workload.Name = "dark"
	res.Recommendations[0].Workload.Namespace = "grid"

	var buf bytes.Buffer
	if err := HTML(&buf, res, HTMLOptions{Redact: true}); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	out := buf.String()

	// Both names occur in the compiled-in stylesheet, which must be untouched.
	for _, want := range []string{"color-scheme: light dark", "grid"} {
		if !strings.Contains(out, want) {
			t.Errorf("the sweep damaged the stylesheet: %q is missing", want)
		}
	}

	// And they must still be gone from the parts that came from the cluster.
	if strings.Contains(out, "grid/dark") {
		t.Error("the workload reference survived redaction")
	}
}

// identifiersIn collects every name the fixture put into the result, by
// walking the serialised form. Deriving the list from the data rather than
// restating it here is what stops the test drifting away from the fixture.
func identifiersIn(t *testing.T, res *api.Result) []string {
	t.Helper()

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var walk func(v any, key string, out map[string]bool)
	// Only fields that hold names are harvested. Free prose is not an
	// identifier, and a summary sentence legitimately shares words with the
	// document's own text.
	nameKeys := map[string]bool{
		"namespace": true, "name": true, "identity": true,
		"context": true, "rootName": true, "targets": true,
		"members": true,
	}
	walk = func(v any, key string, out map[string]bool) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				// The tool block holds our own name and version, which the
				// report is supposed to state.
				if k == "tool" {
					continue
				}
				walk(vv, k, out)
			}
		case []any:
			for _, vv := range x {
				walk(vv, key, out)
			}
		case string:
			if !nameKeys[key] {
				return
			}
			for _, part := range regexp.MustCompile(`[/ ]`).Split(x, -1) {
				if len(part) >= 2 {
					out[part] = true
				}
			}
		}
	}
	var any0 any
	if err := json.Unmarshal(b, &any0); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	set := map[string]bool{}
	walk(any0, "", set)
	if len(set) < 5 {
		t.Fatalf("fixture yielded only %d identifiers; the test would prove nothing", len(set))
	}

	var names []string
	for n := range set {
		names = append(names, n)
	}
	return names
}

func redactFixture() *api.Result {
	cost := 1234.0
	return &api.Result{
		APIVersion: "ullage.dev/v1alpha1",
		Scan: api.ScanMeta{
			Tool:                 api.Tool{Name: "ullage", Version: "test"},
			Context:              "prod-eu-west-1",
			Window:               api.ISODuration(336 * 3600 * 1e9),
			PrometheusURL:        "http://prom.monitoring.svc:9090",
			AcceleratorsObserved: 8,
			AcceleratorsAnalyzed: 8,
			GPUHoursPaid:         2688,
			GPUHoursUnused:       672,
		},
		Recommendations: []api.Finding{{
			Rank:    1,
			ID:      "idle-pod/ml-platform/finetune-carol",
			Check:   "idle-pod",
			Summary: "ml-platform/finetune-carol: 2 accelerators held with no work for 14d",
			Because: "held 2 accelerators and did no work on them",
			Workload: api.Workload{
				Namespace: "ml-platform",
				Kind:      "Notebook",
				Name:      "finetune-carol",
				Grouped:   1,
				Members:   []string{"finetune-notebook-carol-0"},
			},
			Owner:    api.Owner{Identity: "alice@example.com", ResolvedVia: "label"},
			Impact:   api.Impact{GPUHoursUnused: 672, WindowCost: &cost, Currency: "USD"},
			Evidence: api.Evidence{Window: api.ISODuration(336 * 3600 * 1e9), UnusedDuration: api.ISODuration(336 * 3600 * 1e9), SampleCompleteness: 0.99},
			Fix: api.Fix{
				Targets:   "ml-platform/finetune-carol",
				Command:   "kubectl delete pod -n ml-platform finetune-notebook-carol-0",
				Rationale: "the pod holds accelerators it is not using",
			},
			Provenance:          api.Provenance{Controlled: true, RootKind: "Notebook", RootName: "finetune-carol"},
			EvidenceConfidence:  "high",
			OwnershipConfidence: "high",
		}},
	}
}
