# Checks

Every finding ullage reports names the check that produced it, and every check
has a page here explaining exactly what it claims, how it is measured, and —
most importantly — when it is wrong.

| Check | Claims | Needs metrics |
| --- | --- | --- |
| [`idle-pod`](idle-pod.md) | Every utilization sample for these devices read exactly zero | Yes |
| [`stuck-pod`](stuck-pod.md) | Devices are allocated but the containers are not running | No |
| [`unused-node`](unused-node.md) | The node advertises accelerators and holds no accelerator work | Partially |

`ullage checks` prints the same claims and risks from the running binary, which
is the authoritative source — these pages expand on them.

Each page follows the same shape:

- **What the finding claims** — the narrowest true statement, and nothing wider
- **How it is measured** — the actual conditions, thresholds, and coverage floor
- **What it does not mean** — the conclusions people reach that the data does
  not support
- **When this finding is wrong** — read this before acting in bulk
- **What to do** — usually "ask the owner", never "let the tool fix it"
- **Suppressing** — how to record a decision so it is not rediscovered

A check that cannot describe its own failure modes has no business
recommending that you delete something.
