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
# case in the ~93-145min range; 4h leaves ~95-147min of margin on top. See
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

# report_test_outcomes prints a completed/still-running/never-started
# breakdown of every top-level test from a `go test -json` log, so a killed
# run's partial state is legible instead of a bare FAIL indistinguishable
# from a real regression. The built-in -timeout panic dump only reports tests
# that were actively executing — a test parked waiting for a free -parallel
# slot never appears there at all. Classification here is by each test's
# *last state-transition* action: pass/fail/skip -> completed; run/cont ->
# still running at kill time; pause -> never started (queued behind
# -parallel). "output" events are excluded before taking the last action —
# every test emits output as its final events in practice (-v RUN/PAUSE/CONT
# lines, t.Log calls, and for the test that actually timed out, the entire
# panic/goroutine dump is itself a stream of "output" events on that test) —
# without this exclusion, group_by's last-element pick would land on
# "output" for nearly every test and the timed-out test itself would vanish
# from every bucket instead of showing up as still-running.
# Subtests (names containing "/") are folded into their parent's timeline;
# per-test granularity is what an operator needs, not per-subtest.
# Non-JSON lines (e.g. `go: downloading ...` progress, a raw panic dump) are
# skipped rather than aborting the whole report.
report_test_outcomes() {
  local jsonlog="$1"
  jq -R 'fromjson? // empty' "$jsonlog" \
    | jq -s -r '
        [ .[] | select(.Test != null and (.Test | contains("/") | not) and .Action != "output") ]
        | group_by(.Test)
        | map({test: .[0].Test, last: .[-1].Action})
        | group_by(.last)
        | map({(.[0].last): (map(.test) | sort)})
        | add // {}
        | (.pass // []) as $pass
        | (.fail // []) as $fail
        | (.skip // []) as $skip
        | (((.run // []) + (.cont // [])) | sort) as $running
        | (.pause // []) as $paused
        | "completed - pass (\($pass|length)): \($pass|join(", "))\n"
        + "completed - fail (\($fail|length)): \($fail|join(", "))\n"
        + "completed - skip (\($skip|length)): \($skip|join(", "))\n"
        + "still running at kill time (\($running|length)): \($running|join(", "))\n"
        + "never started - queued behind -parallel cap (\($paused|length)): \($paused|join(", "))"
      '
}

# report_test_timings prints a ranked (slowest-first) per-test wall-clock
# summary from a `go test -json` log, labeled with which leg (mode) it came
# from. This is the evidence #1355's R3 exists to produce: "which scenarios
# cost the most" should be a measured number from a real gate run, not a
# guess, so future cost/value tradeoffs on expensive-and-thin scenarios are
# evidence-based.
#
# Elapsed is only meaningful on a test's terminal event (Go's test2json emits
# it on pass/fail/skip; run/cont/pause events carry no useful duration), so
# the filter selects those three actions directly rather than reusing
# report_test_outcomes's "last action, any type" grouping. Subtests (names
# containing "/") are excluded — same per-test (not per-subtest) granularity
# as report_test_outcomes. Called unconditionally (pass or fail) so a timing
# table is always emitted for every leg that actually ran, not just failing
# ones — see switch_and_run below for why this can't be a single combined
# report across both legs.
report_test_timings() {
  local jsonlog="$1"
  local mode="$2"
  echo "== per-test wall-clock (leg: ${mode}), slowest first =="
  jq -R 'fromjson? // empty' "$jsonlog" \
    | jq -s -r '
        [ .[] | select(.Test != null and (.Test | contains("/") | not)
            and (.Action == "pass" or .Action == "fail" or .Action == "skip")) ]
        | group_by(.Test)
        | map({test: .[0].Test, result: .[-1].Action, elapsed: (.[-1].Elapsed // 0)})
        | sort_by(-.elapsed)
        | .[]
        | [(.elapsed | tostring) + "s", .result, .test] | @tsv
      ' \
    | { column -t -s "$(printf '\t')" 2>/dev/null || cat; }
}

# switch_and_run stops the bed, flips FABRIK_MERGE_TRAIN to $1 in its .env,
# restarts it (via the dedicated TestSwitchTrainMode invocation — a separate
# `go test` process so the restart completes, bed fully back up, before the
# suite invocation that follows even starts), then runs the suite with
# E2E_TRAIN_MODE=$1 exported.
#
# The suite invocation runs under `go test -json`, teed to a per-leg log, so
# a non-zero exit can be classified (report_test_outcomes) and, if it was
# specifically an E2E_TIMEOUT kill, followed by best-effort teardown — see
# the header comment. `|| rc=$?` (rather than toggling set -e/+e) is what
# lets this function inspect the pipeline's exit status without the script
# aborting first: it's not the last element of an AND/OR list, so `set -e`
# does not trigger on it, and `pipefail` ensures $? reflects go test's own
# exit code even though it isn't the last command in the pipeline. The
# report_test_outcomes call below is guarded with `|| echo warning...` for
# the same reason: it's a standalone statement under `set -e`, so an
# unguarded jq failure there (e.g. an unexpected future `go test -json`
# schema change) would abort the script before ever reaching the
# timeout-panic check and auto-teardown that follow it — silently
# defeating the hardening this function exists to provide.
switch_and_run() {
  local mode="$1"
  shift
  echo "== switching test bed to FABRIK_MERGE_TRAIN=${mode} =="
  # KNOWN GAP: this restart step has none of the classification/auto-teardown
  # machinery below — it's a plain `-v` (non-`-json`) run with its own fixed
  # 3m timeout and no exit-code capture. If the bed fails to come back up
  # here (e.g. restart hangs), this line's own `-timeout 3m` panic or a
  # non-zero exit takes down the whole script via `set -e` with no
  # classification report and no reset.sh call — leaving the bed possibly
  # stopped or mid-restart with no diagnostics beyond raw `go test -v`
  # output. Deliberately left as-is: this step is a fast (~seconds), narrow
  # bed-restart check, not a scenario run, so it's a much smaller exposure
  # window than the suite invocation below, and extending it the same
  # machinery would mean auto-tearing-down a bed that may just be mid-restart
  # rather than actually stuck.
  E2E_TRAIN_SWITCH=1 E2E_TRAIN_MODE="$mode" go test -tags=e2e -v -count=1 -timeout 3m \
    -run '^TestSwitchTrainMode$' ./tests/e2e/...
  echo "== running suite with E2E_TRAIN_MODE=${mode} =="
  local jsonlog="${TMPDIR:-/tmp}/fabrik-e2e-${mode}-$$.json"
  local rc=0
  E2E_TRAIN_MODE="$mode" go test -tags=e2e -json -count=1 -timeout "$TIMEOUT" -parallel "$PARALLEL" \
      ./tests/e2e/... "$@" 2>&1 \
    | tee "$jsonlog" \
    | { jq -R -r 'fromjson? // empty | select(.Action=="output") | .Output' 2>/dev/null || true; } \
    || rc=$?

  report_test_timings "$jsonlog" "$mode" \
    || echo "warning: failed to compute test timings (jq error) — inspect the raw JSON log directly: $jsonlog" >&2

  if [ "$rc" -ne 0 ]; then
    echo "== suite FAILED (leg: ${mode}, exit ${rc}) — classifying test outcomes ==" >&2
    echo "JSON log: $jsonlog" >&2
    report_test_outcomes "$jsonlog" >&2 \
      || echo "warning: failed to classify test outcomes (jq error) — inspect the raw JSON log directly: $jsonlog" >&2
    # KNOWN LIMITATION: this is a literal text match against the whole log,
    # not scoped to output from the outer `go test` process itself. No
    # current e2e scenario shells out to a nested `go test` (checked via
    # grep across tests/e2e/*.go), so there's nothing today whose own
    # captured "Output" JSON events could contain this exact string other
    # than the outer suite's own -timeout kill. If a future scenario ever
    # does invoke `go test` (or otherwise prints this literal string) as
    # part of its own test body, this check would misfire and trigger
    # teardown on a run that wasn't actually an E2E_TIMEOUT kill of the
    # outer suite. Revisit with a more targeted signal (e.g. checking the
    # panic line has no attributed Test, or checking the outer process's
    # own exit code pattern) if that ever becomes true.
    if grep -q 'panic: test timed out after' "$jsonlog"; then
      echo "== E2E_TIMEOUT kill detected (leg: ${mode}) — running best-effort teardown ==" >&2
      "$REPO_ROOT/scripts/e2e/reset.sh" \
        || echo "warning: automatic teardown failed; run scripts/e2e/reset.sh manually" >&2
      echo "NOTE: worktrees were NOT cleaned automatically (that requires stopping the bed first)." >&2
      echo "      Run scripts/e2e/reset.sh --worktrees for full parity before the next release-gate run." >&2
    fi
    return "$rc"
  fi
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
