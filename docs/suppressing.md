# Suppressing findings

Every scanner produces findings its owners have looked at and accepted. Without
a way to record that, the tool gets muted or wrapped in a `grep`, and the
decision is lost. The next person rediscovers the finding and reopens the
argument.

`ullage explain` prints the command to use, including the finding id:

```console
  Suppress: ullage ignore idle-pod/research/jupyter-alice --reason "..." --until 2026-11-11
```

That writes `.ullage.yaml`:

```yaml
suppress:
  - id: "idle-pod/research/jupyter-alice"
    reason: "reserved for the Q3 eval"
    until: "2026-11-11"
```

Ids are slash-separated and `*` works per segment: `unused-node/pool/*` covers
one check across every pool, `*/research/*` covers one namespace. A `*` never
crosses a `/`, so you cannot turn "cluster-scoped" into "every namespace" by
accident.

## Rules

**A reason is required.** Six months on, an entry without one is
indistinguishable from a mistake.

**Expired entries stop applying, and are named when they do.** Nothing is
renewed for you and nothing is rewritten for you.

**Entries that match nothing are reported.** Either the id is wrong and you are
not suppressing what you think, or the problem is fixed and the entry is litter.

**The suppressed total is printed with its size,** not as a bare count:

```
1 finding suppressed by .ullage.yaml (1.0k accelerator-hours, ~$3,427).
```

A malformed config file is a hard error, not a warning. Continuing would print
findings you had asked not to see, and you would read that as the tool being
broken.

Use `--config` to point at a different file. `ullage ignore --config` writes to
the same one.
