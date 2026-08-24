#!/usr/bin/env bash
# Stop the research deployment's background processes (gateway, control,
# orchestrator, controller, outbox, runtime adapters). Data in PostgreSQL is
# preserved; drop the schema manually if you also want a data wipe.
set -euo pipefail

echo "[cleanup] stopping AgentOS research processes"
for pattern in \
  agentos-gateway agentos-control$ agentos-orchestrator \
  agentos-controller agentos-outbox agentos-runtime-adapter; do
  pkill -f "$pattern" 2>/dev/null && echo "  stopped $pattern" || true
done
echo "[cleanup] done"
