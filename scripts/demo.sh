#!/usr/bin/env bash
# Walks through the module lifecycle against a running core.
#   terminal 1: ./bin/amp serve --embedded-db          (no --dev-workers: this script starts the workloads itself)
#   terminal 2: ./scripts/demo.sh                        (keep the console open at http://localhost:8080 to watch it change)
# Set AMP_SERVER to point at another replica, e.g. http://localhost:8081.
set -euo pipefail
cd "$(dirname "$0")/.."
AMP=${AMP:-./bin/amp}
WORKER=${WORKER:-./bin/amp-eval-worker}
S=${AMP_SERVER:-http://localhost:8080}
export AMP_SERVER=$S
PAUSE=${PAUSE:-0}
PIDS=()
trap 'for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done' EXIT

step() { printf '\n\033[1;33m== %s\033[0m\n' "$*"; [ "$PAUSE" = 1 ] && read -r -p "(enter) " _ || true; }
json() { python3 -c 'import sys,json; print(json.dumps(json.load(sys.stdin), indent=2))'; }
runs() {
  curl -sf "$S/m/evaluation/runs?agentId=$1" | python3 -c '
import sys, json
for r in json.load(sys.stdin):
    print("  %s  %-8s  %-7s  %-9s  scored=%s  avg=%s  %s" % (r["id"][:8], r["lane"], r["evaluator"], r["status"],
          r["evaluated"], r["average"], (r["message"] or r["evaluatedBy"])[:70]))'
}

step "Core only: no modules installed"
$AMP module list

step "Register an agent in the core (before any module exists)"
curl -sf -XPOST "$S/api/agents" -d '{"name":"support-bot","description":"Answers tickets"}' | json

step "Install evaluation: refused, it requires traces.query/v1"
$AMP module install evaluation || true

step "Install evaluation with its dependencies"
$AMP module install evaluation --with-deps

step "evaluation is active but degraded: its workloads run outside the core and none is connected yet"
$AMP module health evaluation

step "Install ai-gateway"
$AMP module install ai-gateway

step "The core create-agent API now accepts an ai-gateway extension, validated by the module's hook"
curl -s -XPOST "$S/api/agents" -d '{"name":"bad-bot","extensions":{"ai-gateway":{"provider":"openai","model":"nope"}}}' | json
ID=$(curl -sf -XPOST "$S/api/agents" -d '{"name":"billing-bot","extensions":{"ai-gateway":{"provider":"anthropic","model":"claude-sonnet-4-5"}}}' | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
sleep 0.5
curl -sf "$S/m/ai-gateway/bindings/$ID" | json

step "Queue a custom-code evaluation: it goes to the isolated lane and waits (no workloads yet)"
python3 - "$ID" > /tmp/amp-custom-run.json <<'EOF'
import json, sys
code = '''import os
def evaluate(trace):
    leaked = [k for k in os.environ if "TOKEN" in k or "CRED" in k]
    if not trace["output"]:
        return {"score": 0.0, "reason": "no answer"}
    v = judge("Is this helpful? " + trace["output"])
    return {"score": v["score"] or 0.0, "reason": "judge %s/%s, credentials visible: %d" % (v.get("provider"), v.get("model"), len(leaked))}
'''
print(json.dumps({"agentId": sys.argv[1], "evaluator": "custom", "code": code}))
EOF
curl -sf -XPOST "$S/m/evaluation/runs" -d @/tmp/amp-custom-run.json > /dev/null
runs "$ID"

step "Start the workloads as separate processes: eval-controller and shared-worker"
CTRL=$($AMP module credential evaluation eval-controller | sed -n 2p | tr -d ' ')
POOL=$($AMP module credential evaluation shared-worker | sed -n 2p | tr -d ' ')
AMP_WORKLOAD_CREDENTIAL=$CTRL $WORKER controller --core "$S" --instance demo-ctrl > /tmp/amp-ctrl.log 2>&1 & PIDS+=($!)
AMP_WORKLOAD_CREDENTIAL=$POOL $WORKER shared-worker --core "$S" --instance demo-pool > /tmp/amp-pool.log 2>&1 & PIDS+=($!)
sleep 5
$AMP module health evaluation

step "Shared lane: built-in evaluators, judged through the AI Gateway. Isolated lane: the custom code ran in its own process, with no credentials"
runs "$ID"
curl -sf "$S/m/evaluation/scores?agentId=$ID" | python3 -c '
import sys, json
for s in json.load(sys.stdin): print("  %.2f  %-45s  %s" % (s["score"], s["evaluator"][:45], s["reason"]))'

step "Permissions come from modules too: a viewer may read scores but not run evaluations"
curl -s -H 'X-AMP-Role: viewer' "$S/api/me" | json
curl -s -H 'X-AMP-Role: viewer' -XPOST "$S/m/evaluation/runs" -d "{\"agentId\":\"$ID\"}" | json

step "Workload routes take core-issued tokens only, not user sessions"
curl -s -XPOST "$S/m/evaluation/internal/leases" -d '{"lane":"isolated"}' | json

step "observability cannot be disabled while evaluation needs it"
$AMP module disable observability || true

step "Disable ai-gateway: its routes go away, evaluation falls back to the heuristic judge"
$AMP module disable ai-gateway
curl -s "$S/m/ai-gateway/providers" | json
curl -sf "$S/m/evaluation/judge?agentId=$ID" | json
$AMP module enable ai-gateway

step "Delete the agent: each module's finalize hook removes its own data"
curl -sf -o /dev/null -w 'HTTP %{http_code}\n' -XDELETE "$S/api/agents/$ID"
sleep 0.3
curl -s "$S/m/ai-gateway/bindings/$ID" | json
curl -sf "$S/m/observability/traces?agentId=$ID" | json

step "Uninstall keeps the data; --purge drops the schema"
$AMP module uninstall ai-gateway
$AMP module install ai-gateway
$AMP module uninstall ai-gateway --purge
$AMP module list

printf '\nDone (the workloads stop with this script). Open %s to see the console.\n' "$S"
