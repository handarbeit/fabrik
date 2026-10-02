#!/usr/bin/env bash
# scripts/e2e/run.sh — entry point for the Fabrik end-to-end integration suite.
#
# This is a thin shim. The gate runner is the Go program in tests/gate
# (#1994, ADR-1994): it builds here and is exec'd, so it — not a `go run`
# wrapper — receives INT/TERM and reaps every process it started. Everything
# run.sh used to do (preconditions, pre-gate, bed preflight, the auth × train
# legs, hang protection, the exit-code contract scripts/cut-release.sh keys on)
# lives there; see tests/gate/README.md and tests/e2e/README.md. Its flags and
# environment variables are unchanged:
#
#   scripts/e2e/run.sh                       # full gate: off then on, under pat then app auth
#   scripts/e2e/run.sh --clean               # reset boards/PRs/branches first (must be the first argument)
#   scripts/e2e/run.sh --resume              # run only the (test, leg) pairs the per-SHA coverage ledger still lacks (#1972)
#   scripts/e2e/run.sh --clean --resume      # both, in either order; both must LEAD the arguments
#   scripts/e2e/run.sh coverage [--sha S] [--format notes]   # read-only: is live coverage complete for S? (exit 0 / 8)
#   scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch   # anything else is passed to `go test`
#   E2E_TRAIN_MODE=off E2E_AUTH_MODE=pat scripts/e2e/run.sh -run Smoke
#
# Exit codes: 3 budget exhausted (RUN INVALID), 4 bed preflight failed, 5 pre-gate
# failed, 6 post-suite watchdog, 7 operational precondition failed, 8 (--resume and
# `coverage`) every leg passed but required live coverage is still incomplete;
# otherwise the suite's own code.
#
# E2E_GATE_BIN_DIR overrides where the runner binary is built (default:
# a per-user directory under $TMPDIR).

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

BIN_DIR="${E2E_GATE_BIN_DIR:-${TMPDIR:-/tmp}/fabrik-e2e-gate-$(id -u)}"
mkdir -p "$BIN_DIR"

# Build to a unique name and rename into place: rename is atomic, so a gate
# run already executing the previous binary (or a concurrent invocation) is
# never disturbed by a rebuild. `go build` is a no-op fast path when nothing
# changed, so the shim costs ~a second on an unchanged tree.
TMP_BIN="$BIN_DIR/gate.$$"
if ! go build -o "$TMP_BIN" ./tests/gate/cmd/gate; then
  rm -f "$TMP_BIN"
  echo "scripts/e2e/run.sh: building the gate runner (./tests/gate/cmd/gate) failed" >&2
  exit 1
fi
mv -f "$TMP_BIN" "$BIN_DIR/gate"

# `run.sh coverage ...` is the read-only ledger acceptance check; anything else
# is the gate itself.
if [[ "${1:-}" == "coverage" ]]; then
  exec "$BIN_DIR/gate" "$@"
fi
exec "$BIN_DIR/gate" run "$@"
