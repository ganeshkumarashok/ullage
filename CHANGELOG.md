# Changelog

## v0.1.0 — unreleased

First working end-to-end release.

### Goal

Build a POC that runs against a real cluster and demonstrates the value
proposition without ambiguity: recommendations that are actionable, attributed,
and safe, rather than another utilization dashboard.

### What it does

- Three checks: `idle-pod`, `stuck-pod`, `unused-node`.
- Provenance walk to the root owner, so fixes target the controller rather than
  a pod its controller will immediately recreate.
- Blocker diagnosis on unused node pools — the finding no dashboard produces.
- `fallow by design` for capacity held by an autoscaler minimum.
- Loud exclusions for time-sliced, MIG, initialising and unmetriced devices; the
  accelerator census always reconciles.
- `ullage demo`: a complete scan against an in-process fake Kubernetes API and
  Prometheus, served over real HTTP and read by the production clients.
- `ullage doctor`, `explain`, `ignore`, `checks`; JSON output on a versioned
  contract; exit code 1 on findings for CI use.

### Findings during the build

- **Both label schemas are real, and the wrong choice is silent.**
  kube-prometheus-stack relabelling moves DCGM's `pod` to `exported_pod` and
  puts the *exporter's own* pod in `pod`. Preferring `pod` when both exist
  attributes every GPU in the cluster to `dcgm-exporter`. Detection now prefers
  `exported_`.
- **Gating idleness on zero across the whole window was wrong.** A pod idle for
  nine of fourteen days was missed, and the longer the window the more it missed
  — the wrong way round. The gate is now the trailing run of zeroes; zero across
  the whole window is retained as a confidence signal. The strict zero itself
  was never relaxed.
- **Corroborating with a window-wide power average contradicts correct
  findings.** A device that worked for the first three days shows a healthy
  fourteen-day mean. Power is now averaged over the last 24h, matching the claim
  being corroborated.
- **DRA pods request no extended resource at all**, so a fully occupied DRA node
  read as empty and was recommended for deletion. Device counts now include
  ResourceClaim allocations.
- **A unit test caught a live bug**: the unused-node check trusted `Evictable`
  without re-checking `IsDaemonSet`, so the DaemonSet rule held only by accident
  of the gather layer. Every node runs dcgm-exporter, the device plugin, CNI and
  CSI, so this would have attached a spurious blocker to every finding.
- **Devices with no metrics vanished from the accounting** — 40 of 68
  accelerators were neither analysed nor explained. Added `ULL-105`.

### Failed attempts

- `golang.org/x/term` for TTY detection was dropped to preserve the
  single-dependency posture; stdlib `ModeCharDevice` plus `COLUMNS` is enough.
- The original metrics plan issued a 14d raw range query per device. At 1000
  GPUs that is 40–120M samples, over Prometheus's default 50M limit. Replaced
  with instant aggregate push-down (`max_over_time`, `avg_over_time`,
  `count_over_time`) plus one coarse `step=1h` range query for sparkline shape
  only.
- A fixture that emitted metrics only for busy devices hid the accounting bug
  above. Real dcgm-exporter reports every GPU whether held or not.

### Architecture

Re-architected mid-build after review, around a `Check` interface over a
normalized fact layer. Checks are pure detectors reading `inventory.Cluster`;
ownership, fixes, grouping, ranking, pricing and rendering happen once
downstream. The JSON contract lives in `pkg/ullage/api`, outside `internal/`,
so embedders can depend on it.

## Post-POC review round

Goal: have subagents review the high-level idea and every layer of the stack for
adoption fit, then act on what came back. The review was run against the working
artifact rather than a plan, and it found two ways the tool would recommend
deleting hardware that was in use.

### Critical — wrong recommendations on a real cluster

- **MIG slices were invisible.** Under the mixed strategy a pod requests
  `nvidia.com/mig-1g.5gb`, never `nvidia.com/gpu`, so its whole-device count is
  zero. `unused-node` built occupancy from whole devices, read a MIG pool at
  capacity as empty, priced it and attached a scale-to-zero command. The demo
  showed it in the open: `a100-mig-0` carried a 30%-utilization series *and*
  appeared as a priced recommendation, while the same two devices were listed
  under "not analysed" — so the headline total charged hours against devices the
  tool said it had not analysed.
  Fixed by asking occupancy in the broadest terms (whole devices, DRA claims,
  MIG profiles, time-sliced replicas). `PodView.Occupies` is a **method**, not a
  field: a field can be left unset by whoever adds the next allocation model,
  and a unit test caught exactly that during the fix.
- **The fallow duration was asserted, not measured.** `unused-node` read only
  the current pod snapshot and then reported the node's *age* as how long it had
  been fallow, so a batch pool empty at scan time was billed for its whole
  lifetime. Node age is now an upper bound only; the trailing zero-utilization
  run wins where devices were measured, a pool that did work inside the idle
  threshold is dropped, and where nothing was measured the evidence says the
  number is the node's age rather than passing it off as an observation.
  Measurement is also now a second, independent occupancy check — if any device
  reported work, work ran there, whatever the object model said.

### Correctness

- **Karpenter.** The "this pool may be held at a deliberate minimum" caution is
  a cluster-autoscaler concept. Karpenter has no minimum size, so a Karpenter
  cluster read as having no autoscaler and every finding collected a hedge that
  could not apply. Karpenter is detected via NodePools; a zero-node disruption
  budget is treated as a floor, a schedule-bounded one lowers confidence, and
  `karpenter.sh/do-not-disrupt` is a named blocker. Karpenter pools no longer
  receive `eksctl`/`az` commands, which name objects that do not exist.
- **Autoscaler floors are summed across a pool's node groups**, since AKS makes
  one VMSS per zone and EKS one ASG per zone. With zone minimums of {0,0,2} the
  old longest-name tiebreak returned 0 (calling reserved capacity waste) or 2.
  Summing naively then sums "gpu-big" into "gpu", because pool names nest, so
  each node group is assigned to the longest pool name that matches it before
  the floors are added.
- **List pagination.** Every list was a single unpaged GET. On a cluster large
  enough to be worth scanning the API server truncates, the missing pods are
  invisible, and the nodes they occupy read as empty — the worst failure mode
  here, because the output still looks plausible. The demo API server now pages
  at three items so every run exercises the path.
- **Range aggregates are chunked at 24h.** `max_over_time` makes the response
  small but Prometheus still loads every raw sample to evaluate it, against a
  50M default. One whole-window query fails at ~1,240 GPUs, inside the target
  range. Chunking moves the ceiling to ~17,000.
- **Findings rank by cost when a price is known.** Ranking 2,700 L4-hours above
  1,000 A100-hours put the cheapest finding first, in a tool printing dollars
  on every row.

### Honesty

- `explain` no longer prints "Peak utilization 100%" three lines above "read
  exactly zero" — the peak is over the window, the claim is over the trailing
  fallow run, and unqualified it reads as self-contradiction.
- `--prometheus-token-file` was accepted, plumbed through two structs, and never
  read. Now implemented, and re-read per request because a projected
  ServiceAccount token rotates hourly and a large scan can outlive one.
- `--prometheus-auth azure-monitor` was an alias for bearer. Removed: naming a
  provider while only setting a static header promises support that does not
  exist and fails after someone has committed.
- `--output yaml` was in the help and returned "not implemented".
- `--idle-threshold` help said 72h against an actual default of 24h.
- README gained a support matrix stating what is *not* covered — AMD/Intel
  discovered but not measured, MIG/time-slicing counted but never analysed,
  managed Prometheus needing a signing proxy.

### Adoption

- Distroless image (16.3MB, non-root, verified under `--read-only`), weekly
  CronJob, and an RBAC manifest annotated per `apiGroups` block, with ConfigMap
  access scoped by `resourceNames` to the single autoscaler status object.
- `--exit-zero`, because exit 1 on findings is right for a CI gate and wrong for
  a CronJob, where it turns every successful scan into a failed Job.
- CI runs gofmt, vet, `test -race`, the demo, and asserts the contract invariant
  that analysed + excluded == observed.
- Progress output is gated on `isTerminal(os.Stderr)`; it previously wrote
  `\r\033[K` unconditionally and concatenated into one line when piped.

### Next

- JSON Schema plus a golden test over the contract.
- Reconcile the field names in the v0.1 UX spec, which now lags the code.
- Per-client discovery cache; exec credential plugin expiry.
- `.ullage.yaml` is written by `ignore` but still never read.
- A second metric source (AMD `device-metrics-exporter`) to make the
  vendor-neutral fact layer true in practice rather than only in structure.

## Review round 3 — the fix that reopened the bug it closed

**Goal.** Verify the previous round's fixes held at the cause, and close the
`ullage ignore` loop, which wrote a file nothing read.

**Findings.**

- **C2 was not fixed, only relocated.** The fallow duration no longer came from
  node age, but the replacement had no sample-coverage gate. One zero sample out
  of a fortnight set the node's idle run to the whole window, and the evidence
  then called that number *measured*. Any node that joined mid-window, or whose
  dcgm-exporter had restarted, produced a 14-day idleness claim at high
  confidence with a scale-to-zero command attached — the same overstatement,
  reached by a different route. Sample coverage is what makes a zero mean
  anything.
- The `lastWork` map mixed two combination rules: minimum for devices that had
  worked, first-wins for devices that had not. Three idle GPUs could outvote a
  working one. Rewritten so the per-node answer is unambiguously the minimum.
- **One metric series is not one accelerator.** dcgm-exporter labels utilization
  with the pod holding the device, so a GPU handed to a second pod during the
  window returns two series — ordinary job churn at 14 days. The census counted
  records, so it would have reported analysing more accelerators than the
  cluster has. The honest denominator is the single claim the whole tool rests
  on.
- **The demo could not reproduce that bug**, because its series were keyed by
  host and GPU index, making churn structurally unrepresentable. The fixture was
  green on a defect that fires on any real cluster with turnover. Fixed by
  making the series a slice and adding a finished job to the scenario; reverting
  the census fix now makes the demo print 61 + 8 = 69 against 68 observed.

**Failed attempts.**

- Deduplicating `cl.Devices` itself, as first suggested. It breaks idle-pod:
  collapsing a busy pod-A series and an idle pod-B series onto one physical
  device makes pod B's genuine idleness invisible. Per-series records are
  correct for attribution; only the census collapses them.
- Writing the completeness gate as a patch to the idle branch. The surrounding
  min/first-wins mix was itself incoherent, so the whole loop was rewritten.

**Suppressions.** `ullage ignore` wrote a `.ullage.yaml` that nothing ever
opened, and `explain` printed the command with a workload reference while
matching is on the finding id — so copying the tool's own advice produced an
entry that could never match. Both halves were broken in a way that looked like
it worked. Now: unmatched entries are reported, expired entries stop applying
and are named, malformed files are a hard error, and the suppressed total prints
with its accelerator-hours and cost so suppression cannot quietly hide waste.

Also found by writing a three-line consumer of the JSON: list fields serialised
as `null` rather than `[]`, which would have broken consumers only on clusters
healthy enough to have no suppressions and no warnings — never in a demo.

**Files changed.** `internal/check/unusednode.go`, `internal/scan/gather.go`,
`internal/scan/fix.go`, `internal/scan/analyse.go`, `internal/inventory/facts.go`,
`internal/render/{explain,table,printer}.go`, `internal/demo/{server,scenario}.go`,
new `internal/config/`, `cmd/ullage/main.go`, `pkg/ullage/ullage.go`, `README.md`,
tests in `internal/check`, `internal/config`, `pkg/ullage`.

**Next step.** A JSON Schema and golden test over `pkg/ullage/api`; nothing
currently pins the contract's shape, so a rename breaks embedders silently.
