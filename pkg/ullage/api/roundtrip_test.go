package api

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestISO8601RoundTrip(t *testing.T) {
	cases := []time.Duration{
		0,
		time.Second,
		15 * time.Second,
		time.Minute,
		90 * time.Second,
		time.Hour,
		6*time.Hour + 30*time.Minute,
		24 * time.Hour,
		14 * 24 * time.Hour,
		13*24*time.Hour + 21*time.Hour,
		30*24*time.Hour + 5*time.Hour + 7*time.Minute + 9*time.Second,
	}
	for _, want := range cases {
		s := ISO8601(want)
		got, err := ParseISO8601(s)
		if err != nil {
			t.Fatalf("ISO8601(%v) = %q, which will not parse back: %v", want, s, err)
		}
		if got != want {
			t.Errorf("round trip of %v via %q gave %v", want, s, got)
		}
	}
}

// A step of fifteen seconds used to render as PT0S. The params block claims to
// be everything needed to reproduce a scan, so a step that reads as zero is not
// a rounding nicety, it is a false claim.
func TestSubMinuteStepSurvives(t *testing.T) {
	if got := ISO8601(15 * time.Second); got != "PT15S" {
		t.Fatalf("15s rendered as %q, want PT15S", got)
	}
}

func TestParseISO8601Rejects(t *testing.T) {
	// Weeks, months and years have no fixed length in hours. Guessing one
	// would put an invented number into an accelerator-hour claim.
	bad := []string{
		"", "P", "PT", "14d", "P14", "PT1", "P1W", "P1Y", "P1M",
		"1D", "PD", "P-1D", "PT1X", "PP1D", "P1DT", "P1H", "PT1D",
		"PT1HT1M", "p14d", "P 1D", "P1.5D",
	}
	for _, s := range bad {
		if d, err := ParseISO8601(s); err == nil {
			t.Errorf("ParseISO8601(%q) accepted, returning %v; want an error", s, d)
		}
	}
}

func TestParseISO8601Accepts(t *testing.T) {
	cases := map[string]time.Duration{
		"PT0S":       0,
		"P14D":       14 * 24 * time.Hour,
		"PT6H30M":    6*time.Hour + 30*time.Minute,
		"P13DT21H":   13*24*time.Hour + 21*time.Hour,
		"PT15S":      15 * time.Second,
		"P1DT2H3M4S": 24*time.Hour + 2*time.Hour + 3*time.Minute + 4*time.Second,
	}
	for in, want := range cases {
		got, err := ParseISO8601(in)
		if err != nil {
			t.Fatalf("ParseISO8601(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseISO8601(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestISODurationJSONRoundTrip(t *testing.T) {
	in := ISODuration(14 * 24 * time.Hour)
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ISODuration
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("cannot decode %s that we ourselves encoded: %v", b, err)
	}
	if out != in {
		t.Errorf("round trip changed %v into %v", in, out)
	}
	if err := json.Unmarshal([]byte(`123`), &out); err == nil {
		t.Error("a bare number was accepted as a duration; the wire form is a string")
	}
}

// The point of this package is that an embedder can decode a stored ullage
// document into these types. Marshalling a fully populated Result and reading
// it back is the only test that actually checks that, and it is the test that
// was missing while every duration field was write-only.
func TestResultSurvivesRoundTrip(t *testing.T) {
	want := fullyPopulatedResult()

	first, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	var got Result
	if err := json.Unmarshal(first, &got); err != nil {
		t.Fatalf("a Result this package produced cannot be read back by this package: %v", err)
	}

	// Compare the wire form rather than the structs. Evidence.Sparkline is
	// json:"-", so a struct comparison would quietly pass over any field that
	// had been excluded from the contract by accident.
	second, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("the document changed when read back and written again.\n before: %s\n  after: %s", first, second)
	}

	if got.Scan.Window.Duration() != 14*24*time.Hour {
		t.Errorf("window decoded as %v, want 336h", got.Scan.Window.Duration())
	}
	if got.Scan.Params.Step.Duration() != 15*time.Second {
		t.Errorf("step decoded as %v, want 15s", got.Scan.Params.Step.Duration())
	}
	if len(got.Recommendations) != 1 || got.Recommendations[0].ID != want.Recommendations[0].ID {
		t.Fatalf("recommendations did not survive: %+v", got.Recommendations)
	}
	if !reflect.DeepEqual(want.Recommendations[0].Evidence.Notes, got.Recommendations[0].Evidence.Notes) {
		t.Error("evidence notes did not survive the round trip")
	}
}

func fullyPopulatedResult() Result {
	cost := 1234.5
	last := time.Date(2025, 2, 18, 8, 0, 0, 0, time.UTC)
	return Result{
		APIVersion: Version,
		Scan: ScanMeta{
			Tool:    Tool{Name: "ullage", Version: "v0.1.0", Commit: "abc1234"},
			Context: "prod-westus3",
			Started: time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC),
			Window:  ISODuration(14 * 24 * time.Hour),
			Params: Params{
				IdleThreshold:  ISODuration(24 * time.Hour),
				StuckThreshold: ISODuration(time.Hour),
				MinConfidence:  "medium",
				Step:           ISODuration(15 * time.Second),
				Checks:         []string{"idle-pod", "stuck-pod", "unused-node"},
			},
			PrometheusURL:        "http://prom:9090",
			PodLabelSchema:       "pod",
			AcceleratorsObserved: 12,
			AcceleratorsAnalyzed: 10,
			AllocationModels: AllocationCounts{
				DevicePluginExclusive: 8, TimeSliced: 1, MIG: 1, DRA: 0,
			},
			GPUHoursPaid:     4032,
			GPUHoursFallow:   1008,
			ProfilingMetrics: true,
		},
		Recommendations: []Finding{{
			Rank:                1,
			ID:                  "idle-pod/research/jupyter-alice",
			Check:               "idle-pod",
			EvidenceConfidence:  "high",
			OwnershipConfidence: "confident",
			Summary:             "no GPU work for 13d",
			Workload: Workload{
				Namespace: "research", Kind: "StatefulSet", Name: "jupyter-alice",
				Grouped: 1, Members: []string{"jupyter-alice-0"},
			},
			Accelerators: []Accelerator{{
				Model: "NVIDIA-A100-SXM4-80GB", Vendor: "nvidia", Count: 3,
				Allocation: "device-plugin-exclusive", TDPWatts: 400,
			}},
			Evidence: Evidence{
				Window:                 ISODuration(14 * 24 * time.Hour),
				FallowDuration:         ISODuration(13*24*time.Hour + 21*time.Hour),
				LastNonZeroUtilization: &last,
				UtilizationMax:         0,
				PowerDrawWatts:         56,
				PowerDrawTDPRatio:      0.14,
				SampleCompleteness:     0.98,
				Notes:                  []string{"profiling metrics unavailable"},
			},
			Impact: Impact{
				GPUHoursFallow: 1008, WindowCost: &cost, Currency: "USD",
				PricingSource: "built-in list prices", PricingScope: "list",
			},
			Owner: Owner{Identity: "alice@example.com", ResolvedVia: "namespace annotation"},
			Provenance: Provenance{
				Controlled: true, RootKind: "StatefulSet", RootName: "jupyter-alice",
				APIVersion: "apps/v1", Recognized: true,
				Chain: []OwnerRef{{Kind: "StatefulSet", Name: "jupyter-alice", APIVersion: "apps/v1"}},
			},
			Fix: Fix{
				RequiresHumanConfirmation: true,
				ConfirmWith:               "alice@example.com",
				Targets:                   FixTargetController,
				Command:                   "kubectl -n research delete statefulset jupyter-alice",
				Rationale:                 "deleting the pod would be recreated by its controller",
				Blockers:                  []Blocker{{Object: "pdb/jupyter", Reason: "minAvailable 1"}},
				Prevention:                "set an activeDeadlineSeconds on notebook sessions",
			},
			Risk: "the notebook holds unsaved state",
			Docs: "https://ullage.dev/checks/idle-pod",
		}},
		ByDesign:       []Finding{},
		Suppressed:     []Finding{},
		BelowThreshold: 2,
		NotAnalyzed: []Exclusion{{
			Code: ExclNoMetrics, Reason: "no samples in the window",
			Accelerators: 2, Detail: "node aks-gpu-3", Remedy: "check dcgm-exporter",
		}},
		UnmetDemand: &UnmetDemand{Pods: 4, Accelerators: 8, Detail: "pending for 2h"},
		Pricing: &Pricing{
			Source: "built-in list prices", Currency: "USD",
			PerSKUGPUHour: map[string]float64{"NVIDIA-A100-SXM4-80GB": 3.67},
		},
		Warnings: []string{"no cluster-autoscaler status ConfigMap was readable"},
	}
}

// A consumer that must distinguish "no capacity is held by design" from "we did
// not evaluate whether any is" cannot do it if the first serializes as null.
// This applies to a zero Result, which is the shape a code path that returns
// early produces -- the one most likely to regress.
func TestListsAreNeverNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Result
	}{
		{"zero value", Result{}},
		{"explicitly nil lists", Result{
			APIVersion:      "ullage.dev/v1alpha1",
			Recommendations: nil, ByDesign: nil, Suppressed: nil,
			NotAnalyzed: nil, Warnings: nil,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.r)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"recommendations", "byDesign", "suppressed", "notAnalyzed", "warnings"} {
				v, ok := got[k]
				if !ok {
					t.Errorf("%q is missing; it must always be present", k)
					continue
				}
				if string(v) == "null" {
					t.Errorf("%q serialized as null; it must be [] when empty", k)
				}
			}
			// The optional members must stay genuinely optional, so that
			// absence keeps meaning "not applicable".
			for _, k := range []string{"unmetDemand", "pricing"} {
				if _, ok := got[k]; ok {
					t.Errorf("%q present on an empty result; it should be omitted", k)
				}
			}
		})
	}
}

// MarshalJSON is defined on the value receiver so it applies through a pointer
// too. If it were only on the pointer, encoding a Result value -- which is what
// happens when one is nested in another struct -- would silently skip it.
func TestNullPolicyAppliesThroughAPointer(t *testing.T) {
	r := &Result{}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"warnings":null`)) {
		t.Error("the null policy did not apply when marshalling a *Result")
	}
}
