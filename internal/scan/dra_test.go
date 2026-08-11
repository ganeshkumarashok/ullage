package scan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ullage-project/ullage/internal/check"
	"github.com/ullage-project/ullage/internal/kube"
	"github.com/ullage-project/ullage/internal/promql"
)

// fakeAPI serves a tiny cluster: one GPU node, one pod on it that holds its
// accelerator through a DRA ResourceClaim, and no metrics at all.
//
// Under DRA a pod requests no extended resource, so `nvidia.com/gpu` never
// appears in its spec. The claim is the only record that the pod is sitting on
// hardware. deny controls whether the claim can be read.
type fakeAPI struct {
	deny   bool
	denied bool

	denyPDBs   bool
	deniedPDBs bool
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	t.Helper()
	const node = "gpu-a"

	write := func(w http.ResponseWriter, items ...any) {
		w.Header().Set("Content-Type", "application/json")
		if items == nil {
			items = []any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{},
			"items":    items,
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/resourceclaims"):
			if f.deny {
				f.denied = true
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"kind":"Status","code":403,"message":"resourceclaims is forbidden"}`))
				return
			}
			write(w, map[string]any{
				"metadata": map[string]any{"name": "gpu-claim", "namespace": "research", "uid": "claim-1"},
				"status": map[string]any{
					"allocation":  map[string]any{"devices": map[string]any{"results": []any{map[string]any{"device": "gpu-0"}}}},
					"reservedFor": []any{map[string]any{"uid": "pod-1", "resource": "pods", "name": "trainer"}},
				},
			})

		case strings.HasSuffix(path, "/poddisruptionbudgets"):
			if f.denyPDBs {
				f.deniedPDBs = true
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"kind":"Status","code":403,"message":"poddisruptionbudgets is forbidden"}`))
				return
			}
			write(w)

		case strings.HasSuffix(path, "/pods"):
			write(w, map[string]any{
				"metadata": map[string]any{
					"name": "trainer", "namespace": "research", "uid": "pod-1",
					"creationTimestamp": "2026-07-01T00:00:00Z",
					"labels":            map[string]any{"app": "trainer"},
					// Owned, because an uncontrolled pod is unevictable for a
					// different reason and would never reach the budget check.
					"ownerReferences": []any{map[string]any{
						"apiVersion": "apps/v1", "kind": "StatefulSet",
						"name": "trainer", "uid": "sts-1", "controller": true,
					}},
				},
				"spec": map[string]any{
					"nodeName":       node,
					"resourceClaims": []any{map[string]any{"name": "gpu", "resourceClaimName": "gpu-claim"}},
					"containers":     []any{map[string]any{"name": "train"}},
				},
				"status": map[string]any{"phase": "Running"},
			})

		case strings.HasSuffix(path, "/nodes"):
			write(w, map[string]any{
				"metadata": map[string]any{
					"name":              node,
					"creationTimestamp": "2026-07-01T00:00:00Z",
					"labels": map[string]any{
						"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB",
						"nvidia.com/gpu.count":   "1",
					},
				},
				"status": map[string]any{
					"capacity":    map[string]any{"nvidia.com/gpu": "1"},
					"allocatable": map[string]any{"nvidia.com/gpu": "1"},
					"conditions":  []any{map[string]any{"type": "Ready", "status": "True"}},
				},
			})

		case strings.Contains(path, "/statefulsets/"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "apps/v1", "kind": "StatefulSet",
				"metadata": map[string]any{"name": "trainer", "namespace": "research", "uid": "sts-1"},
			})

		case strings.HasSuffix(path, "/apis/apps/v1"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resources": []any{map[string]any{
					"name": "statefulsets", "kind": "StatefulSet", "namespaced": true}},
			})

		case strings.HasSuffix(path, "/version"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"major":"1","minor":"34"}`))

		default:
			// Everything else -- namespaces, PDBs, autoscaler CRs -- is an
			// empty list, so this test isolates the claim read.
			write(w)
		}
	})
}

// idleProm reports the node's one device at zero utilization for the whole
// window, and attaches no pod labels to it.
//
// That combination is ordinary under DRA rather than exotic: DCGM exports
// device-level series, the pod association comes from the device plugin, and a
// DRA cluster has no device plugin. So the device looks unattributed and idle,
// which is exactly the reading that makes an occupied node indistinguishable
// from an empty one -- and exactly why the claim has to be readable.
func idleProm(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		var result []any
		if !strings.Contains(q, "pod") { // schema detection asks for pod labels; there are none
			result = append(result, map[string]any{
				"metric": map[string]string{"Hostname": "gpu-a", "gpu": "0"},
				"value":  []any{float64(time.Now().Unix()), "0"},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": result},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func gatherAgainst(t *testing.T, api *fakeAPI) (*Gatherer, Options, check.Params) {
	t.Helper()

	kapi := httptest.NewServer(api.handler(t))
	t.Cleanup(kapi.Close)
	prom := idleProm(t)

	kc, err := kube.New(kube.Config{APIServer: kapi.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	g := &Gatherer{Kube: kc, Prom: promql.New(promql.Config{URL: prom.URL})}

	opts := Options{
		Now:      time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		Window:   14 * 24 * time.Hour,
		Step:     time.Hour,
		Progress: func(string) {},
	}
	opts.Defaults()

	params := check.Params{
		IdleThreshold:  opts.IdleThreshold,
		StuckThreshold: opts.StuckThreshold,
		InitGrace:      opts.InitGrace,
	}
	return g, opts, params
}

// The scenario rev-api asked for, and the one that would end the project's
// credibility: RBAC has not been updated for resource.k8s.io, so the claim
// that proves a node is full cannot be read. The node advertises an
// accelerator, no metrics exist, and the pod on it appears -- to anything that
// only counts extended resources -- to hold nothing.
//
// Before the fix, `claims, _ :=` discarded the 403 and unused-node offered to
// delete a pool that was training a model.
func TestGatherFailsClosedWhenDRAOccupancyIsUnavailable(t *testing.T) {
	api := &fakeAPI{deny: true}
	g, opts, params := gatherAgainst(t, api)

	cl, _, warnings, err := g.Gather(context.Background(), opts)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if !api.denied {
		t.Fatal("the test never exercised the forbidden claim read")
	}

	if len(cl.Nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(cl.Nodes))
	}
	if !cl.Nodes[0].OccupancyUnknown {
		t.Fatal("ResourceClaims returned 403, so what this node is holding is unknown, " +
			"but it was handed to the checks as an ordinary node whose emptiness can be measured")
	}

	var mentioned bool
	for _, w := range warnings {
		if strings.Contains(w, "ResourceClaims") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Fatalf("nothing in the warnings tells the operator their DRA claims were unreadable: %q", warnings)
	}

	// The end-to-end assertion: no check may claim this node is idle capacity.
	for _, c := range check.All() {
		found, err := c.Run(context.Background(), cl, params)
		if err != nil {
			t.Fatalf("%s: %v", c.Describe().ID, err)
		}
		for _, f := range found {
			if c.Describe().ID == "unused-node" {
				t.Fatalf("unused-node reported %q as fallow while its DRA occupancy was "+
					"unreadable; a node that is training a model would be offered for deletion",
					f.Subject.Name)
			}
		}
	}
}

// The same cluster with the claim readable must still be understood, or the
// fix above would be indistinguishable from ullage simply ignoring DRA.
func TestGatherSeesDRAOccupancyWhenClaimsAreReadable(t *testing.T) {
	api := &fakeAPI{}
	g, opts, _ := gatherAgainst(t, api)

	cl, _, _, err := g.Gather(context.Background(), opts)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(cl.Nodes) != 1 || cl.Nodes[0].OccupancyUnknown {
		t.Fatalf("a readable ResourceClaim left the node marked unknown: %+v", cl.Nodes)
	}
	if len(cl.Pods) != 1 {
		t.Fatalf("got %d pods, want 1", len(cl.Pods))
	}
	if !cl.Pods[0].Occupies() {
		t.Fatalf("the pod holds a device through an allocated ResourceClaim but Occupies() is false: %+v", cl.Pods[0])
	}
}

// Whether a PodDisruptionBudget forbids evicting a pod decides whether an
// "unused" node can actually be reclaimed. `pdbs, _ :=` turned an unreadable
// list into an empty one, and an empty list means nothing blocks anything --
// the confident version of the wrong answer, on a cluster where RBAC simply
// does not grant policy/v1.
func TestGatherWillNotAssumePodsAreEvictableWhenPDBsAreUnreadable(t *testing.T) {
	api := &fakeAPI{denyPDBs: true}
	g, opts, _ := gatherAgainst(t, api)

	cl, _, warnings, err := g.Gather(context.Background(), opts)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if !api.deniedPDBs {
		t.Fatal("the test never exercised the forbidden PDB read")
	}
	if len(cl.Pods) != 1 {
		t.Fatalf("got %d pods, want 1", len(cl.Pods))
	}
	if cl.Pods[0].Evictable {
		t.Fatal("policy/v1 was forbidden, so nothing is known about what blocks a drain, " +
			"but the pod was reported as safe to evict")
	}
	if !strings.Contains(cl.Pods[0].BlockReason, "could not be read") {
		t.Fatalf("BlockReason = %q; it has to say the list was unreadable, or the operator "+
			"reads it as a real budget that does not exist", cl.Pods[0].BlockReason)
	}

	var mentioned bool
	for _, w := range warnings {
		if strings.Contains(w, "PodDisruptionBudget") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Fatalf("nothing warned that PDBs were unreadable: %q", warnings)
	}
}
