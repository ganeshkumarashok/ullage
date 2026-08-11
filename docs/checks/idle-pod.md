# idle-pod

> A pod is Running and holding accelerators, and every utilization sample for
> those devices has read exactly zero for at least `--idle-threshold`.

## What the finding claims

Only this: **no kernel was resident on these devices at any sampled moment in
the fallow span the finding names.**

The span is the trailing run of zeroes, not the whole window. A pod that worked
nine days ago and has read exactly zero since is idle now, and a rule demanding
zero across the entire window would miss it — and would miss more of them the
longer the window, which is the wrong way round. The finding always states the
span it measured, so the claim can be checked against its own evidence.

That is a narrow claim, and it is narrow on purpose. It is not a claim that the
workload is unimportant, not a claim that the owner did something wrong, and
not an efficiency score. GPU utilization is a poor measure of how well a job
uses a device — a training loop that is starved by its data pipeline can sit at
8% and still be doing exactly what it should. ullage does not report that,
because it cannot tell that apart from a well-tuned job that happens to be
memory-bound.

Zero across an entire window is different. There is no kernel that runs at 0%.
The device did nothing.

## How it is measured

1. Find every pod that holds an accelerator — by extended resource
   (`nvidia.com/gpu` and friends), MIG profile, time-sliced replica, or DRA
   resource claim.
2. Ask the metrics backend for per-device utilization across `--window`
   (default `14d`), attributed to that pod.
3. Take the trailing run of zeroes on each device, and use the **shortest** of
   them: one busy device makes the pod busy.
4. Require sample coverage of at least **80%**, measured over the pod's own
   observable lifetime rather than the scan window. Against the window a pod
   that has existed for two days of a fortnight tops out near 14% coverage
   however completely it was watched, so a window-relative floor would discard
   every young pod — and "someone started an expensive pod last week and forgot
   it" is the most actionable finding here.
5. Require that fallow span to be at least `--idle-threshold` (default `24h`),
   and no longer than the pod has existed.

A pod that fails any of 3–5 is not reported. There is no partial credit and no
"looks low" band: no average is taken and no threshold is applied to the
utilization value, because "low" is a judgement about someone else's workload
and "zero" is not.

A device whose series stopped arriving is **not** treated as a device reading
zero. A stale series disqualifies the pod outright, because a dead exporter
would otherwise become a cluster-wide deletion recommendation.

### Confidence

`high` requires all of the following. Anything short of it is `medium`; this
check never emits `low`.

| Requirement | Why |
| --- | --- |
| Sample coverage at or above 95% | A gap wide enough to hide a working period |
| A power series exists for every device | One sensor agreeing with itself is not corroboration |
| Mean power draw below the idle fraction of TDP | A device drawing real power is doing something the utilization metric did not see |
| Zero throughout the window, or a fallow span of at least half of it | A short run of zeroes near the end of a long window leans on the query resolution |

`--min-confidence` (default `medium`) sets what is shown. Raise it to `high`
when you are about to act in bulk. Power draw is what separates "the metric
says zero" from "the device is demonstrably doing nothing", so a cluster with
no power series will not produce a `high`-confidence idle-pod finding at all.

## What it does not mean

- **It does not mean the pod is safe to delete.** These pods are `Running`, not
  `Completed`. Anything held only in the container filesystem — a notebook
  someone has not saved, a checkpoint written to `emptyDir` — is gone.
- **It does not measure intent.** A licence server, a warm standby, a
  scheduled job that fires next Tuesday and an abandoned notebook all look
  identical to a utilization metric. ullage cannot distinguish them, and does
  not try. It reports the measurement and names the owner, because the owner
  can.
- **It does not mean the GPU was the bottleneck.** A pod can be idle on the GPU
  and pinned on the CPU.

## When this finding is wrong

Read this list before acting on a batch.

- **The workload does not use the GPU through a path your exporter sees.** Most
  commonly: the metrics backend exports DCGM per-GPU but the pod runs on a
  device plugin configuration that reports under a different label set, so
  samples are attributed to the wrong pod or to none. `ullage doctor` checks
  for this and will tell you when attribution coverage is poor.
- **Sampling is coarser than the work.** If the exporter scrapes every 30s and
  the job runs a two-second kernel each minute, every sample can legitimately
  land in a gap. This is rare on real training and inference workloads and
  common on tiny cron-style jobs.
- **The window is shorter than the duty cycle.** A job that runs on the first
  of the month reads zero for a 14d window in the middle of the month. Widen
  `--window`, or suppress with an expiry.
- **The device was intentionally held warm.** Reserved capacity is a real and
  legitimate pattern. Mark it — see below — so it stops being rediscovered.

If you find a case where a genuinely busy GPU reported zero, that is a bug in
this tool and we want it: please open an issue with the exporter, the device
plugin configuration, and `ullage doctor` output.

## What to do

`ullage explain <id>` prints the evidence, the owner, and the single command
that frees the capacity. It never runs it.

The right first action is almost always **to ask the owner**, not to scale
anything. The output gives you the owner annotation, the controller that
created the pod, and a contact where one is set — so the conversation starts
with evidence instead of a spreadsheet.

## Suppressing

```console
$ ullage ignore idle-pod/research/jupyter-alice \
    --reason "held warm for the Q3 eval" --until 2026-05-14
```

A reason is required, and an expiry is strongly encouraged: reserved capacity
usually stops being reserved and nobody sends a note when it does. Expired
entries stop applying and are named in the output rather than silently renewed.

See [Suppressing](../../README.md#suppressing) for the id-matching rules.

## Flags that affect this check

| Flag | Default | Effect |
| --- | --- | --- |
| `--window` | `14d` | Period examined |
| `--idle-threshold` | `24h` | Minimum fallow span before reporting |
| `--min-confidence` | `medium` | Confidence floor for display |
