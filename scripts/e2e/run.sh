#!/usr/bin/env bash
# scripts/e2e/run.sh — runner for the Fabrik end-to-end integration suite.
#
# Usage:
#   scripts/e2e/run.sh                       # full two-mode validation gate (off, then on)
#   scripts/e2e/run.sh --clean               # reset boards/PRs/branches first, then the gate
#   scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch    # one test, both modes
#   scripts/e2e/run.sh -run 'Smoke|NoWork'                 # subset, both modes
#   E2E_TRAIN_MODE=off scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch  # single mode only
#   E2E_PARALLEL=2 scripts/e2e/run.sh        # tighten the parallelism cap for a heavy run
#
# --clean (if given, must be the first argument) runs scripts/e2e/reset.sh for a
# clean-slate bed before the run. Anything else is passed to `go test`.
#
# Two-mode validation gate (E2E_TRAIN_MODE, default: both "off" and "on"):
#   FABRIK_MERGE_TRAIN is read at Fabrik startup, so exercising both landing
#   paths requires a bed restart between them — never an in-run flip while
#   t.Parallel() scenarios might be in flight (FR-5 of issue #1217). By
#   default this script drives that restart itself: for each of "off" then
#   "on", it runs a narrow go test invocation of TestSwitchTrainMode (which
#   stops the bed, edits FABRIK_MERGE_TRAIN in its .env, and restarts it),
#   THEN the normal suite invocation with E2E_TRAIN_MODE exported so
#   scenarios resolve mode explicitly (resolveTrainMode) instead of
#   re-reading the bed's .env themselves. "off" runs first because it's the
#   path nearly all real usage takes (see #1217) — a regression there
#   surfaces before spending time on the less-common train-on run.
#
#   Set E2E_TRAIN_MODE=on or E2E_TRAIN_MODE=off to force a single mode
#   (one switch + one suite invocation) instead of the two-mode default —
#   useful for iteration, a single -run scenario, or a future CI matrix leg.
#   A two-mode run is roughly double the single-mode GitHub API cost — see
#   #1219 for the budget headroom this assumes; merge it before attempting
#   a full two-mode run.
#
#   Failure mode: this script runs under set -e, so if the "off" leg fails
#   (switch step or suite), the script aborts immediately and the "on" leg
#   never runs — the shared bed is left running in FABRIK_MERGE_TRAIN=off,
#   NOT restored to whatever mode it was in before this invocation started.
#   If you hit a failure here, check/reset the bed's .env mode before
#   assuming it's still in its prior state.
#
# Timeout (E2E_TIMEOUT, default 4h): raised from the original 90m after a
# real two-mode gate run was killed mid-run — TestCIFixReinvokeCycleLimit was
# still executing at 1h26m, already past its own documented 30-60min ceiling,
# when the 90m wall clock hit. Paired off/on timings for two scenarios showed
# a ~1.55-1.61x contention multiplier under load (TestConjunctiveCIReviewGate
# 1335s->2152s, TestPausedMergedPRRecovery 1382s->2146s). Applying that factor
# to the heaviest documented per-scenario ceilings puts the contended worst
# case in the ~120-180min range; 4h leaves ~30-60min of margin on top. See
# tests/e2e/README.md's "How the defaults are derived" section for the full
# arithmetic — this number is provisional and should be revisited after
# future real runs, not treated as load-bearing precision.
#
# Parallelism cap (E2E_PARALLEL, default 4): 16 of the 17 e2e tests are
# t.Parallel(), but they all drive ONE shared Fabrik bed (5 workers by default)
# against ONE shared board + API budget. Go's default -parallel is GOMAXPROCS
# (~8-12 cores), which oversubscribes the bed and produces cascading timeouts
# even though each scenario passes standalone. Capping -parallel keeps the bed
# from being flooded. See issue #971 and tests/e2e/README.md. Kept at 4 rather
# than lowered further: the bed has 5 workers so 4 already reserves headroom,
# and the available contention data doesn't clearly indict 4 itself (the "on"
# leg simply has more real work — 17 scenarios vs 13, since Train-only
# scenarios skip near-instantly under "off"). A scenario failing purely from
# bed starvation is instead addressed by the timeout increase above, which
# gives a starved scenario enough wall-clock room to actually finish.
#
# Timeout/failure reporting and teardown-on-kill: the suite invocation below
# runs `go test -json`, tees the stream to a per-leg log under $TMPDIR, and
# on a non-zero exit classifies every top-level test by its last observed
# action into completed (pass/fail/skip), still-running (was executing when
# killed), and never-started (queued behind -parallel, never got a slot) —
# the built-in `-timeout` panic dump only reports the "still-running" half.
# If the log shows Go's own timeout-panic text, this was a wall-clock kill
# specifically (not a normal scenario FAIL), and the script automatically
# runs scripts/e2e/reset.sh (the non---worktrees form) as best-effort
# teardown, since a killed run's t.Cleanup calls never execute. Worktrees are
# NOT auto-cleaned (the only worktree-cleanup path is a destructive full-bed
# nuke requiring the bed to be stopped first) — see tests/e2e/README.md's
# "Teardown on kill" section for the remaining manual step.
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

# Optional clean-slate reset before the run (must be the first argument).
if [ "${1:-}" = "--clean" ]; then
  shift
  echo "== --clean: resetting the test bed via scripts/e2e/reset.sh =="
  "$REPO_ROOT/scripts/e2e/reset.sh"
  echo "== reset complete; starting run =="
fi

# Default timeout — generous because scenarios can wait on Claude for
# minutes, and a full two-mode gate run under contention needs headroom above
# the heaviest single-scenario ceilings (see header comment for derivation).
TIMEOUT="${E2E_TIMEOUT:-4h}"

# Cap concurrent scenarios so the full suite doesn't oversubscribe the single
# shared bed (see header + issue #971). Default 4; override with E2E_PARALLEL.
PARALLEL="${E2E_PARALLEL:-4}"

# switch_and_run stops the bed, flips FABRIK_MERGE_TRAIN to $1 in its .env,
# restarts it (via the dedicated TestSwitchTrainMode invocation — a separate
# `go test` process so the restart completes, bed fully back up, before the
# suite invocation that follows even starts), then runs the suite with
# E2E_TRAIN_MODE=$1 exported.
switch_and_run() {
  local mode="$1"
  shift
  echo "== switching test bed to FABRIK_MERGE_TRAIN=${mode} =="
  E2E_TRAIN_SWITCH=1 E2E_TRAIN_MODE="$mode" go test -tags=e2e -v -timeout 3m \
    -run '^TestSwitchTrainMode$' ./tests/e2e/...
  echo "== running suite with E2E_TRAIN_MODE=${mode} =="
  E2E_TRAIN_MODE="$mode" go test -tags=e2e -v -timeout "$TIMEOUT" -parallel "$PARALLEL" \
    ./tests/e2e/... "$@"
}

if [ -n "${E2E_TRAIN_MODE:-}" ]; then
  # Single mode forced by the caller — one switch + one suite invocation.
  switch_and_run "$E2E_TRAIN_MODE" "$@"
else
  # Default: the full two-mode validation gate. "off" first — see header
  # comment for why.
  switch_and_run off "$@"
  switch_and_run on "$@"
fi
