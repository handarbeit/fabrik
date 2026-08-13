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
#   E2E_PARALLEL_ON=1 scripts/e2e/run.sh     # tighten just the "on" leg's cap further
#
# --clean (if given, must be the first argument) runs scripts/e2e/reset.sh for a
# clean-slate bed before the run. Anything else is passed to `go test`.
#
# Bed preflight (on by default — see preflight_bed below for the full rationale):
#   Before anything runs, the bed checkout is fast-forwarded to the ref under
#   test, its binary is rebuilt IN PLACE, stage-config drift is reported, and the
#   engine is restarted onto that binary — with the run aborting if the binary or
#   the running engine does not actually carry that ref. Nothing here used to be
#   automated, and a bed 194 commits behind main produced runs indistinguishable
#   from real ones.
#
#   Ordering is load-bearing: preflight leaves the bed STOPPED, --clean's
#   reset.sh runs against the stopped bed (it refuses a live one), and only then
#   is the engine started.
#
#   E2E_BED_REF=<ref>      ref to test (default: origin/main)
#   E2E_SKIP_PREP=1        skip preflight entirely (bed prepared by hand)
#   E2E_BED_NO_BUILD=1     verify and report only; never stop/build/start, and
#                          fail loud if the bed is not already on the ref
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
# "on"-leg-specific parallelism cap (E2E_PARALLEL_ON, default 2): #1527 found
# the "on" leg's extra GraphQL cost over "off" is NOT the merge-train worker's
# own CI polling (that's REST, a separate budget) but the ADR-1270/ADR-1208
# per-poll settle-scan pattern (settleAwaitingCIScan, and merge-train-exclusive
# settleQueuedReviewFindings) — each does an unconditional, no-cooldown
# GraphQL deep-fetch per matching item per poll, a cost proportional to how
# many items are concurrently Queued/awaiting-CI, not to how fast the train
# itself polls. That per-poll-per-item semantics is load-bearing for ADR-1208's
# correctness guarantee (a Queued member's review feedback must be seen within
# one batch cycle), so it isn't weakened here — instead, this cap shrinks the
# population those scans iterate by admitting fewer concurrent scenarios into
# the "on" leg specifically. Default 2 (half of E2E_PARALLEL's default 4) is a
# reasoned starting point given the "on" leg's larger scenario count (17 vs 13
# for "off") — not yet empirically tuned against a live gate run; see
# tests/e2e/README.md's GraphQL budget section for measured numbers as they
# become available. Only the default two-mode gate's "on" leg uses this
# — a forced single-mode run (E2E_TRAIN_MODE set explicitly) always uses
# E2E_PARALLEL, unchanged, so every documented iteration workflow above still
# behaves exactly as before this variable existed.
#
# Budget-exhaustion detection (R2, #1527): a throttled run must fail loudly
# and distinctly from a normal test failure — see "GraphQL budget exhaustion
# detection" further below for the mechanism (exit code 3).
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
# GraphQL budget exhaustion detection (R2, #1527): once backoff engages, the
# engine's polling slows enough that board items sit past scenarios' wait
# deadlines, producing a wall of test timeouts indistinguishable from real
# regressions (see #1527's own incident writeup for the shape of this: three
# separate 0.0.78 pre-release gate runs, all of whose failures were timeouts,
# not assertion violations). To make a throttled run fail loudly instead:
# after each leg's suite invocation, this script scans the bed's own
# fabrik.log (at $ENGINE_LOG) in full — not from a captured byte offset. Each
# leg's own bed-restart step (TestSwitchTrainMode, via StopFabrikTestBed +
# StartFabrikTestBed) always launches a brand-new engine process, and
# engine/poll.go's Run() opens fabrik.log with O_TRUNC on every startup — so
# by construction the file contains only this leg's own content the moment
# the restart completes, covering the freshly-restarted engine's very first
# poll (before the suite even starts) through the end of the suite run, with
# no offset bookkeeping needed and no blind spot between legs. (An earlier
# version of this script tried to track a pre-restart byte offset instead;
# that offset was measured against the pre-truncation file and so almost
# always exceeded the truncated file's size, making `tail -c +N` — and thus
# the whole leg's detection — silently return nothing. See #1547's review
# thread for the incident.) The scan looks for the engine's one-shot
# rate-limit-backoff-activation line
# ("...activating rate-limit backoff", engine/poll.go — NOT the companion
# per-poll "...is low (...) consider reducing poll frequency" line, which
# fires on every poll while low and would over-trigger on a run that dipped
# briefly without ever crossing the 20% hysteresis-activation threshold). A
# match means this leg's verdict cannot be trusted — any test
# failures/timeouts above may be throttling artifacts. The script prints an
# unambiguous "RUN INVALID" banner and exits $BUDGET_EXHAUSTED_EXIT (3),
# distinct from go test's propagated 1 and from a generic shell usage error
# (2), so automation (e.g. cut-release.sh) can branch on "budget exhausted,
# rerun" vs. "real regression, investigate" programmatically. This check runs
# regardless of the leg's own pass/fail exit code — a throttled run can
# present as a pass just as easily as a fail — and short-circuits any
# remaining legs immediately (exit, not return).
#
# This is a log-scrape, not a live `gh api rate_limit` polling loop: it needs
# zero new API calls against the very budget being protected, and the log
# line already fires at the moment of interest. Each leg is also bracketed
# with a `gh api rate_limit` call before/after (a REST metadata call, free
# against the GraphQL budget), scoped to the bed's own FABRIK_TOKEN (read
# from $TEST_BED/.env into $BED_TOKEN, mirroring reset.sh's gh_() token-
# scoping precedent — the ambient `gh` CLI's own auth is very likely a
# different identity than the bed's @arbeithand token and would silently
# measure the wrong account's budget) to report the leg's actual consumed
# points — this is a complementary, separate purpose (durable cost
# visibility, A3 of #1527) from the log-scrape's fail-loud detection.
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

# Test bed location and its engine log — same FABRIK_TEST_DIR convention as
# scripts/e2e/reset.sh. Used by the GraphQL budget exhaustion detection below
# (R2, #1527) to scope its scan to this bed's own fabrik.log.
TEST_BED="${FABRIK_TEST_DIR:-$HOME/dev/fabrik-test}"
ENGINE_LOG="$TEST_BED/.fabrik/fabrik.log"

# The bed's own PAT (same FABRIK_TOKEN reset.sh reads and scopes its gh_()
# helper to) — the A3 budget report below must measure the bed's own
# GraphQL consumption, never whatever identity the ambient `gh` CLI happens
# to be authenticated as in the invoking shell, which is very likely a
# different account than the bed's @arbeithand token and would silently
# report an unrelated budget. A missing/unreadable token only degrades the
# budget report (skipped, warned) — unlike reset.sh, nothing else in this
# script depends on it, so it's not fatal here.
BED_TOKEN=$( { grep '^FABRIK_TOKEN=' "$TEST_BED/.env" 2>/dev/null | head -1 | cut -d= -f2-; } || echo "")
if [ -z "$BED_TOKEN" ]; then
  echo "warning: could not read FABRIK_TOKEN from $TEST_BED/.env — GraphQL budget reporting will be skipped" >&2
fi

# Distinct exit code for a run invalidated by GraphQL budget exhaustion — see
# "GraphQL budget exhaustion detection" in the header comment above.
readonly BUDGET_EXHAUSTED_EXIT=3

# Distinct exit code for a preflight failure — the suite never started, so a
# non-zero exit here must not be read as "the engine is broken."
readonly PREFLIGHT_FAILED_EXIT=4

# ---------------------------------------------------------------------------
# Bed preflight: guarantee the bed is running the engine we mean to test.
#
# The bed (~/dev/fabrik-test) is a full fabrik source checkout with its own
# built binary, and NOTHING in this suite rebuilds it. That is a silent
# correctness hole: a stale bed produces a run that looks exactly like a real
# one — same scenarios, same duration, same green/red — while testing an
# engine nobody asked about. Found the hard way: a bed sat 194 commits behind
# main, and a "run the suite against main" would have measured the engine as
# of #1531 and reported it as current.
#
# The steps below were previously tribal knowledge rediscovered by hand every
# time (README §"Test bed prerequisites" + item 17, the build-in-place rule,
# the no---auto-upgrade rule). They are encoded here so the default path is
# the correct one.
#
# Knobs:
#   E2E_SKIP_PREP=1   skip entirely (bed already prepared by hand)
#   E2E_BED_REF=<ref> ref to test (default: origin/main)
#   E2E_BED_NO_BUILD=1  verify the bed's state but never rebuild/restart —
#                     fails loud on mismatch instead of fixing it
# ---------------------------------------------------------------------------
preflight_bed() {
  local ref="${E2E_BED_REF:-origin/main}"

  echo "== preflight: bed at $TEST_BED, target ref $ref =="

  if [ ! -d "$TEST_BED/.git" ]; then
    echo "preflight: $TEST_BED is not a git checkout — see tests/e2e/README.md" >&2
    exit "$PREFLIGHT_FAILED_EXIT"
  fi

  # Never clobber uncommitted engine work in the bed. Untracked files (logs,
  # .env backups) are normal there and deliberately ignored; modified TRACKED
  # files usually mean someone is mid-experiment (e.g. the neutralize/rebuild
  # cycle README §AC3 describes) and must not be silently reset.
  local dirty
  dirty="$(cd "$TEST_BED" && git status --porcelain --untracked-files=no)"
  if [ -n "$dirty" ]; then
    echo "preflight: $TEST_BED has modified tracked files — refusing to update it." >&2
    echo "$dirty" >&2
    echo "Commit, stash, or revert them, or re-run with E2E_SKIP_PREP=1." >&2
    exit "$PREFLIGHT_FAILED_EXIT"
  fi

  ( cd "$TEST_BED" && git fetch origin --quiet )

  local want
  want="$(cd "$TEST_BED" && git rev-parse "$ref")"
  local want_short="${want:0:7}"
  BED_REF_SHORT="$want_short"
  local have
  have="$(cd "$TEST_BED" && git rev-parse HEAD)"

  if [ "$have" != "$want" ]; then
    # Report both directions: the target is usually ahead (a stale bed), but it
    # can equally be an ancestor (deliberately testing an older ref), and
    # "0 commits behind" alone reads as a contradiction of the mismatch above.
    local counts ahead behind divergence
    counts="$(cd "$TEST_BED" && git rev-list --left-right --count "HEAD...$want" 2>/dev/null || echo "? ?")"
    ahead="$(printf '%s' "$counts" | awk '{print $1}')"
    behind="$(printf '%s' "$counts" | awk '{print $2}')"
    divergence="$behind commit(s) behind, $ahead ahead"

    if [ -n "${E2E_BED_NO_BUILD:-}" ]; then
      echo "preflight: bed is at ${have:0:7}, want $want_short ($divergence) and E2E_BED_NO_BUILD is set" >&2
      exit "$PREFLIGHT_FAILED_EXIT"
    fi
    echo "   bed checkout ${have:0:7} is $divergence vs $ref — updating to $want_short"
    ( cd "$TEST_BED" && git checkout --quiet --detach "$want" )
  else
    echo "   bed checkout already at $want_short"
  fi

  # Build IN PLACE. A binary built elsewhere and copied in can be SIGKILL'd on
  # Apple Silicon (README item 17), so this is not merely a convenience.
  if [ -z "${E2E_BED_NO_BUILD:-}" ]; then
    echo "   building bed binary in place"
    ( cd "$TEST_BED" && go build -o fabrik . )
  fi

  # Fail loud if the binary does not actually carry the ref under test. This is
  # the check whose absence made the 194-commit run possible.
  local ver
  ver="$( cd "$TEST_BED" && ./fabrik --version 2>&1 | head -1 )"
  if ! printf '%s' "$ver" | grep -q "$want_short"; then
    echo "preflight: bed binary reports '$ver' but the ref under test is $want_short" >&2
    exit "$PREFLIGHT_FAILED_EXIT"
  fi
  echo "   bed binary: $ver"

  # Stage-config drift is reported, never auto-applied: .fabrik/stages/ is bed
  # configuration, and silently rewriting it could change what the suite means.
  local drift
  drift="$( cd "$TEST_BED" && ./fabrik refresh-stages 2>&1 || true )"
  if [ -n "$drift" ]; then
    echo "   stage-config drift detected (not applied — run 'fabrik refresh-stages --apply' in the bed if intended):"
    printf '%s\n' "$drift" | sed 's/^/     /'
  else
    echo "   stage configs current"
  fi

  # Verify-only mode inspects and reports; it never stops, builds, or starts
  # anything, so a bed someone else is driving stays untouched.
  if [ -n "${E2E_BED_NO_BUILD:-}" ]; then
    echo "== preflight (verify-only) complete — bed left as-is =="
    return 0
  fi

  # Leave the bed STOPPED on exit. reset.sh refuses to run against a live
  # instance (see its header and the --worktrees guard), and a running engine
  # would still be holding the pre-build binary anyway — so the instance is
  # started by preflight_bed_start() only after any --clean reset has run.
  stop_bed_instance

  echo "== preflight (build) complete — bed stopped, ready for reset =="
}

# stop_bed_instance terminates a running bed engine and waits for it to exit.
# SIGTERM, not SIGKILL: the engine's clean-stop path (#1393) durably pauses
# in-flight issues rather than abandoning them mid-stage.
stop_bed_instance() {
  local lock="$TEST_BED/.fabrik/fabrik.lock"
  [ -f "$lock" ] || return 0

  local pid
  pid="$(cat "$lock" 2>/dev/null || echo "")"
  [ -n "$pid" ] || return 0
  kill -0 "$pid" 2>/dev/null || return 0

  echo "   stopping running bed instance (pid $pid)"
  kill -TERM "$pid" 2>/dev/null || true
  local i=0
  while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 60 ]; do sleep 1; i=$((i + 1)); done
  if kill -0 "$pid" 2>/dev/null; then
    echo "preflight: bed instance $pid did not exit within 60s" >&2
    exit "$PREFLIGHT_FAILED_EXIT"
  fi
}

# preflight_bed_start brings the bed engine up on the freshly built binary and
# refuses to continue unless its own startup banner names the ref under test.
# Runs after any --clean reset, since reset.sh requires a stopped instance.
preflight_bed_start() {
  local want_short="$1"

  # No --auto-upgrade: it would replace this freshly built binary with a
  # release mid-suite (README item 17).
  echo "== preflight: starting bed instance (-notui, no --auto-upgrade) =="
  ( cd "$TEST_BED" && nohup ./fabrik -notui > "$TEST_BED/bed-run.log" 2>&1 & )

  local i=0
  while [ "$i" -lt 90 ]; do
    if grep -q 'Fabrik starting' "$ENGINE_LOG" 2>/dev/null; then break; fi
    sleep 1
    i=$((i + 1))
  done

  local banner
  banner="$(grep -m1 'Fabrik starting' "$ENGINE_LOG" 2>/dev/null || echo "")"
  if [ -z "$banner" ]; then
    echo "preflight: bed did not report startup within 90s — see $TEST_BED/bed-run.log" >&2
    exit "$PREFLIGHT_FAILED_EXIT"
  fi
  if ! printf '%s' "$banner" | grep -q "$want_short"; then
    echo "preflight: running bed reports '$banner' but the ref under test is $want_short" >&2
    exit "$PREFLIGHT_FAILED_EXIT"
  fi
  echo "   bed running: $banner"
}

# BED_REF_SHORT is set by preflight_bed and consumed by preflight_bed_start;
# when prep is skipped it is derived so the start-time banner check still runs.
BED_REF_SHORT=""

if [ -n "${E2E_SKIP_PREP:-}" ]; then
  echo "== preflight skipped (E2E_SKIP_PREP set) — bed assumed prepared =="
else
  preflight_bed
fi

# Optional clean-slate reset before the run (must be the first argument).
# Runs with the bed stopped (preflight leaves it that way) because reset.sh
# refuses to operate against a live instance.
if [ "${1:-}" = "--clean" ]; then
  shift
  echo "== --clean: resetting the test bed via scripts/e2e/reset.sh =="
  "$REPO_ROOT/scripts/e2e/reset.sh"
  echo "== reset complete =="
fi

if [ -z "${E2E_SKIP_PREP:-}" ] && [ -z "${E2E_BED_NO_BUILD:-}" ]; then
  preflight_bed_start "$BED_REF_SHORT"
fi

# Default timeout — generous because scenarios can wait on Claude for
# minutes, and a full two-mode gate run under contention needs headroom above
# the heaviest single-scenario ceilings (see header comment for derivation).
TIMEOUT="${E2E_TIMEOUT:-4h}"

# Cap concurrent scenarios so the full suite doesn't oversubscribe the single
# shared bed (see header + issue #971). Default 4; override with E2E_PARALLEL.
PARALLEL="${E2E_PARALLEL:-4}"

# Tighter cap for the default two-mode gate's "on" leg specifically — see the
# header comment's "on"-leg-specific parallelism cap section (#1527).
PARALLEL_ON="${E2E_PARALLEL_ON:-2}"

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

# detect_rate_limit_backoff scans $1 (a fabrik.log path) for the engine's
# one-shot rate-limit-backoff-activation line and returns 0 (match) or 1 (no
# match / file absent). Extracted into its own function — rather than left
# inline in switch_and_run — specifically so
# scripts/e2e/backoff_detection_test.sh can exercise it directly against
# fixtures, independent of an actual gate run.
#
# Scans the WHOLE current file, not a captured byte offset: each leg's own
# bed-restart step (TestSwitchTrainMode, via StopFabrikTestBed +
# StartFabrikTestBed) always launches a brand-new engine process, and
# engine/poll.go's Run() opens fabrik.log with O_TRUNC on every startup — so
# by the time that restart completes the file already contains only this
# leg's own content, and scanning it in full naturally covers the freshly-
# restarted engine's first poll through the end of the suite run, with no
# offset bookkeeping needed and no blind spot between legs. (An earlier
# version of this script tried to track a pre-restart byte offset instead;
# that offset was measured against the pre-truncation file and so almost
# always exceeded the truncated file's size, making `tail -c +N` — and thus
# the whole leg's detection — silently return nothing. See #1547's review
# thread for the incident, and backoff_detection_test.sh's
# "historical broken (byte-offset) approach" case for a fixture that
# reproduces it and confirms the current whole-file scan is not susceptible.)
#
# Matches the literal one-shot hysteresis-activation line — NOT the
# companion per-poll "...is low (...) consider reducing poll frequency"
# line, which fires on every poll while low and would over-trigger on a run
# that dipped briefly without ever crossing the 20% hysteresis-activation
# threshold. `[ -f "$1" ]` guards against a bed that hasn't produced a log
# yet (e.g. misconfigured FABRIK_TEST_DIR) — treated as "no match" rather
# than an error, consistent with the budget-report guards elsewhere in this
# script.
detect_rate_limit_backoff() {
  local logfile="$1"
  [ -f "$logfile" ] && grep -q 'activating rate-limit backoff' "$logfile" 2>/dev/null
}

# switch_and_run stops the bed, flips FABRIK_MERGE_TRAIN to $1 in its .env,
# restarts it (via the dedicated TestSwitchTrainMode invocation — a separate
# `go test` process so the restart completes, bed fully back up, before the
# suite invocation that follows even starts), then runs the suite with
# E2E_TRAIN_MODE=$1 exported and -parallel capped at $2.
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
#
# After the suite invocation, regardless of $rc, this function also checks
# fabrik.log in full (not from a captured byte offset — see below) for the
# engine's rate-limit-backoff-activation line and, on a match, prints a RUN
# INVALID banner and `exit`s the whole script with $BUDGET_EXHAUSTED_EXIT —
# see the header comment's "GraphQL budget exhaustion detection" section
# (#1527). This is an `exit`, not a `return`: an invalidated leg must
# short-circuit any remaining leg immediately rather than let the two-mode
# gate continue on to a second leg against a bed whose GraphQL budget is
# already compromised for the current hour.
switch_and_run() {
  local mode="$1"
  local parallel="$2"
  shift 2

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
  echo "== running suite with E2E_TRAIN_MODE=${mode}, -parallel=${parallel} =="
  local jsonlog="${TMPDIR:-/tmp}/fabrik-e2e-${mode}-$$.json"
  local rc=0

  # Snapshot the bed's GraphQL budget right before the suite runs — this is
  # purely for the A3 cost report, which should reflect the suite's own
  # consumption, not the restart step's (unlike the backoff scan below, which
  # deliberately covers the restart too — see its own comment for why the two
  # have different windows). Guarded with `|| echo ...` so a transient `gh`
  # failure degrades to a skipped report rather than aborting the script
  # under `set -e`.
  local budget_before budget_after
  budget_before=""
  if [ -n "$BED_TOKEN" ]; then
    budget_before=$(GH_TOKEN="$BED_TOKEN" gh api rate_limit --jq '.resources.graphql.remaining' 2>/dev/null || echo "")
  fi

  E2E_TRAIN_MODE="$mode" go test -tags=e2e -json -count=1 -timeout "$TIMEOUT" -parallel "$parallel" \
      ./tests/e2e/... "$@" 2>&1 \
    | tee "$jsonlog" \
    | { jq -R -r 'fromjson? // empty | select(.Action=="output") | .Output' 2>/dev/null || true; } \
    || rc=$?

  budget_after=""
  if [ -n "$BED_TOKEN" ]; then
    budget_after=$(GH_TOKEN="$BED_TOKEN" gh api rate_limit --jq '.resources.graphql.remaining' 2>/dev/null || echo "")
  fi
  if [ -n "$budget_before" ] && [ -n "$budget_after" ]; then
    if [ "$budget_after" -le "$budget_before" ]; then
      echo "== GraphQL budget (leg: ${mode}): ${budget_before} -> ${budget_after} remaining (consumed $((budget_before - budget_after)) pts) =="
    else
      echo "== GraphQL budget (leg: ${mode}): ${budget_before} -> ${budget_after} remaining (budget reset mid-leg; consumption not computable) =="
    fi
  else
    echo "warning: could not read GraphQL rate_limit before/after leg ${mode} (gh api call failed) — skipping budget report" >&2
  fi

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
  fi

  # R2 (#1527): check whether the engine's own rate-limit backoff engaged at
  # any point during this leg, regardless of $rc — a throttled run can just
  # as easily present as a pass (if timeouts happened to land after the
  # relevant waits already succeeded) as a fail, so this must not be
  # conditioned on the leg having failed. See detect_rate_limit_backoff's own
  # comment (above its definition) for why this scans the whole current
  # fabrik.log rather than a captured byte offset — extracted into its own
  # function so scripts/e2e/backoff_detection_test.sh can exercise it
  # directly against fixtures without running an actual gate leg.
  if detect_rate_limit_backoff "$ENGINE_LOG"; then
    {
      echo ""
      echo "############################################################"
      echo "## RUN INVALID (leg: ${mode}): GraphQL rate-limit backoff engaged mid-run."
      echo "## Any test failures/timeouts above may be throttling artifacts, not real"
      echo "## regressions — this run's verdict cannot be trusted."
      echo "##"
      echo "## Engine log: $ENGINE_LOG"
      echo "## (look for 'activating rate-limit backoff' for the exact event(s))"
      echo "##"
      echo "## See tests/e2e/README.md's GraphQL budget section for mitigation"
      echo "## (E2E_PARALLEL_ON, splitting the leg across budget windows)."
      echo "############################################################"
    } >&2
    exit "$BUDGET_EXHAUSTED_EXIT"
  fi

  if [ "$rc" -ne 0 ]; then
    return "$rc"
  fi
}

# Guarded so scripts/e2e/backoff_detection_test.sh can `source` this file to
# reach detect_rate_limit_backoff (and the other helper functions above)
# without triggering an actual gate run. Everything above this guard (repo
# root resolution, TEST_BED/ENGINE_LOG/BED_TOKEN setup, function
# definitions) is safe to execute on source — read-only or pure function
# definitions, no gate invocation. When executed directly (./run.sh or
# `bash run.sh`), BASH_SOURCE[0] equals $0, so this still dispatches exactly
# as before this guard existed.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  if [ -n "${E2E_TRAIN_MODE:-}" ]; then
    # Single mode forced by the caller — one switch + one suite invocation.
    # Always uses E2E_PARALLEL (not E2E_PARALLEL_ON), unchanged from before
    # E2E_PARALLEL_ON existed — see the header comment.
    switch_and_run "$E2E_TRAIN_MODE" "$PARALLEL" "$@"
  else
    # Default: the full two-mode validation gate. "off" first — see header
    # comment for why. "on" gets the tighter E2E_PARALLEL_ON cap.
    switch_and_run off "$PARALLEL" "$@"
    switch_and_run on "$PARALLEL_ON" "$@"
  fi
fi
