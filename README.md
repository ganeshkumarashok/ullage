<h1 align="center">ullage</h1>

<p align="center"><strong>The GPU your cluster paid for and didn't use.</strong></p>

<p align="center">
  <a href="https://github.com/ganeshkumarashok/ullage/actions/workflows/ci.yaml"><img alt="ci" src="https://github.com/ganeshkumarashok/ullage/actions/workflows/ci.yaml/badge.svg"></a>
  <a href="LICENSE"><img alt="Apache 2.0" src="https://img.shields.io/badge/licence-Apache--2.0-blue"></a>
  <img alt="Go 1.24+" src="https://img.shields.io/badge/go-1.24%2B-00ADD8">
  <img alt="cluster access: read-only" src="https://img.shields.io/badge/cluster%20access-read--only-3fb950">
  <img alt="status: alpha" src="https://img.shields.io/badge/status-alpha-d29922">
</p>

<p align="center">
  <img src="docs/hero.svg" width="100%"
       alt="Three nodes whose accelerators are all allocated. Eleven of the twelve did no GPU work for fourteen days, but four of those are held open deliberately by an autoscaler minimum, so they are shown and never counted as waste. Cluster-wide: 5.9k of 22k accelerator-hours unused.">
</p>

`ullage` finds GPUs your cluster pays for but nothing uses. For each one you get
the evidence, the owner, and the command that frees it.

It only reads. It cannot change your cluster.

*"Ullage" is the empty space in a wine barrel: capacity you paid for and didn't
fill.*

**[Thirty seconds](#thirty-seconds)** ·
[How it measures](#how-it-measures) ·
[Why another tool](#why-another-tool) ·
[How it compares](#how-it-compares) ·
[What it will not do](#what-it-will-not-do) ·
[Install](#install) ·
[Use it for real](#use-it-for-real) ·
[Checks](#checks) ·
[Reference](#reference)

## Thirty seconds

No Kubernetes, no Prometheus, no cloud account, no configuration:

```console
$ git clone https://github.com/ganeshkumarashok/ullage && cd ullage
$ make demo
```

```
ullage v0.1.0  demo  window 14d

  5.9k of 22k accelerator-hours unused (27%)
  60 of 68 accelerators analysed  (8 excluded, see below)

      WORKLOAD                      GPUS   ULLAGE    FOR OWNER           
  1.  research/jupyter-alice           3     1.0k    14d alice@…         
      3 pods, no GPU work since the window began · owned by StatefulSet
      ~$3,427  ·  3 × NVIDIA-A100-SXM4-80GB

  2.  pool/l4-serving                  8     2.7k    14d platform        
      2 nodes, nothing scheduled · 2 pods block scale-down
      ~$2,016  ·  8 × NVIDIA-L4

  3.  research/dra-sandbox-erin        4     1.1k    11d erin@…          
      no GPU work since 31 Jul 06:00
      ~$1,690  ·  4 × NVIDIA-L40S

  4.  research/scratch-pod-bob         2      432     9d bob@…           
      no GPU work since 2 Aug 06:00
      ~$1,469  ·  2 × NVIDIA-A100-SXM4-80GB

  5.  ml-platform/finetune-carol       1      336    14d ml-platform@…   
      no GPU work since the window began · owned by Notebook (no safe automatic fix)
      ~$1,142  ·  1 × NVIDIA-A100-SXM4-80GB

  6.  research/gappy-session-frank     1      240    10d …arch-platform@…
      no GPU work since 1 Aug 06:00
      ~$816  ·  1 × NVIDIA-A100-SXM4-80GB
      confidence: medium — sample coverage is incomplete over the window

  7.  serving/embed-v2                 1       96     4d serving-team@…  
      CrashLoopBackOff
      ~$326  ·  1 × NVIDIA-A100-SXM4-80GB

  Reserved on purpose
  16 accelerators, 5.4k — held empty on purpose, not counted as waste
    · pool/h100-reserve: 16 accelerators on pool/h100-reserve are held empty deliberately
      pool/h100-reserve is held at a minimum of 2 nodes by the cluster
      autoscaler, so these nodes are kept on purpose. ullage cannot tell whether
      that reservation is still needed — this is shown so the decision stays
      visible, not because it is wrong.

  Not analysed
    · 2 accelerators — shared-device
      pool t4-shared is time-slicing, 4 replicas per device. Device-level
      utilization reflects every co-tenant, so an idle pod sharing a device with
      a busy one is invisible
    · 2 accelerators — mig-instance
      pool a100-mig has MIG enabled (mixed strategy). Device-level utilization
      is not meaningful per MIG instance
    · 4 accelerators — driver-initialising
      pool a10-new has GPU hardware that is not yet allocatable — the driver
      or device plugin is still starting

  Unmet demand
    4 pods are waiting for 4 accelerators — 4 reported Unschedulable by the scheduler
    This is context, not a finding: pending pods hold no devices.

  Costs use built-in list prices (approximate; override with --pricing).

  Next: ullage explain research/jupyter-alice
        shows the evidence, the owner, and the exact command to fix it
```

Every number above is computed, not canned. The demo runs the production
pipeline — the same client, queries, checks and renderer — against in-memory
HTTP servers that serve Kubernetes- and Prometheus-shaped responses. They are
stand-ins for the real thing, not a real API server and a real Prometheus, but
nothing between them and the output is staged. `ullage demo --serve` exposes
those endpoints so you can point the real CLI at them and confirm that.

### Then ask why

A number nobody can act on is a number nobody reads. `ullage explain` opens one
finding: the evidence behind the claim, what the claim deliberately stops short
of saying, who owns the workload, the next command to run, and what that
command will cost whoever is using it. Where something blocks the obvious fix,
the command it gives you is the one that finds the blocker — and it says so,
rather than handing you a scale-down that will silently do nothing.

```console
$ ./bin/ullage explain research/jupyter-alice --demo
```

<details>
<summary>the whole thing, verbatim — 3 accelerators, an owner, one command, and the reason the obvious command is wrong</summary>

```
  research/jupyter-alice
  idle-pod · research/jupyter-alice: 3 accelerators held with no work for 14d

  Evidence
    Window           14d ending 11 Aug 2026 07:00 UTC
    Unused for       14d
    Last GPU work    none within the window
    Peak utilization 0% across the whole window
    Power draw       56 W mean (14% of 400 W TDP)
    Sample coverage  100% of expected samples present

    Utilization      ▁▁▁▁▁▁▁▁▁▁▁▁▁▁  all zero
                 14d ago    now

  What this means
    Every utilization sample for these accelerators read exactly zero for the
    last 14d. ullage does not claim the workload is unimportant, and it does not
    estimate how efficiently it ran — GPU utilization is a poor measure of
    that. It claims only what the metric can prove: no CUDA kernel was resident
    on these devices at any sampled moment in that time. Power draw
    independently agrees: the devices are drawing near-idle wattage.

  Accelerators
    Held             3 × NVIDIA-A100-SXM4-80GB (exclusive)
    Unused           1.0k accelerator-hours
    Cost             ~$3,427 over the window
                 built-in list prices (approximate; override with --pricing) rate for NVIDIA-A100-SXM4-80GB; ullage never blends rates across models

  Managed by
    Root owner       StatefulSet/jupyter-alice

  Owner
    Owner            alice@example.com
    Resolved via     pod-annotation
               ullage.dev/owner=alice@example.com

  What to do
    Deleting the pods will not free the devices — StatefulSet jupyter-alice
    recreates them.

    kubectl scale statefulset -n research jupyter-alice --replicas=0

    Confirm with alice@example.com before running this.

  Before you do
    These pods are Running, not Completed. State held only in the container
    filesystem will be lost. ullage measures idleness, not intent, and cannot
    distinguish an abandoned session from capacity held warm on purpose.

  Stop it happening again
    Interactive GPU sessions are the most common source of this finding. A TTL
    controller, an activity-based idle culler, or a scheduled scale-to-zero for
    notebook workloads removes the class of problem rather than this instance of
    it.

  Suppress: ullage ignore idle-pod/research/jupyter-alice --reason "..." --until 2026-11-11
  Docs:     https://github.com/ganeshkumarashok/ullage/blob/main/docs/checks/idle-pod.md
```

</details>

Note what it targets. Three idle pods owned by a StatefulSet do not go away
with `kubectl delete pod`; the controller recreates them and nothing is freed.
`ullage` walks the ownership chain to the root and suggests scaling the
StatefulSet instead. Where the root is a CRD it does not recognise, it names
the resource and prints **no command at all**.

Three more things worth running before you decide whether to trust it. All use
the same demo cluster, and none of them touch anything real:

| run this | and you see |
|---|---|
| `make tour` | the two-minute version of the whole idea, including what the tool refuses to claim |
| [`examples/ci-gate.sh`](examples/ci-gate.sh) | a build failing when waste goes over a budget, and why a scan that broke must not exit like a scan that found nothing |
| [`examples/weekly-digest.sh`](examples/weekly-digest.sh) | one scan turned into a Markdown report grouped by owner, with `jq` |

## How it measures

Four stages. Each one is allowed to answer *"I don't know"*, and most of the
work goes into making it refuse when it should.

```
   Kubernetes API                   Prometheus
   nodes · pods · controllers       DCGM metrics
   PDBs · resource claims           per device
          │                                │
          └───────────────┬────────────────┘
                          ▼
   1. census ─▶ 2. measure ─▶ 3. judge ─▶ 4. attribute
   what is      did it do     is that     who can
   there?       any work?     waste?      act on it
                                               │
                                               ▼
                                  a recommendation, ranked
                                  by what it is costing
```

**1. Census — what hardware is there, and can it be judged at all.**
Nodes are read from the Kubernetes API and classified by how their accelerators
are allocated. A device gets measured only when one pod exclusively holds one
physical device, because that is the only arrangement in which "the device was
idle" and "this workload was idle" are the same sentence. Time-sliced and MIG
devices are counted and then set aside — device-level utilization there reflects
every co-tenant, so it cannot convict any single pod. Nothing that is set aside
leaves the accounting; it appears as **No usable metric**.

**2. Measure — five signals, not one gauge.**
`DCGM_FI_DEV_GPU_UTIL` reports SM activity alone, and a device can read exactly
zero for a fortnight while doing continuous, expensive, real work: a video
pipeline living on NVENC/NVDEC, a data loader saturating the copy engines, or a
model held resident in framebuffer, which is the point of a warm replica and
exactly what someone would be furious to have scaled to zero. So four more
series are consulted, all of them in dcgm-exporter's default counter set:

| series | catches |
|---|---|
| `DCGM_FI_DEV_GPU_UTIL` | compute on the SMs |
| `DCGM_FI_DEV_ENC_UTIL` / `DCGM_FI_DEV_DEC_UTIL` | video encode / decode |
| `DCGM_FI_DEV_MEM_COPY_UTIL` | host↔device transfer, data loading, checkpointing |
| `DCGM_FI_DEV_FB_USED` | a model parked in framebuffer between requests |

**A device where any of them was ever non-zero is not idle**, whatever the SM
gauge said. A sixth series, `DCGM_FI_DEV_POWER_USAGE`, corroborates the verdict
from a different sensor entirely. If a series is missing the scan says so in a
warning, rather than quietly narrowing what "no work" means.

**3. Judge — a duration, and the right to refuse.**
Unused time is the trailing run of zeros up to now, not an average and not the
age of the object. The rules that stop a number from being produced matter more
than the arithmetic:

- **No samples is not zero samples.** A series that returned nothing is
  *unknown*. An exporter that died last week would otherwise generate a
  cluster-wide recommendation to delete everything.
- **When two queries disagree, believe the disagreement.** The aggregate
  (`max_over_time`) and the stepped range query are asked separately. If the
  aggregate proves the device did work but the range query cannot say when, no
  claim is made. This is not hypothetical: at a 14-day window the range query
  exceeded Prometheus's point limit and a GPU running at 78% was briefly
  reported as having done nothing at all.
- **Thin evidence caps confidence, and very thin evidence withdraws the claim.**
  Under 80% sample coverage nothing is reported at all. Under 95%, or with no
  power series to corroborate, the finding is capped at `medium` — and
  `--min-confidence` decides what you are shown.
- **Power has to agree.** Idle is corroborated when mean draw is under 20% of
  board TDP. An A100 doing nothing draws roughly 50–60 W of its 400 W.

**4. Attribute — who can act on it.**
Ownership resolves pod → controller → namespace, taking the first of
`ullage.dev/owner`, `owner`, `app.kubernetes.io/owner` or `team`, then contact
annotations; node-level findings fall back to the node pool. Every finding
records *how* it was resolved, so a wrong attribution can be traced.
`app.kubernetes.io/managed-by` is **not** consulted: it names the deploying
tool, and "go talk to Helm" helps nobody. `unowned` is a first-class answer,
because a device nobody claims is itself the finding.

Hours become money by multiplying accelerator-hours by a per-SKU rate; rates are
approximate list prices unless you supply your own with `--pricing`, and the
source is printed under every figure. Paid capacity is summed per node as
`min(node age, window) × accelerators`, so a node created this morning is not
billed for the whole fortnight.

Every query is printed on request, and none of it is a black box:

```console
ullage --prometheus http://localhost:9090 --explain-queries
```

## Why another tool

Every GPU dashboard can already show you a utilization graph. A graph answers
*what is happening*; the question you usually have is *what should I do about
it*, and the distance between the two is a person with a spreadsheet.

`ullage` is built around four things a dashboard is not trying to do.

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
minimum is reserved, not wasted. It appears under *unused by design*, with no
removal command attached. Printing reserved capacity in the same list as waste
is the fastest way for a tool to be dismissed as not understanding the business.

**It says what it did not look at.** Time-sliced and MIG devices are excluded,
by name and with a reason, because a device-level metric there reflects every
co-tenant. The accelerator census always reconciles: analysed plus excluded
equals observed. A percentage over a denominator you cannot account for turns a
monitoring gap into a claim about efficiency.

### How it compares

| | what it answers | acts on the cluster |
|---|---|---|
| **ullage** | which accelerators were paid for and idle, whose they are, and the one command that frees them | no — read-only by construction |
| [Robusta KRR](https://github.com/robusta-dev/krr) | what CPU/memory requests should be, from usage history | not by default — an optional enforcer can apply them |
| [OpenCost](https://www.opencost.io/) | what each workload cost, including GPU | no — allocation and showback |
| [Kubecost](https://www.kubecost.com/) | cost allocation, with efficiency and savings reports | optionally — self-hosted Actions can resize and turn down |
| [Cast AI](https://cast.ai/) | how to run the same workloads for less | yes — bin-packs and replaces nodes |
| [nvidia-dcgm-exporter](https://github.com/NVIDIA/dcgm-exporter) | per-device utilization metrics | no — it is the data source ullage reads |

KRR is the closest in spirit and the clearest influence: right-sizing from
observed usage, delivered as a recommendation rather than an action. It does not
cover accelerators, which are the expensive part of a GPU cluster and the part
whose idleness is hardest to read from a utilization metric.

OpenCost and Kubecost answer *what did this cost*. `ullage` answers *what did
this cost for nothing, and what do I type to stop it*. They compose: run
OpenCost for allocation, `ullage` for the subset that bought nothing.

## What it will not do

- **It will not call a workload inefficient.** GPU utilization is a poor measure
  of how hard a device is working — a single-threaded kernel reads 100%. The
  only thing the metric supports is its zero, so *zero* is the only thing
  `ullage` claims.
- **It will not trust one gauge to mean "idle".** `DCGM_FI_DEV_GPU_UTIL`
  reports SM activity alone. A transcoding pipeline living on NVENC, a job
  moving data over the copy engines, and a model sitting resident in
  framebuffer waiting for requests all read a clean zero on it. `ullage` also
  reads the encoder, decoder, copy-engine and framebuffer gauges, and a device
  busy on any of them is not reported. Where those gauges are not exported it
  says so in the output rather than treating the SM zero as the whole story.
- **It will not treat a low average as idle.** A job running one hard hour in
  twenty-four averages 4%. Tools that threshold on averages flag it; its owner
  loses real work.
- **It will not read absent samples as zero.** An exporter that died a week ago
  is not a fleet that went idle.
- **It will not write to your cluster.** The Kubernetes client has no write
  methods. That is a property of the code, not a promise.
- **It will not phone home.** There is no telemetry and no analytics, and
  `ullage` itself opens no connection except to your API server and your
  Prometheus. The one exception is not its own: if your kubeconfig uses an exec
  credential plugin, `ullage` runs it exactly as `kubectl` would, and `az`,
  `aws` or `gke-gcloud-auth-plugin` will talk to your identity provider.

## Install

One binary, one third-party Go dependency ([`yaml.v3`](https://gopkg.in/yaml.v3)).
No client-go, no controller-runtime, no vendored Kubernetes tree, no CRDs, no
agent, no operator, and nothing to install into your cluster. A clone builds in
a few seconds and the test suite runs without a cluster.

From source — works today, needs Go 1.24+:

```console
git clone https://github.com/ganeshkumarashok/ullage && cd ullage
make install          # go install into $GOBIN (use `make build` for ./bin/ullage)
```

Or build the container — around 10MB, distroless, and non-root (`USER 65532`). The
supplied CronJob additionally runs it with a read-only root filesystem and every
capability dropped:

```console
make image     # tags ghcr.io/ganeshkumarashok/ullage:$(git describe --tags)
docker run --rm ghcr.io/ganeshkumarashok/ullage:v0.1.0 demo
```

Released builds work the same way:

```console
go install github.com/ganeshkumarashok/ullage/cmd/ullage@latest
docker run --rm ghcr.io/ganeshkumarashok/ullage:v0.1.0 demo
```

Each release also publishes signed checksums and a krew plugin manifest
(`krew-v0.1.0.yaml`). `kubectl krew install ullage` does **not** work yet — that
needs the plugin to be accepted into
[krew-index](https://github.com/kubernetes-sigs/krew-index), which has not been
submitted. Until then the manifest can be installed directly:

```console
kubectl krew install --manifest-url https://github.com/ganeshkumarashok/ullage/releases/download/v0.1.0/krew-v0.1.0.yaml
```

## Run it on a schedule

```console
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/cronjob.yaml
```

[`deploy/rbac.yaml`](deploy/rbac.yaml) is the permission set, one `apiGroups`
block at a time with a comment explaining why each is needed. Every verb is
`get` or `list` — nothing in it can change anything. ConfigMap access is scoped
by `resourceNames` to the single autoscaler status object rather than granted
cluster-wide.

It cannot cover custom controllers, and on a GPU cluster it often will not: if
your pods are owned by a `PyTorchJob`, a `RayCluster` or an Argo `Workflow`,
ullage cannot read them under this file. Rather than silently attributing those
pods to nobody, it warns and names the kind, so the missing grant is a two-line
edit instead of a mystery.

The CronJob runs weekly, not hourly, and that is deliberate: `ullage` measures a
two-week window and reports capacity that has been unused for days. Running it
sixty times to produce the same seven findings is how a tool becomes background
noise.

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

### What is supported today

|  | Status |
|---|---|
| NVIDIA, `dcgm-exporter` | Supported |
| Exclusive whole-device allocation | Supported |
| DRA (`resource.k8s.io`, GA in 1.34) | Supported — claims are filtered by driver, so a NIC claim is not counted as a GPU, and a claim shared between pods is counted once |
| cluster-autoscaler | Supported, including a pool spread across zonal node groups |
| Karpenter | Supported — NodePools, zero-node disruption budgets, `karpenter.sh/do-not-disrupt` |
| Thanos, Mimir, Cortex | Should work; anything speaking the Prometheus query API. If the endpoint holds more than one cluster, pass `--metrics-selector 'cluster="prod-eastus"'` — node names are not unique across clusters, and `ullage` warns rather than merging them silently |
| MIG, time-slicing, MPS | **Counted and named, never analysed per pod.** Device-level utilization cannot separate co-tenants, so shared devices are excluded from idle-pod analysis. A node where nothing at all is running is still reported, whatever its sharing mode. Both MIG strategies are detected, including `single`, where instances are advertised as whole `nvidia.com/gpu` |
| AMD, Intel, Habana | **Discovered, not measured.** Their resource names are recognised and counted in the census of a mixed cluster, and land in the exclusions with `no metric source`. No utilization is read for them, so nothing is attributed to their owners. A cluster with no NVIDIA devices at all has no `DCGM_FI_DEV_GPU_UTIL` series and the scan stops rather than guessing |
| Amazon Managed Prometheus, Azure Monitor, Google Managed Prometheus | **Not directly.** `ullage` implements no provider-native signing or token exchange: Amazon wants SigV4, Azure a Microsoft Entra bearer token, Google an OAuth2/ADC credential. Point `ullage` at a signing proxy, or obtain a token yourself and pass `--prometheus-auth bearer --prometheus-token-file FILE` — both flags, the file is ignored without the mode. It is re-read on every request, so a rotating projected token keeps working |

The fact layer is vendor-neutral by construction — checks never see a
Kubernetes or Prometheus type — so adding AMD's `device-metrics-exporter` is a
change to one file. Nobody has done it yet, and this table says so rather than
letting the architecture imply a capability that does not exist.
## Output

`ullage` prints a table. `--output html` writes a self-contained report to hand
to whoever approves the change. `--output json` emits a versioned document for
dashboards and CI. Exit codes: `0` nothing found, `1` findings present, `2` the
scan could not complete.

## Checks

| ID | Finds |
|---|---|
| [`idle-pod`](docs/checks/idle-pod.md) | Pods holding accelerators that have read exactly zero for longer than the threshold |
| [`stuck-pod`](docs/checks/stuck-pod.md) | Pods holding accelerators whose containers are not running (crash loops, image pull failures, wedged init) |
| [`unused-node`](docs/checks/unused-node.md) | Accelerator nodes nothing has been scheduled on, and what is stopping the autoscaler from reclaiming them |

`ullage checks` prints each one's claim and its risk. Every check page says how
the check is measured and when it is wrong. Read that before acting on a batch.

## Reference

- [Output formats, exit codes and the JSON contract](docs/output.md)
- [Suppressing findings](docs/suppressing.md)
- [Costs and pricing](docs/costs.md)
- [Developing and adding a check](docs/developing.md)

## Status

v0.2, with three checks and a stable JSON output format.

The JSON document is a contract. `pkg/ullage/api` round-trips it and a golden
file fails on any change to the wire shape. Fields may be added within v0.x;
existing ones will not change meaning without a major version.

Checks live under `internal/` for now. The three here have not yet disagreed
with each other enough to show where a plugin boundary belongs, and publishing
one early would freeze the wrong shape. See [ROADMAP.md](ROADMAP.md).

Issues and checks welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

Apache 2.0.
