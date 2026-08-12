package main

import (
	"fmt"
	"io"
	"strings"
)

// explainQueries prints the PromQL a scan issued, once each.
//
// The scan runs most of these per device, so a modest cluster produced eighty-odd
// lines covering nine distinct queries -- which reads as noise at exactly the
// moment someone is trying to check the tool's work. What a reader wants here is
// the shape of the evidence, so each query is shown once and told what question
// it answers. Without that, "max_over_time(DCGM_FI_DEV_FB_USED[1d])" is a string
// only a DCGM expert can evaluate.
func explainQueries(w io.Writer, queries []string) {
	seen := make(map[string]bool, len(queries))
	unique := make([]string, 0, len(queries))
	for _, q := range queries {
		if seen[q] {
			continue
		}
		seen[q] = true
		unique = append(unique, q)
	}

	fmt.Fprintf(w, "%d distinct queries, each issued once per device or node.\n", len(unique))
	fmt.Fprintf(w, "Every idle claim this scan makes rests on these and nothing else.\n\n")
	for _, q := range unique {
		if p := purposeOf(q); p != "" {
			fmt.Fprintf(w, "# %s\n", p)
		}
		fmt.Fprintln(w, q)
	}
}

// purposeOf explains a query in the terms of the decision it feeds.
func purposeOf(q string) string {
	switch {
	case strings.Contains(q, "@ range"):
		return "when work last ran, to date the start of the fallow run"
	case strings.Contains(q, "count_over_time") && strings.Contains(q, "[1h]"):
		return "scrape interval, inferred from how many samples land in an hour"
	case strings.Contains(q, "count_over_time"):
		return "how many samples actually exist -- absent samples are not zero samples"
	case strings.Contains(q, "POWER_USAGE"):
		return "mean board power, to corroborate idle against a second, independent sensor"
	case strings.Contains(q, "ENC_UTIL"):
		return "video encoder activity -- work the SM gauge reports as zero"
	case strings.Contains(q, "DEC_UTIL"):
		return "video decoder activity -- work the SM gauge reports as zero"
	case strings.Contains(q, "MEM_COPY_UTIL"):
		return "copy-engine activity: data loading and checkpointing"
	case strings.Contains(q, "FB_USED"):
		return "framebuffer held -- a warm model parked between requests"
	case strings.Contains(q, "PROF_SM_ACTIVE"):
		return "fine-grained SM occupancy, where the profiling counters are exported"
	case strings.Contains(q, "GPU_UTIL"):
		return "peak SM activity: the primary compute signal"
	}
	return ""
}
