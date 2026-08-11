package scan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ullage-project/ullage/internal/inventory"
	"github.com/ullage-project/ullage/internal/promql"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

var (
	base  = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	sched = promql.LabelSchema{Found: true, Pod: "pod", Namespace: "namespace"}
)

func at(d time.Duration) time.Time { return base.Add(d) }

// ---------------------------------------------------------------------------
// Series and device identity
// ---------------------------------------------------------------------------

// A physical card carries one series per holder over a window, because
// dcgm-exporter stamps the holding pod onto the series. Collapsing them by
// device made shape lookups last-write-wins, and a stale series overwrote a
// running job's — reporting a GPU at 78% utilization as having done no work.
func TestSeriesKeySeparatesTwoHoldersOfOneDevice(t *testing.T) {
	dev := map[string]string{"Hostname": "gpu-a", "gpu": "0"}

	first := map[string]string{"Hostname": "gpu-a", "gpu": "0", "pod": "job-1", "namespace": "ml"}
	second := map[string]string{"Hostname": "gpu-a", "gpu": "0", "pod": "job-2", "namespace": "ml"}

	if deviceKey(first) != deviceKey(second) {
		t.Fatal("two tenures of one card must share a device key; they are the same hardware")
	}
	if seriesKey(first, sched) == seriesKey(second, sched) {
		t.Fatalf("seriesKey collapsed two holders of gpu-a/0 into %q. One holder's shape then "+
			"overwrites the other's, and whichever is written last decides what the report "+
			"says about both", seriesKey(first, sched))
	}
	_ = dev
}

// Two teams naming a pod the same thing is routine, and the namespace is the
// only thing separating them.
func TestSeriesKeySeparatesIdenticalPodNamesInDifferentNamespaces(t *testing.T) {
	a := map[string]string{"Hostname": "gpu-a", "gpu": "0", "pod": "trainer", "namespace": "team-a"}
	b := map[string]string{"Hostname": "gpu-a", "gpu": "0", "pod": "trainer", "namespace": "team-b"}
	if seriesKey(a, sched) == seriesKey(b, sched) {
		t.Fatalf("team-a/trainer and team-b/trainer share the key %q on one device",
			seriesKey(a, sched))
	}
}

// Without attribution every series on a device is the device itself; inventing
// a distinction from unread labels would split one card into several.
func TestSeriesKeyIgnoresPodLabelsWhenNoSchemaWasDetected(t *testing.T) {
	none := promql.LabelSchema{}
	a := map[string]string{"Hostname": "gpu-a", "gpu": "0", "pod": "one"}
	b := map[string]string{"Hostname": "gpu-a", "gpu": "0", "pod": "two"}
	if seriesKey(a, none) != seriesKey(b, none) {
		t.Fatal("with no detected schema the pod label is not trusted, so it must not " +
			"split one device into two")
	}
}

func TestDeviceKeyAcceptsTheLabelNamesExportersActuallyUse(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{"dcgm Hostname and gpu", map[string]string{"Hostname": "gpu-a", "gpu": "0"}},
		{"lowercase hostname", map[string]string{"hostname": "gpu-a", "gpu": "0"}},
		{"kubernetes node label", map[string]string{"node": "gpu-a", "gpu": "0"}},
		{"scrape instance", map[string]string{"instance": "gpu-a", "device": "0"}},
		{"MIG instance id", map[string]string{"Hostname": "gpu-a", "GPU_I_ID": "0"}},
		{"UUID as the device", map[string]string{"Hostname": "gpu-a", "UUID": "0"}},
	}
	want := "gpu-a/0"
	for _, tc := range cases {
		if got := deviceKey(tc.labels); got != want {
			t.Errorf("%s: deviceKey = %q, want %q; an unrecognised label name silently "+
				"merges every card in the cluster into one key", tc.name, got, want)
		}
	}
}

func TestDeviceKeyPrefersTheMoreSpecificLabel(t *testing.T) {
	// An exporter may emit both; Hostname is the exporter's own view.
	got := deviceKey(map[string]string{"Hostname": "real", "instance": "10.0.0.1:9400", "gpu": "0"})
	if got != "real/0" {
		t.Fatalf("deviceKey = %q, want real/0: a scrape address changes when a pod restarts "+
			"and would make one card look like several", got)
	}
}

func TestDeviceKeyTreatsAnEmptyLabelAsAbsent(t *testing.T) {
	got := deviceKey(map[string]string{"Hostname": "", "node": "gpu-a", "gpu": "0"})
	if got != "gpu-a/0" {
		t.Fatalf("deviceKey = %q, want gpu-a/0: an empty label is not a value, and falling "+
			"through to the next name is the difference between one key and one per card", got)
	}
}

// ---------------------------------------------------------------------------
// refine
// ---------------------------------------------------------------------------

func series(samples ...promql.Sample) promql.Series {
	return promql.Series{Samples: samples}
}

func sample(d time.Duration, v float64) promql.Sample {
	return promql.Sample{T: at(d), V: v}
}

// An exporter that dies, a node that leaves, or a holder label that stops being
// emitted all present as a device that has been perfectly idle ever since.
// Without staleness the monitoring breaking generates deletion recommendations.
func TestRefineMarksASeriesThatStoppedArriving(t *testing.T) {
	step := time.Hour
	end := at(24 * time.Hour)

	st := &inventory.Stats{}
	refine(st, series(sample(0, 0), sample(time.Hour, 0), sample(2*time.Hour, 0)), base, end, step)

	if !st.Stale {
		t.Fatal("a series whose last sample is 22 hours before the window closed was not " +
			"marked stale; it is not a device reading zero, it is a device nobody is watching")
	}
	if st.LastSample == nil || !st.LastSample.Equal(at(2*time.Hour)) {
		t.Fatalf("LastSample = %v, want %v", st.LastSample, at(2*time.Hour))
	}
}

func TestRefineToleratesAFewMissedScrapes(t *testing.T) {
	step := time.Hour
	end := at(24 * time.Hour)

	st := &inventory.Stats{}
	// Last sample two resolutions back: a delayed scrape plus grid alignment.
	refine(st, series(sample(21*time.Hour, 0), sample(22*time.Hour, 0)), base, end, step)

	if st.Stale {
		t.Fatal("a series two resolutions behind the window close was called stale; a " +
			"delayed scrape and the alignment of the range grid must not read as a dead exporter")
	}
}

// A node that joined yesterday cannot give evidence about last week.
func TestRefineWillNotClaimIdlenessFromBeforeTheFirstSample(t *testing.T) {
	step := time.Hour
	end := at(24 * time.Hour)

	st := &inventory.Stats{}
	refine(st, series(sample(20*time.Hour, 0), sample(21*time.Hour, 0), sample(22*time.Hour, 0)), base, end, step)

	if st.FallowSince.Before(at(20 * time.Hour)) {
		t.Fatalf("FallowSince = %v, before the first sample at %v. A device cannot give "+
			"evidence about time before it was being watched, and dating idleness to the "+
			"window's start invents nearly a day of it", st.FallowSince, at(20*time.Hour))
	}
}

// The stepped series is a downsample of the aggregate, so it can find work the
// aggregate missed but must never talk it down.
func TestRefineOnlyEverRaisesTheMeasuredPeak(t *testing.T) {
	step := time.Hour
	end := at(24 * time.Hour)

	st := &inventory.Stats{Max: 0.9, ZeroThroughout: false}
	refine(st, series(sample(0, 0), sample(time.Hour, 0)), base, end, step)
	if st.Max != 0.9 {
		t.Fatalf("Max fell to %v from 0.9; a downsampled series missing the busy moment is "+
			"not evidence the busy moment did not happen", st.Max)
	}

	st = &inventory.Stats{Max: 0, ZeroThroughout: true}
	refine(st, series(sample(0, 0), sample(time.Hour, 0.5)), base, end, step)
	if st.Max != 0.5 || st.ZeroThroughout {
		t.Fatalf("Max=%v ZeroThroughout=%v; the shape found work the aggregate missed and "+
			"that must clear the zero claim", st.Max, st.ZeroThroughout)
	}
}

func TestRefineOnAnEmptySeriesLeavesNoDating(t *testing.T) {
	st := &inventory.Stats{}
	refine(st, series(), base, at(24*time.Hour), time.Hour)
	if st.LastSample != nil {
		t.Fatalf("LastSample = %v for a series with no samples", st.LastSample)
	}
	if st.Stale {
		t.Fatal("an empty series was marked stale rather than left unknown; staleness is a " +
			"claim about when data stopped, and there was never any data to stop")
	}
}

// ---------------------------------------------------------------------------
// sortFindings
// ---------------------------------------------------------------------------

func cost(v float64) *float64 { return &v }

func TestPricedFindingsRankAboveUnpricedOnes(t *testing.T) {
	f := []api.Finding{
		{ID: "unpriced", Impact: api.Impact{GPUHoursFallow: 5000}},
		{ID: "priced", Impact: api.Impact{WindowCost: cost(10), GPUHoursFallow: 1}},
	}
	sortFindings(f)
	if f[0].ID != "priced" {
		t.Fatalf("order = %v; a missing price is not a small number, but a number you "+
			"cannot compare cannot be claimed to be larger either", idsOf(f))
	}
}

// 2,700 hours of a cheap card is worth less than 1,000 hours of an expensive
// one, so ranking by hours puts the smallest finding first.
func TestRankingFollowsMoneyNotHours(t *testing.T) {
	f := []api.Finding{
		{ID: "many-cheap-hours", Impact: api.Impact{WindowCost: cost(900), GPUHoursFallow: 2700}},
		{ID: "fewer-costly-hours", Impact: api.Impact{WindowCost: cost(2800), GPUHoursFallow: 1000}},
	}
	sortFindings(f)
	if f[0].ID != "fewer-costly-hours" {
		t.Fatalf("order = %v; the first row is the only one some readers will act on", idsOf(f))
	}
}

func TestUnpricedFindingsRankAmongThemselvesByHours(t *testing.T) {
	f := []api.Finding{
		{ID: "small", Impact: api.Impact{GPUHoursFallow: 10}},
		{ID: "large", Impact: api.Impact{GPUHoursFallow: 900}},
	}
	sortFindings(f)
	if f[0].ID != "large" {
		t.Fatalf("order = %v, want the larger unpriced finding first", idsOf(f))
	}
}

func TestRankingIsStableOnAnUnchangedCluster(t *testing.T) {
	build := func() []api.Finding {
		return []api.Finding{
			{ID: "c", Impact: api.Impact{WindowCost: cost(5), GPUHoursFallow: 2},
				Evidence: api.Evidence{FallowDuration: api.ISODuration(time.Hour)}},
			{ID: "a", Impact: api.Impact{WindowCost: cost(5), GPUHoursFallow: 2},
				Evidence: api.Evidence{FallowDuration: api.ISODuration(time.Hour)}},
			{ID: "b", Impact: api.Impact{WindowCost: cost(5), GPUHoursFallow: 2},
				Evidence: api.Evidence{FallowDuration: api.ISODuration(time.Hour)}},
		}
	}
	f := build()
	sortFindings(f)
	want := idsOf(f)
	for i := 0; i < 50; i++ {
		g := build()
		sortFindings(g)
		if got := idsOf(g); !equal(got, want) {
			t.Fatalf("order changed between runs: %v then %v. A list that reshuffles on an "+
				"unchanged cluster cannot be diffed, and diffing it is how people track "+
				"whether the waste is shrinking", want, got)
		}
	}
	if want[0] != "a" {
		t.Fatalf("ties broke to %v, want the ID order a, b, c", want)
	}
}

// ---------------------------------------------------------------------------
// Confidence and scope
// ---------------------------------------------------------------------------

func TestConfidenceGateAdmitsEqualAndStrongerOnly(t *testing.T) {
	cases := []struct {
		have, min string
		want      bool
	}{
		{"high", "high", true},
		{"high", "medium", true},
		{"high", "low", true},
		{"medium", "medium", true},
		{"medium", "high", false},
		{"low", "medium", false},
		{"low", "low", true},
	}
	for _, tc := range cases {
		if got := meetsConfidence(tc.have, tc.min); got != tc.want {
			t.Errorf("meetsConfidence(%q, %q) = %v, want %v", tc.have, tc.min, got, tc.want)
		}
	}
}

// An unrecognised confidence must not outrank a real one. Ranking it above
// "high" would let a typo publish everything.
func TestAnUnknownConfidenceNeverClearsARealBar(t *testing.T) {
	if meetsConfidence("bogus", "medium") {
		t.Fatal("an unrecognised confidence cleared a medium bar; a typo in a check must " +
			"not publish findings the operator asked to filter out")
	}
}

func TestNamespaceScopeDefaultsToTheWholeCluster(t *testing.T) {
	o := &Options{}
	o.Defaults()
	if !o.inScope("anything") {
		t.Fatal("with no namespace filters set, every namespace must be in scope; " +
			"defaulting to none would silently produce an empty report")
	}
}

// Node and pool findings carry no namespace. A namespace filter must not
// silently delete them, or `--namespace ml` reports pod findings while quietly
// hiding an entire idle node pool.
func TestNamespaceFilterDoesNotHideClusterScopedFindings(t *testing.T) {
	o := &Options{Namespaces: []string{"ml"}}
	if !o.inScope("") {
		t.Fatal("a finding with no namespace was filtered out by a namespace allow-list; " +
			"an idle node pool belongs to no namespace and would vanish from the report")
	}
}

func TestNamespaceInclusionExcludesEverythingElse(t *testing.T) {
	o := &Options{Namespaces: []string{"ml", "research"}}
	if !o.inScope("ml") || !o.inScope("research") {
		t.Fatal("a named namespace was excluded")
	}
	if o.inScope("kube-system") {
		t.Fatal("an unnamed namespace was included despite an explicit allow-list")
	}
}

// ---------------------------------------------------------------------------

func idsOf(f []api.Finding) []string {
	out := make([]string, 0, len(f))
	for _, x := range f {
		out = append(out, x.ID)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// chunked
// ---------------------------------------------------------------------------

// promStub serves a canned vector per query instant, so a chunked aggregate can
// be driven over several sub-windows without a Prometheus.
type promStub struct {
	mu     sync.Mutex
	byTime map[string][]map[string]any // RFC3339 instant -> result entries
	fail   map[string]bool
	seen   []string
}

func (p *promStub) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parsing request: %v", err)
		}
		ts := r.FormValue("time")
		p.mu.Lock()
		p.seen = append(p.seen, ts)
		failed := p.fail[ts]
		entries := p.byTime[ts]
		p.mu.Unlock()

		if failed {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"error","error":"block not found"}`))
			return
		}
		if entries == nil {
			entries = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": entries},
		})
	}
}

func vec(labels map[string]string, at time.Time, v float64) map[string]any {
	return map[string]any{
		"metric": labels,
		"value":  []any{float64(at.Unix()), strconv.FormatFloat(v, 'f', -1, 64)},
	}
}

func gatherer(t *testing.T, p *promStub) *Gatherer {
	t.Helper()
	srv := httptest.NewServer(p.handler(t))
	t.Cleanup(srv.Close)
	return &Gatherer{Prom: promql.New(promql.Config{URL: srv.URL})}
}

func instantOf(tm time.Time) string { return strconv.FormatInt(tm.Unix(), 10) }

// A window longer than one chunk is evaluated in sub-windows, and the peak of
// the whole window is the peak of any of them. Taking the last chunk's value,
// or the first, would report a device as idle because it happened to be idle
// yesterday.
func TestChunkedCombinesPeaksAcrossSubWindows(t *testing.T) {
	end := base.Add(72 * time.Hour)
	labels := map[string]string{"Hostname": "gpu-a", "gpu": "0"}

	p := &promStub{byTime: map[string][]map[string]any{
		instantOf(end):                      {vec(labels, end, 0)},
		instantOf(end.Add(-24 * time.Hour)): {vec(labels, end, 0.91)},
		instantOf(end.Add(-48 * time.Hour)): {vec(labels, end, 0.2)},
	}}
	g := gatherer(t, p)

	got, err := g.chunked(context.Background(), "m", "max_over_time", sched, base, end, maxCombine)
	if err != nil {
		t.Fatalf("chunked: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d samples for one device across three chunks, want 1", len(got))
	}
	if got[0].Value != 0.91 {
		t.Fatalf("combined peak = %v, want 0.91. A device busy two days ago and idle since "+
			"has not been idle for the window", got[0].Value)
	}
}

func TestChunkedSumsWhereSummingIsTheAggregate(t *testing.T) {
	end := base.Add(48 * time.Hour)
	labels := map[string]string{"Hostname": "gpu-a", "gpu": "0"}
	p := &promStub{byTime: map[string][]map[string]any{
		instantOf(end):                      {vec(labels, end, 10)},
		instantOf(end.Add(-24 * time.Hour)): {vec(labels, end, 5)},
	}}
	g := gatherer(t, p)

	got, err := g.chunked(context.Background(), "m", "count_over_time", sched, base, end, sumCombine)
	if err != nil {
		t.Fatalf("chunked: %v", err)
	}
	if len(got) != 1 || got[0].Value != 15 {
		t.Fatalf("combined count = %v, want 15; sample counts must accumulate over the "+
			"sub-windows or coverage reads as a fraction of one chunk", got)
	}
}

// A Prometheus that has lost older blocks, or whose retention is shorter than
// the window, should shorten the evidence rather than abort the scan.
func TestChunkedSurvivesAMissingOlderBlock(t *testing.T) {
	end := base.Add(48 * time.Hour)
	labels := map[string]string{"Hostname": "gpu-a", "gpu": "0"}
	p := &promStub{
		byTime: map[string][]map[string]any{instantOf(end): {vec(labels, end, 0.4)}},
		fail:   map[string]bool{instantOf(end.Add(-24 * time.Hour)): true},
	}
	g := gatherer(t, p)

	got, err := g.chunked(context.Background(), "m", "max_over_time", sched, base, end, maxCombine)
	if err != nil {
		t.Fatalf("chunked failed entirely because one sub-window was unavailable: %v. "+
			"Short retention is normal and must shorten the evidence, not abort the scan", err)
	}
	if len(got) != 1 || got[0].Value != 0.4 {
		t.Fatalf("got %v, want the surviving chunk's value", got)
	}
}

// Silence is different from partial data. If nothing succeeded there is no
// evidence at all, and returning an empty result would read as "every device
// is idle".
func TestChunkedRefusesWhenEverySubWindowFails(t *testing.T) {
	end := base.Add(48 * time.Hour)
	p := &promStub{fail: map[string]bool{
		instantOf(end):                      true,
		instantOf(end.Add(-24 * time.Hour)): true,
	}}
	g := gatherer(t, p)

	if got, err := g.chunked(context.Background(), "m", "max_over_time", sched, base, end, maxCombine); err == nil {
		t.Fatalf("chunked returned %v and no error with every sub-window failing; an empty "+
			"result reads as a cluster of perfectly idle GPUs", got)
	}
}

// Two tenures of one card must not be merged by the aggregate either. Summing
// a count across them inflates one holder's coverage with another's samples.
func TestChunkedKeepsTwoHoldersOfOneDeviceApart(t *testing.T) {
	end := base.Add(24 * time.Hour)
	a := map[string]string{"Hostname": "gpu-a", "gpu": "0", "pod": "job-a", "namespace": "ml"}
	b := map[string]string{"Hostname": "gpu-a", "gpu": "0", "pod": "job-b", "namespace": "ml"}

	p := &promStub{byTime: map[string][]map[string]any{
		instantOf(end): {vec(a, end, 100), vec(b, end, 7)},
	}}
	g := gatherer(t, p)

	got, err := g.chunked(context.Background(), "m", "count_over_time", sched, base, end, sumCombine)
	if err != nil {
		t.Fatalf("chunked: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results for two holders of one card, want 2. Merging them lets a "+
			"pod that was watched for minutes inherit the coverage of one watched for days: %v",
			len(got), got)
	}
}

// The same pod name in two namespaces is routine, and the namespace is the only
// thing telling them apart.
func TestChunkedKeepsIdenticalPodNamesInDifferentNamespacesApart(t *testing.T) {
	end := base.Add(24 * time.Hour)
	a := map[string]string{"Hostname": "gpu-a", "gpu": "0", "pod": "trainer", "namespace": "team-a"}
	b := map[string]string{"Hostname": "gpu-a", "gpu": "0", "pod": "trainer", "namespace": "team-b"}

	p := &promStub{byTime: map[string][]map[string]any{
		instantOf(end): {vec(a, end, 100), vec(b, end, 7)},
	}}
	g := gatherer(t, p)

	got, err := g.chunked(context.Background(), "m", "count_over_time", sched, base, end, sumCombine)
	if err != nil {
		t.Fatalf("chunked: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results for team-a/trainer and team-b/trainer on one card, want 2. "+
			"Merging them attributes one team's samples to the other: %v", len(got), got)
	}
}
