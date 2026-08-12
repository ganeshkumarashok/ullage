package promql

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// serve stands up a Prometheus-shaped endpoint that replies with the given body
// and status, and records what it was asked.
type capture struct {
	mu      chan struct{}
	path    string
	form    url.Values
	headers http.Header
	calls   int32
}

func serve(t *testing.T, status int, body string) (*Client, *capture) {
	t.Helper()
	cap := &capture{mu: make(chan struct{}, 1)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cap.calls, 1)
		_ = r.ParseForm()
		cap.path = r.URL.Path
		cap.form = r.PostForm
		cap.headers = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(Config{URL: srv.URL}), cap
}

func matrix(values string) string {
	return `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"gpu":"0"},"values":` + values + `}]}}`
}

func vector(value string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"gpu":"0"},"value":` + value + `}]}}`
}

// A NaN reading means "this device could not tell us", and dcgm-exporter emits
// it for a GPU whose driver has wedged. If it survives parsing it behaves as a
// confirmed zero: it never raises the max, never clears ZeroThroughout, and
// still counts towards coverage. That turns broken hardware into a
// high-confidence recommendation to delete it.
func TestNonFiniteSamplesAreDroppedNotReadAsZero(t *testing.T) {
	for _, bad := range []string{`"NaN"`, `"+Inf"`, `"-Inf"`} {
		t.Run(bad, func(t *testing.T) {
			c, _ := serve(t, 200, matrix(`[[100,`+bad+`],[200,"0"]]`))
			got, err := c.QueryRange(context.Background(), "q", time.Unix(100, 0), time.Unix(200, 0), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(got[0].Samples); n != 1 {
				t.Fatalf("kept %d samples, want 1: a %s reading was accepted as data", n, bad)
			}
			if v := got[0].Samples[0].V; v != 0 || math.IsNaN(v) {
				t.Fatalf("surviving sample is %v, want the real 0", v)
			}
		})
	}
}

// The aggregate path decides an entire device from a single value. A dropped
// value defaulting to zero there is worse than no answer, because zero is the
// exact reading that justifies deletion.
func TestQueryExprDropsUnusableValuesRatherThanDefaultingToZero(t *testing.T) {
	c, _ := serve(t, 200, vector(`[100,"NaN"]`))
	got, err := c.QueryExpr(context.Background(), "max_over_time(x[14d])", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d samples with Value=%v, want the series dropped; a NaN max reads as a device that did nothing", len(got), got[0].Value)
	}
}

// Prometheus sends sample values as JSON strings, not numbers. Reading them as
// numbers yields zero for every sample.
func TestValuesArriveAsStringsAndAreParsed(t *testing.T) {
	c, _ := serve(t, 200, matrix(`[[100,"42.5"]]`))
	got, err := c.QueryRange(context.Background(), "q", time.Unix(100, 0), time.Unix(200, 0), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Samples[0].V != 42.5 {
		t.Fatalf("V=%v, want 42.5", got[0].Samples[0].V)
	}
	if !got[0].Samples[0].T.Equal(time.Unix(100, 0).UTC()) {
		t.Fatalf("T=%v, want unix 100 UTC", got[0].Samples[0].T)
	}
}

func TestSamplesAreSortedByTime(t *testing.T) {
	c, _ := serve(t, 200, matrix(`[[300,"1"],[100,"2"],[200,"3"]]`))
	got, _ := c.QueryRange(context.Background(), "q", time.Unix(100, 0), time.Unix(300, 0), time.Minute)
	for i := 1; i < len(got[0].Samples); i++ {
		if got[0].Samples[i].T.Before(got[0].Samples[i-1].T) {
			t.Fatalf("samples out of order: %v", got[0].Samples)
		}
	}
	// Summarise reads LastNonZero as "the last non-zero in iteration order",
	// which is only the latest if the series is sorted.
	if got[0].Samples[2].V != 1 {
		t.Fatalf("last sample V=%v, want the one stamped 300", got[0].Samples[2].V)
	}
}

func TestAuthFailureIsDistinguishedFromEveryOtherFailure(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		c, _ := serve(t, code, `denied`)
		_, err := c.Query(context.Background(), "q", time.Now())
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("status %d gave %T (%v), want *AuthError so the remedy printed is about credentials", code, err, err)
		}
		if ae.Status != code {
			t.Fatalf("AuthError.Status=%d, want %d", ae.Status, code)
		}
	}
}

func TestServerErrorCarriesTheBodyBecauseThatIsWhereTheReasonIs(t *testing.T) {
	c, _ := serve(t, http.StatusBadRequest, `parse error at char 3`)
	_, err := c.Query(context.Background(), "q", time.Now())
	if err == nil || !strings.Contains(err.Error(), "parse error at char 3") {
		t.Fatalf("err=%v, want it to include the server's explanation", err)
	}
}

func TestPrometheusLevelErrorIsNotSilentlySuccessful(t *testing.T) {
	c, _ := serve(t, 200, `{"status":"error","errorType":"bad_data","error":"invalid parameter"}`)
	_, err := c.Query(context.Background(), "q", time.Now())
	if err == nil {
		t.Fatal("a 200 carrying status=error was treated as success; every device would look absent")
	}
	if !strings.Contains(err.Error(), "invalid parameter") {
		t.Fatalf("err=%v, want the upstream message", err)
	}
}

func TestUnparseableBodyNamesTheEndpoint(t *testing.T) {
	c, _ := serve(t, 200, `<html>a proxy ate this</html>`)
	_, err := c.Query(context.Background(), "q", time.Now())
	if err == nil {
		t.Fatal("HTML decoded as a Prometheus response")
	}
	if !strings.Contains(err.Error(), c.URL()) {
		t.Fatalf("err=%v, want the URL so the user knows which endpoint lied", err)
	}
}

// A projected ServiceAccount token rotates roughly hourly and a large scan can
// outlive one. Reading the file once at startup turns a long scan into a 401
// partway through.
func TestBearerTokenFileIsRereadOnEveryRequest(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(vector(`[1,"1"]`)))
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, Auth: AuthBearer, TokenFile: tok})
	if _, err := c.Query(context.Background(), "q", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tok, []byte("  second  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Query(context.Background(), "q", time.Now()); err != nil {
		t.Fatal(err)
	}
	want := []string{"Bearer first", "Bearer second"}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("request %d sent %q, want %q (rotation not picked up, or whitespace not trimmed)", i, seen[i], want[i])
		}
	}
}

func TestBasicAuthAndExtraHeadersAreSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "u" || p != "p" {
			t.Errorf("basic auth = %q/%q ok=%v", u, p, ok)
		}
		if r.Header.Get("X-Scope-OrgID") != "tenant-a" {
			t.Errorf("missing tenant header; multi-tenant Mimir/Cortex would return an empty result set")
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		_, _ = w.Write([]byte(vector(`[1,"1"]`)))
	}))
	defer srv.Close()
	c := New(Config{URL: srv.URL, Auth: AuthBasic, Username: "u", Password: "p",
		Headers: map[string]string{"X-Scope-OrgID": "tenant-a"}})
	if _, err := c.Query(context.Background(), "q", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// A trailing slash on the configured URL must not produce a double slash; some
// ingress controllers 404 on it.
func TestTrailingSlashInURLDoesNotBreakThePath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(vector(`[1,"1"]`)))
	}))
	defer srv.Close()
	c := New(Config{URL: srv.URL + "/"})
	if _, err := c.Query(context.Background(), "q", time.Now()); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/query" {
		t.Fatalf("path=%q, want /api/v1/query", path)
	}
}

// Queries go by POST form, not query string: a fortnight-wide PromQL join is
// long enough to trip the default URL length limit on nginx ingress.
func TestQueriesAreSentAsPostForm(t *testing.T) {
	c, cap := serve(t, 200, matrix(`[]`))
	start, end := time.Unix(1000, 0), time.Unix(2000, 0)
	if _, err := c.QueryRange(context.Background(), "up", start, end, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if cap.form.Get("query") != "up" {
		t.Fatalf("query=%q", cap.form.Get("query"))
	}
	if cap.form.Get("start") != "1000" || cap.form.Get("end") != "2000" {
		t.Fatalf("start/end = %q/%q", cap.form.Get("start"), cap.form.Get("end"))
	}
	if cap.form.Get("step") != "30s" {
		t.Fatalf("step=%q, want 30s", cap.form.Get("step"))
	}
}

func TestContextCancellationIsHonoured(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	// Release the handler before Close, which waits for outstanding requests.
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := New(Config{URL: srv.URL}).Query(ctx, "q", time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hung endpoint did not surface as an error; a scan would hang forever")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client ignored its context deadline against a hung endpoint")
	}
}

// When kube-prometheus-stack scrapes dcgm-exporter, the exporter's own pod
// label collides with the target label and is renamed. Assuming `pod` returns
// zero attribution on one of the most popular installations.
func TestDetectLabelSchema(t *testing.T) {
	cases := []struct {
		name   string
		labels string
		want   LabelSchema
		errIs  error
	}{
		{"plain dcgm", `{"pod":"trainer","namespace":"ml"}`, Standard, nil},
		{"relabelled by kube-prometheus-stack", `{"exported_pod":"trainer","exported_namespace":"ml"}`, Exported, nil},
		{"no attribution at all", `{"gpu":"0"}`, LabelSchema{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serve(t, 200, `{"status":"success","data":{"resultType":"vector","result":[{"metric":`+tc.labels+`,"value":[1,"1"]}]}}`)
			got, err := c.DetectLabelSchema(context.Background(), "DCGM_FI_DEV_GPU_UTIL", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("schema=%+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("both present prefers the one carrying identity", func(t *testing.T) {
		body := `{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"pod":"","exported_pod":"trainer","exported_namespace":"ml"},"value":[1,"1"]}]}}`
		c, _ := serve(t, 200, body)
		got, _ := c.DetectLabelSchema(context.Background(), "m", time.Now())
		if got.Pod != Exported.Pod {
			t.Fatalf("schema=%+v; an empty `pod` label must not beat a populated `exported_pod`", got)
		}
	})

	t.Run("no series is its own signal", func(t *testing.T) {
		c, _ := serve(t, 200, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
		_, err := c.DetectLabelSchema(context.Background(), "m", time.Now())
		if !errors.Is(err, ErrNoSeries) {
			t.Fatalf("err=%v, want ErrNoSeries so the message is 'dcgm-exporter is not installed', not 'detection failed'", err)
		}
	})
}

func TestRangeRendersDurationsPromQLAccepts(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{14 * 24 * time.Hour, "14d"},
		{24 * time.Hour, "1d"},
		{6 * time.Hour, "6h"},
		{90 * time.Minute, "90m"},
		{36 * time.Hour, "36h"}, // not a whole number of days
		{15 * time.Minute, "15m"},
	}
	for _, tc := range cases {
		if got := Range(tc.in); got != tc.want {
			t.Errorf("Range(%v)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSummarise(t *testing.T) {
	start := time.Unix(0, 0).UTC()
	step := time.Minute
	end := start.Add(9 * step) // 10 expected samples

	at := func(i int, v float64) Sample { return Sample{T: start.Add(time.Duration(i) * step), V: v} }

	t.Run("all zero over a full window is the reportable case", func(t *testing.T) {
		var s Series
		for i := 0; i < 10; i++ {
			s.Samples = append(s.Samples, at(i, 0))
		}
		st := Summarise(s, start, end, step, 4)
		if !st.ZeroThroughout {
			t.Fatal("ZeroThroughout=false for an all-zero series")
		}
		if st.LastNonZero != nil {
			t.Fatalf("LastNonZero=%v, want nil", st.LastNonZero)
		}
		if !st.UnusedSince.Equal(start) {
			t.Fatalf("UnusedSince=%v, want the window start %v", st.UnusedSince, start)
		}
		if st.Completeness != 1 {
			t.Fatalf("Completeness=%v, want 1", st.Completeness)
		}
	})

	t.Run("one non-zero sample resets the claim", func(t *testing.T) {
		var s Series
		for i := 0; i < 10; i++ {
			v := 0.0
			if i == 3 {
				v = 55
			}
			s.Samples = append(s.Samples, at(i, v))
		}
		st := Summarise(s, start, end, step, 4)
		if st.ZeroThroughout {
			t.Fatal("ten minutes of real work in the window did not reset ZeroThroughout")
		}
		if st.Max != 55 {
			t.Fatalf("Max=%v, want 55", st.Max)
		}
		if st.LastNonZero == nil || !st.LastNonZero.Equal(at(3, 0).T) {
			t.Fatalf("LastNonZero=%v, want the sample at index 3", st.LastNonZero)
		}
		if !st.UnusedSince.Equal(*st.LastNonZero) {
			t.Fatal("UnusedSince must start at the last real work, not at the window start")
		}
	})

	t.Run("an empty series is unknown, not idle", func(t *testing.T) {
		st := Summarise(Series{}, start, end, step, 4)
		if st.ZeroThroughout {
			t.Fatal("a series with no samples claimed to be measurably zero; absence of data is not evidence of idleness")
		}
		if st.Completeness != 0 {
			t.Fatalf("Completeness=%v, want 0", st.Completeness)
		}
	})

	t.Run("partial coverage is reported as partial", func(t *testing.T) {
		var s Series
		for i := 0; i < 2; i++ {
			s.Samples = append(s.Samples, at(i, 0))
		}
		st := Summarise(s, start, end, step, 4)
		if st.Completeness >= 0.8 {
			t.Fatalf("Completeness=%v for 2 of 10 samples; the idle checks gate on this", st.Completeness)
		}
		if got := 2.0 / 10.0; math.Abs(st.Completeness-got) > 1e-9 {
			t.Fatalf("Completeness=%v, want %v", st.Completeness, got)
		}
	})

	t.Run("completeness never exceeds one", func(t *testing.T) {
		var s Series
		for i := 0; i < 50; i++ {
			s.Samples = append(s.Samples, at(i, 0))
		}
		st := Summarise(s, start, end, step, 4)
		if st.Completeness > 1 {
			t.Fatalf("Completeness=%v; a value above 1 would read as impossible confidence", st.Completeness)
		}
	})

	t.Run("mean is over samples present", func(t *testing.T) {
		s := Series{Samples: []Sample{at(0, 0), at(1, 100)}}
		st := Summarise(s, start, end, step, 4)
		if st.Mean != 50 {
			t.Fatalf("Mean=%v, want 50", st.Mean)
		}
	})
}

// The sparkline exists to answer "did anything happen here". An average would
// hide a short burst, which is the one thing it must never do.
func TestBucketiseKeepsPeaksAndStaysInBounds(t *testing.T) {
	start := time.Unix(0, 0).UTC()
	end := start.Add(100 * time.Second)
	samples := []Sample{
		{T: start, V: 1},
		{T: start.Add(10 * time.Second), V: 90}, // a burst inside bucket 0
		{T: start.Add(50 * time.Second), V: 5},
		{T: end, V: 7},                   // exactly at the end
		{T: end.Add(time.Hour), V: 3},    // beyond the end
		{T: start.Add(-time.Hour), V: 4}, // before the start
	}
	got := bucketise(samples, start, end, 4)
	if len(got) != 4 {
		t.Fatalf("len=%d, want 4", len(got))
	}
	if got[0] < 90 {
		t.Fatalf("bucket 0 = %v, want the 90 burst preserved", got[0])
	}
	if got[3] < 7 {
		t.Fatalf("bucket 3 = %v, want the boundary and out-of-range samples clamped in, not dropped", got[3])
	}
	if bucketise(samples, start, start, 4) == nil {
		t.Fatal("a zero-length span returned nil rather than empty buckets")
	}
	if bucketise(nil, start, end, 4) != nil {
		t.Fatal("no samples should render no sparkline")
	}
	if bucketise(samples, start, end, 0) != nil {
		t.Fatal("zero buckets should render nothing")
	}
}

func TestPingSurfacesAuthErrorsDistinctly(t *testing.T) {
	c, _ := serve(t, http.StatusUnauthorized, `no`)
	var ae *AuthError
	if err := c.Ping(context.Background()); !errors.As(err, &ae) {
		t.Fatalf("Ping err=%T (%v), want *AuthError", err, err)
	}
}

func TestEnvHeaders(t *testing.T) {
	got, err := EnvHeaders([]string{"X-Scope-OrgID=tenant-a", "X-Other= spaced "})
	if err != nil {
		t.Fatal(err)
	}
	if got["X-Scope-OrgID"] != "tenant-a" {
		t.Fatalf("got %v", got)
	}
	if got["X-Other"] != "spaced" {
		t.Fatalf("value not trimmed: %q", got["X-Other"])
	}
	if _, err := EnvHeaders([]string{"no-equals-sign"}); err == nil {
		t.Fatal("a malformed header flag was accepted; the user would see no header and no reason")
	}
	t.Run("value may contain equals", func(t *testing.T) {
		got, err := EnvHeaders([]string{"Authorization=Basic dXNlcjpwYXNz=="})
		if err != nil {
			t.Fatal(err)
		}
		if got["Authorization"] != "Basic dXNlcjpwYXNz==" {
			t.Fatalf("base64 padding was truncated: %q", got["Authorization"])
		}
	})
}

func TestTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t")
	if err := os.WriteFile(p, []byte("\n  abc \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := TokenFromFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc" {
		t.Fatalf("token=%q, want %q; a trailing newline in an Authorization header is rejected by many proxies", got, "abc")
	}
	if _, err := TokenFromFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing token file must be an error, not an empty token sent as `Bearer `")
	}
}

func TestJSONNumbersAreAcceptedAsWellAsStrings(t *testing.T) {
	// Some Prometheus-compatible backends emit real JSON numbers.
	if v, ok := toFloat(json.Number("1.5")); !ok || v != 1.5 {
		t.Fatalf("json.Number: got %v %v", v, ok)
	}
	if v, ok := toFloat(float64(2)); !ok || v != 2 {
		t.Fatalf("float64: got %v %v", v, ok)
	}
	if _, ok := toFloat(true); ok {
		t.Fatal("a bool parsed as a number")
	}
	if _, ok := toFloat(json.Number("NaN")); ok {
		t.Fatal("json.Number NaN accepted")
	}
}
