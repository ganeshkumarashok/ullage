# idle-pod

> A pod is Running and holding accelerators, and every utilization sample for
> those devices read exactly zero for the whole window.

## What the finding claims

Only this: **no kernel was resident on these devices at any sampled moment in
the window.**

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
3. Keep the pod only if **every** returned sample is exactly zero.
4. Require sample coverage of at least **80%** of the window. A device that
   reported for twenty minutes out of fourteen days has not demonstrated
   anything, so it is not reported.
5. Require the fallow span to be at least `--idle-threshold` (default `24h`).

A pod that fails any of 3–5 is not reported. There is no partial credit and no
"looks low" band.

### Confidence

| Level | When |
| --- | --- |
| `high` | Full-window coverage, every sample zero |
| `medium` | Coverage above the floor but not the whole window, or the pod is younger than the window |
| `low` | Coverage close to the floor — enough to report, not enough to act on without looking |

`--min-confidence` (default `medium`) sets what is shown. Raise it to `high`
when you are about to act in bulk.

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
