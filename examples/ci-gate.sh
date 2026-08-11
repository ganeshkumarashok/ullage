#!/usr/bin/env bash
# Use ullage as a pipeline gate.
#
# The naive version -- "fail if there are any findings" -- fails on day one and
# stays red, which trains everyone to ignore it. This gates on the thing that
# is actually actionable: waste above a budget you chose, at a confidence you
# trust, excluding anything the team has already decided about.
#
#   ./examples/ci-gate.sh                      against the demo cluster
#   PROMETHEUS=http://prom:9090 ./examples/ci-gate.sh   against a real one
#
# Environment:
#   BUDGET_USD   fail above this much wasted spend per window (default 500)
#   PROMETHEUS   metrics endpoint; omit to run against the built-in demo
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ULLAGE="${ULLAGE:-$ROOT/bin/ullage}"
BUDGET_USD="${BUDGET_USD:-500}"

command -v jq >/dev/null || { echo "this example needs jq"; exit 2; }
[ -x "$ULLAGE" ] || (cd "$ROOT" && make build >/dev/null)

# --exit-zero because this script decides what is a failure, not the tool. Note
# that we still have to tell a scan that found nothing apart from a scan that
# could not run: the first is good news, the second is a broken exporter, and a
# gate that treats them alike is worse than no gate.
set +e
report="$("$ULLAGE" ${PROMETHEUS:+--prometheus "$PROMETHEUS"} ${PROMETHEUS:-demo} \
            --output json --exit-zero --min-confidence high 2>/dev/null)"
status=$?
set -e

if [ $status -ne 0 ] || [ -z "$report" ]; then
  echo "ullage could not complete a scan — failing rather than reporting a clean cluster"
  "$ULLAGE" doctor ${PROMETHEUS:+--prometheus "$PROMETHEUS"} || true
  exit 2
fi

# Suppressed findings are deliberately excluded: someone already made that call,
# with a reason and an expiry, and re-litigating it every build is how a gate
# becomes noise. Findings under "byDesign" are excluded for the same reason.
wasted=$(jq '[.recommendations[].impact.windowCost // 0] | add // 0' <<<"$report")
count=$(jq '.recommendations | length' <<<"$report")
window=$(jq -r '.scan.window | sub("^P";"") | sub("D$";"d")' <<<"$report")

printf '\n%s findings above high confidence, $%.0f wasted over %s (budget $%s)\n\n' \
  "$count" "$wasted" "$window" "$BUDGET_USD"

jq -r '.recommendations[]
       | "  \(.impact.windowCost // 0 | floor | tostring | "$" + .)\t\(.id)\t\(.owner.identity // "unowned")"' \
   <<<"$report" | column -t -s $'\t' 2>/dev/null || true

over=$(jq -n --argjson w "$wasted" --argjson b "$BUDGET_USD" '$w > $b')
if [ "$over" = "true" ]; then
  cat <<EOF

FAIL: \$$(printf '%.0f' "$wasted") of idle accelerator spend exceeds the \$$BUDGET_USD budget.

  Investigate one with:   ullage explain <id>
  Disagree with one with: ullage ignore <id> --reason "..." --until YYYY-MM-DD

  A suppression is committed to .ullage.yaml, so the decision is reviewable in
  the same place as the code.
EOF
  exit 1
fi

echo
echo "PASS: idle accelerator spend is within budget."
