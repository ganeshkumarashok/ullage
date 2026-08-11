package scan

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// multiClusterProm serves DCGM series from two clusters, and honours a cluster
// selector when one is present -- exactly as a central Thanos would.
func multiClusterProm(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")

		var result []any
		if strings.Contains(q, "DCGM_") && !strings.Contains(q, "pod") {
			for _, cluster := range []string{"prod-eastus", "staging-westus"} {
				if sel := `cluster="`; strings.Contains(q, sel) &&
					!strings.Contains(q, sel+cluster+`"`) {
					continue
				}
				result = append(result, map[string]any{
					"metric": map[string]string{
						"Hostname": "gpu-a", "gpu": "0", "cluster": cluster,
					},
					"value": []any{float64(time.Now().Unix()), "0"},
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "query_range") {
			var out []any
			for range result {
				out = append(out, map[string]any{
					"metric": map[string]string{"Hostname": "gpu-a", "gpu": "0"},
					"values": []any{},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   map[string]any{"resultType": "matrix", "result": out},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": result},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A central Thanos, Mimir or Grafana Cloud tenant holds every cluster that
// remote-writes to it. An unqualified metric name returns all of them, and node
// names are not unique across clusters -- kubeadm and kind both produce
// "gpu-worker-0" -- so a busy device in one cluster can answer for an idle
// device of the same name in another. The merge leaves no trace once the
// samples are joined to nodes, which makes the warning the only chance to
// notice.
func TestMergedClustersAreReported(t *testing.T) {
	g, opts := gatherWithProm(t, multiClusterProm(t))
	_, _, warnings, err := g.Gather(t.Context(), opts)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var found string
	for _, w := range warnings {
		if strings.Contains(w, "more than one cluster") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("no warning that the endpoint holds several clusters, so this cluster's "+
			"idle GPUs are indistinguishable from another cluster's busy ones and nothing "+
			"says so. Warnings: %q", warnings)
	}
	if !strings.Contains(found, "--metrics-selector") {
		t.Errorf("the warning does not say how to fix it: %q", found)
	}
	for _, want := range []string{"prod-eastus", "staging-westus"} {
		if !strings.Contains(found, want) {
			t.Errorf("the warning does not name the cluster %q that was found, so the "+
				"operator cannot tell which selector to pass: %q", want, found)
		}
	}
}

// Setting the selector is the answer to the warning, so it must silence it --
// and it must actually reach the queries, or it silences the warning without
// fixing anything, which is strictly worse than not having the flag.
func TestSelectorIsAppliedToEveryQuery(t *testing.T) {
	g, opts := gatherWithProm(t, multiClusterProm(t))
	g.Selector = `cluster="prod-eastus"`
	g.Trace = true

	_, _, warnings, err := g.Gather(t.Context(), opts)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "more than one cluster") {
			t.Errorf("the cluster was named but the warning still fired: %q", w)
		}
	}

	qs := g.Queries()
	if len(qs) == 0 {
		t.Fatal("no queries were recorded")
	}
	for _, q := range qs {
		if !strings.Contains(q, "DCGM_") {
			continue
		}
		if !strings.Contains(q, `cluster="prod-eastus"`) {
			t.Errorf("a metric query went out without the selector, so it read every "+
				"cluster on the endpoint: %q", q)
		}
	}
}

func TestSelectorIsOptional(t *testing.T) {
	g := &Gatherer{}
	if got := g.sel("DCGM_FI_DEV_GPU_UTIL"); got != "DCGM_FI_DEV_GPU_UTIL" {
		t.Fatalf("sel with no selector = %q: a single-cluster Prometheus must be queried "+
			"exactly as before", got)
	}
	g.Selector = `  cluster="a"  `
	if got := g.sel("M"); got != `M{cluster="a"}` {
		t.Fatalf("sel = %q, want M{cluster=\"a\"}", got)
	}
}
