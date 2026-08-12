# unused-node

> A node advertises accelerators, is Ready and schedulable, and nothing that
> holds an accelerator has been placed on it.

## What the finding claims

The node is **available, paid for, and empty of accelerator work**. No pod
holding an accelerator — by extended resource, MIG profile, time-sliced replica
or DRA claim — is placed on it, and no accelerator on it reported work within
the window.

This is the most expensive category ullage reports, because the unit is a whole
node rather than a device, and because an empty GPU node is usually empty for a
structural reason that will keep it empty.

## How it is measured

A node is reported when all of these hold:

1. It advertises at least one accelerator.
2. It is `Ready`, schedulable, and not cordoned or draining.
3. No accelerator-holding pod is placed on it.
4. No accelerator on it reported work within `--idle-threshold`. Any evidence
   of recent work excludes the node, whether or not the coverage was good
   enough to make a positive claim: evidence that something ran is not held to
   the same bar as evidence that nothing did, because the two errors are not
   symmetric.
5. Its age is at least `--idle-threshold` (default `24h`), so a node that
   joined an hour ago is not reported for having nothing on it yet.

Nodes that are cordoned or draining are excluded, because they are already the
subject of a deliberate decision.

### Why it is empty usually matters more than that it is empty

An empty GPU node is rarely an accident. The common causes are worth checking
before you scale anything down:

- A taint that nothing tolerates — frequently added during an incident and
  never removed.
- A node label the workloads' `nodeSelector` no longer matches, after a pool
  was renamed or a new SKU was introduced.
- A pool that only ever receives a workload that has since been deleted.
- Genuine reserved headroom.

ullage reports the taints and labels alongside the finding for this reason.

## What it does not mean

- **It does not mean the node should be removed.** Removing capacity is a
  capacity decision, not only a cost one.
- **It does not mean the autoscaler is broken.** An autoscaler that will not
  scale a pool down is often being blocked by something on the node — local
  storage, a PDB, an unevictable pod. Where ullage can see the blocker, it says
  so.
- **It does not mean the pool is unused.** A pool sized for a weekly batch job
  is empty most of the week by design.

## When this finding is wrong

- **The pool is reserved.** This is the single most common false positive, and
  it is not really a false positive — the measurement is correct and the
  conclusion is not. Mark reserved pools (see below) and ullage will account
  for them separately as *unused by design*, which keeps them out of the
  actionable list without hiding them from the totals.
- **The window straddles a quiet period.** A pool that serves a monthly close
  reads empty for most windows.
- **The node is warm standby for failover.** Deliberate, and worth suppressing
  with a reason so the next person does not re-litigate it.

## What to do

`ullage explain <id>` prints the node's taints, labels, age, and the command
that would scale the pool — which it will never run for you.

Before acting:

```console
$ kubectl describe node <node> | grep -A5 Taints
$ kubectl get nodes -l <your-pool-label> -o wide
```

Confirm the pool is not reserved for a launch, a failover, or a periodic job.
Then scale the pool, not the node — deleting a node that a node group will
immediately recreate achieves nothing.

## Suppressing

For a single pool:

```console
$ ullage ignore unused-node/pool/h100-reserve \
    --reason "reserved for the launch, revisit after GA" --until 2026-06-30
```

Wildcards work per path segment, so `unused-node/pool/*` covers one check
across every pool. A `*` never crosses a `/`.

See [Suppressing](../../README.md#suppressing) for the full rules.

## Flags that affect this check

| Flag | Default | Effect |
| --- | --- | --- |
| `--window` | `14d` | Period examined |
| `--idle-threshold` | `24h` | Minimum node age before reporting |
| `--min-confidence` | `medium` | Confidence floor for display |
