package scan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scrapePromise answers count_over_time with a count consistent with a series
// scraped every `every` that is present for `covered` of any interval asked
// about, and answers everything else with a single zero-valued series.
//
// The range is read out of the query rather than assumed, because the real
// count is gathered in chunks and a fixed answer per chunk would multiply.
func scrapePromise(t *testing.T, every time.Duration, covered float64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")

		var result []any
		if !strings.Contains(q, "pod") {
			v := "0"
			switch {
			case strings.Contains(q, "count_over_time") && strings.Contains(q, "[1h]"):
				// The probe: how many points arrive in an hour.
				v = fmtFloat(time.Hour.Seconds()/every.Seconds() + 1)
			case strings.Contains(q, "count_over_time"):
				v = fmtFloat(rangeOf(q).Seconds() / every.Seconds() * covered)
			}
			result = append(result, map[string]any{
				"metric": map[string]string{"Hostname": "gpu-a", "gpu": "0"},
				"value":  []any{float64(time.Now().Unix()), v},
			})
		}

		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "query_range") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   map[string]any{"resultType": "matrix", "result": []any{}},
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

// rangeOf pulls the [24h] out of a range query.
func rangeOf(q string) time.Duration {
	i, j := strings.Index(q, "["), strings.Index(q, "]")
	if i < 0 || j < i {
		return time.Hour
	}
	// Prometheus spells a day "1d", which Go's ParseDuration rejects.
	raw := q[i+1 : j]
	if strings.HasSuffix(raw, "d") {
		days, err := time.ParseDuration(strings.TrimSuffix(raw, "d") + "h")
		if err != nil {
			return time.Hour
		}
		return days * 24
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return time.Hour
	}
	return d
}

func fmtFloat(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Coverage is samples divided by window/interval, and the interval used to be
// assumed to be 30s unconditionally.
//
// dcgm-exporter scraped by kube-prometheus-stack at 15s -- an entirely
// ordinary configuration -- halves the expected count. Seven days of data
// across a fourteen-day window then divides out to 100% coverage: a full
// fortnight of confident observation conjured from half a window of data, on
// the one number that decides whether a recommendation gets printed at all.
func TestCoverageIsNotInflatedByAFasterScrape(t *testing.T) {
	const window = 14 * 24 * time.Hour
	prom := scrapePromise(t, 15*time.Second, 0.5)
	g, opts := gatherWithProm(t, prom)

	cl, _, _, err := g.Gather(context.Background(), opts)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if cl.Step != 15*time.Second {
		t.Fatalf("measured scrape interval = %s, want 15s: the exporter's real rate was "+
			"never measured, so every coverage figure is scaled by the ratio between the "+
			"assumption and the truth", cl.Step)
	}

	got := cl.Devices[0].Util.CoverageOver(window, cl.Step)
	if got > 0.6 {
		t.Fatalf("coverage = %.0f%% over a window in which only half the expected samples "+
			"exist. Half the observation must not read as all of it; this is the number "+
			"that decides whether ullage is confident enough to print a command.", got*100)
	}
	if got < 0.4 {
		t.Fatalf("coverage = %.0f%%, want about 50%%: overcorrecting hides real findings",
			got*100)
	}
}

// The default must survive a cluster that cannot answer the probe, because a
// tool that refuses to run when it cannot measure something is worse than one
// that documents its assumption.
func TestScrapeIntervalFallsBackWhenUnmeasurable(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{"resultType": "vector", "result": []any{map[string]any{
				"metric": map[string]string{"Hostname": "gpu-a", "gpu": "0"},
				"value":  []any{float64(time.Now().Unix()), "0"},
			}}},
		})
	}))
	t.Cleanup(prom.Close)

	g, opts := gatherWithProm(t, prom)
	cl, _, _, err := g.Gather(context.Background(), opts)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if cl.Step != ScrapeInterval {
		t.Fatalf("Step = %s, want the documented %s default when the probe cannot answer",
			cl.Step, ScrapeInterval)
	}
}
