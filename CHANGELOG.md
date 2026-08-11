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

### Next

- Kubernetes list pagination before the 1000-GPU target.
- JSON Schema plus a golden test over the contract.
- Reconcile the field names in the v0.1 UX spec, which now lags the code.
- Per-client discovery cache; exec credential plugin expiry.
