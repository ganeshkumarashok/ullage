package scan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ullage-project/ullage/internal/kube"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// ownerAPI serves a pod owned by a ReplicaSet which is in turn owned by a
// Deployment. denyRS makes the ReplicaSet unreadable, which is what an RBAC
// role granting pods but not replicasets produces.
type ownerAPI struct{ denyRS bool }

func (o *ownerAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/apis/apps/v1"):
			_ = json.NewEncoder(w).Encode(map[string]any{"resources": []any{
				map[string]any{"name": "replicasets", "kind": "ReplicaSet", "namespaced": true},
				map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true},
			}})

		case strings.Contains(r.URL.Path, "/replicasets/"):
			// Names are honoured so that "deleted" can be told apart from
			// "refused": a handler that returns the same object whatever it is
			// asked for cannot express an owner that is gone.
			if !strings.HasSuffix(r.URL.Path, "/featurizer-7d9") {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"kind":"Status","code":404,"message":"not found"}`))
				return
			}
			if o.denyRS {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"kind":"Status","code":403,"message":"forbidden"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "apps/v1", "kind": "ReplicaSet",
				"metadata": map[string]any{
					"name": "featurizer-7d9", "namespace": "ml", "uid": "rs-1",
					"ownerReferences": []any{map[string]any{
						"apiVersion": "apps/v1", "kind": "Deployment",
						"name": "featurizer", "uid": "deploy-1", "controller": true,
					}},
				},
			})

		case strings.Contains(r.URL.Path, "/deployments/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "featurizer", "namespace": "ml", "uid": "deploy-1"},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","code":404}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func podOwnedByReplicaSet() *kube.Pod {
	p := &kube.Pod{}
	p.Metadata.Name = "featurizer-7d9-abcde"
	p.Metadata.Namespace = "ml"
	yes := true
	p.Metadata.OwnerReferences = []kube.OwnerReference{{
		APIVersion: "apps/v1", Kind: "ReplicaSet",
		Name: "featurizer-7d9", UID: "rs-1", Controller: &yes,
	}}
	return p
}

// An RBAC role granting pods but not replicasets is a completely ordinary
// mistake, and it used to produce the worst possible output: the walk stopped
// at the ReplicaSet, called it the root, and emitted
// `kubectl scale replicaset featurizer-7d9 --replicas=0`.
//
// The Deployment above it restores the replica count within seconds. The
// operator runs the command, watches the pods come straight back, and
// concludes the tool does not work -- which is fair, because it recommended an
// action against an object it had already failed to read.
func TestTruncatedOwnerWalkEmitsNoCommand(t *testing.T) {
	srv := (&ownerAPI{denyRS: true}).server(t)
	kc, err := kube.New(kube.Config{APIServer: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}

	prov := NewResolver(kc).Resolve(context.Background(), podOwnedByReplicaSet())
	if !prov.Truncated {
		t.Fatal("the ReplicaSet could not be read, so the walk never reached the root, " +
			"but the provenance claims a complete chain")
	}

	fix := SynthesiseFix(prov, "ml", []string{"featurizer-7d9-abcde"}, api.Owner{}, "", false)
	if fix.Command != "" {
		t.Fatalf("a command was emitted against an object whose owner is unknown: %q.\n"+
			"If that object is a ReplicaSet, its Deployment reverses this within seconds.",
			fix.Command)
	}
	if fix.Targets != api.FixTargetNone {
		t.Fatalf("Targets = %q, want %q", fix.Targets, api.FixTargetNone)
	}
	if !strings.Contains(fix.Rationale, "could not be followed") {
		t.Fatalf("the rationale does not tell the reader why no command is offered: %q",
			fix.Rationale)
	}
}

// The complement: when the walk completes, it must reach the Deployment and
// emit the command against it, not against the ReplicaSet in between.
func TestCompleteOwnerWalkReachesTheDeployment(t *testing.T) {
	srv := (&ownerAPI{}).server(t)
	kc, err := kube.New(kube.Config{APIServer: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}

	prov := NewResolver(kc).Resolve(context.Background(), podOwnedByReplicaSet())
	if prov.Truncated {
		t.Fatal("every object in the chain was readable, but the walk is marked truncated; " +
			"this withholds the command from findings that deserve one")
	}
	if prov.RootKind != "Deployment" || prov.RootName != "featurizer" {
		t.Fatalf("root = %s/%s, want Deployment/featurizer", prov.RootKind, prov.RootName)
	}

	fix := SynthesiseFix(prov, "ml", []string{"featurizer-7d9-abcde"}, api.Owner{}, "", false)
	if !strings.Contains(fix.Command, "deployment") {
		t.Fatalf("command = %q, want it to target the Deployment", fix.Command)
	}
}

// Refusing to guess is right, but staying quiet about why is not. A truncated
// chain has two very different causes -- the object is gone, or you are not
// allowed to read it -- and only one of them is fixed by editing a Role.
//
// This matters more on GPU clusters than anywhere else, because the thing
// owning a training pod is usually a custom resource the shipped RBAC cannot
// know about: PyTorchJob, MPIJob, RayCluster, Workflow. Without the kind named
// in a warning, an evaluator sees a tool that cannot attribute their most
// important workloads and has no way to discover that a two-line grant fixes
// it.
func TestRefusedOwnerLookupIsReportedAsAPermissionsGap(t *testing.T) {
	srv := (&ownerAPI{denyRS: true}).server(t)
	kc, err := kube.New(kube.Config{APIServer: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}

	r := NewResolver(kc)
	r.Resolve(context.Background(), podOwnedByReplicaSet())

	denied := r.Denied()
	if len(denied) != 1 || denied[0] != "ReplicaSet.apps" {
		t.Fatalf("Denied() = %v, want [ReplicaSet.apps]: a 403 during the owner walk has to "+
			"be distinguishable from an owner that genuinely does not exist, and named in "+
			"the form an RBAC rule uses.", denied)
	}
}

// A chain that ends because the object was deleted is not a permissions
// problem, and telling someone to edit their RBAC over it sends them to fix
// something that is not broken. Job pods reach this state routinely: the Job
// is cleaned up by TTL while its pod is still being reported.
func TestDeletedOwnerIsNotReportedAsAPermissionsGap(t *testing.T) {
	srv := (&ownerAPI{}).server(t)
	kc, err := kube.New(kube.Config{APIServer: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}

	pod := podOwnedByReplicaSet()
	pod.Metadata.OwnerReferences[0].Name = "vanished"

	r := NewResolver(kc)
	r.Resolve(context.Background(), pod)

	if denied := r.Denied(); len(denied) != 0 {
		t.Fatalf("Denied() = %v, want none: the owner returned 404, which means it is gone, "+
			"not that we were refused. Reporting it as a permissions gap sends the reader "+
			"to edit a Role that is already correct.", denied)
	}
}

// app.kubernetes.io/managed-by names the tool that applied the manifest, not a
// person. It used to be the last owner fallback, which meant that on any
// cluster deployed by Helm or Argo CD -- most of them -- "the person
// responsible for this idle GPU" resolved to "helm" for nearly every pod.
func TestManagedByIsNotTreatedAsAnOwner(t *testing.T) {
	pod := &kube.Pod{}
	pod.Metadata.Labels = map[string]string{
		"app.kubernetes.io/managed-by": "helm",
	}

	got := AttributeOwner(pod, nil, nil)
	if got.Identity == "helm" {
		t.Fatalf("owner resolved to %q via %q: managed-by names the deploying tool, "+
			"and a finding that says to go talk to Helm helps nobody",
			got.Identity, got.ResolvedVia)
	}
}
