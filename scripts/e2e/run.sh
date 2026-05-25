#!/usr/bin/env bash
# scripts/e2e/run.sh — runner for the Fabrik end-to-end integration suite.
#
# Usage:
#   scripts/e2e/run.sh                       # full suite
#   scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch    # one test
#   scripts/e2e/run.sh -run 'Smoke|NoWork'                 # subset
#
# Anything after the script name is passed to `go test`.
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

# Default timeout — generous because scenarios can wait on Claude for minutes.
TIMEOUT="${E2E_TIMEOUT:-90m}"

# Default to verbose because these tests are long-running and the operator
# wants progress.
exec go test -tags=e2e -v -timeout "$TIMEOUT" ./tests/e2e/... "$@"
