# ullage

**The GPU your cluster paid for and didn't use.**

*Ullage* (n.) — the amount by which a container falls short of being full.

`ullage` measures the accelerator capacity a Kubernetes cluster is paying for
and not using, attributes it to the workload and the person responsible, and
tells you the one command that will actually free it.

It is a measurement, not a verdict. It never writes to your cluster.

```console
$ ullage demo
```

```
ullage v0.1.0  prod-westus3  window 14d

  6.3k of 23k accelerator-hours fallow (28%)
  60 of 68 accelerators analysed  (8 excluded, see below)

      WORKLOAD                    GPUS   ULLAGE    FOR OWNER
  1.  pool/l4-serving                8     2.7k    14d platform
      2 nodes, nothing scheduled · 2 pods block scale-down
      ~$2,016  ·  8 × NVIDIA-L4

  2.  research/dra-sandbox-erin      4     1.1k    11d erin@…
      no GPU work since 31 Jul 03:00
      ~$1,696  ·  4 × NVIDIA-L40S

  3.  research/jupyter-alice         3     1.0k    14d alice@…
      3 pods, no GPU work since the window began · owned by StatefulSet
      ~$3,427  ·  3 × NVIDIA-A100-SXM4-80GB

  ...

  Fallow by design
  16 accelerators, 5.4k — held empty on purpose, not counted as waste
    · pool/h100-reserve is held at a minimum of 2 nodes by the cluster
      autoscaler, so these nodes are kept on purpose.

  Not analysed
    · 2 accelerators — shared-device (t4-shared is time-slicing)
    · 2 accelerators — mig-instance
    · 4 accelerators — driver-initialising

  Unmet demand
    4 pods are waiting for 4 accelerators — 4 reported Unschedulable
    This is context, not a finding: pending pods hold no devices.

  Next: ullage explain pool/l4-serving
```

## Why another tool

Every GPU dashboard can already show you a utilization graph. None of them
answer the question you actually have, which is *what should I do about it.*

`ullage` is built around four things a dashboard does not do.

**It finds what is blocking your autoscaler.** An empty node is visible
anywhere. *"These two pods with `safe-to-evict: false` are why the autoscaler
cannot reclaim it"* is causal, is not visible anywhere else, and is the finding
most likely to be worth real money.

**It targets the controller, not the pod.** Three idle notebook pods owned by a
StatefulSet do not need `kubectl delete pod` — the controller recreates them
within seconds and nothing is freed. `ullage` walks the ownership chain to the
root and suggests `kubectl scale statefulset --replicas=0` instead. When the
root is a CRD it does not recognise, it names the resource and emits **no
command at all**, because refusing to guess is worth more than a plausible
command that does the wrong thing.

**It separates deliberate from wasteful.** Capacity held empty by an autoscaler
minimum is reserved, not wasted. It appears under *fallow by design*, with no
removal command attached. Printing reserved capacity in the same list as waste
is the fastest way for a tool to be dismissed as not understanding the business.

**It says what it did not look at.** Time-sliced and MIG devices are excluded,
by name and with a reason, because a device-level metric there reflects every
co-tenant. The accelerator census always reconciles: analysed plus excluded
equals observed. A percentage over a denominator you cannot account for turns a
monitoring gap into a claim about efficiency.

## What it will not do

- **It will not call a workload inefficient.** GPU utilization is a poor measure
  of how hard a device is working — a single-threaded kernel reads 100%. The
  only thing the metric proves is its zero, so *zero* is the only thing `ullage`
  claims.
- **It will not treat a low average as idle.** A job running one hard hour in
  twenty-four averages 4%. Tools that threshold on averages flag it; its owner
  loses real work.
- **It will not read absent samples as zero.** An exporter that died a week ago
  is not a fleet that went idle.
- **It will not write to your cluster.** The Kubernetes client has no write
  methods. That is a property of the code, not a promise.
- **It will not phone home.** There is no telemetry, no analytics, and no
  network access beyond your API server and your Prometheus.

## Install

```console
go install github.com/ullage-project/ullage/cmd/ullage@latest
```

## Try it without a cluster

```console
ullage demo
```

Runs a complete scan against a built-in fake cluster served over real HTTP —
same clients, same queries, same code path. No credentials, no GPUs, no
Prometheus. Use `ullage demo --serve` to point your own tooling at it.

## Use it for real

```console
ullage doctor --prometheus https://prometheus.example.com
ullage --prometheus https://prometheus.example.com
ullage explain research/jupyter-alice
```

`doctor` first. It tells you which prerequisite is missing, so an empty first
run is never ambiguous between *"your cluster is efficient"* and *"my setup is
broken"* — two outcomes that look identical and mean opposite things.

### Requirements

- Cluster-wide **read** on nodes, pods, namespaces, poddisruptionbudgets and
  (for DRA) resourceclaims.
- A Prometheus-compatible endpoint carrying
  [`dcgm-exporter`](https://github.com/NVIDIA/dcgm-exporter) metrics.
- `DCGM_EXPORTER_KUBERNETES=true` for per-pod attribution. Without it, only
  node-level findings are possible, and `ullage` says so rather than going
  quiet.

Both the `pod`/`namespace` and the `exported_pod`/`exported_namespace` label
schemas are detected automatically. The latter is what kube-prometheus-stack
produces after relabelling, and assuming the former there attributes every GPU
in the cluster to `dcgm-exporter`.

## Output

`--output json` emits a versioned, stable document (`ullage.dev/v0.1`) defined
in [`pkg/ullage/api`](pkg/ullage/api). It records the exact PromQL used, the
effective thresholds, and the accelerator census, so a result can be reproduced
and two results can be honestly compared.

Exit codes: `0` nothing found, `1` findings present, `2` the scan could not
complete. Suitable as a CI gate.

Embed it directly:

```go
res, err := ullage.Scan(ctx, ullage.Options{
    Prometheus: ullage.PrometheusOptions{URL: promURL},
})
```

## Checks

| ID | Finds |
|---|---|
| `idle-pod` | Pods holding accelerators that have read exactly zero for longer than the threshold |
| `stuck-pod` | Pods holding accelerators whose containers are not running (crash loops, image pull failures, wedged init) |
| `unused-node` | Accelerator nodes nothing has been scheduled on — and what is stopping the autoscaler from reclaiming them |

`ullage checks` prints each one's claim and its risk.

## Adding a check

A check is one file. It reads a normalized fact layer — no Kubernetes types, no
Prometheus types — and returns what it saw. Ownership, provenance, the fix
command, grouping, ranking, pricing and rendering all happen downstream, so a
new check inherits the entire pipeline for free:

```go
type MyCheck struct{}

func (MyCheck) Describe() check.Descriptor        { ... }
func (MyCheck) Applicable(d inventory.Device) bool { ... }
func (MyCheck) Run(ctx context.Context, cl *inventory.Cluster, p check.Params) ([]check.RawFinding, error)

func init() { check.Register(MyCheck{}) }
```

`Describe` requires you to state both what the check **claims** and the **risk**
of acting on it. A check with nothing to warn about has not thought about being
wrong.

Because checks read facts, their tests are literals — no server, no fixtures.
See [`internal/check/check_test.go`](internal/check/check_test.go).

## Suppressing

```console
ullage ignore research/jupyter-alice --reason "reserved for the Q3 eval" --until 2026-03-01
```

Writes to `.ullage.yaml`. A reason is required: a suppression without one is
indistinguishable from a mistake six months later.

## Costs

Costs use built-in approximate list prices and are wrong for almost everyone —
reservations, savings plans, spot and negotiated discounts all move them, often
by more than half. Override with `--pricing`, or drop them with `--no-cost`. The
source is printed under every figure. Rates are never blended across models: an
H100 and a T4 differ roughly tenfold, so a single averaged rate is a fabricated
number wearing a decimal point.

## Status

v0.1, and honest about it. The claims are deliberately narrow, the exclusions
are deliberately loud, and the fixes are deliberately conservative. Issues and
checks welcome.

## Licence

Apache 2.0.
