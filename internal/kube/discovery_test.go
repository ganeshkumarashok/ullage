package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// discoveryServer answers discovery for one group and serves one object,
// tagging both with which server replied.
func discoveryServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/apis/example.com/v1"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resources": []any{map[string]any{
					// The plural differs per server: this is the whole point.
					// A cache shared between clusters answers one cluster's
					// discovery from the other's, and the resulting request
					// goes to a path that does not exist there.
					"name": tag + "widgets", "kind": "Widget", "namespaced": true,
				}},
			})
		case strings.Contains(r.URL.Path, tag+"widgets"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "example.com/v1", "kind": "Widget",
				"metadata": map[string]any{"name": "w", "namespace": "default", "uid": tag},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","code":404}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Two clusters in one process. The discovery cache used to be package-level,
// so whichever client resolved a kind first answered for both -- and the
// second cluster's lookup was sent to a resource path that does not exist
// there, which surfaces as a spurious "not found" on an owner that is present.
//
// A single process holding two clients is not hypothetical: it is what a
// multi-cluster gate, an operator, or anything importing pkg/ullage does.
func TestDiscoveryIsNotSharedBetweenClusters(t *testing.T) {
	a, b := discoveryServer(t, "a"), discoveryServer(t, "b")

	ca, err := New(Config{APIServer: a.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := New(Config{APIServer: b.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ca.GetObject(context.Background(), "example.com/v1", "Widget", "default", "w"); err != nil {
		t.Fatalf("first cluster: %v", err)
	}
	got, err := cb.GetObject(context.Background(), "example.com/v1", "Widget", "default", "w")
	if err != nil {
		t.Fatalf("second cluster resolved Widget through the first cluster's discovery, so "+
			"the request went to a resource path that does not exist there: %v", err)
	}
	if got.Metadata.UID != "b" {
		t.Fatalf("second cluster returned the object from %q", got.Metadata.UID)
	}
}

// Client is part of the public surface through pkg/ullage, so an embedder can
// scan several clusters from a worker pool. An unsynchronised map write is not
// a slow path or a stale read -- the Go runtime kills the process with
// "concurrent map writes", which an embedder cannot catch or recover from.
//
// The CLI itself resolves owners sequentially, so this guards the library
// contract rather than a path the binary exercises. Run under -race it fails
// outright if the discovery cache loses its lock.
func TestConcurrentDiscoveryIsSafe(t *testing.T) {
	srv := discoveryServer(t, "a")
	c, err := New(Config{APIServer: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.GetObject(context.Background(), "example.com/v1", "Widget", "default", "w")
		}()
	}
	wg.Wait()
}
