// Package promql is a Prometheus HTTP API client covering the query surface
// ullage needs, plus the authentication and multi-tenancy that real production
// backends require. "Reads an existing Prometheus" is easy to write and hard to
// deliver: the query endpoint is often not a Service, is usually authenticated,
// and is frequently multi-tenant.
package promql

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Auth modes.
//
// There is deliberately no "azure-monitor" or "aws-sigv4" mode. Managed
// Prometheus offerings need real credential acquisition — AAD token exchange,
// SigV4 request signing — and a mode that named the provider while only setting
// a static bearer header would be worse than no mode at all: it would advertise
// support that does not exist and fail at the point where someone had already
// committed to the tool. Point ullage at a signing proxy instead, or supply the
// token yourself with --prometheus-token-file, which is re-read on every
// request so a rotating projected ServiceAccount token keeps working.
const (
	AuthNone   = "none"
	AuthBearer = "bearer"
	AuthBasic  = "basic"
)

// Config describes how to reach a Prometheus-compatible query endpoint.
type Config struct {
	URL   string
	Auth  string
	Token string

	// TokenFile is read on every request so a rotating projected
	// ServiceAccount token keeps working through a long scan.
	TokenFile string

	Username string
	Password string
	Headers  map[string]string
	Insecure bool
	Timeout  time.Duration
}

// Client queries a Prometheus-compatible backend.
type Client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}

	// Insecure was accepted, stored, and never used: the client kept the
	// default transport, so --insecure-skip-tls-verify silently did nothing
	// for Prometheus and every scan against a self-signed monitoring endpoint
	// failed with a certificate error the flag claimed to have handled.
	//
	// The default transport is cloned rather than replaced so that proxy
	// settings, connection pooling and HTTP/2 keep working.
	tr := http.DefaultTransport
	if cfg.Insecure {
		clone := http.DefaultTransport.(*http.Transport).Clone()
		if clone.TLSClientConfig == nil {
			clone.TLSClientConfig = &tls.Config{}
		}
		clone.TLSClientConfig.InsecureSkipVerify = true
		tr = clone
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout, Transport: tr}}
}

func (c *Client) URL() string { return c.cfg.URL }

// Sample is one point in a series.
type Sample struct {
	T time.Time
	V float64
}

// Series is a labelled sequence of samples.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

type apiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][]any           `json:"values"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

func (c *Client) do(ctx context.Context, path string, form url.Values) (*apiResponse, error) {
	endpoint := strings.TrimRight(c.cfg.URL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	switch c.cfg.Auth {
	case AuthBearer:
		tok := c.cfg.Token
		if c.cfg.TokenFile != "" {
			// Re-read every request rather than once at startup. A projected
			// ServiceAccount token is rotated roughly hourly, and a scan of a
			// large cluster can outlive one; caching the first value turns a
			// long scan into a 401 partway through.
			if b, err := os.ReadFile(c.cfg.TokenFile); err == nil {
				tok = strings.TrimSpace(string(b))
			}
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	case AuthBasic:
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &AuthError{Status: resp.StatusCode, URL: c.cfg.URL}
	}
	// No truncation cap. A silently truncated response decodes as a JSON error
	// on a large cluster, which reads as "the metrics are broken" when in fact
	// the answer was simply big. The aggregate-pushdown queries keep responses
	// small; anything genuinely huge should fail loudly, not be cut short.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out apiResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("unparseable response from %s: %w", c.cfg.URL, err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("query error: %s: %s", out.ErrorType, out.Error)
	}
	return &out, nil
}

// AuthError distinguishes a credential problem from every other failure, so the
// remedy printed to the user is the right one.
type AuthError struct {
	Status int
	URL    string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("metrics endpoint rejected the request (%d)", e.Status)
}

// QueryRange runs a range query and returns matrix series.
func (c *Client) QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]Series, error) {
	form := url.Values{}
	form.Set("query", q)
	form.Set("start", strconv.FormatInt(start.Unix(), 10))
	form.Set("end", strconv.FormatInt(end.Unix(), 10))
	form.Set("step", strconv.Itoa(int(step.Seconds()))+"s")

	resp, err := c.do(ctx, "/api/v1/query_range", form)
	if err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		s := Series{Labels: r.Metric, Samples: make([]Sample, 0, len(r.Values))}
		for _, pair := range r.Values {
			if len(pair) != 2 {
				continue
			}
			ts, ok := toFloat(pair[0])
			if !ok {
				continue
			}
			v, ok := toFloat(pair[1])
			if !ok {
				continue
			}
			s.Samples = append(s.Samples, Sample{T: time.Unix(int64(ts), 0).UTC(), V: v})
		}
		sort.Slice(s.Samples, func(i, j int) bool { return s.Samples[i].T.Before(s.Samples[j].T) })
		out = append(out, s)
	}
	return out, nil
}

// VectorSample is one labelled value from an instant query.
type VectorSample struct {
	Labels map[string]string
	Value  float64
	At     time.Time
}

// QueryExpr runs an instant query and returns one value per series.
//
// This is the workhorse. Asking "was this device ever non-zero in the last
// fortnight" is an aggregate question, and max_over_time answers it with a
// single value per device. Pulling the raw samples to answer it in Go would
// move tens of millions of points to compute a few hundred booleans, and would
// hit a query frontend's sample limit long before it hit a large cluster.
func (c *Client) QueryExpr(ctx context.Context, expr string, at time.Time) ([]VectorSample, error) {
	form := url.Values{}
	form.Set("query", expr)
	form.Set("time", strconv.FormatInt(at.Unix(), 10))

	resp, err := c.do(ctx, "/api/v1/query", form)
	if err != nil {
		return nil, err
	}
	out := make([]VectorSample, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		ts, okT := toFloat(r.Value[0])
		v, okV := toFloat(r.Value[1])
		if !okT || !okV {
			// Dropped, never defaulted. A discarded value left at the zero
			// value is indistinguishable from a device that measurably did
			// nothing, and this is the aggregate path — one bad value here
			// decides a whole device.
			continue
		}
		out = append(out, VectorSample{
			Labels: r.Metric,
			Value:  v,
			At:     time.Unix(int64(ts), 0).UTC(),
		})
	}
	return out, nil
}

// Range renders a duration as a PromQL range selector.
func Range(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d.Minutes())) + "m"
	}
}

// Query runs an instant query and returns vector samples.
func (c *Client) Query(ctx context.Context, q string, at time.Time) ([]Series, error) {
	form := url.Values{}
	form.Set("query", q)
	form.Set("time", strconv.FormatInt(at.Unix(), 10))

	resp, err := c.do(ctx, "/api/v1/query", form)
	if err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		s := Series{Labels: r.Metric}
		if len(r.Value) == 2 {
			ts, okT := toFloat(r.Value[0])
			v, okV := toFloat(r.Value[1])
			if okT && okV {
				s.Samples = []Sample{{T: time.Unix(int64(ts), 0).UTC(), V: v}}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// toFloat parses a Prometheus value, rejecting anything that is not a finite
// number.
//
// The rejection is the point. Prometheus serialises NaN as the string "NaN",
// which ParseFloat accepts happily, and a NaN then behaves as a *confirmed
// zero* everywhere downstream: `v > max` is false so it never raises the
// maximum, `v > 0` is false so it never clears ZeroThroughout, and it still
// counts towards sample coverage. The result is a device reported as
// measurably idle, at full confidence, on the strength of readings that said
// nothing at all.
//
// dcgm-exporter emits NaN for fields a device cannot report — which includes a
// GPU whose driver has wedged or which is mid-reset. That is precisely the
// hardware that must not be recommended for deletion. Dropping the sample
// lowers completeness instead, which weakens the claim exactly as it should.
func toFloat(v any) (float64, bool) {
	var f float64
	switch t := v.(type) {
	case float64:
		f = t
	case string:
		p, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0, false
		}
		f = p
	case json.Number:
		p, err := t.Float64()
		if err != nil {
			return 0, false
		}
		f = p
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// LabelSchema records which label carries pod identity on DCGM series.
//
// This exists because of the single most common real-world dcgm-exporter
// gotcha: when scraped through kube-prometheus-stack, the exporter's own `pod`
// and `namespace` labels collide with the target labels Prometheus attaches,
// and relabelling renames them to `exported_pod` and `exported_namespace`. A
// join that assumes `pod` silently returns zero attribution on one of the most
// popular installations — and the tool would then report "no pod attribution"
// on a cluster where attribution is perfectly present under a different name.
type LabelSchema struct {
	Pod       string
	Namespace string
	Found     bool
}

// Standard is the schema when DCGM's own labels survive scraping.
var Standard = LabelSchema{Pod: "pod", Namespace: "namespace", Found: true}

// Exported is the schema after kube-prometheus-stack relabelling.
var Exported = LabelSchema{Pod: "exported_pod", Namespace: "exported_namespace", Found: true}

// DetectLabelSchema probes both schemas against a metric and returns whichever
// actually carries pod identity.
func (c *Client) DetectLabelSchema(ctx context.Context, metric string, at time.Time) (LabelSchema, error) {
	series, err := c.Query(ctx, metric, at)
	if err != nil {
		return LabelSchema{}, err
	}
	if len(series) == 0 {
		return LabelSchema{}, ErrNoSeries
	}
	var sawStandard, sawExported bool
	for _, s := range series {
		if s.Labels[Standard.Pod] != "" {
			sawStandard = true
		}
		if s.Labels[Exported.Pod] != "" {
			sawExported = true
		}
	}
	switch {
	// When both are present, exported_ wins, and the order matters more than it
	// looks. Under kube-prometheus-stack relabelling `pod` is the scrape
	// target's identity — the dcgm-exporter DaemonSet pod itself — while the
	// workload that holds the device moved to `exported_pod`. Preferring `pod`
	// here attributes every GPU in the cluster to dcgm-exporter, which is both
	// wrong and plausible enough to survive review.
	case sawExported:
		return Exported, nil
	case sawStandard:
		return Standard, nil
	default:
		return LabelSchema{Found: false}, nil
	}
}

// ErrNoSeries means the backend answered, but with nothing at all. This is
// never rendered as a clean cluster: Mimir and Cortex return empty results
// rather than an error when a tenant header is missing, so an unexpectedly
// empty response is a degraded state, not good news.
var ErrNoSeries = fmt.Errorf("the metrics endpoint returned no series")

// Ping verifies connectivity and credentials before the scan starts, so a
// credential problem surfaces in one second rather than fifteen.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Query(ctx, "vector(1)", time.Now())
	return err
}

// Stats summarises one series over the observation window. Every claim ullage
// makes about a device is computed here, from raw samples, so it is
// reproducible by anyone with the same Prometheus.
type Stats struct {
	Max            float64
	Mean           float64
	LastNonZero    *time.Time
	ZeroThroughout bool
	FallowSince    time.Time
	Completeness   float64
	Buckets        []float64
	Samples        int
}

// Summarise reduces a series to the facts the checks need.
//
// The strict-zero rule lives here: ZeroThroughout is true only when *every*
// sample is zero. DCGM_FI_DEV_GPU_UTIL is unreliable as a measure of how much
// work a GPU is doing — a one-thread kernel reads 100% — but it is completely
// reliable when it reads zero. Asking it only that question is what makes a
// finding defensible, and it is why a workload that ran for ten minutes in a
// fortnight is not reported: real work in the window resets the claim.
func Summarise(s Series, start, end time.Time, step time.Duration, buckets int) Stats {
	st := Stats{ZeroThroughout: true, FallowSince: start, Samples: len(s.Samples)}
	if len(s.Samples) == 0 {
		st.ZeroThroughout = false
		return st
	}

	sum := 0.0
	for _, p := range s.Samples {
		if p.V > st.Max {
			st.Max = p.V
		}
		sum += p.V
		if p.V > 0 {
			st.ZeroThroughout = false
			t := p.T
			st.LastNonZero = &t
		}
	}
	st.Mean = sum / float64(len(s.Samples))

	if st.LastNonZero != nil {
		st.FallowSince = *st.LastNonZero
	}

	expected := int(end.Sub(start)/step) + 1
	if expected > 0 {
		st.Completeness = math.Min(1, float64(len(s.Samples))/float64(expected))
	}

	st.Buckets = bucketise(s.Samples, start, end, buckets)
	return st
}

// bucketise reduces samples to N peak values for the sparkline. Peak rather
// than mean, deliberately: the sparkline exists to answer "did anything happen
// here", and an average would hide a short burst.
func bucketise(samples []Sample, start, end time.Time, n int) []float64 {
	if n <= 0 || len(samples) == 0 {
		return nil
	}
	out := make([]float64, n)
	span := end.Sub(start)
	if span <= 0 {
		return out
	}
	for _, p := range samples {
		idx := int(float64(p.T.Sub(start)) / float64(span) * float64(n))
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		if p.V > out[idx] {
			out[idx] = p.V
		}
	}
	return out
}

// EnvHeaders parses repeated key=value header flags.
func EnvHeaders(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("header %q must be in key=value form", p)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// TokenFromFile reads a bearer token from disk.
func TokenFromFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
