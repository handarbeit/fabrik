#!/usr/bin/env bash
# scripts/e2e/reset.sh — clear state from prior e2e runs to a clean slate.
#
# A thin shim over the Go gate runner's `reset` subcommand (tests/gate/reset.go,
# #1994, ADR-1994) — the behaviour is unchanged:
#
#   scripts/e2e/reset.sh             # close every open PR (deleting its branch) and issue in
#                                    # alpha + beta, delete leftover fabrik/* branches, drain the board
#   scripts/e2e/reset.sh --worktrees # ALSO remove Fabrik's worktrees + bare clones from the bed
#                                    # (destructive; refuses while the bed engine is running)
#   scripts/e2e/reset.sh --bed <dir> # reset only that bed; without it, every E2E_BEDS bed is
#                                    # reset with its own token, repos and board (#1976)
#
# Overridable via env: FABRIK_TEST_DIR, FABRIK_TEST_REPO_ALPHA, FABRIK_TEST_REPO_BETA,
# FABRIK_TEST_PROJECT_OWNER, FABRIK_TEST_PROJECT_NUMBER.
#
# Run it against a STOPPED bed (the run.sh preflight leaves it stopped for exactly
# this reason); see tests/e2e/README.md's "Reset between runs".

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

BIN_DIR="${E2E_GATE_BIN_DIR:-${TMPDIR:-/tmp}/fabrik-e2e-gate-$(id -u)}"
mkdir -p "$BIN_DIR"

TMP_BIN="$BIN_DIR/gate.$$"
if ! go build -o "$TMP_BIN" ./tests/gate/cmd/gate; then
  rm -f "$TMP_BIN"
  echo "scripts/e2e/reset.sh: building the gate runner (./tests/gate/cmd/gate) failed" >&2
  exit 1
fi
mv -f "$TMP_BIN" "$BIN_DIR/gate"

exec "$BIN_DIR/gate" reset "$@"
