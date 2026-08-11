# Security Policy

## Reporting a vulnerability

Please report security issues privately via
[GitHub Security Advisories](https://github.com/ullage-project/ullage/security/advisories/new).
If that link does not work — private reporting has to be enabled on the
repository, and this project is young — email the maintainer listed in
[MAINTAINERS.md](MAINTAINERS.md) at the address on their GitHub profile rather
than assuming the report was received.

Do not open a public issue for a security problem.

ullage is maintained by one person in their own time. That does not come with
a response-time guarantee, and publishing one this project cannot honour would
be worse than saying so: a reporter who is told five working days and hears
nothing has no way to tell a busy maintainer from a lost report. What is
promised instead is that reports are read, that you will be told what is
happening rather than left waiting, that a confirmed vulnerability gets a
disclosure timeline agreed with you, that you will be credited unless you ask
not to be, and that an advisory is published alongside the fix. If a week
passes with no reply, escalate by opening a public issue that says only that
you sent a private report and heard nothing — with no details of the
vulnerability itself.

## Supported versions

While ullage is pre-1.0, only the latest released minor version receives
security fixes.

## What ullage does, and what that means for your threat model

This matters more than the usual boilerplate, because ullage asks for
cluster-wide read access.

**ullage never writes to your cluster.** It has no `create`, `update`, `patch`
or `delete` verb anywhere in `deploy/rbac.yaml`, and no code path that would use
one. Fix commands are *printed*, never executed. If you find a code path that
mutates cluster state, that is a security bug and we want to hear about it.

**ullage reads pod, node, namespace and PodDisruptionBudget metadata**, plus
ResourceClaims and Karpenter NodePools where they exist. It reads object
metadata, spec and status; it does not read Secrets or ConfigMaps, with one
exception: the cluster-autoscaler status ConfigMap in `kube-system`, by name,
which it needs to tell capacity held open on purpose from capacity being wasted.

**Names leak into output.** Findings contain namespace names, pod names,
controller names, node pool names and — where you have annotated them — owner
email addresses. Treat `ullage --output json` the way you would treat
`kubectl get pods -A -o json`.

**The container runs unprivileged.** Distroless base, non-root UID 65532,
read-only root filesystem, all capabilities dropped, `seccompProfile:
RuntimeDefault`. See `deploy/cronjob.yaml`.

**Credentials.** ullage reads your kubeconfig or in-cluster service account
token, and optionally a Prometheus bearer token via `--prometheus-token-file`.
Prefer the file form over `--prometheus-token`: an argument is visible in the
process table to every user on the host.

## Dependencies

ullage has exactly one runtime dependency: `gopkg.in/yaml.v3`. The Kubernetes
and Prometheus clients are implemented in this repository. This is a deliberate
security posture as much as a build-time one — it is a supply chain you can
read in an afternoon.
