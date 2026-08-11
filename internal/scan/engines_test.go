package scan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ullage-project/ullage/internal/kube"
	"github.com/ullage-project/ullage/internal/promql"
)

// enginePromise is a fake Prometheus that answers per-metric, so a test can say
// "the SM gauge read zero for a fortnight, but the video encoder did not".
//
// values maps a metric name to the value every query about it returns. A metric
// absent from the map returns no series at all, which is how a dcgm-exporter
// configured down to a handful of counters behaves and must be distinguishable
// from one that answered zero.
func enginePromise(t *testing.T, values map[string]float64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")

		var result []any
		for metric, v := range values {
			if !strings.Contains(q, metric) {
				continue
			}
			// Schema detection asks whether pod labels exist; this cluster has
			// none, which keeps the test on the device path.
			if strings.Contains(q, "pod") {
				continue
			}
			result = append(result, map[string]any{
				"metric": map[string]string{"Hostname": "gpu-a", "gpu": "0"},
				"value":  []any{float64(time.Now().Unix()), jsonNum(v)},
			})
			break
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

func jsonNum(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func gatherWithProm(t *testing.T, prom *httptest.Server) (*Gatherer, Options) {
	t.Helper()
	kapi := httptest.NewServer((&fakeAPI{}).handler(t))
	t.Cleanup(kapi.Close)

	kc, err := kube.New(kube.Config{APIServer: kapi.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Now:      time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		Window:   14 * 24 * time.Hour,
		Step:     time.Hour,
		Progress: func(string) {},
	}
	opts.Defaults()
	return &Gatherer{Kube: kc, Prom: promql.New(promql.Config{URL: prom.URL})}, opts
}

// The single most damaging thing this tool could get wrong.
//
// DCGM_FI_DEV_GPU_UTIL reports SM activity. A video transcoding pipeline lives
// on NVENC and NVDEC and touches the SMs barely or not at all, so it reads a
// clean zero across a fortnight on the one gauge v0.1 consulted. The device is
// busy, expensive, and entirely in use -- and the tool would have printed
// `kubectl scale --replicas=0` against it with high confidence, because a
// fortnight of zeroes is exactly the evidence the idle check is looking for.
//
// Reading zero on the SMs is the absence of one kind of work, not the presence
// of idleness.
func TestDeviceBusyOnAnotherEngineIsNotIdle(t *testing.T) {
	prom := enginePromise(t, map[string]float64{
		MetricGPUUtil:               0,
		"DCGM_FI_DEV_ENC_UTIL":      87,
		"DCGM_FI_DEV_DEC_UTIL":      0,
		"DCGM_FI_DEV_MEM_COPY_UTIL": 0,
	})
	g, opts := gatherWithProm(t, prom)

	cl, _, _, err := g.Gather(context.Background(), opts)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(cl.Devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(cl.Devices))
	}
	d := cl.Devices[0]

	if d.Util.ZeroThroughout {
		t.Fatal("the video encoder ran at 87% for the whole window, but the device was " +
			"reported as having done nothing throughout -- this is the reading that " +
			"produces a confident recommendation to delete a running transcoding pipeline")
	}
	if d.Util.BusyEngine == "" {
		t.Fatal("the device was correctly not called idle, but nothing records why; " +
			"an operator asking why their obviously-busy GPU is missing from the report " +
			"has nothing to read")
	}
	if !strings.Contains(d.Util.BusyEngine, "encoder") {
		t.Fatalf("BusyEngine = %q, want it to name the video encoder", d.Util.BusyEngine)
	}
}

// The complement, and the reason the fix is not simply "never call anything
// idle": when every engine DCGM exports reads zero, the device really did do
// nothing, and withholding the finding would make the tool useless.
func TestDeviceIdleOnEveryEngineIsStillIdle(t *testing.T) {
	prom := enginePromise(t, map[string]float64{
		MetricGPUUtil:               0,
		"DCGM_FI_DEV_ENC_UTIL":      0,
		"DCGM_FI_DEV_DEC_UTIL":      0,
		"DCGM_FI_DEV_MEM_COPY_UTIL": 0,
		"DCGM_FI_DEV_FB_USED":       0,
	})
	g, opts := gatherWithProm(t, prom)

	cl, _, warnings, err := g.Gather(context.Background(), opts)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if !cl.Devices[0].Util.ZeroThroughout {
		t.Fatal("every engine read zero, so the device did nothing; refusing to say so " +
			"leaves the tool unable to report the case it exists for")
	}
	if len(cl.EnginesChecked) != 4 {
		t.Fatalf("EnginesChecked = %v, want all four engines recorded as consulted", cl.EnginesChecked)
	}
	for _, w := range warnings {
		if strings.Contains(w, "SM activity alone") {
			t.Fatalf("all four engines answered, but the report still hedges: %q", w)
		}
	}
}

// dcgm-exporter can be configured down to a minimal counter set, and refusing
// to run against those clusters would be worse than running with the SM gauge
// alone. But the operator must be told, because "no GPU work" from a tool that
// only looked at the SMs is a materially weaker claim than one that checked
// the encoder, the decoder and the copy engines too.
func TestMissingEngineMetricsAreDisclosedNotAssumedIdle(t *testing.T) {
	prom := enginePromise(t, map[string]float64{MetricGPUUtil: 0})
	g, opts := gatherWithProm(t, prom)

	cl, _, warnings, err := g.Gather(context.Background(), opts)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(cl.EnginesChecked) != 0 {
		t.Fatalf("EnginesChecked = %v, want empty: nothing but the SM gauge answered", cl.EnginesChecked)
	}

	var told bool
	for _, w := range warnings {
		if strings.Contains(w, "SM activity alone") {
			told = true
		}
	}
	if !told {
		t.Fatalf("only the SM gauge was available and the report does not say so, so a "+
			"reader cannot tell this scan from one that ruled out encoder and copy-engine "+
			"work: %q", warnings)
	}
}
