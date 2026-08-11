package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient points a Client at a test server, bypassing kubeconfig.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{server: srv.URL, http: srv.Client(), context: "test"}
}

// Truncating a node or pod list is the worst possible failure in this tool: the
// pods it did not see are the ones that make a node occupied, so a dropped page
// turns busy hardware into a deletion recommendation. Every list must be walked
// to the end.
func TestListAllFollowsEveryPage(t *testing.T) {
	var seen []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RawQuery)
		cont := r.URL.Query().Get("continue")
		switch cont {
		case "":
			fmt.Fprint(w, `{"metadata":{"continue":"page2"},"items":[{"metadata":{"name":"a"}}]}`)
		case "page2":
			fmt.Fprint(w, `{"metadata":{"continue":"page3"},"items":[{"metadata":{"name":"b"}}]}`)
		case "page3":
			fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"name":"c"}}]}`)
		default:
			t.Errorf("unexpected continue token %q", cont)
		}
	})

	got, err := listAll[Pod](context.Background(), c, "/api/v1/pods")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d items from a 3-page list, want 3: an unfollowed page is capacity the "+
			"scan cannot see, and unseen pods read as an empty node", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].Metadata.Name != want {
			t.Fatalf("item %d = %q, want %q", i, got[i].Metadata.Name, want)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("made %d requests, want 3", len(seen))
	}
	for _, q := range seen {
		if !strings.Contains(q, "limit=") {
			t.Fatalf("request %q carried no limit; an unpaged list of a large cluster is what "+
				"the API server refuses", q)
		}
	}
}

// A partial result is not a usable result. Returning what was collected so far
// understates occupancy on exactly the pages that were not read.
func TestListAllFailsRatherThanReturningAPartialList(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			fmt.Fprint(w, `{"metadata":{"continue":"page2"},"items":[{"metadata":{"name":"a"}}]}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `boom`)
	})

	got, err := listAll[Pod](context.Background(), c, "/api/v1/pods")
	if err == nil {
		t.Fatalf("a failed second page returned %d items and no error; the caller cannot tell "+
			"this from a cluster that really has one pod", len(got))
	}
	if got != nil {
		t.Fatalf("got %d items alongside the error, want none", len(got))
	}
}

// A server that keeps handing back the same continue token would otherwise spin
// forever, holding the scan open with no output.
func TestListAllRefusesToLoopForever(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"metadata":{"continue":"same"},"items":[{"metadata":{"name":"a"}}]}`)
	})
	_, err := listAll[Pod](context.Background(), c, "/api/v1/pods")
	if err == nil {
		t.Fatal("an endlessly paging server did not terminate the walk")
	}
	if !strings.Contains(err.Error(), "refusing to loop") {
		t.Fatalf("err=%v, want it to name the runaway paging", err)
	}
}

// Field selectors and other pre-existing query parameters must survive, or the
// pagination parameter silently widens the query.
func TestListAllPreservesAnExistingQueryString(t *testing.T) {
	var raw string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.RawQuery
		fmt.Fprint(w, `{"metadata":{},"items":[]}`)
	})
	if _, err := listAll[Pod](context.Background(), c, "/api/v1/pods?fieldSelector=status.phase%3DRunning"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "fieldSelector=status.phase%3DRunning") {
		t.Fatalf("query=%q, dropped the field selector", raw)
	}
	if !strings.Contains(raw, "limit=") {
		t.Fatalf("query=%q, dropped the page limit", raw)
	}
	if strings.Count(raw, "?") != 0 {
		t.Fatalf("query=%q contains a stray separator", raw)
	}
}

func TestGPUsRecognisesEveryVendorAndReportsWhichOne(t *testing.T) {
	cases := []struct {
		name  string
		in    ResourceList
		want  int
		wantR string
	}{
		{"nvidia", ResourceList{"nvidia.com/gpu": "4"}, 4, "nvidia.com/gpu"},
		{"amd", ResourceList{"amd.com/gpu": "2"}, 2, "amd.com/gpu"},
		{"whitespace", ResourceList{"nvidia.com/gpu": " 1 "}, 1, "nvidia.com/gpu"},
		{"zero is not a request", ResourceList{"nvidia.com/gpu": "0"}, 0, ""},
		{"unparseable", ResourceList{"nvidia.com/gpu": "many"}, 0, ""},
		{"absent", ResourceList{"cpu": "4"}, 0, ""},
		{"empty", ResourceList{}, 0, ""},
		{"nil", nil, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, res := tc.in.GPUs()
			if n != tc.want || res != tc.wantR {
				t.Fatalf("GPUs()=(%d,%q), want (%d,%q)", n, res, tc.want, tc.wantR)
			}
		})
	}
}

// A tool that counts only whole devices concludes that a MIG node packed with
// busy pods has nothing scheduled on it — and then offers to delete it.
func TestSlicesAreCountedSeparatelyFromWholeDevices(t *testing.T) {
	mig := ResourceList{"nvidia.com/mig-1g.5gb": "2", "nvidia.com/mig-3g.20gb": "1"}

	if n, _ := mig.GPUs(); n != 0 {
		t.Fatalf("GPUs()=%d for a MIG request; a 1g.5gb profile is not a whole A100 and must "+
			"never be added to a device count", n)
	}
	n, res := mig.Slices()
	if n != 3 {
		t.Fatalf("Slices()=%d, want 3", n)
	}
	if res != "nvidia.com/mig-1g.5gb" {
		t.Fatalf("Slices() named %q; the name must be stable across map iterations or the "+
			"finding text changes between runs", res)
	}

	// Stable across repeated calls: map ordering must not leak out.
	for i := 0; i < 50; i++ {
		if _, r := mig.Slices(); r != res {
			t.Fatalf("Slices() named %q on iteration %d, previously %q", r, i, res)
		}
	}

	t.Run("time-sliced replicas count as occupancy too", func(t *testing.T) {
		ts := ResourceList{"nvidia.com/gpu.shared": "4"}
		if n, _ := ts.Slices(); n != 4 {
			t.Fatalf("Slices()=%d for a time-sliced request", n)
		}
	})
	t.Run("a whole device is not a slice", func(t *testing.T) {
		if n, _ := (ResourceList{"nvidia.com/gpu": "1"}).Slices(); n != 0 {
			t.Fatalf("Slices()=%d, want 0", n)
		}
	})
}

func TestPodRequestsAreSummedAcrossContainers(t *testing.T) {
	pod := &Pod{Spec: PodSpec{Containers: []Container{
		{Resources: ResourceRequirements{Limits: ResourceList{"nvidia.com/gpu": "1"}}},
		{Resources: ResourceRequirements{Limits: ResourceList{"nvidia.com/gpu": "2"}}},
		{Resources: ResourceRequirements{}},
	}}}
	if n, _ := pod.GPURequest(); n != 3 {
		t.Fatalf("GPURequest()=%d, want 3: a multi-container pod holds the sum, and "+
			"undercounting understates how much hardware the pod occupies", n)
	}
}

// Kubernetes defaults requests to limits for extended resources, and charts
// write it either way. Reading only one of the two loses the pod entirely.
func TestRequestsAreUsedWhenLimitsAreAbsent(t *testing.T) {
	pod := &Pod{Spec: PodSpec{Containers: []Container{
		{Resources: ResourceRequirements{Requests: ResourceList{"nvidia.com/gpu": "2"}}},
	}}}
	if n, _ := pod.GPURequest(); n != 2 {
		t.Fatalf("GPURequest()=%d, want 2", n)
	}
	t.Run("limits win when both are present, and are not double counted", func(t *testing.T) {
		pod := &Pod{Spec: PodSpec{Containers: []Container{{Resources: ResourceRequirements{
			Limits:   ResourceList{"nvidia.com/gpu": "1"},
			Requests: ResourceList{"nvidia.com/gpu": "1"},
		}}}}}
		if n, _ := pod.GPURequest(); n != 1 {
			t.Fatalf("GPURequest()=%d, want 1; counting both sides doubles every pod", n)
		}
	})
}

// Under DRA no extended resource appears in the pod spec at all, so a scan
// keyed on nvidia.com/gpu sees an empty node full of running jobs.
func TestDRAPodsAreRecognisedWithoutAnyExtendedResource(t *testing.T) {
	pod := &Pod{Spec: PodSpec{ResourceClaims: []PodResourceClaim{{Name: "gpu"}}}}
	if n, _ := pod.GPURequest(); n != 0 {
		t.Fatalf("GPURequest()=%d; DRA pods carry no extended resource", n)
	}
	if !pod.UsesDRA() {
		t.Fatal("a pod with a ResourceClaim was not recognised as using DRA, so it would " +
			"count as holding nothing")
	}
	if (&Pod{}).UsesDRA() {
		t.Fatal("a pod with no claims reported DRA use")
	}
}

func TestNodeReadyReadsTheReadyConditionAndNothingElse(t *testing.T) {
	cases := []struct {
		name  string
		conds []NodeCondition
		want  bool
	}{
		{"ready", []NodeCondition{{Type: "Ready", Status: "True"}}, true},
		{"not ready", []NodeCondition{{Type: "Ready", Status: "False"}}, false},
		{"unknown means not ready", []NodeCondition{{Type: "Ready", Status: "Unknown"}}, false},
		{"no conditions at all", nil, false},
		{"other conditions are ignored", []NodeCondition{
			{Type: "MemoryPressure", Status: "True"},
			{Type: "Ready", Status: "True"},
		}, true},
		{"a true MemoryPressure alone is not readiness", []NodeCondition{
			{Type: "MemoryPressure", Status: "True"},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &Node{Status: NodeStatus{Conditions: tc.conds}}
			if got := n.Ready(); got != tc.want {
				t.Fatalf("Ready()=%v, want %v", got, tc.want)
			}
		})
	}
}

// The pool name is what every recommendation is addressed to, so reading the
// wrong label produces a correct finding with an unusable command attached.
func TestPoolPrefersProviderLabelsAndFallsBackToTheNodeName(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"aks", map[string]string{"agentpool": "gpupool"}, "gpupool"},
		{"eks", map[string]string{"eks.amazonaws.com/nodegroup": "ng-1"}, "ng-1"},
		{"gke", map[string]string{"cloud.google.com/gke-nodepool": "np-a"}, "np-a"},
		{"karpenter", map[string]string{"karpenter.sh/nodepool": "default"}, "default"},
		{"no labels falls back to the node name", nil, "node-1"},
		{"an empty label is not a pool", map[string]string{"agentpool": ""}, "node-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &Node{Metadata: ObjectMeta{Name: "node-1", Labels: tc.labels}}
			if got := n.Pool(); got != tc.want {
				t.Fatalf("Pool()=%q, want %q", got, tc.want)
			}
		})
	}

	t.Run("aks wins over karpenter when both are set", func(t *testing.T) {
		n := &Node{Metadata: ObjectMeta{Name: "node-1", Labels: map[string]string{
			"agentpool": "gpupool", "karpenter.sh/nodepool": "default",
		}}}
		if got := n.Pool(); got != "gpupool" {
			t.Fatalf("Pool()=%q; the provider's own pool label is the one its CLI takes", got)
		}
	})
}

func TestProviderIsInferredFromProviderID(t *testing.T) {
	cases := map[string]string{
		"azure:///subscriptions/x/resourceGroups/y": "azure",
		"aws:///us-east-1a/i-0abc":                  "aws",
		"gce://project/zone/instance":               "gcp",
		// A provider that cannot be named is "unknown", never "": the empty
		// string reads as a missing field rather than as a real answer, and
		// the fix renderer keys on this to fall back to kubectl.
		"": "unknown",
	}
	for id, want := range cases {
		n := &Node{Spec: NodeSpec{ProviderID: id}}
		if got := n.Provider(); got != want {
			t.Errorf("Provider(%q)=%q, want %q", id, got, want)
		}
	}
}

// A selector that cannot be evaluated must report that it could not be
// evaluated. Silently returning "no match" turns an unknown into a licence to
// recommend eviction.
//
// The operators themselves are covered exhaustively in selector_test.go. Two
// subtests once lived here asserting that matchExpressions were unevaluable and
// that `{}` matched nothing; the second said in its own failure message that
// Kubernetes means the opposite. Both are now implemented rather than dodged.
func TestLabelSelectorReportsWhenItCannotDecide(t *testing.T) {
	t.Run("a nil selector is a definite non-match", func(t *testing.T) {
		var s *LabelSelector
		matched, ok := s.Matches(map[string]string{"app": "x"})
		if matched || !ok {
			t.Fatalf("nil selector: matched=%v ok=%v, want false,true", matched, ok)
		}
	})
	t.Run("plain matchLabels are decided", func(t *testing.T) {
		s := &LabelSelector{MatchLabels: map[string]string{"app": "trainer"}}
		if matched, ok := s.Matches(map[string]string{"app": "trainer"}); !matched || !ok {
			t.Fatalf("matched=%v ok=%v, want true,true", matched, ok)
		}
		if matched, ok := s.Matches(map[string]string{"app": "other"}); matched || !ok {
			t.Fatalf("matched=%v ok=%v, want false,true", matched, ok)
		}
	})
}

func TestControllerFindsTheOwningWorkload(t *testing.T) {
	yes := true
	no := false
	p := &Pod{Metadata: ObjectMeta{OwnerReferences: []OwnerReference{
		{Kind: "ReplicaSet", Name: "rs-a", Controller: &no},
		{Kind: "StatefulSet", Name: "sts-a", Controller: &yes},
	}}}
	c := p.Controller()
	if c == nil || c.Name != "sts-a" {
		t.Fatalf("Controller()=%+v, want the reference with controller=true; grouping by the "+
			"wrong owner splits one finding into many", c)
	}
	if (&Pod{}).Controller() != nil {
		t.Fatal("a pod with no owners reported a controller")
	}
	// Some controllers omit the flag. Falling back to the first owner keeps
	// pods of one workload grouped together, which is the whole purpose of
	// asking; the alternative is forty identical single-pod findings.
	t.Run("falls back to the first owner when nothing is flagged", func(t *testing.T) {
		p := &Pod{Metadata: ObjectMeta{OwnerReferences: []OwnerReference{{Kind: "Job", Name: "j"}}}}
		got := p.Controller()
		if got == nil || got.Name != "j" {
			t.Fatalf("Controller()=%+v, want the sole owner", got)
		}
	})
}

// The API server returns errors as a JSON Status object. Surfacing the raw
// body, or a bare status code, hides the one line that says what to fix.
func TestAPIErrorsCarryTheServerExplanation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind":    "Status",
			"status":  "Failure",
			"message": `nodes is forbidden: User "system:serviceaccount:x:y" cannot list resource "nodes"`,
			"code":    403,
		})
	})
	var out map[string]any
	err := c.get(context.Background(), "/api/v1/nodes", &out)
	if err == nil {
		t.Fatal("a 403 was treated as success")
	}
	if !strings.Contains(err.Error(), "cannot list resource") {
		t.Fatalf("err=%v, want the server's own message: it names the verb, the resource and "+
			"the identity, which is everything needed to write the missing RBAC rule", err)
	}
	var forbidden *Forbidden
	if !errors.As(err, &forbidden) {
		t.Fatalf("err=%T, want *Forbidden: optional APIs are probed by type, and losing the "+
			"type turns a missing permission into a failed scan", err)
	}
}

// Optional-API probing depends on recognising these through any wrapping a
// caller adds on the way up.
func TestTypedErrorsSurviveWrapping(t *testing.T) {
	wrapped := fmt.Errorf("reading autoscaler status: %w", &Forbidden{Path: "/p"})
	var f *Forbidden
	if !errors.As(wrapped, &f) {
		t.Fatal("a wrapped Forbidden was not recognised; graceful degradation would become " +
			"a hard failure the first time a caller annotated the error")
	}
	var nf *NotFound
	if errors.As(wrapped, &nf) {
		t.Fatal("a Forbidden matched NotFound")
	}
}

// A body that is not a Kubernetes Status — an ingress error page, say — must
// contribute nothing rather than noise.
func TestNonStatusBodiesDoNotPolluteTheError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<html><body>403 Forbidden</body></html>`)
	})
	var out map[string]any
	err := c.get(context.Background(), "/api/v1/nodes", &out)
	if err == nil {
		t.Fatal("a 403 was treated as success")
	}
	if strings.Contains(err.Error(), "html") {
		t.Fatalf("err=%v, want no HTML in the message", err)
	}
}

func TestContextIsPropagatedToRequests(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"metadata":{},"items":[]}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := listAll[Pod](ctx, c, "/api/v1/pods"); err == nil {
		t.Fatal("a cancelled context still issued the request; ^C would not stop a scan")
	}
}
