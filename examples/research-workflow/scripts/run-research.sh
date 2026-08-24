#!/usr/bin/env bash
# Submit one research workflow and follow it to completion.
#
# Usage:
#   scripts/run-research.sh "What does the agent runtime market look like in 3 years?"
#
# Env:
#   AGENTOS_CONTROL_API  Control API endpoint (default http://127.0.0.1:8080)
set -euo pipefail

GOAL="${1:?usage: run-research.sh \"<research goal>\"}"
EXAMPLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/..")"
ENDPOINT="${AGENTOS_CONTROL_API:-http://127.0.0.1:8080}"
WORKFLOW_FILE="$EXAMPLE_DIR/workflow/research-workflow.json"
REPO_ROOT="$(cd "$EXAMPLE_DIR/../.." && pwd)"

echo "[run-research] submitting workflow for goal: $GOAL"
REPLY="$(go run "$REPO_ROOT/cmd/agentos" workflow create \
  -endpoint "$ENDPOINT" -file "$WORKFLOW_FILE" -goal "$GOAL" \
  -idempotency-key "research-$(date +%s)-$RANDOM")"
echo "$REPLY"

WORKFLOW_ID="$(printf '%s' "$REPLY" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
if [[ -z "$WORKFLOW_ID" ]]; then
  echo "[run-research] could not parse workflow id from reply; poll manually" >&2
  exit 1
fi

echo "[run-research] workflow $WORKFLOW_ID; polling until terminal"
while true; do
  STATUS_REPLY="$(go run "$REPO_ROOT/cmd/agentos" workflow get -endpoint "$ENDPOINT" -workflow "$WORKFLOW_ID")"
  STATUS="$(printf '%s' "$STATUS_REPLY" | sed -n 's/.*"status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  printf '[run-research] status=%s\r' "${STATUS:-PENDING}"
  case "$STATUS" in
    SUCCEEDED|FAILED|CANCELLED)
      echo
      echo "[run-research] terminal status=$STATUS workflow=$WORKFLOW_ID"
      echo "$STATUS_REPLY"
      exit 0
      ;;
  esac
  sleep 2
done
