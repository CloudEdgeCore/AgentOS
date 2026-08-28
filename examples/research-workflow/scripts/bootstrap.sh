#!/usr/bin/env bash
# Bootstrap the AgentOS control/data plane for the multi-agent research
# workflow: migrations, gateway, controllers, orchestrator (with the dynamic
# spawn service), runtime adapter fleet, the research agent host, and the
# application-layer research API (design doc §13).
#
# Prerequisites:
#   - PostgreSQL reachable at $DATABASE_URL (schema migrations are applied)
#   - Go toolchain to build cmd/ binaries
#   - A research agent runtime listening on $AGENT_ENDPOINT speaking
#     agentos.adapter-http/v1 (see examples/research-workflow/runtime/cmd)
#
# Everything is idempotent; re-running repairs a partially started stack.
#
# Note on tools: the local gateway runs in -dev-mode, whose Tool Gateway uses
# the in-process development executor (tools echo their arguments). The real
# webhook-backed web.search/web.fetch/citation.check tools are exercised by
# the e2e harness, which mounts the production WebhookExecutor. Running the
# gateway without -dev-mode requires SPIFFE mTLS, OpenBao, and an embedding
# endpoint (see cmd/agentos-gateway).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
EXAMPLE_DIR="$REPO_ROOT/examples/research-workflow"
RUN_DIR="${AGENTOS_RUN_DIR:-/tmp/agentos-research}"
LOG_DIR="$RUN_DIR/logs"
mkdir -p "$LOG_DIR"

: "${DATABASE_URL:?set DATABASE_URL to the PostgreSQL connection URL}"
TENANT_ID="${TENANT_ID:-research-tenant}"
GATEWAY_LISTEN="${GATEWAY_LISTEN:-127.0.0.1:9091}"
CONTROL_LISTEN="${CONTROL_LISTEN:-127.0.0.1:9092}"
SPAWN_LISTEN="${SPAWN_LISTEN:-127.0.0.1:9094}"
RESEARCH_API_LISTEN="${RESEARCH_API_LISTEN:-127.0.0.1:9095}"
ADAPTER_ENDPOINT="${ADAPTER_ENDPOINT:?set ADAPTER_ENDPOINT to the research runtime HTTP endpoint}"
WORKER_COUNT="${WORKER_COUNT:-3}"
ARTIFACT_ROOT="${ARTIFACT_ROOT:-$RUN_DIR/artifacts}"
mkdir -p "$ARTIFACT_ROOT"

echo "[bootstrap] building binaries"
(cd "$REPO_ROOT" && go build -o "$RUN_DIR" \
  ./cmd/agentos-gateway ./cmd/agentos-controller ./cmd/agentos-orchestrator \
  ./cmd/agentos-control ./cmd/agentos-outbox ./cmd/agentos-runtime-adapter \
  ./cmd/agentos-migrate \
  ./examples/research-workflow/app/cmd/research-api)

echo "[bootstrap] applying migrations"
"$RUN_DIR/agentos-migrate" -database-url "$DATABASE_URL"

CONFIG="$EXAMPLE_DIR/config"
BIN="$RUN_DIR"

launch() { # name, command...
  local name="$1"; shift
  if pgrep -f "agentos[-]$name" > /dev/null 2>&1; then
    echo "[bootstrap] $name already running"
    return
  fi
  nohup "$@" > "$LOG_DIR/$name.log" 2>&1 &
  echo "[bootstrap] started $name (pid $!, log $LOG_DIR/$name.log)"
}

launch gateway "$BIN/agentos-gateway" \
  -database-url "$DATABASE_URL" \
  -listen "$GATEWAY_LISTEN" \
  -tenant "$TENANT_ID" \
  -dev-mode \
  -tenant-policies "$CONFIG/research-policy.json" \
  -model-providers "$CONFIG/model-providers.json" \
  -tool-endpoints "$CONFIG/tool-endpoints.json"

launch control "$BIN/agentos-control" \
  -database-url "$DATABASE_URL" -listen "$CONTROL_LISTEN"

launch orchestrator "$BIN/agentos-orchestrator" \
  -database-url "$DATABASE_URL" \
  -orchestrator-id "research-orchestrator-$(hostname)" \
  -artifact-root "$ARTIFACT_ROOT" \
  -listen "$SPAWN_LISTEN"

launch controller "$BIN/agentos-controller" \
  -database-url "$DATABASE_URL" \
  -controller-id "research-controller-$(hostname)" \
  -runtime-pools "$CONFIG/runtime-pools.json" \
  -tenant-policies "$CONFIG/research-policy.json" \
  -admission-runtime-classes "research-reasoning,research-network,research-sandbox,research-remote"

launch outbox "$BIN/agentos-outbox" -database-url "$DATABASE_URL"

for index in $(seq 0 $((WORKER_COUNT - 1))); do
  instance="$(printf 'research-worker-%02d' "$index")"
  launch "runtime-adapter-$instance" "$BIN/agentos-runtime-adapter" \
    -control-address "$CONTROL_LISTEN" \
    -gateway-address "$GATEWAY_LISTEN" \
    -mcp-listen "127.0.0.1:$((9100 + index))" \
    -runtime-bindings "$CONFIG/runtime-bindings.json" \
    -tenant "$TENANT_ID" \
    -runtime-instance-id "$instance" \
    -artifact-root "$ARTIFACT_ROOT"
done

launch research-api "$BIN/research-api" \
  -listen "$RESEARCH_API_LISTEN" \
  -control-endpoint "http://$CONTROL_LISTEN" \
  -workflow-template "$EXAMPLE_DIR/workflow/research-workflow.json" \
  -artifact-root "$ARTIFACT_ROOT" \
  -tenant "$TENANT_ID"

echo "[bootstrap] done. Next: scripts/publish-agents.sh, then:"
echo "  scripts/run-research.sh \"<goal>\""
echo "  (or: agentos research --endpoint http://$RESEARCH_API_LISTEN --goal \"<goal>\")"
