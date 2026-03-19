#!/usr/bin/env bash
# End-to-end test runner for k8s-service-proxy.
#
# Builds the Compose stack (proxy + Go test-runner) and runs the tests.
#
# Requirements: docker (compose v2)
#
# Usage:
#   ./tests/e2e/run.sh

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
COMPOSE_FILE="$SCRIPT_DIR/compose.yml"

# ── Pre-flight ──────────────────────────────────────────────────────────────
if ! docker compose version &>/dev/null; then
  echo "ERROR: 'docker compose' (v2 CLI plugin) is required." >&2
  exit 1
fi

# ── Cleanup ─────────────────────────────────────────────────────────────────
cleanup() {
  echo "--- Cleaning up ---" >&2

  docker compose -f "$COMPOSE_FILE" down --volumes --remove-orphans --timeout 30 2>/dev/null || true

  echo "--- Cleanup complete ---" >&2
}
trap cleanup EXIT TERM

# ── Build ───────────────────────────────────────────────────────────────────
echo "=== Building images ===" >&2
docker compose -f "$COMPOSE_FILE" build --pull 2>&1 >&2

# ── Run tests ───────────────────────────────────────────────────────────────
echo "=== Running e2e tests ===" >&2
set +e
docker compose -f "$COMPOSE_FILE" run --rm test-runner 2>&1
EXIT_CODE=$?
set -e

if [ "$EXIT_CODE" -eq 0 ]; then
  echo "=== All e2e tests passed ===" >&2
else
  echo "=== E2E tests FAILED (exit code $EXIT_CODE) ===" >&2
fi

exit "$EXIT_CODE"
