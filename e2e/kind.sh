#!/usr/bin/env bash
#
# Stand up a complete ullage test environment on a local kind cluster, scan it,
# and assert the scan says what it should.
#
# Everything is real except the GPUs: a real Kubernetes API, a real Prometheus,
# real pods holding real extended resources, and a fake dcgm-exporter whose
# readings we dictate. That combination is what found five bugs the unit suite
# and the demo fixture both missed.
#
#   ./e2e/kind.sh up      create the cluster, deploy, scan, assert
#   ./e2e/kind.sh scan    re-run the scan against a cluster already up
#   ./e2e/kind.sh down    delete the cluster
#
# Requires: kind, kubectl, docker, go. No cloud account, no GPU, no money.

set -euo pipefail

CLUSTER="${ULLAGE_E2E_CLUSTER:-ullage-e2e}"
NS=ullage-e2e
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(dirname "$HERE")"
BIN="$ROOT/bin/ullage"
PORT="${ULLAGE_E2E_PORT:-19090}"

# The analysis window for the E2E scan. It is short because the script has to
# wait out a real Prometheus filling it -- ullage will not judge a window it
# only partly saw, which is the whole point of the tool and also the reason
# this cannot be hurried.
WINDOW="${ULLAGE_E2E_WINDOW:-3m}"
IDLE="${ULLAGE_E2E_IDLE:-1m}"
STEP="${ULLAGE_E2E_STEP:-15s}"
SCRAPE_SECONDS=5   # must match scrape_interval in prom.yaml

kube() { kubectl --context "kind-${CLUSTER}" "$@"; }
say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required but not installed. $2"
}

preflight() {
  need kind   "See https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
  need kubectl "See https://kubernetes.io/docs/tasks/tools/"
  need go     "See https://go.dev/dl/"
  docker info >/dev/null 2>&1 || fail "docker is not running; kind needs it."
}

up() {
  preflight

  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    say "cluster ${CLUSTER} already exists, reusing it"
  else
    say "creating kind cluster ${CLUSTER} (two nodes)"
    kind create cluster --name "$CLUSTER" --config=- <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
YAML
  fi

  # The workloads and the exporter both need to agree on which node is "node
  # zero", and kind names its nodes for us rather than the other way round.
  #
  # Deliberately not `mapfile`: macOS still ships bash 3.2, and a contributor
  # on a Mac is the most likely person to run this.
  workers="$(kube get nodes \
    -l '!node-role.kubernetes.io/control-plane' \
    -o jsonpath='{range .items[*]}{.metadata.name} {end}')"
  set -- $workers
  [ "$#" -ge 2 ] || fail "expected two worker nodes, found $#"
  node0="$1"
  say "node zero is ${node0}"

  kube create namespace "$NS" --dry-run=client -o yaml | kube apply -f -

  say "advertising fake GPUs on the workers"
  # Kubernetes has no way to fake a device plugin cheaply, but the scheduler
  # only cares about the node's advertised capacity. Kubelet copies capacity
  # into allocatable within about twenty seconds.
  for n in $workers; do
    kube patch node "$n" --subresource=status --type=json \
      -p='[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"2"}]' >/dev/null
  done

  say "deploying the fake dcgm-exporter and Prometheus"
  sed "s/__NODE0__/${node0}/g" "$HERE/exporter.yaml" | kube apply -f -
  kube apply -f "$HERE/prom.yaml"

  say "waiting for the exporter and Prometheus"
  kube -n "$NS" rollout status daemonset/dcgm-exporter --timeout=180s
  kube -n "$NS" rollout status deployment/prometheus --timeout=180s

  say "waiting for allocatable GPUs to appear"
  for _ in $(seq 1 30); do
    got="$(kube get node "$node0" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}' 2>/dev/null || true)"
    [ "$got" = "2" ] && break
    sleep 2
  done
  [ "${got:-}" = "2" ] || fail "kubelet never advertised allocatable GPUs on ${node0}"

  say "scheduling the workloads"
  sed "s/__NODE0__/${node0}/g" "$HERE/workloads.yaml" | kube apply -f -
  kube -n ml wait --for=condition=Ready pod/llama-train-0 pod/idle-notebook-0 --timeout=180s

  scan
}

# Read a single scalar out of an instant query.
promq() {
  curl -sf -m 5 -G "http://127.0.0.1:${PORT}/api/v1/query" \
    --data-urlencode "query=$1" 2>/dev/null |
    python3 -c 'import json,sys
try:
    r = json.load(sys.stdin)["data"]["result"]
except Exception:
    r = []
print(r[0]["value"][1] if r else "0")' 2>/dev/null || echo 0
}

# Wait until Prometheus actually holds a full window of readings for the pod
# the assertions are about.
#
# This replaces a fixed sleep. ullage deliberately refuses to call a GPU idle
# on a window it only partly observed, so scanning too early does not produce
# a wrong answer -- it produces no answer, and the assertions below would fail
# for a reason that has nothing to do with the code under test.
await_coverage() {
  local want=$(( $(seconds_in "$WINDOW") / SCRAPE_SECONDS ))
  want=$(( want * 9 / 10 ))
  say "waiting for ${WINDOW} of readings (need ~${want} samples per series)"
  echo "    ullage will not judge a window it only partly saw, so this cannot be hurried."

  local deadline=$(( $(date +%s) + 420 ))
  local got=0
  while [ "$(date +%s)" -lt "$deadline" ]; do
    got="$(promq "min(count_over_time(DCGM_FI_DEV_GPU_UTIL{pod=\"idle-notebook-0\"}[${WINDOW}]))")"
    got="${got%%.*}"
    [ -n "$got" ] || got=0
    if [ "$got" -ge "$want" ]; then
      printf '    %s samples, ready.\n' "$got"
      return 0
    fi
    printf '\r    %s/%s samples ' "$got" "$want"
    sleep 5
  done
  fail "Prometheus only reached ${got}/${want} samples for idle-notebook-0. Is the exporter scraping?"
}

seconds_in() {
  case "$1" in
    *h) echo $(( ${1%h} * 3600 )) ;;
    *m) echo $(( ${1%m} * 60 )) ;;
    *s) echo "${1%s}" ;;
    *)  echo "$1" ;;
  esac
}

scan() {
  command -v kubectl >/dev/null || fail "kubectl is required"
  say "building ullage"
  (cd "$ROOT" && go build -o "$BIN" ./cmd/ullage)

  say "port-forwarding Prometheus to :${PORT}"
  kube -n "$NS" port-forward svc/prometheus "${PORT}:9090" >/dev/null 2>&1 &
  local pf=$!
  trap 'kill '"$pf"' 2>/dev/null || true' EXIT
  for _ in $(seq 1 30); do
    curl -sf -m 2 "http://127.0.0.1:${PORT}/-/ready" >/dev/null 2>&1 && break
    sleep 1
  done

  await_coverage

  say "ullage doctor"
  "$BIN" doctor --prometheus "http://127.0.0.1:${PORT}" || true

  say "ullage scan"
  local args=(--prometheus "http://127.0.0.1:${PORT}"
              --window "$WINDOW" --idle-threshold "$IDLE" --step "$STEP")
  "$BIN" "${args[@]}" --exit-zero || true

  say "checking the scan says what it should"
  local json
  json="$("$BIN" "${args[@]}" --output json --quiet --exit-zero)"
  printf '%s' "$json" | python3 -c '
import json, sys
r = json.load(sys.stdin)
ids = [f["id"] for f in r["recommendations"]]
problems = []

# The idle pod must be found. This is the whole product.
if not any("idle-notebook-0" in i for i in ids):
    problems.append("ml/idle-notebook-0 holds a GPU that has read zero throughout "
                    "and was NOT reported. findings=%s" % ids)

# The busy pod must not be. A GPU at 78%% utilization being called idle is the
# bug that shipped past every unit test until a real cluster caught it.
if any("llama-train-0" in i for i in ids):
    problems.append("ml/llama-train-0 is at 78%% utilization and WAS reported as idle")

# The census has to reconcile or none of the numbers mean anything.
s = r["scan"]
excl = sum(e["accelerators"] for e in r["notAnalyzed"])
if s["acceleratorsAnalyzed"] + excl != s["acceleratorsObserved"]:
    problems.append("census does not reconcile: %d analysed + %d excluded != %d observed"
                    % (s["acceleratorsAnalyzed"], excl, s["acceleratorsObserved"]))

# Attribution must be per-node, or a namesake elsewhere inflates the holding.
for f in r["recommendations"]:
    if "idle-notebook-0" in f["id"]:
        n = sum(a["count"] for a in f["accelerators"])
        if n != 1:
            problems.append("idle-notebook-0 reported as holding %d GPUs, it holds 1" % n)

if problems:
    for p in problems:
        print("  FAIL: " + p)
    sys.exit(1)
print("  idle pod reported, busy pod not reported, census reconciles, attribution correct")
' || fail "the scan did not say what it should"

  printf '\n\033[32mE2E PASSED\033[0m\n'
  printf 'Prometheus is still forwarded to http://127.0.0.1:%s while this script runs.\n' "$PORT"
  printf 'Tear the cluster down with: ./e2e/kind.sh down\n'
}

down() {
  say "deleting kind cluster ${CLUSTER}"
  kind delete cluster --name "$CLUSTER"
}

case "${1:-up}" in
  up)   up ;;
  scan) scan ;;
  down) down ;;
  *)    fail "usage: $0 [up|scan|down]" ;;
esac
