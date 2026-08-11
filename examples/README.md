# Examples

Every script here runs against the built-in demo cluster by default, so you can
see what it does before pointing it at anything real. Set `PROMETHEUS` to run
the same script against your own cluster:

```console
PROMETHEUS=http://prometheus.monitoring.svc:9090 ./examples/ci-gate.sh
```

They need `bash`, and the two JSON ones need [`jq`](https://jqlang.github.io/jq/).

---

### [`tour.sh`](tour.sh) — start here

```console
make tour
```

A narrated two-minute walkthrough of the whole idea: find the waste, ask why,
see what the tool deliberately refuses to claim, record a disagreement, and feed
the result to something else. No cluster, no GPU, no configuration.

It is the fastest way to decide whether this tool is worth your time.

---

### [`ci-gate.sh`](ci-gate.sh) — fail a pipeline on new waste

```console
BUDGET_USD=2000 ./examples/ci-gate.sh
```

Fails the build when idle accelerator spend exceeds a budget you choose.

The naive version of this — *fail if there are any findings* — goes red on day
one and stays red, which teaches everyone to ignore it. This one gates on a
budget, at high confidence only, and skips anything the team has already
suppressed with a reason and an expiry.

It also distinguishes **exit 1** (findings) from **exit 2** (the scan could not
run) and fails loudly on the second. A gate that reads a dead exporter as a
clean cluster is worse than no gate.

---

### [`weekly-digest.sh`](weekly-digest.sh) — a report someone will read

```console
./examples/weekly-digest.sh > digest.md
```

Turns a scan into Markdown grouped **by owner**, because "who do I talk to" is
the question that decides whether anything actually gets fixed, and no dashboard
answers it. Pipe it into an email, a PR comment, a wiki page, or reshape it for
a Slack webhook.

It carries the caveats with the numbers: capacity held on purpose, accelerators
that were not analysed and why, and any warnings from the scan. A percentage
over a denominator the reader cannot account for is how a monitoring gap turns
into an argument.

---

## Using the JSON

Both JSON examples read the documented contract in
[`pkg/ullage/api`](../pkg/ullage/api). It is versioned under `apiVersion`, the
four finding lists are always present (empty rather than `null`), and a test
compares re-marshalled bytes so a field cannot quietly disappear between
releases.

The fields worth knowing:

| path | meaning |
|---|---|
| `scan.gpuHoursFallow` / `scan.gpuHoursPaid` | the headline ratio, in accelerator-hours |
| `scan.acceleratorsAnalyzed` / `acceleratorsObserved` | always reconciles with `notAnalyzed` |
| `recommendations[]` | findings you should act on |
| `byDesign[]` | capacity held empty on purpose — **not** waste |
| `suppressed[]` | matched an entry in `.ullage.yaml` |
| `notAnalyzed[]` | excluded, with a machine-readable `code` and a reason |
| `.impact.windowCost` | absent when no price is known; never guessed |
| `.fix.command` | absent when there is no safe automatic fix |
| `.owner.identity` | absent when no owner could be resolved |

Anything optional is absent rather than zero. `windowCost: 0` would mean free;
a missing `windowCost` means unknown, and the difference matters when you are
summing.

## Against a real cluster

```console
ullage doctor --prometheus http://<your-prometheus>:9090
```

`doctor` names every prerequisite it cannot satisfy and what to do about it.
If you have no cluster to try this on, [`make e2e-kind`](../e2e/README.md)
builds a throwaway one with fake accelerator capacity and a synthetic exporter.
