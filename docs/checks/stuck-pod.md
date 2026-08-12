# stuck-pod

> A pod is scheduled and holding allocated accelerators, but its containers are
> not running.

## What the finding claims

The devices are **allocated and unavailable to anything else**, and nothing is
using them, because the container that would use them is not up.

This is a state claim, not a utilization judgement. It does not depend on a
metrics backend at all — it is read from the Kubernetes API. That makes it the
most reliable check ullage has, and the one most likely to be true on a cluster
with no exporter installed.

## How it is measured

A pod is reported when all of these hold:

1. It is scheduled to a node and holds at least one accelerator — by extended
   resource, MIG profile, time-sliced replica, or DRA claim.
2. Its containers are not running: `CrashLoopBackOff`, `ImagePullBackOff`,
   `CreateContainerError`, `Error`, or stuck in `Init`.
3. It has been in that state for at least `--stuck-threshold` (default `1h`).

Init containers get their own grace period, because a pod pulling a large image
is not stuck, it is slow. The clock starts from the last state transition, so a
pod that is crash-looping is measured from the beginning of the loop rather
than from the last individual restart.

The reported unused duration is capped at the analysis window, so a pod that
has been broken for a month does not claim a month of waste inside a 14-day
window.

## What it does not mean

- **It does not mean the pod should be deleted.** A crash loop is never
  intentional, but the fix belongs to whoever owns the workload. Deleting it
  frees the device and destroys the evidence.
- **It does not mean the image is wrong.** `ImagePullBackOff` on a GPU node is
  frequently a registry credential or a node-level networking problem, not a
  bad tag.

## When this finding is wrong

- **The pod is mid-deploy.** A rollout that is 90 seconds into pulling a 12GB
  CUDA image is not stuck. `--stuck-threshold` exists for exactly this; the
  default of `1h` is deliberately generous.
- **The controller is already handling it.** A Job with a retry budget that is
  partway through backing off is behaving correctly. It still holds the device,
  so ullage still reports it — the capacity really is unavailable — but the
  right action may be to wait.

## What to do

Start with the logs, not with `kubectl delete`:

```console
$ kubectl logs -n <namespace> <pod> --previous
$ kubectl describe pod -n <namespace> <pod>
```

`ullage explain <id>` prints the owning controller, so you know whether
deleting the pod will simply produce another one in the same state — which,
for a Deployment or StatefulSet with a broken image, it will.

This check is often the fastest real money in a cluster. A crash-looping pod
holding eight H100s costs the same as a working one, and unlike an idle
notebook, nobody is getting any value from it at all.

## Suppressing

```console
$ ullage ignore stuck-pod/team-a/trainer-7 \
    --reason "known bad image, fix tracked in PLAT-4471" --until 2026-03-01
```

See [Suppressing](../../README.md#suppressing) for the id-matching rules.

## Flags that affect this check

| Flag | Default | Effect |
| --- | --- | --- |
| `--stuck-threshold` | `1h` | Minimum time in a non-running state before reporting |
| `--window` | `14d` | Caps the reported unused duration |
