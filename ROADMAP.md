# Roadmap

What ullage intends to do next, and — as importantly — what it does not intend
to do. If something you need is in the "not planned" list, that is an invitation
to argue, not a closed door.

## Now (v0.2)

Three checks, read-only, NVIDIA via DCGM, one binary, one runtime dependency.

- `idle-pod` — a pod holds accelerators and none of them has done work.
- `stuck-pod` — a pod holds accelerators and cannot run.
- `unused-node` — a node has accelerators and nothing scheduled on them.

## Next

**Make the vendor-neutral fact layer true in practice, not only in structure.**
AMD accelerators are currently *discovered* but not *measured*. Adding AMD
`device-metrics-exporter` as a second metric source is the single change that
would most widen who can use this. Intel follows the same shape. We would
particularly like a contributor who actually runs these.

**Managed Prometheus.** Azure Monitor managed Prometheus and Google Managed
Prometheus both work as remote-read endpoints today only by accident. Explicit
support, and a `doctor` check that recognises them, is a small change that
unblocks a large share of the cloud audience.

**A JSON Schema for the contract**, generated from `pkg/ullage/api`, so that the
enum-valued fields are machine-discoverable rather than reverse-engineered.

**A `kubectl ullage` Krew plugin**, so the tool is reachable from where its
users already are.

**A findings sink for the CronJob.** Today a scheduled scan writes to pod logs
and nobody reads them. Both success and failure are silent, which is the worst
property a scheduled job can have.

## Later

**A public check ABI.** The check and provider contracts are under `internal/`
today so that out-of-tree extension is impossible — deliberately, because
freezing an extension ABI before the third check taught us what it should look
like would freeze the wrong shape. Once the fact model has survived a second
metric vendor, these move to `pkg/`.

**Fractional under-use.** ullage currently judges only unambiguous idleness: a
device that did nothing. It refuses to call a GPU at 12% utilization "wasted",
because the metric that would justify that claim (`DCGM_FI_PROF_SM_ACTIVE`, and
memory occupancy alongside it) is often absent, and because "12% utilization" is
a legitimate steady state for plenty of inference workloads. Reporting it
*where the profiling metrics exist and only there* is the honest version of this
feature, and it is a larger design problem than it looks.

**Time-sliced and MIG devices.** Both are excluded today with a stated reason
(`ULL-101`, `ULL-102`) because device-level utilization cannot be attributed to
a co-tenant. Per-instance DCGM series would change that for MIG.

## Not planned

**Automatic remediation.** ullage prints commands; it will not run them. A tool
that deletes workloads is a tool that gets uninstalled the first time it is
wrong, and being wrong is unavoidable — intent is not in the metrics. The
read-only posture is the product, not a limitation of it.

**A dashboard.** Grafana is better at this than we would be. ullage emits JSON
so you can build one; it will not ship one.

**A cluster-resident controller.** A CronJob is enough. If the answer changes
slowly — and eleven days of idleness changes slowly — then polling on a
schedule is the right architecture, and a permanently resident controller is
mostly a new thing to page someone about.

**Cost allocation and showback.** [OpenCost](https://www.opencost.io/) does this
properly and is already a CNCF incubating project. ullage answers "which of
these should not be running", not "who should be billed". Where they overlap, we
would rather emit something OpenCost can consume.
