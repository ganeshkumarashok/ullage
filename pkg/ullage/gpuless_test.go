package ullage_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ullage-project/ullage/pkg/ullage"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// gpulessCluster is a Kubernetes API that answers every list with an empty
// collection. It is the smallest cluster ullage can be pointed at, and the one
// whose result used to break the contract: the early return for "no
// accelerators anywhere" built its ScanMeta by hand and skipped both the
// params block and the warnings guard that the main path applies.
func gpulessCluster(t *testing.T) (kubeURL, promURL string) {
	t.Helper()

	k := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/apis/") && strings.Count(r.URL.Path, "/") == 2:
			// Group discovery.
			_, _ = w.Write([]byte(`{"kind":"APIGroup","versions":[]}`))
		case r.URL.Path == "/apis":
			_, _ = w.Write([]byte(`{"kind":"APIGroupList","groups":[]}`))
		default:
			_, _ = w.Write([]byte(`{"kind":"List","apiVersion":"v1","items":[]}`))
		}
	}))
	t.Cleanup(k.Close)

	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(p.Close)

	return k.URL, p.URL
}

func scanGpuless(t *testing.T) *api.Result {
	t.Helper()
	kubeURL, promURL := gpulessCluster(t)
	now := time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)

	res, err := ullage.Scan(context.Background(), ullage.Options{
		APIServer:  kubeURL,
		Prometheus: ullage.PrometheusOptions{URL: promURL},
		Now:        now,
		Version:    "test",
	})
	if err != nil {
		t.Fatalf("scanning a cluster with no GPUs should succeed, not fail: %v", err)
	}
	return res
}

// A cluster with no accelerators is a complete, correct answer — and it must
// satisfy the same contract as a full one.
func TestGpulessClusterIsACompleteAnswer(t *testing.T) {
	res := scanGpuless(t)

	if len(res.Recommendations) != 0 {
		t.Errorf("a cluster with no GPUs produced %d recommendations", len(res.Recommendations))
	}
	if res.Scan.AcceleratorsObserved != 0 {
		t.Errorf("observed %d accelerators on a cluster with none", res.Scan.AcceleratorsObserved)
	}
	if len(res.Warnings) == 0 {
		t.Error("scanning a cluster with no GPUs said nothing about why there were no findings; " +
			"silence here is indistinguishable from a healthy cluster")
	}
	if res.APIVersion != api.Version {
		t.Errorf("apiVersion = %q, want %q", res.APIVersion, api.Version)
	}
}

// The reproducibility block has to be populated on every path that emits a
// Result. It claims to be everything needed to reproduce the scan; an empty one
// is a false claim, and its zero-valued Checks slice serialises as null.
func TestGpulessClusterStillReportsItsParameters(t *testing.T) {
	res := scanGpuless(t)

	if res.Scan.Window.Duration() == 0 {
		t.Error("no window recorded")
	}
	if res.Scan.Params.IdleThreshold.Duration() == 0 {
		t.Error("params.idleThreshold is zero; the scan does not describe itself")
	}
	if res.Scan.Params.MinConfidence == "" {
		t.Error("params.minConfidence is empty")
	}
	want := []string{"idle-pod", "stuck-pod", "unused-node"}
	if !reflect.DeepEqual(res.Scan.Params.Checks, want) {
		t.Errorf("params.checks = %v, want %v — a consumer cannot tell which checks ran",
			res.Scan.Params.Checks, want)
	}
	if res.Scan.PrometheusURL == "" {
		t.Error("params record no Prometheus URL, so the scan cannot be repeated")
	}
}

// The same never-null rule as the populated document, applied to the branch the
// demo fixture can never reach.
func TestGpulessClusterHasNoNullLists(t *testing.T) {
	res := scanGpuless(t)

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for path, isSlice := range sliceFields(reflect.TypeOf(res), "") {
		if !isSlice {
			continue
		}
		if got := lookup(doc, path); got == nil {
			t.Errorf("%q is null or absent on a cluster with no accelerators; "+
				"a consumer that asks for its length crashes", path)
		}
	}
}

// The published types must be able to read the document, on this path too.
func TestGpulessResultRoundTrips(t *testing.T) {
	res := scanGpuless(t)

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var back api.Result
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the empty-cluster document does not decode into api.Result: %v", err)
	}
	if back.Scan.Params.IdleThreshold != res.Scan.Params.IdleThreshold {
		t.Error("idleThreshold did not survive the round trip")
	}
}
