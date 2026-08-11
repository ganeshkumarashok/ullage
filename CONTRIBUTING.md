# Contributing to ullage

Thank you for considering it. This is a young project and the shape of it is
still negotiable.

## Before anything else: the standard of evidence

ullage exists to be believed. Its entire value is that when it says a GPU did
nothing for eleven days, a platform engineer can act on that without checking.
So the bar for a change is not "the tests pass" — it is **"could this ever say
something false?"**

Concretely, that means:

- **A check must never guess.** If the data cannot distinguish waste from
  intent, the correct behaviour is to say so and exclude the device from the
  analysed count, not to report it with a hedge. See `internal/inventory` for
  the exclusion codes (`ULL-1xx`).
- **"Nothing found" and "could not look" must never look alike.** A scan that
  could not reach Prometheus exits 2. A scan that looked and found nothing
  exits 0 and says which window it actually had.
- **Every number in a finding must be reproducible from its own evidence
  block.** If you add a number, add the evidence that justifies it.

## Getting set up

```sh
git clone https://github.com/ullage-project/ullage
cd ullage
make demo      # runs against a built-in fake cluster, no Kubernetes needed
```

That is the whole setup. ullage has one runtime dependency (`gopkg.in/yaml.v3`);
the Kubernetes and Prometheus clients are hand-rolled precisely so that
contributors are not required to understand `client-go` to fix a rendering bug.

## The loop

```sh
make check     # fmt + vet + race tests. Run this before you push.
make cover     # per-package coverage
make e2e-kind  # a real Kubernetes cluster with fake GPUs (needs kind + docker)
```

`make e2e-kind` is worth the eight minutes if you touched anything in
`internal/scan`, `internal/inventory` or `internal/promql`. Every bug that
shipped past the unit suite so far was caught by a real cluster and not by a
fixture — see `e2e/README.md` for what it does and does not fake.

## Documentation is tested

The transcript on the front page is not pasted, it is asserted. `make check`
re-runs the exact command the README shows and diffs the output, so changing
the explain renderer fails the build until the README is regenerated:

```sh
ULLAGE_DEMO_NOW=2026-08-11T07:00:00Z go run ./cmd/ullage explain research/jupyter-alice --demo
```

`ULLAGE_DEMO_NOW` pins the demo cluster's clock. Without it the demo floats
with wall-clock time, which is right for a human and useless for a document.

Every check also needs a page in `docs/checks/`, because every finding prints a
link to one. The build asserts that the page exists, that it has the sections a
reader needs before acting — including **when this finding is wrong** — and
that no page documents a check that no longer exists.

## Writing a check

A check is one Go file plus its documentation. Implement three methods,
register it in an `init`, and the whole pipeline — ownership resolution,
provenance, grouping, ranking, pricing, suppression, rendering, JSON — applies
to your findings for free.

```go
func init() { check.Register(myCheck{}) }

func (myCheck) Describe() check.Descriptor { ... }

// Applicable reports whether the check can make a claim about a device at all.
// False is not "found nothing": those devices are counted as not-analysed, so
// the output can always tell "clean" apart from "never looked at".
func (myCheck) Applicable(d inventory.Device) bool { ... }

func (myCheck) Run(ctx context.Context, cl *inventory.Cluster, p check.Params) ([]check.RawFinding, error)
```

Three files change, and the tests enforce all three — `go test ./internal/check/`
will tell you if you miss one:

1. `internal/check/<id>.go` — the check itself.
2. `docs/checks/<id>.md` — opening with `# <id>` and carrying the sections
   `## What the finding claims`, `## How it is measured`, `## What it does not
   mean`, `## When this finding is wrong`, `## What to do` and `## Suppressing`,
   plus a copyable `ullage ignore <id>/...` line.
3. `docs/checks/README.md` — a link to the new page.

The "when this finding is wrong" section is not a formality. A check that
cannot describe its own false positives should not be recommending that anyone
delete anything.

Use `internal/check/idlepod.go` as the model. A check returns *raw* findings —
subject, evidence, confidence — and decides nothing about presentation.

> **Checks are in-tree for v0.x.** The check and provider contracts live under
> `internal/`, so they cannot be imported from another module yet. This is
> deliberate: publishing an extension ABI before the third check taught us what
> it should look like would freeze the wrong shape. Promoting these to a public
> package is on the roadmap. Until then, a new check is a pull request here,
> and we would rather have that conversation than have you fork.

## Writing a cloud provider

`internal/scan/fix.go` holds the `Provider` seam — the cloud-specific half of a
remediation command. The core carries no cloud SDK and never will. If you
operate a cloud we render badly, that file is yours to correct.

## Commit messages

Say what changed about the world, not what you did to the files. A message
should let someone six months from now understand why the old behaviour was
wrong. If a change fixes something a user could have hit, describe what they
would have seen.

## Sign-off

The pre-release history of this repository predates this requirement and
carries no sign-offs; it is not retroactively certified, and nothing in CI
enforces the rule yet. It applies from the first public contribution onward,
and the honest reason to follow it is that a provenance rule adopted after a
project has contributors is far harder to adopt than one it starts with.

Commits must carry a `Signed-off-by` line certifying the
[Developer Certificate of Origin](https://developercertificate.org/):

```sh
git commit -s
```

We use DCO rather than a CLA. It is one line, it requires no paperwork, and it
is what most CNCF projects use.

## Code of Conduct

This project follows the [CNCF Code of Conduct](CODE_OF_CONDUCT.md).

## Reporting a security issue

Do not open a public issue. See [SECURITY.md](SECURITY.md).
