#!/usr/bin/env bash
# Walks through the module lifecycle against a running core.
#   terminal 1: ./bin/amp serve --embedded-db
#   terminal 2: ./scripts/demo.sh            (keep the console open at http://localhost:8080 to watch it change)
# Set AMP_SERVER to point at another replica, e.g. http://localhost:8081.
set -euo pipefail
cd "$(dirname "$0")/.."
AMP=${AMP:-./bin/amp}
S=${AMP_SERVER:-http://localhost:8080}
export AMP_SERVER=$S
PAUSE=${PAUSE:-0}

step() { printf '\n\033[1;33m== %s\033[0m\n' "$*"; [ "$PAUSE" = 1 ] && read -r -p "(enter) " _ || true; }
json() { python3 -c 'import sys,json; print(json.dumps(json.load(sys.stdin), indent=2))'; }

step "Core only: no modules installed"
$AMP module list

step "Register an agent in the core (before any module exists)"
curl -sf -XPOST "$S/api/agents" -d '{"name":"support-bot","description":"Answers tickets"}' | json

step "Install evaluation: refused, it requires traces.query/v1"
$AMP module install evaluation || true

step "Install evaluation with its dependencies"
$AMP module install evaluation --with-deps

step "observability caught up: traces were backfilled for the agent created earlier, and evaluation scored them"
curl -sf "$S/m/evaluation/scores" | json | head -20

step "Install ai-gateway"
$AMP module install ai-gateway

step "The core create-agent API now accepts an ai-gateway extension, validated by the module's hook"
curl -s -XPOST "$S/api/agents" -d '{"name":"bad-bot","extensions":{"ai-gateway":{"provider":"openai","model":"nope"}}}' | json
ID=$(curl -sf -XPOST "$S/api/agents" -d '{"name":"billing-bot","extensions":{"ai-gateway":{"provider":"anthropic","model":"claude-sonnet-4-5"}}}' | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
sleep 0.5
curl -sf "$S/m/ai-gateway/bindings/$ID" | json

step "evaluation now uses its optional dependency (llm.proxy/v1) as the judge"
curl -sf -XPOST "$S/m/evaluation/runs" -d "{\"agentId\":\"$ID\"}" | json

step "Permissions come from modules too: a viewer may read scores but not run evaluations"
curl -s -H 'X-AMP-Role: viewer' "$S/api/me" | json
curl -s -H 'X-AMP-Role: viewer' -XPOST "$S/m/evaluation/runs" -d "{\"agentId\":\"$ID\"}" | json

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

printf '\nDone. Open %s to see the console; run "./bin/amp module install ai-gateway" again and watch the UI change.\n' "$S"
