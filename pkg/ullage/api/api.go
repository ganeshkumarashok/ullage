// Package api defines ullage's stable public contract.
//
// The JSON rendering of these types is what other tools consume, and it is
// versioned: fields are added, never repurposed, and never removed within a
// major version. This package is deliberately outside internal/ so that
// embedders — a k8sgpt analyzer, a Grafana panel, an operator, a CI gate — can
// depend on the result shape without depending on how it was produced.
//
// It contains data and marshalling only. Anything that decides how a value is
// shown to a person belongs in the renderer, not here.
package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Version is the contract version carried in Result.APIVersion.
const Version = "ullage.dev/v0.1"

// Check identifiers. Semantic and stable: they are the documentation anchor,
// the suppression key, and the JSON discriminator.
const (
	CheckIdlePod    = "idle-pod"
	CheckUnusedNode = "unused-node"
	CheckStuckPod   = "stuck-pod"
)

// Evidence confidence describes how certain the measurement is. It filters.
const (
	EvidenceHigh   = "high"
	EvidenceMedium = "medium"
	EvidenceLow    = "low"
)

// Ownership confidence describes how certain the attribution is. It never
// filters and never demotes a finding; it is rendered independently.
const (
	OwnerResolved = "resolved"
	OwnerInferred = "inferred"
	OwnerUnowned  = "unowned"
)

// Allocation modes. Only Exclusive supports a per-pod idleness claim.
const (
	AllocExclusive  = "exclusive"
	AllocTimeSliced = "time-sliced"
	AllocMIG        = "mig"
	AllocDRA        = "dra"
)

// FixTarget records what a suggested command acts on, so that any future
// automation knows whether it is touching a pod or a controller — and so that
// "no safe command exists" is representable rather than implied by silence.
const (
	FixTargetPod        = "pod"
	FixTargetController = "controller"
	FixTargetNodePool   = "node-pool"
	FixTargetNone       = "none"
)

// ISODuration marshals as an ISO-8601 duration. The human renderer formats
// durations for people; the stable contract is parsed by machines.
type ISODuration time.Duration

func (d ISODuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(ISO8601(time.Duration(d)))
}

// UnmarshalJSON reads back what MarshalJSON wrote.
//
// Without this, the published types cannot decode the published documents:
// json.Unmarshal would try to read the string "P14D" into an int64 and fail on
// the first duration field, so every embedder named at the top of this file
// would have had to write its own decoder or fork the type. A contract that
// can only be written and not read is not a contract.
func (d *ISODuration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be an ISO-8601 string such as \"P14D\": %w", err)
	}
	v, err := ParseISO8601(s)
	if err != nil {
		return err
	}
	*d = ISODuration(v)
	return nil
}

// ParseISO8601 parses the durations ISO8601 emits: an optional day component
// and an optional time component of hours, minutes and seconds.
//
// Weeks, months and years are deliberately rejected. A month is not a fixed
// number of hours, and no claim this tool makes about accelerator time may
// rest on a unit whose length depends on when you asked.
func ParseISO8601(s string) (time.Duration, error) {
	bad := func() (time.Duration, error) {
		return 0, fmt.Errorf("%q is not an ISO-8601 duration (want e.g. P14D, PT6H30M, PT0S)", s)
	}
	if len(s) < 3 || s[0] != 'P' {
		return bad()
	}
	rest := s[1:]

	var total time.Duration
	var inTime, sawAny, sawTimePart bool
	for len(rest) > 0 {
		if rest[0] == 'T' {
			if inTime {
				return bad()
			}
			inTime = true
			rest = rest[1:]
			continue
		}
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 || i == len(rest) {
			return bad()
		}
		n, err := strconv.Atoi(rest[:i])
		if err != nil {
			return bad()
		}
		var unit time.Duration
		switch c := rest[i]; {
		case c == 'D' && !inTime:
			unit = 24 * time.Hour
		case c == 'H' && inTime:
			unit = time.Hour
		case c == 'M' && inTime:
			unit = time.Minute
		case c == 'S' && inTime:
			unit = time.Second
		default:
			// Includes W, Y, and a bare M outside the time part, which is
			// months. Ambiguous units are refused rather than guessed.
			return bad()
		}
		total += time.Duration(n) * unit
		sawAny = true
		sawTimePart = sawTimePart || inTime
		rest = rest[i+1:]
	}
	// A bare "T" with nothing after it, as in P1DT, is malformed rather than
	// harmless: it usually means a component was dropped somewhere upstream.
	if !sawAny || (inTime && !sawTimePart) {
		return bad()
	}
	return total, nil
}

// Duration returns the underlying duration.
func (d ISODuration) Duration() time.Duration { return time.Duration(d) }

// ISO8601 renders a duration as e.g. P13DT21H.
//
// Seconds are emitted only when they are non-zero. Windows and thresholds are
// always whole minutes, but --step may legitimately be finer, and rounding it
// away would make the params block claim a step of zero -- a number that
// cannot reproduce the scan it claims to describe.
func ISO8601(d time.Duration) string {
	if d <= 0 {
		return "PT0S"
	}
	days := int(d / (24 * time.Hour))
	rem := d % (24 * time.Hour)
	hours := int(rem / time.Hour)
	rem %= time.Hour
	mins := int(rem / time.Minute)
	rem %= time.Minute
	secs := int(rem / time.Second)

	var b strings.Builder
	b.WriteString("P")
	if days > 0 {
		fmt.Fprintf(&b, "%dD", days)
	}
	if hours > 0 || mins > 0 || secs > 0 {
		b.WriteString("T")
		if hours > 0 {
			fmt.Fprintf(&b, "%dH", hours)
		}
		if mins > 0 {
			fmt.Fprintf(&b, "%dM", mins)
		}
		if secs > 0 {
			fmt.Fprintf(&b, "%dS", secs)
		}
	}
	if b.Len() == 1 {
		return "PT0S"
	}
	return b.String()
}

// Accelerator is one physical device, or a group of identical devices.
type Accelerator struct {
	Model      string  `json:"model"`
	Vendor     string  `json:"vendor"`
	Count      int     `json:"count"`
	Allocation string  `json:"allocation"`
	TDPWatts   float64 `json:"tdpWatts,omitempty"`
}

// Workload is the grouped subject of a finding: never a single pod unless the
// pod is genuinely standalone.
type Workload struct {
	Namespace string   `json:"namespace,omitempty"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Grouped   int      `json:"grouped"`
	Members   []string `json:"members,omitempty"`
}

// Ref is the stable, human-typeable reference to a finding.
func (w Workload) Ref() string {
	if w.Namespace == "" {
		return w.Name
	}
	return w.Namespace + "/" + w.Name
}

// Evidence is the audit trail. A recommendation that cannot be checked will not
// be believed, so every number a finding asserts is reproducible from here.
type Evidence struct {
	// Window is the period examined; FallowDuration is how much of it the
	// accelerator did no work for. FallowDuration is never greater than Window.
	Window         ISODuration `json:"window"`
	FallowDuration ISODuration `json:"fallowDuration"`

	// LastNonZeroUtilization is absent when the accelerator did no work at any
	// point in the window. Absent means "not within the window", not "never":
	// ullage cannot see past its own window and does not claim to.
	LastNonZeroUtilization *time.Time `json:"lastNonZeroUtilization,omitempty"`

	// UtilizationMax is a percentage, 0-100, matching DCGM_FI_DEV_GPU_UTIL.
	// It is the highest value observed, not the mean: the mean of a job that
	// works one hour in twenty-four is 4%, which says nothing about whether the
	// device was needed.
	UtilizationMax float64 `json:"utilizationMax"`

	// PowerDrawWatts is instantaneous draw in watts. PowerDrawTDPRatio
	// expresses it as a fraction of the device's rated TDP, 0-1, and is absent
	// when the TDP for that model is unknown.
	PowerDrawWatts    float64 `json:"powerDrawWatts,omitempty"`
	PowerDrawTDPRatio float64 `json:"powerDrawTDPRatio,omitempty"`

	// SampleCompleteness is a fraction, 0-1: the share of the samples the
	// window should have contained that were actually present. Below the
	// confidence floor ullage reports lower confidence rather than treating
	// missing samples as zeroes, because a dead exporter and an idle fleet
	// produce the same absence.
	SampleCompleteness float64 `json:"sampleCompleteness"`

	// Sparkline is for terminal rendering and is deliberately not serialized;
	// consumers should derive their own shape from their own query.
	Sparkline []float64 `json:"-"`

	Notes []string `json:"notes,omitempty"`
}

// Impact is measured, never judged. GPUHoursFallow is what the device did not
// do; whether that time was recoverable requires an intent no metric carries.
type Impact struct {
	GPUHoursFallow float64  `json:"gpuHoursFallow"`
	WindowCost     *float64 `json:"windowCost,omitempty"`
	Currency       string   `json:"currency,omitempty"`
	PricingSource  string   `json:"pricingSource,omitempty"`
	PricingScope   string   `json:"pricingScope,omitempty"`
}

// Owner records not just who, but how it was resolved — which is what makes a
// wrong attribution debuggable instead of infuriating.
type Owner struct {
	Identity    string `json:"identity,omitempty"`
	ResolvedVia string `json:"resolvedVia,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// Provenance is the precondition for a correct fix. Without it, the obvious
// remediation is a no-op for every controller-managed workload.
type Provenance struct {
	Controlled bool   `json:"controlled"`
	RootKind   string `json:"rootKind,omitempty"`
	RootName   string `json:"rootName,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
	// Recognized reports whether the root controller is a kind whose removal
	// semantics ullage knows. Spelled the American way, like analyzed and
	// utilization elsewhere in this contract: consumers hard-code these
	// strings, so one dialect is worth more than any one spelling.
	Recognized bool `json:"recognized"`

	// Chain is typed rather than prose because consumers will read it, and a
	// slice of formatted strings is an invitation to parse them back.
	Chain []OwnerRef `json:"chain,omitempty"`

	// Truncated marks a chain that stopped because an object could not be
	// read, rather than because the root was reached. RootKind is then the
	// last link that happened to be readable, not the root -- usually a
	// ReplicaSet, whose Deployment reverses any change made to it.
	//
	// No remediation command is emitted for a truncated chain.
	Truncated bool `json:"truncated,omitempty"`
}

// OwnerRef is one link in an ownership chain.
type OwnerRef struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	APIVersion string `json:"apiVersion,omitempty"`
}

func (o OwnerRef) String() string { return o.Kind + "/" + o.Name }

// Fix carries a command only where one is safe to emit.
type Fix struct {
	RequiresHumanConfirmation bool      `json:"requiresHumanConfirmation"`
	ConfirmWith               string    `json:"confirmWith,omitempty"`
	Targets                   string    `json:"targets"`
	Command                   string    `json:"command,omitempty"`
	Rationale                 string    `json:"rationale,omitempty"`
	Blockers                  []Blocker `json:"blockers,omitempty"`
	Prevention                string    `json:"prevention,omitempty"`

	// Frees says when the command actually returns the capacity. Empty means
	// immediately, which is the case for every fix except a CronJob suspend:
	// that stops the next run and frees nothing until the Job already running
	// finishes on its own.
	//
	// Automation needs to know the difference. A gate that runs the command
	// and re-scans would otherwise report the fix as having failed.
	Frees string `json:"frees,omitempty"`
}

// When a fix returns the capacity.
const (
	// FreesLater means the command prevents recurrence but does not reclaim
	// anything from the run in progress.
	FreesLater = "later"
)

// Blocker names something preventing the obvious remediation from working.
type Blocker struct {
	Object string `json:"object"`
	Reason string `json:"reason"`
}

// Finding is one recommendation.
type Finding struct {
	Rank                int           `json:"rank"`
	ID                  string        `json:"id"`
	Check               string        `json:"check"`
	EvidenceConfidence  string        `json:"evidenceConfidence"`
	OwnershipConfidence string        `json:"ownershipConfidence"`
	Summary             string        `json:"summary"`
	Workload            Workload      `json:"workload"`
	Accelerators        []Accelerator `json:"accelerators"`
	Evidence            Evidence      `json:"evidence"`
	Impact              Impact        `json:"impact"`
	Owner               Owner         `json:"owner"`
	Provenance          Provenance    `json:"provenance"`
	Fix                 Fix           `json:"fix"`
	Risk                string        `json:"risk,omitempty"`
	Docs                string        `json:"docs"`

	// ByDesign marks capacity that is empty on purpose. It is reported so the
	// decision stays visible, and deliberately kept out of the ranked list and
	// out of every headline total.
	ByDesign bool   `json:"byDesign,omitempty"`
	Because  string `json:"because,omitempty"`

	// Suppressed records a local .ullage.yaml match.
	Suppressed       bool   `json:"suppressed,omitempty"`
	SuppressedReason string `json:"suppressedReason,omitempty"`
}

// TotalAccelerators sums the devices a finding covers.
func (f Finding) TotalAccelerators() int {
	n := 0
	for _, a := range f.Accelerators {
		n += a.Count
	}
	return n
}

// Exclusion records devices the scan could not reason about. This is as
// important as the findings: a consumer must always be able to tell "nothing
// found" from "not examined".
type Exclusion struct {
	// Code is stable and machine-readable. Prose changes; codes do not, and a
	// consumer that wants to alert on "we stopped being able to see the DRA
	// devices" needs something it can match on.
	Code         string `json:"code"`
	Reason       string `json:"reason"`
	Accelerators int    `json:"accelerators"`
	Detail       string `json:"detail"`
	Remedy       string `json:"remedy,omitempty"`
}

// Exclusion codes.
const (
	ExclTimeSliced   = "ULL-101"
	ExclMIG          = "ULL-102"
	ExclDRA          = "ULL-103"
	ExclInitialising = "ULL-104"
	ExclNoMetrics    = "ULL-105"
	ExclNotReady     = "ULL-106"
)

// UnmetDemand is context, not a finding. Pending pods hold no device, so they
// can never be waste — but they are the reason the waste matters.
type UnmetDemand struct {
	Pods         int    `json:"pods"`
	Accelerators int    `json:"accelerators"`
	Detail       string `json:"detail"`
}

// Pricing describes where money figures came from, or that none were available.
type Pricing struct {
	Source        string             `json:"source"`
	Currency      string             `json:"currency"`
	PerGPUHour    float64            `json:"perGPUHour,omitempty"`
	PerSKUGPUHour map[string]float64 `json:"perSKUGPUHour,omitempty"`
}

// Rate returns the hourly price for a SKU, and whether one is known.
//
// A blended rate across mixed SKUs is a fabricated number wearing a decimal
// point — T4 and H100 differ by roughly tenfold — so callers must apply this
// only where a finding's devices are a single model.
func (p *Pricing) Rate(model string) (float64, bool) {
	if p == nil {
		return 0, false
	}
	if v, ok := p.PerSKUGPUHour[model]; ok {
		return v, true
	}
	if p.PerGPUHour > 0 {
		return p.PerGPUHour, true
	}
	return 0, false
}

// AllocationCounts is the per-mode device census.
type AllocationCounts struct {
	DevicePluginExclusive int `json:"devicePluginExclusive"`
	TimeSliced            int `json:"timeSliced"`
	MIG                   int `json:"mig"`
	DRA                   int `json:"dra"`
}

// Tool identifies the binary that produced a Result, so a stored JSON document
// can be correlated with the code that made its claims.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
}

// Params records the effective analysis settings. Without them a Result cannot
// be reproduced, and two Results cannot be honestly compared.
type Params struct {
	IdleThreshold  ISODuration `json:"idleThreshold"`
	StuckThreshold ISODuration `json:"stuckThreshold"`
	MinConfidence  string      `json:"minConfidence"`
	Step           ISODuration `json:"step"`
	Checks         []string    `json:"checks"`
}

// ScanMeta is everything about the run itself.
type ScanMeta struct {
	Tool                 Tool             `json:"tool"`
	Context              string           `json:"context"`
	Started              time.Time        `json:"started"`
	Window               ISODuration      `json:"window"`
	Params               Params           `json:"params"`
	PrometheusURL        string           `json:"prometheusURL"`
	PodLabelSchema       string           `json:"podLabelSchema"`
	AcceleratorsObserved int              `json:"acceleratorsObserved"`
	AcceleratorsAnalyzed int              `json:"acceleratorsAnalyzed"`
	AllocationModels     AllocationCounts `json:"allocationModels"`
	GPUHoursPaid         float64          `json:"gpuHoursPaid"`
	GPUHoursFallow       float64          `json:"gpuHoursFallow"`
	ProfilingMetrics     bool             `json:"profilingMetricsAvailable"`

	// Queries is the exact PromQL issued, populated on request. The trust
	// argument for this tool is that its claims are checkable, which is empty
	// unless the queries are visible.
	Queries []string `json:"queries,omitempty"`
}

// Result is the whole output of one scan.
type Result struct {
	APIVersion string   `json:"apiVersion"`
	Scan       ScanMeta `json:"scan"`

	// The four finding lists are always present, and empty rather than null
	// when nothing qualified. "No capacity is held by design" and "we did not
	// evaluate whether any is" are different claims, and a consumer that has to
	// tell them apart from a missing key will get it wrong. Anything genuinely
	// optional below is a pointer, so absence is unambiguous.
	Recommendations []Finding   `json:"recommendations"`
	ByDesign        []Finding   `json:"byDesign"`
	Suppressed      []Finding   `json:"suppressed"`
	NotAnalyzed     []Exclusion `json:"notAnalyzed"`
	Warnings        []string    `json:"warnings"`

	BelowThreshold int `json:"belowThreshold"`

	// UnmetDemand is absent when nothing was pending. Pricing is absent when
	// costing was disabled or no price was known, which is why every monetary
	// field elsewhere is also a pointer.
	UnmetDemand *UnmetDemand `json:"unmetDemand,omitempty"`
	Pricing     *Pricing     `json:"pricing,omitempty"`
}

// Normalize replaces nil slices with empty ones so the document matches the
// null policy above. Encoding a nil slice yields null, which is easy to
// introduce by adding a code path that never appends.
func (r *Result) Normalize() {
	if r.Recommendations == nil {
		r.Recommendations = []Finding{}
	}
	if r.ByDesign == nil {
		r.ByDesign = []Finding{}
	}
	if r.Suppressed == nil {
		r.Suppressed = []Finding{}
	}
	if r.NotAnalyzed == nil {
		r.NotAnalyzed = []Exclusion{}
	}
	if r.Warnings == nil {
		r.Warnings = []string{}
	}
	if r.Scan.Params.Checks == nil {
		r.Scan.Params.Checks = []string{}
	}
}

// MarshalJSON guarantees the null policy regardless of how the value was built.
func (r Result) MarshalJSON() ([]byte, error) {
	type alias Result
	r.Normalize()
	return json.Marshal(alias(r))
}

// FallowPercent is the share of paid device-time that did no work.
func (r *Result) FallowPercent() float64 {
	if r.Scan.GPUHoursPaid == 0 {
		return 0
	}
	return r.Scan.GPUHoursFallow / r.Scan.GPUHoursPaid * 100
}
