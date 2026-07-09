#!/usr/bin/env bash
# scripts/e2e/run.sh — runner for the Fabrik end-to-end integration suite.
#
# Usage:
#   scripts/e2e/run.sh                       # full suite
#   scripts/e2e/run.sh --clean               # reset boards/PRs/branches first, then full suite
#   scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch    # one test
#   scripts/e2e/run.sh -run 'Smoke|NoWork'                 # subset
#
# --clean (if given, must be the first argument) runs scripts/e2e/reset.sh for a
# clean-slate bed before the suite. Anything else is passed to `go test`.
#
# Prerequisites (one-time setup):
#   - ~/dev/fabrik-test/ exists with .env (FABRIK_TOKEN for @arbeithand)
#   - handarbeit/fabrik-test-alpha + fabrik-test-beta exist and seeded
#   - handarbeit/projects/2 ("Fabrik Test") exists with stage columns
#   - Fabrik instance running at ~/dev/fabrik-test/ (typically with --auto-upgrade)
# See tests/e2e/README.md for setup details.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# Optional clean-slate reset before the suite (must be the first argument).
if [ "${1:-}" = "--clean" ]; then
  shift
  echo "== --clean: resetting the test bed via scripts/e2e/reset.sh =="
  "$REPO_ROOT/scripts/e2e/reset.sh"
  echo "== reset complete; starting suite =="
fi

# Default timeout — generous because scenarios can wait on Claude for minutes.
TIMEOUT="${E2E_TIMEOUT:-90m}"

# Default to verbose because these tests are long-running and the operator
# wants progress.
exec go test -tags=e2e -v -timeout "$TIMEOUT" ./tests/e2e/... "$@"
