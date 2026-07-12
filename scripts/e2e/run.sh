#!/usr/bin/env bash
# scripts/e2e/run.sh — runner for the Fabrik end-to-end integration suite.
#
# Usage:
#   scripts/e2e/run.sh                       # full suite
#   scripts/e2e/run.sh --clean               # reset boards/PRs/branches first, then full suite
#   scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch    # one test
#   scripts/e2e/run.sh -run 'Smoke|NoWork'                 # subset
#   E2E_PARALLEL=2 scripts/e2e/run.sh        # tighten the parallelism cap for a heavy run
#
# --clean (if given, must be the first argument) runs scripts/e2e/reset.sh for a
# clean-slate bed before the suite. Anything else is passed to `go test`.
#
# Parallelism cap (E2E_PARALLEL, default 4): 15 of the 16 e2e tests are
# t.Parallel(), but they all drive ONE shared Fabrik bed (5 workers by default)
# against ONE shared board + API budget. Go's default -parallel is GOMAXPROCS
# (~8-12 cores), which oversubscribes the bed and produces cascading timeouts
# even though each scenario passes standalone. Capping -parallel keeps the bed
# from being flooded. See issue #971 and tests/e2e/README.md.
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

# Cap concurrent scenarios so the full suite doesn't oversubscribe the single
# shared bed (see header + issue #971). Default 4; override with E2E_PARALLEL.
PARALLEL="${E2E_PARALLEL:-4}"

# Default to verbose because these tests are long-running and the operator
# wants progress.
exec go test -tags=e2e -v -timeout "$TIMEOUT" -parallel "$PARALLEL" ./tests/e2e/... "$@"
