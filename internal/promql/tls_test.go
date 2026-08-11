package promql

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --insecure-skip-tls-verify was parsed, carried through the config, stored on
// the client, and never used: the client kept http.DefaultTransport, so the
// flag did nothing for Prometheus.
//
// Self-signed certificates are the norm for in-cluster monitoring, so this is
// not an edge case -- it is the first thing that happens when someone points
// the tool at their own Prometheus, and the failure looks like a bug in the
// tool rather than an unimplemented flag.
func TestInsecureSkipsVerificationForPrometheus(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// The control: without the flag, the self-signed certificate must be
	// rejected. If this ever stops failing the test below proves nothing.
	strict := New(Config{URL: srv.URL})
	if err := strict.Ping(ctx); err == nil {
		t.Fatal("a self-signed certificate was accepted without the flag, so this test " +
			"cannot demonstrate that the flag does anything")
	} else if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Fatalf("expected a certificate error, got %v", err)
	}

	if err := New(Config{URL: srv.URL, Insecure: true}).Ping(ctx); err != nil {
		t.Fatalf("--insecure-skip-tls-verify was set and the certificate was still "+
			"rejected, so the flag is accepted and ignored: %v", err)
	}
}

// The insecure client must be a clone, not a bare transport: replacing the
// default outright drops proxy support and connection pooling, which turns a
// working scan behind a corporate proxy into a hang.
func TestInsecureClientKeepsTheDefaultTransportSettings(t *testing.T) {
	c := New(Config{URL: "https://example.invalid", Insecure: true})
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.http.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("the insecure transport has no Proxy function, so HTTPS_PROXY is ignored " +
			"and a scan from behind a corporate proxy hangs instead of connecting")
	}
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("Insecure was set but the transport still verifies certificates")
	}
}
