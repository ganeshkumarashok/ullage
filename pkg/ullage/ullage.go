// Package ullage is the embedding surface.
//
// Everything an external consumer needs is here and in the api package: options
// in, a Result out. A k8sgpt analyzer, a Grafana backend, an operator, or a CI
// gate can depend on this without depending on how any of it works — no
// Kubernetes client types, no Prometheus wire format, nothing that would turn
// an internal refactor into someone else's broken build.
package ullage

import (
	"context"
	"time"

	"github.com/ganeshkumarashok/ullage/internal/config"
	"github.com/ganeshkumarashok/ullage/internal/kube"
	"github.com/ganeshkumarashok/ullage/internal/promql"
	"github.com/ganeshkumarashok/ullage/internal/scan"
	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// Options configures a scan.
type Options struct {
	// Kubeconfig, Context and APIServer select the cluster. All empty means
	// in-cluster credentials, then the default kubeconfig.
	Kubeconfig string
	Context    string
	APIServer  string

	// Prometheus is the metrics endpoint. Required.
	Prometheus     PrometheusOptions
	Window         time.Duration
	IdleThreshold  time.Duration
	StuckThreshold time.Duration
	Step           time.Duration
	MinConfidence  string
	Namespaces     []string
	Checks         []string
	Pricing        *api.Pricing

	// ConfigFile is a .ullage.yaml suppression list. Empty means no
	// suppressions: a library call reaching into the working directory for a
	// file the caller never mentioned would be a surprise, so the CLI passes
	// the default path explicitly and an embedder opts in.
	ConfigFile string

	// MetricsSelector is a PromQL label matcher applied to every metric read.
	// It is required when the endpoint holds more than one cluster, because
	// node names are not unique across clusters.
	MetricsSelector string

	// Trace records the exact queries issued, so a caller can show its users
	// how a claim was reached.
	Trace bool

	// Now overrides the clock, for reproducible output.
	Now time.Time

	Version  string
	Progress func(string)
}

// PrometheusOptions describes how to reach and authenticate to the metrics
// endpoint.
type PrometheusOptions struct {
	URL       string
	Auth      string
	Token     string
	TokenFile string
	Username  string
	Password  string
	Headers   map[string]string
	Timeout   time.Duration
	Insecure  bool
}

// Scan runs a complete analysis and returns the result.
//
// It never writes to the cluster. That is a property of the code rather than a
// promise: the Kubernetes client this is built on has no write methods at all.
func Scan(ctx context.Context, opts Options) (*api.Result, error) {
	kc, err := kube.New(kube.Config{
		Kubeconfig: opts.Kubeconfig,
		Context:    opts.Context,
		APIServer:  opts.APIServer,
	})
	if err != nil {
		return nil, err
	}
	// Resolved here rather than left to scan.Options.Defaults, because a zero
	// clock makes every expiry date lie in the future and quietly resurrects
	// suppressions their authors time-boxed.
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	opts.Now = now

	// Read before any network call: a malformed suppression file should fail in
	// milliseconds, not after a minute of queries. Only when asked for, so an
	// embedder is never surprised by a file in whatever directory its process
	// happens to have been started in.
	var sup *config.Suppressions
	if opts.ConfigFile != "" {
		var err error
		if sup, err = config.Load(opts.ConfigFile, now); err != nil {
			return nil, err
		}
	}

	pc := promql.New(promql.Config{
		URL:       opts.Prometheus.URL,
		Auth:      opts.Prometheus.Auth,
		Token:     opts.Prometheus.Token,
		TokenFile: opts.Prometheus.TokenFile,
		Username:  opts.Prometheus.Username,
		Password:  opts.Prometheus.Password,
		Headers:   opts.Prometheus.Headers,
		Timeout:   opts.Prometheus.Timeout,
		Insecure:  opts.Prometheus.Insecure,
	})

	// Credentials are verified before the scan so a bad token surfaces in about
	// a second rather than after a minute of queries.
	if err := pc.Ping(ctx); err != nil {
		return nil, err
	}

	so := scan.Options{
		Window:         opts.Window,
		IdleThreshold:  opts.IdleThreshold,
		StuckThreshold: opts.StuckThreshold,
		Step:           opts.Step,
		MinConfidence:  opts.MinConfidence,
		Namespaces:     opts.Namespaces,
		Checks:         opts.Checks,
		Pricing:        opts.Pricing,
		Suppressions:   sup,
		Now:            opts.Now,
		Trace:          opts.Trace,
		Version:        opts.Version,
		Progress:       opts.Progress,
	}
	so.Defaults()

	g := &scan.Gatherer{Kube: kc, Prom: pc, Trace: opts.Trace, Selector: opts.MetricsSelector}
	cluster, inv, warnings, err := g.Gather(ctx, so)
	if err != nil {
		return nil, err
	}
	if cluster == nil {
		// No accelerators anywhere. That is a complete, correct answer, and it
		// has to satisfy the same contract as a full one: a cluster with no
		// GPUs is exactly where a consumer iterating scan.params.checks or
		// warnings hits a null, and it was the one path the shape test never
		// reached because the fixture it runs against has GPUs.
		if warnings == nil {
			warnings = []string{}
		}
		return &api.Result{
			APIVersion: api.Version,
			Scan: api.ScanMeta{
				Tool:          api.Tool{Name: "ullage", Version: opts.Version},
				Context:       kc.Context(),
				Started:       so.Now,
				Window:        api.ISODuration(so.Window),
				Params:        scan.EffectiveParams(so),
				PrometheusURL: pc.URL(),
			},
			Recommendations: []api.Finding{},
			Suppressed:      []api.Finding{},
			NotAnalyzed:     []api.Exclusion{},
			Warnings:        warnings,
		}, nil
	}

	res, err := scan.Analyse(ctx, cluster, inv, so)
	if err != nil {
		return nil, err
	}
	res.Scan.PrometheusURL = pc.URL()
	res.Warnings = append(warnings, res.Warnings...)
	if res.Warnings == nil {
		res.Warnings = []string{}
	}
	if opts.Trace {
		res.Scan.Queries = g.Queries()
	}
	return res, nil
}
