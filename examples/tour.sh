#!/usr/bin/env bash
# A guided tour of what ullage does and why it is different, in about two
# minutes, with no Kubernetes cluster and no GPU.
#
# Everything below runs against a built-in fake cluster: a real API server and a
# real Prometheus, both served from memory. The numbers are computed, not
# canned, by exactly the code a real scan runs.
#
#   ./examples/tour.sh          run it
#   PAUSE=0 ./examples/tour.sh  no pauses, for CI
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ULLAGE="${ULLAGE:-$ROOT/bin/ullage}"
# GNU and BSD mktemp agree on one form only: an explicit template ending in
# at least six X's. BSD's -t takes a bare prefix and GNU's rejects it.
CONFIG="$(mktemp "${TMPDIR:-/tmp}/ullage-tour.XXXXXX")"
trap 'rm -f "$CONFIG"' EXIT

if [ ! -x "$ULLAGE" ]; then
  echo "building ullage..."
  (cd "$ROOT" && make build >/dev/null)
fi

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  B=$'\033[1m'; DIM=$'\033[2m'; C=$'\033[36m'; R=$'\033[0m'
else
  B=""; DIM=""; C=""; R=""
fi

PAUSE="${PAUSE:-1}"
step() {
  printf '\n%s\n' "${B}$1${R}"
  printf '%s\n' "${DIM}$2${R}"
  [ "$PAUSE" = "0" ] || sleep 1
}
# ullage exits 1 when it finds something, which is the whole point of the
# tour, so a plain `set -e` would stop at the first step. Only the documented
# codes are tolerated: 0 nothing found, 1 findings present. Anything else --
# 2 for a failed scan, 127 for a missing binary, 139 for a crash -- is a real
# failure, and swallowing it would let the tour "pass" while printing nothing.
run() {
  printf '\n%s$ %s%s\n\n' "$C" "$*" "$R"
  local code=0
  "$@" || code=$?
  if [ "$code" -gt 1 ]; then
    printf '\n%s failed with exit code %d\n' "$1" "$code" >&2
    exit "$code"
  fi
  [ "$PAUSE" = "0" ] || sleep 1
}

printf '\n%s\n%s\n' "${B}ullage — a two-minute tour${R}" \
  "${DIM}No cluster required. Everything runs against a built-in fake cluster.${R}"

step "1. Find what the cluster paid for and did not use" \
"    Ranked by money, because that is the order anyone will actually work
    through it. Pass --no-cost to rank by accelerator-hours instead."
run "$ULLAGE" demo

step "2. Ask why" \
"    A recommendation nobody can check is a recommendation nobody will act on.
    Every number above is reproducible from the evidence below."
run "$ULLAGE" explain --demo idle-pod/research/jupyter-alice

step "3. Notice what it refused to do" \
"    Three things in that output are the whole point.

      · The fix targets the StatefulSet, not the pods. Deleting the pods frees
        nothing — the controller recreates them within seconds. When the root
        owner is a CRD ullage does not recognise, it names the resource and
        emits no command at all.

      · 16 accelerators appear under 'fallow by design', with no fix attached.
        They are held open by a cluster-autoscaler minimum. Capacity reserved
        on purpose is not waste, and a tool that cannot tell the difference
        gets uninstalled the first time it confuses them.

      · 8 accelerators are listed as not analysed, by name and with a reason.
        A time-sliced or MIG device reports utilization for every co-tenant, so
        an idle pod sharing one with a busy pod is invisible. The census always
        reconciles: analysed + excluded = observed."

step "4. Disagree with it, durably" \
"    Suppose the h100 reserve is deliberate and the platform team knows it.
    Record that so it stops competing for attention — with a reason, and an
    expiry, so the suppression itself gets reviewed rather than forgotten."
run "$ULLAGE" ignore unused-node/pool/h100-reserve \
      --reason "reserved for the Q4 training run; revisit at year end" \
      --until 2026-12-31 \
      --config "$CONFIG"
printf '\n%s\n' "${DIM}    $CONFIG now contains:${R}"
sed 's/^/    /' "$CONFIG"

step "5. Feed it to something else" \
"    The JSON is a contract, not a debug dump: pkg/ullage/api round-trips it,
    and a test compares re-marshalled bytes so a field cannot quietly vanish."
run bash -c "'$ULLAGE' demo --output json 2>/dev/null | jq '{
    fallowHours: .scan.gpuHoursFallow,
    worst: (.recommendations[0] | {id, owner: .owner.identity, cost: .impact.windowCost, fix: .fix.command})
  }'"

step "6. Gate a pipeline on it" \
"    Exit 1 means findings, exit 2 means the scan could not be completed. The
    difference matters: a dead exporter must never read as a clean cluster."
run bash -c "'$ULLAGE' demo >/dev/null 2>&1; echo \"exit \$?  — findings present\""
run bash -c "'$ULLAGE' demo --exit-zero >/dev/null 2>&1; echo \"exit \$?  — --exit-zero, for reporting rather than gating\""

cat <<DONE

${B}That was the tour.${R}

  Against your own cluster:

    ${C}ullage doctor --prometheus http://<your-prometheus>:9090${R}
        checks every prerequisite and names anything missing

    ${C}ullage --prometheus http://<your-prometheus>:9090${R}
        the same scan, against real data

  More examples here:

    ${C}examples/ci-gate.sh${R}         fail a pipeline when waste appears
    ${C}examples/weekly-digest.sh${R}   turn a scan into a report you can send

  No Prometheus to point at? ${C}make e2e-kind${R} builds a throwaway cluster
  with fake GPU capacity and a synthetic exporter, and runs the whole thing
  against real Kubernetes.

DONE
