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

// Duration returns the underlying duration.
func (d ISODuration) Duration() time.Duration { return time.Duration(d) }

// ISO8601 renders a duration as e.g. P13DT21H. Sub-minute precision is dropped
// deliberately: no claim in this tool is meaningful below the minute.
func ISO8601(d time.Duration) string {
	if d <= 0 {
		return "PT0S"
	}
	days := int(d / (24 * time.Hour))
	rem := d % (24 * time.Hour)
	hours := int(rem / time.Hour)
	rem %= time.Hour
	mins := int(rem / time.Minute)

	var b strings.Builder
	b.WriteString("P")
	if days > 0 {
		fmt.Fprintf(&b, "%dD", days)
	}
	if hours > 0 || mins > 0 {
		b.WriteString("T")
		if hours > 0 {
			fmt.Fprintf(&b, "%dH", hours)
		}
		if mins > 0 {
			fmt.Fprintf(&b, "%dM", mins)
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
	Window                 ISODuration `json:"window"`
	FallowDuration         ISODuration `json:"fallowDuration"`
	LastNonZeroUtilization *time.Time  `json:"lastNonZeroUtilization,omitempty"`
	UtilizationMax         float64     `json:"utilizationMax"`
	PowerDrawWatts         float64     `json:"powerDrawWatts,omitempty"`
	PowerDrawTDPRatio      float64     `json:"powerDrawTDPRatio,omitempty"`
	SampleCompleteness     float64     `json:"sampleCompleteness"`
	Sparkline              []float64   `json:"-"`
	Notes                  []string    `json:"notes,omitempty"`
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
	Recognised bool   `json:"recognised"`

	// Chain is typed rather than prose because consumers will read it, and a
	// slice of formatted strings is an invitation to parse them back.
	Chain []OwnerRef `json:"chain,omitempty"`
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
}

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
	APIVersion      string       `json:"apiVersion"`
	Scan            ScanMeta     `json:"scan"`
	Recommendations []Finding    `json:"recommendations"`
	ByDesign        []Finding    `json:"byDesign,omitempty"`
	Suppressed      []Finding    `json:"suppressed"`
	BelowThreshold  int          `json:"belowThreshold"`
	NotAnalyzed     []Exclusion  `json:"notAnalyzed"`
	UnmetDemand     *UnmetDemand `json:"unmetDemand,omitempty"`
	Pricing         *Pricing     `json:"pricing,omitempty"`
	Warnings        []string     `json:"warnings"`
}

// FallowPercent is the share of paid device-time that did no work.
func (r *Result) FallowPercent() float64 {
	if r.Scan.GPUHoursPaid == 0 {
		return 0
	}
	return r.Scan.GPUHoursFallow / r.Scan.GPUHoursPaid * 100
}
