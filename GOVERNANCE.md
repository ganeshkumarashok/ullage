# Governance

ullage is a young project. This document describes how it is run today, and is
deliberately modest: inventing a large governance structure before there is a
community to govern is theatre.

## Roles

**Maintainers** have write access, review and merge pull requests, and are
responsible for releases. They are listed in [MAINTAINERS.md](MAINTAINERS.md).

**Contributors** are anyone who opens an issue or a pull request. No agreement
beyond the [DCO](CONTRIBUTING.md#sign-off) is required.

## Decisions

Day-to-day changes are decided by pull request review: one maintainer approval
merges, and any maintainer may request changes.

Decisions that change what ullage *claims* — a new check, a change to how
confidence is assigned, a change to when a device is excluded rather than
judged, or any change to the JSON contract — require agreement from two
maintainers, or from the only maintainer plus a documented rationale in the
pull request. These are the decisions that determine whether the tool can be
trusted, and they are held to a higher bar than code quality.

Disagreements are resolved by discussion in the issue or pull request. If that
fails, the maintainers decide by simple majority; a tie means no change.

## Becoming a maintainer

Someone who has made sustained, substantive contributions — and who has
demonstrated the judgement described in
[CONTRIBUTING.md](CONTRIBUTING.md#before-anything-else-the-standard-of-evidence)
— may be nominated by an existing maintainer and confirmed by the rest.

There is no minimum contribution count. What matters is whether we would trust
your review of a change to what ullage claims about someone's cluster.

## Stepping down

Maintainers who have been inactive for six months may be moved to emeritus by
the others. This is administrative, not a judgement, and returning is a matter
of asking.

## Releases

Any maintainer may cut a release by pushing a `v*` tag, which triggers
`.github/workflows/release.yaml`. Pre-1.0, minor versions may change the JSON
contract; when they do, `api.Version` changes with them and the change is called
out in [CHANGELOG.md](CHANGELOG.md).

## Changing this document

By pull request, with agreement from all current maintainers.
