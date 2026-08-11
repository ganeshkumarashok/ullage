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

## Testing round — deep unit tests and a live cluster

### Goal

Prove the tool works rather than assert it: real unit tests for the packages
that had none, then an end-to-end run against a real AKS cluster with a real
Prometheus.

### Findings

The E2E run was not a formality. It found five bugs that the unit suite and the
demo fixture both missed, because all three agreed on assumptions the real
world does not share.

- **`--window 14d` was a parse error.** The tool's own documented default,
  printed in the README and `--help`. `time.ParseDuration` has no `d` unit, so
  the first command anyone copies off the front page failed. No unit test
  caught it because no unit test types a flag.
- **A GPU at 78% utilization was reported as idle.** Two causes. `shapeBy` was
  keyed by device while the series it stores are per-holder, so last-write-wins
  let a stale series overwrite a running job's shape. And `FallowFor` believed
  the range query while ignoring `Max` from the aggregate: at a 14-day window
  the range query exceeded Prometheus's point limit, the shape came back
  without its non-zero samples, and the finding printed "peak utilization 78%"
  inside its own evidence for a device it had just called idle.
- **Cross-node attribution.** dcgm-exporter stamps the holder onto the series,
  the series lingers after the holder leaves, and pod names repeat constantly
  under StatefulSets and Jobs. `DevicesOf` matched on namespace and name alone,
  so a finished job's device was attributed to a running namesake elsewhere.
- **Idle-pod printed a coverage figure it had not judged**, so a claim that
  cleared an 80% gate was published beside "0.4% coverage".
- **`podLabelSchema` was declared in the contract and never assigned**, and
  `scan.params.checks` serialised as `null` on the default invocation.

Writing tests found two more, both in code deciding whether capacity is
deliberate:

- **`containsPool` matched `-gpu` against `aks-gpubig-1-vmss`.** The delimiter
  was required on one side only, so pool `gpu` absorbed unrelated pool
  `gpubig`'s node groups and summed its floor — overstating the reservation and
  hiding real waste, the exact failure the matching exists to prevent.
- **`chunked` built its own identity key**, without a separator or the
  namespace, merging `team-a/trainer` and `team-b/trainer` on one card and
  summing one team's sample count into the other's coverage.

### The assumption underneath most of them

A series that stops arriving is not a device reading zero; it is a device
nobody is watching. An exporter that dies, a node that leaves, or a holder
label that stops being emitted otherwise all present as hardware that has been
perfectly idle ever since — the monitoring breaking generates a recommendation
to delete GPUs. `Stats` now records `LastSample` and `Stale`, `idle-pod`
refuses stale devices, and `unused-node` falls through to the weaker node-age
path rather than claiming a measurement it does not have.

### Failed attempts

- A regression test for the young-pod blind spot used a 2-day pod and passed
  for the wrong reason: the coverage gate was the reported problem, but the
  test `IdleThreshold` is 72h, so the real blind spot is pods aged 3–11 days.
- An `httptest` handler blocking on `r.Context().Done()` deadlocked `srv.Close()`,
  which waits for outstanding requests. Tests now use a `release` channel closed
  before the server closes.
- Enumerating the contract's list fields by hand missed the nested one. The test
  now walks the whole document against the declared struct shape, which is how
  `scan.params.checks` was found.

### Verification

Every fix was reverted temporarily to confirm its test failed. All of them did.

### Files changed

`internal/promql/client.go`, `internal/inventory/facts.go`,
`internal/scan/gather.go`, `internal/scan/analyse.go`,
`internal/check/{idlepod,unusednode}.go`, `internal/kube/{client,autoscaler,karpenter,types}.go`,
`internal/humanize/duration_flag.go`, `cmd/ullage/main.go`,
plus new tests for `promql`, `kube`, `inventory`, `scan` and the contract, and
`e2e/` holding the environment that found the five bugs.

### Next

Tests for `internal/render` and `internal/demo`; the three open reviewer minors
(Karpenter NodePool name in `fix.go`, `KnownFields` forward-compatibility in
`config.go`, oscillating suppression warnings).

## v0.1.0 — ready to publish

### Goal

Make the repository safe to make public, and make sure someone who clones it in
six months can still test it properly.

### Important findings

**The RBAC we ship was never exercised.** Every test and every developer run
uses a kubeconfig that is usually cluster-admin, so `deploy/rbac.yaml` — the
file a platform team reads before deciding whether to trust this tool — could
have been wrong in either direction without anything noticing. Running a scan
in-cluster as that ServiceAccount and nothing else is now part of the E2E.
Deleting `pods` from the ClusterRole fails it with the missing verb named.

That immediately exposed an ordering bug: `rbac.yaml` puts a ServiceAccount in
namespace `ullage`, but the Namespace was declared in `cronjob.yaml`, which the
README says to apply second. The documented order could never have worked on a
clean cluster.

**A Forbidden is downgraded to a warning by design**, which means an
insufficient grant shows up as a quietly smaller answer rather than a crash.
The RBAC check therefore asserts on the findings, not just the exit code.

**The README was the least-tested file in the repository.** It promised
`go install` and an image that do not exist until a tag, showed a transcript
from a build nobody could reproduce, and argued against an unnamed "dashboard"
while never naming KRR, OpenCost, Kubecost or Cast AI. Two tests now parse every
documented `ullage` invocation with the real flag set and check the transcript's
census still reconciles.

**`--help` listed ten flags out of twenty-nine**, omitting ones the tool's own
error messages tell people to use. The test written to enforce this failed on
its first run: help promised `--insecure-skip-verify`, the parser wanted
`--insecure-skip-tls-verify`.

### Failed attempts

`kubectl apply --dry-run=server` cannot validate the manifests, because the
namespace the objects need is created by the same apply. Verified against a real
cluster instead.

Capping the doctor probe timeout with `if opts.Prometheus.Timeout == 0` did
nothing — the flag default pre-fills it, so the guard never fired. It needed
`|| > probeTimeout`. Measured per-line timestamps to confirm 30s became 10s.

### Files changed

`deploy/rbac.yaml`, `deploy/cronjob.yaml` (namespace ordering), `e2e/kind.sh`
(new `rbac` mode), `cmd/ullage/main.go` (`splitPositional`, complete `--help`,
streaming `doctor`), `pkg/ullage/doctor.go` (observers, probe timeout),
`pkg/ullage/api/api.go` (documented units, one null policy enforced in
`MarshalJSON`), `README.md`, `.krew.yaml`, plus `cmd/ullage/{args,docs}_test.go`
and null-policy tests.

### Next

Push to GitHub and let the release workflow build the tag. Submit `.krew.yaml`
to krew-index once the release artifacts exist. `internal/scan` and
`internal/kube` remain the thinnest-covered packages.

## Examples

### Goal

Answer "is the value easy to see, and easy to run" — which it was not.

### Important findings

`make demo` showed the output but not the argument. The interesting part of this
tool is what it *refuses* to claim — reserved capacity is not waste, a
time-sliced device cannot be judged, deleting a controller-owned pod frees
nothing — and none of that is legible from one screen. `make tour` now walks it.

There was no answer to "what would I do with this on Monday". Two runnable
examples now exist: a CI gate that fails on a budget rather than on any finding
at all, and a digest grouped by owner.

Writing them found a real bug: three check summaries interpolated a count into a
hard-coded plural — "1 accelerators held with no work" — despite
`humanize.Plural` already existing. The regression test matches the pattern in
the source rather than the rendered string, and immediately found a fourth
instance missed by hand.

### Files changed

`examples/{README.md,tour.sh,ci-gate.sh,weekly-digest.sh}`,
`internal/check/{idlepod,stuckpod,unusednode}.go` (pluralization),
`internal/check/check_test.go`, `Makefile` (`tour`, examples in `smoke`),
`.github/workflows/ci.yaml`, `README.md`, `cmd/ullage/docs_test.go`.

### Next

Push and let the release workflow build the tag.

---

## README and the hero visual

### Goal

Make the front page good enough that someone decides in ten seconds whether this
is for them, and add an animated visual that carries the argument rather than
decorating it.

### Important findings

The argument is not "GPUs are idle" — every dashboard says that. It is
*allocated ≠ used, and unused ≠ wasted*. So the drawing has three nodes: one
partly idle, one wholly idle, and one idle **on purpose**, shown and never
counted as waste. The third node is the entire differentiator, and it now has a
picture.

No terminal-recording tool (`vhs`, `termtosvg`, `asciinema`) was installed, and
installing one puts a machine-level dependency between a contributor and a
rebuild. The hero is hand-authored SVG instead: no build step, diffable, and
reproducible from the repository alone.

One shared 18s CSS timeline drives every element, with percentage keyframes
rather than `animation-delay`, so the acts cannot drift apart. Motion is
opacity-only — the one property every renderer agrees on. Under
`prefers-reduced-motion` the file holds its last frame, so turning motion off
loses the animation and keeps the argument.

The README had never shown what `ullage explain` prints, which is the entire
payoff — the evidence, the owner, the one command, and the reason the obvious
command is wrong. It does now.

### Failed attempts

Chrome's `--virtual-time-budget` does not drive the CSS animation clock
predictably; every screenshot landed in the fade and the image looked blank.
Freezing a frame with `animation-play-state: paused; animation-delay: -Ns`
renders an exact moment deterministically, which is how the timeline was
actually verified rather than assumed.

The first draft of the hero used invented figures — $1,142 against
`jupyter-alice`, five accelerators fallow, 1,680 hours. None of them were what
the tool prints. Redrawn against real `ullage demo` and `ullage explain` output,
and then pinned: `hero_test.go` fails if a figure in the picture stops appearing
in a transcript, so the demo fixture cannot move without the drawing moving too.
Both branches were proven by breaking them.

### Files changed

`docs/hero.svg` (new), `cmd/ullage/hero_test.go` (new), `README.md`
(centred header, badges, hero, nav, `explain` transcript, examples table),
`cmd/ullage/docs_test.go` (internal anchors resolve).

### Next

Push, and let the release workflow build the tag so the install commands and the
CI badge stop being promises.

## Independent review round — four reviewers, everything that fails open

### Goal

Four reviewers read the whole repository independently — API surface, domain
correctness, adoption, and the release path — with no shared context and no
brief beyond "find what is wrong". Two of them concluded the tag should not be
published. This round is their criticals.

The pattern behind almost all of them is one mistake made in nine places: when
ullage could not see something, it behaved as though there were nothing to see.
Every one of those produced a *confident* recommendation, because absence of
evidence entered the pipeline as evidence of absence.

### Recommendations that would have destroyed running work

- **A DRA pod's ResourceClaim was read with `claims, _ :=`.** Under Dynamic
  Resource Allocation a pod requests no extended resource, so the claim is the
  only record that it is sitting on hardware. A 403 from an un-updated RBAC role
  made an occupied node look empty and `unused-node` offered to delete a pool
  that was training a model. Nodes hosting DRA-declaring pods are now marked
  `OccupancyUnknown` and skipped; the blast radius is exact because pod specs
  stay readable.
- **PodDisruptionBudgets failed open three ways.** Expression-only selectors
  (`app in (...)`) returned "no match", so every such budget was ignored; `{}`
  returned "no match" though Kubernetes documents it as matching every pod in
  the namespace — and the test asserting this stated the *correct* semantics in
  its own failure message; and `pdbs, _ :=` turned an unreadable list into an
  empty one. All four operators are now implemented, and an unreadable list is
  uncertainty rather than permission.
- **A zero SM gauge is not an idle GPU.** `DCGM_FI_DEV_GPU_UTIL` reports SM
  activity alone. A video pipeline on NVENC/NVDEC, a data loader saturating the
  copy engines, or a warm model resident in framebuffer all read exactly zero
  across a fortnight while being continuously and expensively busy — and ullage
  would have printed `kubectl scale --replicas=0` at them with high confidence.
  ENC, DEC, MEM_COPY and FB_USED are now consulted; all four are in
  dcgm-exporter's default counter set. An exporter that exports none of them
  disqualifies nothing, but the scan says so, because "no GPU work" from a tool
  that only looked at the SMs is a weaker claim.
- **A partially-scanned window read as a fortnight of idleness.** The earlier
  completeness cap was applied to `Stats.Completeness`, which `unused-node`
  reads — but `idle-pod` reads `CoverageOver`, which counted only samples. The
  protection covered the check that resizes a pool and missed the one that
  prints `kubectl scale --replicas=0`.
- **An autoscaler floor exempted the whole pool.** `Held(pool)` was a yes/no, so
  a 20-node pool with a floor of 2 filed all 18 idle nodes as by-design and hid
  the most expensive waste in the cluster. The floor is now a quantity, spent on
  working nodes first.

### The number on the front page was wrong

Fallow duration is the longest across a group; accelerator-hours were that
maximum multiplied by every device in the group. One accelerator idle a
fortnight beside nine idle four days is 50 device-days — ullage reported 140.

It is the only ranking signal and the only input to the price, so the
overstatement promoted the wrong work to the top of the report and inflated the
cluster total the tool exists to state, in the direction that flatters the tool.
All three reviewers found it independently. Checks now carry a summed per-device
total, and a finding narrowed by an autoscaler floor re-adds only the nodes it
kept.

### Things that simply did not work

- **`--insecure-skip-tls-verify` never reached Prometheus.** Parsed, carried
  through the config, stored on the client, and then ignored: the client kept
  the default transport. Self-signed certificates are the norm for in-cluster
  monitoring, so this was the first thing that happened to anyone pointing
  ullage at their own Prometheus, and it looked like a bug rather than an
  unimplemented flag.
- **The example scripts were broken in the only mode anyone would run them in.**
  `${PROMETHEUS:+--prometheus "$P"} ${PROMETHEUS:-demo}` expands to
  `--prometheus URL URL`; Go's flag package stops at the stray positional, so
  `--output json` vanished and `jq` was fed human-readable text. Demo mode — the
  mode we tested — was fine. The CI gate also treated unpriced findings as $0,
  so a cluster with no pricing data always passed.
- **Krew could not install the release.** The manifest asked for
  `ullage_linux_amd64.tar.gz`; the workflow uploads
  `ullage_v0.1.0_linux_amd64.tar.gz`. The sha256 fields were placeholders that
  nothing filled. The manifest is now rendered from `checksums.txt` at release
  time and fails on a missing digest or a surviving placeholder, and a second
  step diffs its URLs against what was actually uploaded.
- **Suspending a CronJob frees nothing now.** It stops the next run; the Job
  already running keeps its accelerators, and that is the run the finding is
  about. Someone would run the command, watch the GPUs stay pinned, and conclude
  the tool does not work. Fixes now carry `fix.frees`.
- **The discovery cache was a package-level map with no lock.** Owner resolution
  walks ownerReferences concurrently, so the Go runtime killed the process with
  "concurrent map writes" on clusters with enough CRDs. Package scope was wrong
  on its own: two clients in one process answered each other's discovery.

### Confidence built on assumptions nobody checked

- **The scrape interval was assumed to be 30s.** Coverage is samples divided by
  window over interval, so an exporter scraped at 15s — the kube-prometheus-stack
  default — halves the expected count, and seven days of data across a fourteen-day
  window divides out to 100% coverage. A full fortnight of confident observation
  conjured from half a window, on the one number that decides whether a
  recommendation gets printed. It is now measured, and snapped to a configured
  interval because `count_over_time` counts both endpoints and 29.75s would
  inflate every figure derived from it.
- **"Pending" meant "phase Pending OR unscheduled".** Those are opposite claims
  about whether hardware is held. A pod bound to a node and wedged on
  ImagePullBackOff is phase Pending, and the scheduler has already committed
  that node's accelerator to it. The consequences ran both ways at once:
  `unused-node` saw an empty node and offered to delete it, while `stuck-pod` —
  the check written for precisely this pod — skipped it. The one node definitely
  wasting money was reported as reclaimable rather than as wedged.
- **Init containers were excluded from the pod's request.** A job that requests
  its GPU in an init container to download model weights reported zero
  accelerators and was invisible to both occupancy and stuck-pod. Requests now
  follow Kubernetes' effective pod request, `max(sum(app) + sum(sidecars),
  max(init))`; summing would double-count a pod that asks in both places.
- **The ownership walk could not tell "found the root" from "could not read the
  next object".** An RBAC role granting pods but not replicasets stopped the
  walk at the ReplicaSet, called it the root, and printed `kubectl scale
  replicaset ... --replicas=0` — which the Deployment above it reverses within
  seconds. Truncated chains now get no command; the finding still stands,
  because the idleness is the part that was measured. A 404 is not truncation:
  a deleted object genuinely has no parent left to find.

### Failed attempts

Every fix in this round was proven by mutation — break the fix, confirm the test
fails — and that repeatedly failed in two ways worth recording.

The mutation must still *compile*. Removing a value left a variable unused, and
the "passing" test was a build failure wearing a green tick. Mutate a function
body (`case true:`, `&& false`) rather than deleting code.

The mutation must exercise the intended path. Several first attempts changed
code the test never reached; the only reliable check is to `grep` that the
mutation applied and read the raw failure text.

Fixtures were the other repeated cost. A stub `ullage` needs a complete `scan`
header or `jq` divides by null and exits 5. A DRA fixture pod without an
`ownerReference` short-circuits on "not managed by a controller" long before the
budget check it was written to exercise. A test asserting the idle-pod grouping
arithmetic silently proved nothing until its durations cleared the 72h
threshold.

The release workflow was simulated locally end-to-end — four real tarballs, the
extracted workflow steps, digests compared — because a release path that has
never run is not a release path.

### Verification

`go test -race ./...` green after every commit; `make check` green; two new
fail-closed test files (`internal/scan/dra_test.go`, `internal/kube/selector_test.go`)
and four covering the arithmetic and the flags. The pre-existing suite passed
unchanged through the autoscaler-floor fix, which is how we know nothing had
covered it.

### Next

Re-run the kind E2E — the kube and scan paths moved materially — re-tag from a
clean tree, and push so CI and the release workflow run for the first time.
