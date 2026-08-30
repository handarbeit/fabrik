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
# Pre-gate (R1, #1454): before ANY of the above — before bed preflight, before
# the build, before a single live GitHub/Claude call — this script runs the
# free, fast layers first (scripts/sim/run.sh --all, then the github/
# wire-contract tests) and aborts with a distinct exit code if either fails.
# Spending GraphQL quota and Claude tokens to discover a bug the sim or the
# wire-contract tests already caught for $0 is exactly the waste this ordering
# exists to remove. See run_pregate below for the full rationale.
#
#   E2E_SKIP_PREGATE=1     skip the pre-gate entirely (iteration-only escape
#                           hatch, mirroring E2E_SKIP_PREP — scripts/cut-
#                           release.sh never sets this; the release path
#                           always pays the pre-gate cost)
#
#   FABRIK_PREGATE_VERIFIED_SHA=<sha>   tree-scoped dedup signal (R5, #1624):
#                           if set, equal to a freshly-resolved `git
#                           rev-parse HEAD`, AND the working tree is clean
#                           (`git status --porcelain` empty), the pre-gate is
#                           skipped because it was already run, in full,
#                           against this exact tree earlier in the same
#                           invocation. The clean-tree check matters because
#                           HEAD alone only identifies the committed tree —
#                           a step running between the SHA being captured and
#                           this check that rewrites a tracked file on disk
#                           without committing it (cut-release.sh's own
#                           step 4 does this to plugin/known_embedded_versions.go)
#                           would otherwise leave a HEAD match vouching for a
#                           tree that no longer matches what was actually
#                           checked (found during Review, #1624).
#                           scripts/cut-release.sh exports this itself, right
#                           after its own step 3 pre-gate passes, so its step
#                           5 call into this script doesn't pay for the
#                           identical sim + wire-contract checks a second
#                           time. Unlike E2E_SKIP_PREGATE (a blanket,
#                           deliberately-never-set-by-cut-release.sh opt-out),
#                           this is narrow: a mismatched or unset SHA, or a
#                           dirty tree, always falls through to the full
#                           pre-gate, so a standalone `scripts/e2e/run.sh`
#                           invocation (which never sees this var) still pays
#                           full price, exactly as before this existed. See
#                           export_pregate_verified_sha in cut-release.sh.
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
#
# Process-group reap (R3, #1624): every `go test` invocation below runs via
# run_reaped (or an inline equivalent for the one call site with more complex
# redirection — see switch_and_run), so killing this script reaps that
# invocation's process group instead of leaving its own children (most
# concretely, a compiled `*.test` binary) running headless. See run_reaped's
# own doc comment for the mechanism and why it traps INT/TERM only, never
# EXIT.
#
# Hang hardening (R1-R3, #1676): the v0.0.81 cut hung for 17h19m AFTER `go
# test` had already exited — the two `gh api rate_limit` budget-probe calls
# in switch_and_run had no timeout at all (`|| echo ""` only guards a
# *failing* call, not a *hanging* one). Three independent layers now guard
# against this:
#
#   E2E_GH_API_TIMEOUT=<secs>     (R1, default 30) — every ancillary network
#                          call (currently: the two `gh api rate_limit`
#                          budget-probe calls) is wrapped in the with_timeout
#                          helper (defined below, near run_reaped) and killed
#                          — whole process group — if it exceeds this. THIS IS
#                          THE REQUIRED ROUTING POINT for any future network
#                          call added to this script or one it sources; see
#                          with_timeout's own comment.
#
#   E2E_POST_SUITE_WATCHDOG=<secs> (R2, default 300) — a background watchdog,
#                          armed the instant `go test` exits, aborts the
#                          script loudly (exit $POST_SUITE_WATCHDOG_EXIT, see
#                          below) with a diagnostic naming the stuck step
#                          (budget probe / timing report / outcome
#                          classification / backoff scan) if switch_and_run's
#                          own post-suite bookkeeping tail — normally seconds,
#                          not minutes — hasn't finished within this window.
#                          This is the exact failure mode from the v0.0.81
#                          incident: go test long gone, no watchdog to say so.
#                          A backstop against a future regression, not the
#                          expected path — R1 already removes the only two
#                          calls known to cause it.
#
#   E2E_STALL_WARN_MINUTES=<mins> (R3, default 15) — independently of R2,
#                          warns (never aborts) if the suite's own combined
#                          output has gone quiet for this long WHILE go test
#                          is still running, naming the last completed
#                          scenario. Purely advisory: a real scenario can
#                          legitimately wait on Claude for extended periods
#                          (see the TIMEOUT derivation above), so silence
#                          alone is never treated as a hang — only surfaced,
#                          so it's never mistaken for progress either. The
#                          isolated TestMergeTrainRunawayGuardPausesBatch leg
#                          (run alone, deliberately idle for long stretches)
#                          is expected to trigger this on every healthy run —
#                          see tests/e2e/README.md.
#
# R4 (non-gating): the budget probe is a report, never a gate — a timeout
# under R1 degrades exactly like the pre-existing "call failed" case (the
# `budget_before=""`/`budget_after=""` fallback), never affecting the suite's
# own exit code. R3's stall warning is likewise purely advisory and never
# touches $rc. See switch_and_run's own comments for the full mechanism, and
# tests/e2e/README.md for the defaults' derivation.

set -euo pipefail
set -m

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

# Distinct exit code for a pre-gate failure (R1, #1454) — the sim suite
# and/or the github/ wire-contract tests failed before any bed preflight,
# build, or live GitHub/Claude call was made. Distinct from PREFLIGHT_FAILED_EXIT
# (4, a bed-state problem) and BUDGET_EXHAUSTED_EXIT (3, a live-run problem):
# this one means the free layers themselves caught something, so a caller
# (e.g. cut-release.sh) can tell "cheap layer caught it, saved you the live
# run" apart from "live e2e itself failed" or "GraphQL budget exhausted."
readonly PREGATE_FAILED_EXIT=5

# Distinct exit code for R2's post-suite watchdog (#1676) firing — go test
# exited but switch_and_run's own post-suite bookkeeping (budget probe,
# timing/outcome reports, backoff scan) stalled past E2E_POST_SUITE_WATCHDOG.
# Distinct from every code above so a caller (e.g. cut-release.sh) can tell
# "the runner itself hung after the suite finished" apart from a real
# regression, a budget exhaustion, or a preflight/pre-gate failure. See
# switch_and_run's own comment and "Hang hardening" in the header above.
readonly POST_SUITE_WATCHDOG_EXIT=6

# ---------------------------------------------------------------------------
# run_reaped (R3, #1624): run "$@" as a backgrounded job in its own process
# group and reap that whole group if this script is killed while it's in
# flight, instead of leaving its children (most concretely, a compiled
# `*.test` binary) running headless after the runner that spawned them is
# gone. `set -m` (top of file) is what gives every backgrounded job its own
# process group, led by the job itself — any further children it forks
# inherit that same group, so `kill -TERM -"$pid"` (a negative PID targets
# the whole process group, not just that one process) reaches all of them.
#
# Deliberately traps only INT/TERM, never EXIT: scripts/e2e/pregate_test.sh
# and scripts/e2e/backoff_detection_test.sh `source` this file for its
# function definitions and install their OWN EXIT trap for fixture cleanup
# in that same shell (traps are shell-wide, not per-function) — an EXIT trap
# set and cleared here would clobber theirs. On a caught signal the trap
# kills the job's process group and re-exits with the shell-conventional
# 128+signum code (130 for INT, 143 for TERM) rather than letting the script
# continue past an interrupt it was told to stop for.
#
# `setsid`, the usual Linux idiom for giving a child its own process group,
# is not shipped on macOS by default, so this uses portable bash job control
# instead — the same approach scripts/sim/run.sh's own R3 fix uses.
#
# Not used for switch_and_run's main suite invocation below, which needs its
# own inline variant: that call is a multi-stage pipe (go test | tee | jq)
# whose exit code capture already depends on `pipefail`
# (see switch_and_run's own comment), and backgrounding only the pipe's last
# stage would silently lose that — see switch_and_run for the process-
# substitution restructuring that avoids the problem instead.
#
# `pid` is declared and the trap installed BEFORE "$@" is backgrounded, not
# after: a signal landing between starting the job and capturing `$!` would
# otherwise have no handler yet, leaving the just-started job unreaped — the
# exact orphan class this function exists to close (found during Review,
# #1624; mirrors switch_and_run's own fix for the identical gap). A trap's
# command string is re-expanded at signal-delivery time, not install time,
# so referencing $pid before it's assigned is safe — `kill -TERM -""` is a
# harmless no-op under `|| true`.
# ---------------------------------------------------------------------------
run_reaped() {
  local pid=""
  trap 'kill -TERM -"$pid" 2>/dev/null || true; exit 130' INT
  trap 'kill -TERM -"$pid" 2>/dev/null || true; exit 143' TERM
  "$@" &
  pid=$!
  local rc=0
  wait "$pid" || rc=$?
  trap - INT TERM
  return "$rc"
}

# ---------------------------------------------------------------------------
# disarm_deadline_signal <watcher_pid> (#1676): group-kill and reap a
# still-pending deadline-watcher job (the backgrounded `sleep <n>` + a
# conditional `kill`, as armed inline by with_timeout and switch_and_run's
# post-suite watchdog below), so it can't fire a stale, late signal against
# a since-recycled PID once the thing it was guarding has already finished
# or been otherwise handled — the same class of leak run_reaped's own
# comment (above) documents for its single backgrounded job, applied here to
# this second kind of backgrounded job.
#
# NOTE: the watcher itself is armed inline at each call site (`( sleep ...;
# if ...; then ...; fi ) &`), NOT via a shared "arm" function that returns
# the watcher's PID through `$(...)`: command substitution ALWAYS runs its
# command list in a subshell, and — confirmed empirically during Implement —
# `set -m` job control's one-process-group-per-background-job behavior does
# NOT apply inside that subshell, regardless of `set -m` being active in the
# calling shell. A job backgrounded there keeps the SUBSHELL's own process
# group instead of getting one of its own, so a later `kill -TERM -"$pid"`
# (a negative PGID) targets a process group that was never actually created
# and silently fails (`kill` exits 1, "No such process") — the guarded
# command is never killed and just runs to completion (or hangs forever)
# on its own, defeating the timeout entirely without any visible error. This
# is *why* with_timeout's own call sites below capture output via a temp
# file instead of `$(with_timeout ...)` — see with_timeout's own comment.
# Backgrounding directly in the caller's own (non-substituted) function body,
# as both call sites below do, doesn't have this problem — only the teardown
# half (an already-known PID, no substitution needed to obtain it) is safely
# shareable, hence this function exists but its counterpart does not.
disarm_deadline_signal() {
  local watcher_pid="$1"
  kill -TERM -"$watcher_pid" 2>/dev/null || true
  wait "$watcher_pid" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# with_timeout <seconds> <cmd...> (R1, #1676): run "$@", killing it (and its
# whole process group) if it hasn't finished within <seconds>. Returns 124
# (matching GNU timeout(1)'s own convention) if the deadline was hit,
# otherwise the command's own exit code.
#
# THIS IS THE REQUIRED ROUTING POINT for any future network call added to
# the release/e2e script path (this file, or any script it sources). The
# v0.0.81 cut hung for 17h19m — with `go test` long gone — because the two
# `gh api rate_limit` budget-probe calls below had no timeout at all; `||
# echo ""` only guards a *failing* call, not a *hanging* one. A bare `gh
# api`/`curl`/`wget` call added later without this wrapper can reproduce that
# exact incident. See the header comment's "Hang hardening" section.
#
# CALLERS MUST NOT INVOKE THIS VIA `$(with_timeout ...)` (command
# substitution). Confirmed empirically during Implement: command
# substitution always runs its command list in a subshell, and `set -m`
# job control's one-process-group-per-background-job behavior does not
# apply inside that subshell — the guarded command backgrounded below keeps
# the SUBSHELL's own process group rather than getting one of its own, so
# the deadline watcher's group-kill (`kill -TERM -"$pid"`, a negative PGID)
# silently fails against a process group that was never created (`kill`
# exits 1), and the guarded command just runs to completion — or hangs
# forever — on its own, defeating the timeout entirely with no visible
# error. Capture output via a temp file instead (see the budget-probe call
# sites below for the pattern) — invoked as a plain foreground statement,
# not inside `$(...)`, with_timeout runs in the caller's own shell, where
# `set -m` correctly assigns each backgrounded job its own process group.
# See disarm_deadline_signal's own comment above for the full account of
# this finding.
#
# Implemented as portable bash job control (background the command under
# `set -m`, which gives it its own process group; a second backgrounded
# `sleep <seconds>` watcher enforces the deadline) rather than via
# perl/python3 or GNU coreutils' timeout(1) — the latter isn't shipped on
# macOS (confirmed: `command not found` on the release-cut machine) and the
# former would introduce this script's first interpreter dependency where
# run_reaped/stop_bed_instance already establish this same "background job +
# deadline" idiom without one.
#
# A marker file (inside a `mktemp -d` scratch dir, not `mktemp -u` — same
# race-avoidance rationale as switch_and_run's $fifo_dir) is how the deadline
# watcher tells the main path "I actually fired" rather than inferring it
# from the guarded command's own exit status: a command that happens to exit
# with a raw 128+signum status of its own (unusual, but not impossible) must
# not be misreported as a timeout it didn't actually hit.
#
# Group-kills on timeout (`kill -TERM -"$pid"`): the guarded command may fork
# its own children (e.g. `gh`'s underlying transport), and a plain `kill`
# would leave such a child running headless — the exact orphan class
# run_reaped's own R3 (#1624) discipline exists to avoid, applied here to a
# second kind of backgrounded job. The watcher itself is torn down via
# disarm_deadline_signal (above) once no longer needed.
# ---------------------------------------------------------------------------
with_timeout() {
  local timeout_secs="$1"
  shift
  local marker_dir
  marker_dir="$(mktemp -d)"
  "$@" &
  local pid=$!
  # The watcher's own stdout/stderr are redirected away — it never produces
  # useful output of its own (its `kill` is already `2>/dev/null`), so
  # nothing is lost by closing these fds, and it keeps the watcher from
  # holding open anything the caller might itself have redirected.
  (
    sleep "$timeout_secs"
    if kill -0 "$pid" 2>/dev/null; then
      : > "$marker_dir/fired"
      kill -TERM -"$pid" 2>/dev/null
    fi
  ) >/dev/null 2>&1 &
  local watcher_pid=$!
  local rc=0
  wait "$pid" 2>/dev/null || rc=$?
  disarm_deadline_signal "$watcher_pid"
  if [ -f "$marker_dir/fired" ]; then
    echo "with_timeout: command exceeded ${timeout_secs}s, killed: $*" >&2
    rm -rf "$marker_dir"
    return 124
  fi
  rm -rf "$marker_dir"
  return "$rc"
}

# _report_gh_probe_failure <stderr_file> <label> (#1676, found during
# Review, external feedback): relay a failed gh-api-rate_limit probe's
# captured stderr as a warning, one line at a time — including
# with_timeout's own "command exceeded Ns, killed: ..." diagnostic when the
# failure was an enforced R1 timeout rather than an ordinary gh error.
#
# Exists because both real call sites below route with_timeout's entire
# invocation through a caller-side stderr redirect (previously `2>/dev/null`,
# now a captured temp file passed in as $1 here): a redirect on a
# function-call command applies to everything the function's body does, not
# just the wrapped subprocess it backgrounds — confirmed empirically:
# `f() { echo diag >&2; return 1; }; f 2>/dev/null` produces no stderr
# output at all. So the old `2>/dev/null` didn't just suppress a failing
# `gh`'s own error text (the original, intended behavior, unchanged since
# before this issue) — it also silently discarded with_timeout's OWN
# diagnostic on an actual timeout, degrading the one case this whole PR
# exists to make loud (a `gh api rate_limit` call that genuinely hung and
# got killed by the deadline watcher) into the exact same generic "could not
# read GraphQL rate_limit" message as an ordinary API failure, with nothing
# in the log distinguishing a caught hang from a normal error — defeating
# this mechanism's diagnosability goal at the only two call sites that exist
# today.
#
# Never touches $rc — this is visibility only (R4, #1676: these probes
# remain purely a report, never a gate, whether they fail via an ordinary
# error or a with_timeout-enforced kill).
_report_gh_probe_failure() {
  local errfile="$1"
  local label="$2"
  if [ -s "$errfile" ]; then
    sed "s/^/warning: ${label} probe: /" "$errfile" >&2
  fi
}

# ---------------------------------------------------------------------------
# Pre-gate (R1, #1454): refuse to spend live budget until the free, fast
# layers pass.
#
# scripts/sim/run.sh --all (the sim e2e scenarios plus simgh's own model
# tests) and the github/ wire-contract tests are BOTH already unconditional
# inside `go test -race ./...` and already run on every PR (R7 — confirmed,
# not built; see tests/sim/README.md's "Runtime and the `sim` tag decision"
# and github/wire_contract_test.go). Re-running them here, scoped, is
# deliberate rather than redundant: this script can be (and regularly is)
# invoked standalone — E2E_SKIP_PREP=1, a manual `scripts/e2e/run.sh` — and
# must never assume unit tests were "just run" by whoever invoked it.
# Running the scoped subsets (./tests/sim/... and ./github/...) rather than
# the full `go test -race ./...` keeps the pre-gate's own cost close to just
# these two layers and matches the issue's own three-way phrasing (unit ->
# sim e2e -> wire-contract tests as distinct layers) instead of blurring it
# back into one broad "run everything" step.
#
# Ordering is load-bearing, exactly like preflight_bed below: this function
# is called FIRST in the dispatch guard, strictly before prepare_bed_and_reset
# — so a pre-gate failure is proven, by construction, to have made no bed
# preflight, no build, and no live GitHub/Claude call. That ordering — not a
# runtime check inside the suite itself — is what R1's AC1 demonstrates.
# ---------------------------------------------------------------------------
run_pregate() {
  if [ -n "${E2E_SKIP_PREGATE:-}" ]; then
    echo "== pre-gate skipped (E2E_SKIP_PREGATE set) — sim/wire-contract layers assumed already green =="
    return 0
  fi

  # R5, #1624: a tree-scoped dedup signal, not a blanket opt-out. Re-resolve
  # HEAD ourselves rather than trusting the caller's claim — a stale or
  # mismatched value (wrong ref, dirty tree since the caller's own pre-gate
  # ran) always falls through to the full pre-gate below, never a silent
  # false skip. See the header comment for the full rationale.
  #
  # The SHA match alone is not sufficient: `git rev-parse HEAD` only
  # identifies the committed tree, and cut-release.sh's own step 4 (Build,
  # between the SHA being captured and this check) can rewrite
  # plugin/known_embedded_versions.go on disk without committing it —
  # a HEAD match would then be silently vouching for a tree that no longer
  # matches what was actually checked (found during Review, #1624). So this
  # also requires a clean working tree (`git status --porcelain` empty) —
  # any uncommitted change, tracked or untracked, falls through to the full
  # pre-gate exactly like a SHA mismatch does.
  if [ -n "${FABRIK_PREGATE_VERIFIED_SHA:-}" ]; then
    local current_sha dirty
    current_sha="$(git rev-parse HEAD)"
    dirty="$(git status --porcelain)"
    if [ "$FABRIK_PREGATE_VERIFIED_SHA" = "$current_sha" ] && [ -z "$dirty" ]; then
      echo "== pre-gate skipped (already verified for $current_sha in this invocation, R5 #1624) =="
      return 0
    fi
    if [ "$FABRIK_PREGATE_VERIFIED_SHA" != "$current_sha" ]; then
      echo "== pre-gate: FABRIK_PREGATE_VERIFIED_SHA ($FABRIK_PREGATE_VERIFIED_SHA) does not match HEAD ($current_sha) — running the full pre-gate =="
    else
      echo "== pre-gate: FABRIK_PREGATE_VERIFIED_SHA matches HEAD ($current_sha) but the working tree has uncommitted changes since it was verified — running the full pre-gate =="
    fi
  fi

  echo "== pre-gate: sim suite + github wire-contract tests (R1, #1454) =="
  if ! "$REPO_ROOT/scripts/sim/run.sh" --all; then
    echo "pre-gate: sim suite failed — aborting before touching the live bed or making any live call." >&2
    exit "$PREGATE_FAILED_EXIT"
  fi
  if ! run_reaped go test -race -count=1 ./github/...; then
    echo "pre-gate: github wire-contract tests failed — aborting before touching the live bed or making any live call." >&2
    exit "$PREGATE_FAILED_EXIT"
  fi
  echo "== pre-gate passed =="
}

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

  # Wrapped, not bare: under `set -e` a bare fetch aborts the script with git's
  # own exit code (128) and git's own message, so the run looks like it died of
  # nothing in particular. The most common cause is an SSH key that is not
  # loaded or no longer accepted, which reads as "Permission denied
  # (publickey)" with no indication it came from the bed preflight.
  if ! ( cd "$TEST_BED" && git fetch origin --quiet ); then
    echo "preflight: git fetch failed in $TEST_BED — cannot resolve $ref." >&2
    echo "  Most likely the SSH key for the remote is not loaded: try 'ssh-add' (see 'ssh-add -l')." >&2
    echo "  The bed is untouched; nothing was stopped, rebuilt, or reset." >&2
    exit "$PREFLIGHT_FAILED_EXIT"
  fi

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

  # The startup banner goes to the engine's STDOUT (captured in bed-run.log),
  # while ENGINE_LOG holds the structured per-item log — which never contains
  # it. Check both rather than picking one: which stream carries the banner is
  # exactly the kind of detail that drifts, and getting it wrong here fails the
  # run for a bed that is in fact perfectly healthy.
  local bed_stdout="$TEST_BED/bed-run.log"
  local i=0
  while [ "$i" -lt 90 ]; do
    if grep -qh 'Fabrik starting' "$bed_stdout" "$ENGINE_LOG" 2>/dev/null; then break; fi
    sleep 1
    i=$((i + 1))
  done

  local banner
  banner="$(grep -hm1 'Fabrik starting' "$bed_stdout" "$ENGINE_LOG" 2>/dev/null | head -1 || echo "")"
  if [ -z "$banner" ]; then
    echo "preflight: bed did not report startup within 90s — see $bed_stdout and $ENGINE_LOG" >&2
    exit "$PREFLIGHT_FAILED_EXIT"
  fi
  if ! printf '%s' "$banner" | grep -q "$want_short"; then
    echo "preflight: running bed reports '$banner' but the ref under test is $want_short" >&2
    exit "$PREFLIGHT_FAILED_EXIT"
  fi
  echo "   bed running: $banner"
}

# BED_REF_SHORT is set by preflight_bed and consumed by preflight_bed_start.
# Declared here (a bare assignment, safe on source) rather than inside the
# dispatch guard so both functions can see it under `set -u`.
BED_REF_SHORT=""

# prepare_bed_and_reset runs the side-effecting pre-run sequence. It is invoked
# ONLY from the dispatch guard at the bottom of this file, never at top level:
# everything above that guard must be safe to execute on `source`, because
# scripts/e2e/backoff_detection_test.sh sources this file for its function
# definitions and runs in CI, where no bed exists at all. An earlier revision
# called preflight at top level and broke that test (exit 4,
# "not a git checkout") — the preflight was right, its placement wasn't.
#
# Ordering here is load-bearing: preflight leaves the bed stopped, reset.sh
# runs against the stopped bed (it refuses a live one), then the engine starts.
prepare_bed_and_reset() {
  if [ -n "${E2E_SKIP_PREP:-}" ]; then
    echo "== preflight skipped (E2E_SKIP_PREP set) — bed assumed prepared =="
  else
    preflight_bed
  fi

  # Optional clean-slate reset before the run (must be the first argument).
  if [ "${1:-}" = "--clean" ]; then
    echo "== --clean: resetting the test bed via scripts/e2e/reset.sh =="
    "$REPO_ROOT/scripts/e2e/reset.sh"
    echo "== reset complete =="
  fi

  if [ -z "${E2E_SKIP_PREP:-}" ] && [ -z "${E2E_BED_NO_BUILD:-}" ]; then
    preflight_bed_start "$BED_REF_SHORT"
  fi
}

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

# Timeout for the ancillary `gh api rate_limit` budget-report calls (R1,
# #1676) — see with_timeout's own comment above. These are lightweight REST
# metadata calls (see "GraphQL budget exhaustion detection" above), so 30s is
# generous, not tight; override with E2E_GH_API_TIMEOUT.
GH_API_TIMEOUT="${E2E_GH_API_TIMEOUT:-30}"

# Window for R2's post-suite watchdog (#1676) — see switch_and_run's own
# comment for the full mechanism. Everything the watchdog covers (draining
# the tee/jq consumer, the now-with_timeout-bounded budget probe, the
# timing/outcome reports, the backoff scan) is normally seconds, not
# minutes, so 5 minutes is a generous backstop; override with
# E2E_POST_SUITE_WATCHDOG.
POST_SUITE_WATCHDOG="${E2E_POST_SUITE_WATCHDOG:-300}"

# Window for R3's stall detector (#1676), converted from minutes to seconds
# — E2E_STALL_WARN_MINUTES is documented in minutes, matching the issue's own
# phrasing. The one measured real-world inter-event gap on record (5.5
# minutes, TestReviewAuthorityClearsOnApproval, multi-scenario "on" leg — see
# "How the timeout/parallelism defaults are derived" in tests/e2e/README.md)
# sits comfortably under this 15-minute default; override with
# E2E_STALL_WARN_MINUTES.
STALL_WARN_SECS=$(( ${E2E_STALL_WARN_MINUTES:-15} * 60 ))

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

# last_completed_test_name prints the most recently observed pass/fail/skip
# top-level test name from $1 (a `go test -json` log), or "(none yet)" if
# none have completed (or the log doesn't exist/parse). Reuses the same
# terminal-action jq filter as report_test_timings (pass/fail/skip only —
# run/cont/pause carry no useful "completed" signal), but keeps stream order
# rather than sorting by elapsed, so `.[-1]` is the chronologically last
# completion. Extracted into its own function — mirroring
# detect_rate_limit_backoff's precedent — specifically so
# scripts/e2e/hang_hardening_test.sh can exercise it directly against
# fixtures, independent of an actual gate run.
#
# Used by R3's stall detector (#1676) to name what the suite was last making
# progress on when its own output went quiet — see switch_and_run below.
last_completed_test_name() {
  local jsonlog="$1"
  jq -R 'fromjson? // empty' "$jsonlog" 2>/dev/null \
    | jq -s -r '
        [ .[] | select(.Test != null and (.Test | contains("/") | not)
            and (.Action == "pass" or .Action == "fail" or .Action == "skip")) ]
        | if length == 0 then "(none yet)" else .[-1].Test end
      ' 2>/dev/null || echo "(unknown — log parse error)"
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
# the header comment. The report_test_outcomes call below is guarded with
# `|| echo warning...`: it's a standalone statement under `set -e`, so an
# unguarded jq failure there (e.g. an unexpected future `go test -json`
# schema change) would abort the script before ever reaching the
# timeout-panic check and auto-teardown that follow it — silently
# defeating the hardening this function exists to provide.
#
# Process-group reap (R3, #1624): go test itself is backgrounded (not the
# whole `tee | jq` pipe) and its stdout+stderr are routed to it via a named
# pipe on fd 3 (`mkfifo`, a backgrounded `{ tee ... | jq ...; } < "$fifo" &`
# reader, then `go test ... >&3 2>&1 &`) instead — this is NOT run_reaped,
# which backgrounds a plain command and waits for its own exit status
# directly. A pipe's last stage is what `$!`/`wait` would report if the
# whole `cmd1 | cmd2 | cmd3` were backgrounded as one job, and jq is wrapped
# in `|| true` precisely so a jq hiccup can't fail the leg — meaning
# `wait`ing on that PID would silently always see 0, discarding go test's
# real exit code the same `pipefail` trick below exists to preserve for a
# *foreground* pipeline. Backgrounding go test alone sidesteps this: `$!` is
# go test's own PID, its own exit status is what `wait` reports directly (no
# pipefail needed), and it still gets a process group of its own (`set -m`,
# top of file) that the INT/TERM traps below can kill.
#
# The consumer (`tee | jq`) is fed via a named pipe rather than a process
# substitution (`exec 3> >(tee ... | jq ...)`), and backgrounded as a real
# job (`{ ...; } &`, capturing its own `$!` as consumer_pid) rather than left
# anonymous, specifically so it too gets its own process group under `set
# -m` and can be group-killed exactly like $suite_pid. A process substitution
# does NOT get its own process group — traced during review: killing only
# its own top-level PID (with or without the `-`-prefixed group form; the
# group form is simply a no-op there, since no such group exists) leaves the
# `tee`/`jq` pipeline's own descendants running, reparented to pid 1 — i.e.
# it reproduces exactly the orphan class this whole issue exists to close,
# just one process stage further down. The named-pipe form was confirmed
# (during review) to fully reap `tee` and `jq` together via
# `kill -TERM -"$consumer_pid"`, the same idiom used for `$suite_pid` below.
# `mkfifo` + `exec 3> "$fifo"` is also what makes the consumer job's `$!`
# capturable *before* go test starts writing to fd 3 — the `exec 3> "$fifo"`
# open blocks until the backgrounded reader has already opened its end (FIFO
# open rendezvous), so it is safe to `rm -rf "$fifo_dir"` (the directory
# $fifo lives in — see below) immediately afterward: both ends are already
# connected by then, and removing it only removes the now-unneeded directory
# entry, not the open file descriptors.
#
# The consumer's own PID is captured (rather than relying on `wait
# "$suite_pid"` alone) specifically so it can be `wait`ed on too:
# `wait "$suite_pid"` only blocks until go test exits, not until the tee/jq
# consumer reading its now-closed pipe has finished flushing $jsonlog —
# without a second, explicit `wait` on the consumer, report_test_timings/
# report_test_outcomes/the E2E_TIMEOUT grep below can race a still-writing
# consumer and observe a truncated or momentarily-empty $jsonlog (reproduced
# with a deliberately slow consumer during review).
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
#
# Hang hardening (R1-R3, #1676): the two `gh api rate_limit` budget-probe
# calls below are wrapped in with_timeout; a background stall detector warns
# (never aborts) if the suite's own output goes quiet for too long while go
# test is still alive; and a background post-suite watchdog aborts loudly,
# naming the stuck step, if go test has exited but this function's own
# bookkeeping tail stalls past its own window. See each mechanism's own
# comment further down for the full detail, and the header comment's "Hang
# hardening" section for the incident this all exists to prevent.

# stop_post_suite_watchdog tears down R2's background watcher (see
# switch_and_run below, arm_deadline_signal above) and clears the INT/TERM
# trap it (temporarily) owns for the post-suite tail. Called at every one of
# switch_and_run's own exit points (both normal returns and the
# BUDGET_EXHAUSTED_EXIT path) so a not-yet-fired watcher from one leg can
# never survive into, and misfire against, a later leg's unrelated activity
# — switch_and_run runs once per leg, up to 3x per invocation (off, on-main,
# on-isolated).
stop_post_suite_watchdog() {
  local wpid="$1"
  local wdir="$2"
  trap - INT TERM
  disarm_deadline_signal "$wpid"
  rm -rf "$wdir" 2>/dev/null || true
}

# _post_suite_watchdog_signal <exit_code_if_external> (#1676): shared TERM/INT
# handler for R2's post-suite watchdog window, installed by switch_and_run
# below for the duration of its own post-suite tail. Relies on bash's dynamic
# scoping of `local` variables — $mode/$suite_pid/$consumer_pid/$fifo_dir/
# $watchdog_dir/$watchdog_pid/$suite_exit_epoch are all still in scope from
# the still-executing switch_and_run call this trap fires inside of, exactly
# as the inline trap body it replaces already relied on for the same
# variables.
#
# Distinguishes "this is our own watchdog firing" (the $watchdog_dir/fired
# marker is present) from "some other signal landed in this window" (marker
# absent — a real Ctrl-C, or an external `kill` of the whole script), but
# BOTH branches now tear down the same set of resources before exiting
# (found during Review, #1676 — two rounds of external review feedback, see
# below): $suite_pid/$consumer_pid are group-killed, $watchdog_pid is
# disarmed, and $fifo_dir/$watchdog_dir are removed, regardless of which
# branch is taken. Earlier revisions diverged on this:
#
#   - The FIRED branch alone used to skip the suite/consumer group-kill
#     entirely, just printing the diagnostic and exiting. That is backwards:
#     the fired branch is the realistic "actually stuck" case this watchdog
#     exists to catch — the checkpoint file's own text ("waiting for tee/jq
#     consumer to drain") shows the most likely stuck step is the tee/jq
#     consumer drain itself, and every OTHER step in this tail is either
#     already timeout-bounded via with_timeout or a fast local jq/grep call —
#     so $consumer_pid is exactly the process most likely to still be alive
#     and wedged at the moment this branch runs, and leaving it running
#     headless after the script exits is precisely the orphan class
#     run_reaped's own R3 (#1624) discipline exists to avoid.
#   - Neither branch used to disarm $watchdog_pid itself: an earlier
#     revision's TERM-only trap left the still-sleeping watchdog timer
#     running whenever its "external signal" branch fired (its own kill
#     target is this script's now-exiting PID, so the leaked job would
#     eventually no-op after its own sleep rather than misfire, but it was
#     still an unreaped background job for up to $POST_SUITE_WATCHDOG
#     seconds) — and was never wired to INT at all, so a Ctrl-C landing in
#     this exact window fell through to switch_and_run's EARLIER INT trap
#     (installed before the consumer wait, for the suite/consumer-drain
#     phase), which has no idea $watchdog_pid or $watchdog_dir exist and left
#     both behind.
#
# Installed for both signals below so INT and TERM behave identically here,
# matching this whole mechanism's own "process-group discipline should
# extend to any new backgrounded watcher job" design constraint (see
# with_timeout's and disarm_deadline_signal's own comments).
_post_suite_watchdog_signal() {
  local external_exit="$1"
  local fired=0
  [ -f "$watchdog_dir/fired" ] && fired=1

  if [ "$fired" -eq 1 ]; then
    {
      echo ""
      echo "############################################################"
      echo "## POST-SUITE WATCHDOG (leg: ${mode}): go test exited $(( $(date +%s) - suite_exit_epoch ))s ago,"
      echo "## but the script has not progressed past its post-suite steps"
      echo "## within ${POST_SUITE_WATCHDOG}s (E2E_POST_SUITE_WATCHDOG)."
      echo "## Stuck in: $(cat "$watchdog_dir/checkpoint" 2>/dev/null || echo "(unknown step)")"
      echo "## Last engine log line: $(tail -n1 "$ENGINE_LOG" 2>/dev/null || echo "(unavailable)")"
      echo "############################################################"
    } >&2
  fi

  kill -TERM -"$suite_pid" 2>/dev/null || true
  kill -TERM -"$consumer_pid" 2>/dev/null || true
  disarm_deadline_signal "$watchdog_pid"
  rm -rf "$fifo_dir" 2>/dev/null || true
  rm -rf "$watchdog_dir" 2>/dev/null || true

  if [ "$fired" -eq 1 ]; then
    exit "$POST_SUITE_WATCHDOG_EXIT"
  fi
  exit "$external_exit"
}

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
  # rather than actually stuck. It is, however, still run via run_reaped
  # (R3, #1624) so a kill of this script during the restart doesn't leave its
  # own test binary running behind it.
  E2E_TRAIN_SWITCH=1 E2E_TRAIN_MODE="$mode" run_reaped go test -tags=e2e -v -count=1 -timeout 3m \
    -run '^TestSwitchTrainMode$' ./tests/e2e/...
  echo "== running suite with E2E_TRAIN_MODE=${mode}, -parallel=${parallel} =="
  local jsonlog="${TMPDIR:-/tmp}/fabrik-e2e-${mode}-$$.json"
  local rc=0

  # Snapshot the bed's GraphQL budget right before the suite runs — this is
  # purely for the A3 cost report, which should reflect the suite's own
  # consumption, not the restart step's (unlike the backoff scan below, which
  # deliberately covers the restart too — see its own comment for why the two
  # have different windows). Wrapped in with_timeout (R1, #1676) so a hung
  # `gh api` call can never block the script — see with_timeout's own
  # comment above. Captured via a temp file, NOT `$(with_timeout ...)` —
  # with_timeout's own comment explains why command substitution would
  # silently defeat its group-kill entirely. A failed `with_timeout` (gh
  # error OR an enforced kill) both degrade to a skipped report rather than
  # aborting the script under `set -e` (R4, #1676: this probe is a report,
  # never a gate) — but, unlike stdout, stderr is captured to a temp file
  # rather than discarded (`2>/dev/null` would also silently swallow
  # with_timeout's own timeout diagnostic — see _report_gh_probe_failure's
  # own comment, found during Review, #1676) and relayed as a warning on
  # failure.
  local budget_before budget_after
  budget_before=""
  if [ -n "$BED_TOKEN" ]; then
    local budget_before_tmp budget_before_err
    budget_before_tmp="$(mktemp)"
    budget_before_err="$(mktemp)"
    if with_timeout "$GH_API_TIMEOUT" env "GH_TOKEN=$BED_TOKEN" gh api rate_limit --jq '.resources.graphql.remaining' \
        > "$budget_before_tmp" 2>"$budget_before_err"; then
      budget_before="$(cat "$budget_before_tmp" 2>/dev/null || echo "")"
    else
      _report_gh_probe_failure "$budget_before_err" "budget_before (leg: ${mode})"
    fi
    rm -f "$budget_before_tmp" "$budget_before_err"
  fi

  # The consumer (tee | jq) is fed via a named pipe and backgrounded as its
  # own real job, so it gets its own process group and can be group-killed
  # exactly like $suite_pid — see the R3 comment above for why a process
  # substitution can't do this safely. mkfifo's rendezvous means the
  # `rm -f "$fifo"` right after `exec 3>` is race-free: that `exec` blocks
  # until the backgrounded reader below has already opened its end.
  #
  # $suite_pid is declared (empty) and the INT/TERM trap installed
  # immediately after the consumer is backgrounded — before go test is even
  # started — rather than after both are launched: a signal landing in that
  # gap would hit bash's default disposition (immediate termination, no
  # handler run) and orphan whatever had already started, exactly the class
  # of leak this whole fix exists to close (found during Review). Since a
  # trap's command string is re-expanded at signal-delivery time, not at
  # install time, referencing `$suite_pid` here is safe even before it's
  # assigned below — `kill -TERM -""` is a harmless no-op under `|| true`.
  # The trap also removes $fifo_dir (found during Review): $fifo is already
  # set by this point, so it's safe to reference immediately, and without
  # this a signal landing between `mkfifo` and the unconditional `rm -f
  # "$fifo"` below (normal-path cleanup, after the writer end opens) would
  # leave a stray named pipe behind in $TMPDIR — a minor leak, not a process
  # orphan, but avoidable the same way. $stall_pid (R3, #1676 — see below) is
  # declared and added to the same trap for the identical reason.
  #
  # $fifo lives inside its own `mktemp -d` directory rather than being named
  # directly by `mktemp -u` (found during Review): `-u` only reserves a
  # name, it doesn't create anything, so another process (or a pre-existing
  # file) could occupy that path between the reservation and `mkfifo`,
  # aborting the script under `set -euo pipefail`. `mktemp -d` creates the
  # directory atomically, so a path inside it cannot collide with anything
  # else — `mkfifo` there is race-free. Cleanup removes the whole directory
  # (`rm -rf`, not just the fifo) so nothing is left behind either way.
  local fifo_dir fifo
  fifo_dir="$(mktemp -d)"
  fifo="$fifo_dir/fifo"
  mkfifo "$fifo"
  local suite_pid=""
  local stall_pid=""
  { tee "$jsonlog" | { jq -R -r 'fromjson? // empty | select(.Action=="output") | .Output' 2>/dev/null || true; }; } < "$fifo" &
  local consumer_pid=$!
  trap 'kill -TERM -"$suite_pid" 2>/dev/null || true; kill -TERM -"$consumer_pid" 2>/dev/null || true; kill -TERM -"$stall_pid" 2>/dev/null || true; rm -rf "$fifo_dir" 2>/dev/null || true; exit 130' INT
  trap 'kill -TERM -"$suite_pid" 2>/dev/null || true; kill -TERM -"$consumer_pid" 2>/dev/null || true; kill -TERM -"$stall_pid" 2>/dev/null || true; rm -rf "$fifo_dir" 2>/dev/null || true; exit 143' TERM
  exec 3> "$fifo"
  rm -rf "$fifo_dir"

  E2E_TRAIN_MODE="$mode" go test -tags=e2e -json -count=1 -timeout "$TIMEOUT" -parallel "$parallel" \
      ./tests/e2e/... "$@" >&3 2>&1 &
  suite_pid=$!
  exec 3>&-

  # R3 (#1676): stall detector. Independently of whether go test itself has
  # exited (that's R2, below), warn if the suite's own combined output has
  # gone quiet for E2E_STALL_WARN_MINUTES while go test is still running —
  # a real scenario can legitimately wait on Claude for extended periods
  # (see the header comment's TIMEOUT derivation), so silence alone must
  # never be mistaken for either progress or a hang. This only ever warns —
  # it never touches $rc or aborts anything (R4-adjacent: non-gating,
  # mirroring the budget probe's own requirement even though R4's text is
  # written specifically about that probe).
  #
  # Polls $jsonlog's mtime (not the terminal/tee output a human might be
  # watching) every 60s while `kill -0 "$suite_pid"` still succeeds — mtime
  # is the only reliable "is this still producing output" signal for a
  # stopped/redirected run (`bash scripts/e2e/run.sh &`, matching the
  # v0.0.81 incident's own `ps` evidence), where no one is watching a
  # terminal. On a full E2E_STALL_WARN_MINUTES of no mtime change, logs a
  # warning naming the last completed scenario (via last_completed_test_name)
  # and the silence duration, then resets its own counter — so a run that
  # stays quiet for a long time (e.g. the isolated
  # TestMergeTrainRunawayGuardPausesBatch leg — see tests/e2e/README.md)
  # warns once per window rather than once ever or on every single poll.
  (
    prev_mtime=0
    stall_secs=0
    check_interval=60
    while kill -0 "$suite_pid" 2>/dev/null; do
      sleep "$check_interval"
      kill -0 "$suite_pid" 2>/dev/null || break
      cur_mtime=$(stat -f %m "$jsonlog" 2>/dev/null || stat -c %Y "$jsonlog" 2>/dev/null || echo 0)
      if [ "$cur_mtime" = "$prev_mtime" ]; then
        stall_secs=$((stall_secs + check_interval))
      else
        stall_secs=0
      fi
      prev_mtime="$cur_mtime"
      if [ "$stall_secs" -ge "$STALL_WARN_SECS" ]; then
        echo "== STALL WARNING (leg: ${mode}): no new suite output for ${stall_secs}s (E2E_STALL_WARN_MINUTES=$((STALL_WARN_SECS / 60))) — go test is still running. Last completed scenario: $(last_completed_test_name "$jsonlog") ==" >&2
        stall_secs=0
      fi
    done
  ) &
  stall_pid=$!

  wait "$suite_pid" || rc=$?
  # go test has exited — stop the stall detector immediately (its own scope
  # is "while go test is still running", nothing further for it to watch)
  # rather than waiting up to 60s for its own loop condition to notice.
  kill -TERM -"$stall_pid" 2>/dev/null || true
  wait "$stall_pid" 2>/dev/null || true

  # R2 (#1676): post-suite watchdog. go test itself has now exited — from
  # here through this function's return/exit is bookkeeping (draining the
  # consumer, the now-with_timeout-bounded budget probe, timing/outcome
  # reports, the backoff scan) that should normally take seconds. The
  # v0.0.81 cut hung for ~17 hours inside exactly this tail (the unwrapped
  # `gh api rate_limit` calls, fixed by R1 above) with go test long gone and
  # no watchdog to say so. In the common case this should never fire — R1
  # already removes the only two calls known to cause it — it exists as a
  # backstop against a future regression (a call added to this tail without
  # routing through with_timeout), not the expected path.
  #
  # Mechanism: a scratch dir holds a `checkpoint` file (updated as this tail
  # progresses below) and a `fired` marker a directly-backgrounded watcher
  # touches only if it actually times out (armed inline here, NOT via a
  # shared "arm" function returning a PID through `$(...)` — command
  # substitution's subshell semantics make a background job started inside
  # it block the substitution until that job finishes, defeating the whole
  # point; see disarm_deadline_signal's own comment above for the full
  # account of this, found empirically during Implement). The watcher
  # signals the whole running script's PID ($$), not a process group —
  # there's nothing group-shaped to kill here, just this function's own trap
  # to invoke. The TERM and INT traps installed for this window (both routed
  # through the shared _post_suite_watchdog_signal, defined above) check
  # `fired` at fire time: if present, this was our own watchdog — it prints
  # a diagnostic naming the elapsed time and the last checkpoint reached,
  # THEN (found during Review, #1676, second round of external feedback)
  # group-kills $suite_pid/$consumer_pid exactly like the non-watchdog branch
  # below, before exiting $POST_SUITE_WATCHDOG_EXIT: an earlier revision
  # skipped that teardown specifically in the fired branch, which is
  # backwards — the checkpoint text ("waiting for tee/jq consumer to drain")
  # and every other step in this tail being either with_timeout-bounded or a
  # fast local jq/grep call means $consumer_pid is exactly the process most
  # likely to still be alive and wedged at the moment this branch actually
  # runs, so skipping its group-kill left the one realistic "stuck" case
  # orphaning the very pipeline the watchdog was built to catch. If `fired`
  # is absent, this was some other TERM/INT (e.g. an external kill, or
  # Ctrl-C) landing in this same window, and it runs the identical
  # suite/consumer-group-kill teardown this function already had here before
  # this watchdog existed — both branches now also disarm $watchdog_pid
  # itself (found during Review, #1676, first round: neither the original
  # TERM-only trap's fallback branch nor, for INT specifically, any trap in
  # this window at all used to do this, leaking the watchdog's background
  # timer job). An external signal during this window behaves exactly as it
  # did before, it just also gets diagnosed correctly against a genuine
  # watchdog firing, and neither branch leaks a background job doing so.
  # These traps stay installed for the whole remaining tail (unlike the old
  # suite/consumer traps they replace, which were cleared right after the
  # consumer wait) — by design, since the watched window is the whole tail,
  # not just the consumer drain.
  local watchdog_dir
  watchdog_dir="$(mktemp -d)"
  echo "waiting for tee/jq consumer to drain" > "$watchdog_dir/checkpoint"
  local suite_exit_epoch
  # Only referenced from inside the single-quoted TERM trap below,
  # re-expanded at signal-delivery time — invisible to shellcheck's static
  # analysis, same as $mode/$watchdog_dir in that trap.
  # shellcheck disable=SC2034
  suite_exit_epoch=$(date +%s)
  local watchdog_main_pid=$$
  # Stdout/stderr explicitly redirected away, mirroring with_timeout's own
  # watcher (see its comment) — this function isn't currently called via
  # command substitution, but there's nothing useful in this watcher's own
  # output anyway (its `kill` is already `2>/dev/null`), so closing these
  # fds costs nothing and avoids the same class of pipe-holds-open hazard
  # if that ever changes.
  (
    sleep "$POST_SUITE_WATCHDOG"
    if kill -0 "$watchdog_main_pid" 2>/dev/null; then
      : > "$watchdog_dir/fired"
      kill -TERM "$watchdog_main_pid" 2>/dev/null
    fi
  ) >/dev/null 2>&1 &
  local watchdog_pid=$!
  # Installed for BOTH signals (found during Review, #1676 — see
  # _post_suite_watchdog_signal's own comment above): an INT (Ctrl-C) landing
  # in this exact window must be handled identically to a TERM, not silently
  # fall through to the earlier suite/consumer-drain-phase INT trap installed
  # above, which knows nothing about $watchdog_pid/$watchdog_dir.
  trap '_post_suite_watchdog_signal 143' TERM
  trap '_post_suite_watchdog_signal 130' INT

  # Wait for the consumer to drain and finish writing $jsonlog before any of
  # it is read below — see the comment above. Still under a TERM trap (the
  # post-suite watchdog's, installed just above), so a stuck drain is still
  # covered by R2 rather than left unbounded with no signal path.
  wait "$consumer_pid" 2>/dev/null || true

  echo "gh api rate_limit budget_after probe" > "$watchdog_dir/checkpoint"
  budget_after=""
  if [ -n "$BED_TOKEN" ]; then
    # Captured via a temp file, NOT `$(with_timeout ...)` — same rationale
    # as budget_before above; see with_timeout's own comment. stderr is also
    # captured (not discarded) and relayed on failure — same rationale as
    # budget_before above; see _report_gh_probe_failure's own comment.
    local budget_after_tmp budget_after_err
    budget_after_tmp="$(mktemp)"
    budget_after_err="$(mktemp)"
    if with_timeout "$GH_API_TIMEOUT" env "GH_TOKEN=$BED_TOKEN" gh api rate_limit --jq '.resources.graphql.remaining' \
        > "$budget_after_tmp" 2>"$budget_after_err"; then
      budget_after="$(cat "$budget_after_tmp" 2>/dev/null || echo "")"
    else
      _report_gh_probe_failure "$budget_after_err" "budget_after (leg: ${mode})"
    fi
    rm -f "$budget_after_tmp" "$budget_after_err"
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

  echo "report_test_timings" > "$watchdog_dir/checkpoint"
  report_test_timings "$jsonlog" "$mode" \
    || echo "warning: failed to compute test timings (jq error) — inspect the raw JSON log directly: $jsonlog" >&2

  if [ "$rc" -ne 0 ]; then
    echo "failure classification / teardown (leg ${mode} failed, rc=${rc})" > "$watchdog_dir/checkpoint"
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

  # R2 (#1527, NOT this issue's #1676 post-suite-watchdog R2 above — the two
  # numbering schemes collide by coincidence): check whether the engine's own
  # rate-limit backoff engaged at any point during this leg, regardless of
  # $rc — a throttled run can just as easily present as a pass (if timeouts
  # happened to land after the relevant waits already succeeded) as a fail,
  # so this must not be conditioned on the leg having failed. See
  # detect_rate_limit_backoff's own comment (above its definition) for why
  # this scans the whole current fabrik.log rather than a captured byte
  # offset — extracted into its own function so
  # scripts/e2e/backoff_detection_test.sh can exercise it directly against
  # fixtures without running an actual gate leg.
  echo "detect_rate_limit_backoff scan" > "$watchdog_dir/checkpoint"
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
    stop_post_suite_watchdog "$watchdog_pid" "$watchdog_dir"
    exit "$BUDGET_EXHAUSTED_EXIT"
  fi

  stop_post_suite_watchdog "$watchdog_pid" "$watchdog_dir"

  if [ "$rc" -ne 0 ]; then
    return "$rc"
  fi
}

# Guarded so scripts/e2e/backoff_detection_test.sh and
# scripts/e2e/pregate_test.sh can `source` this file to reach
# detect_rate_limit_backoff / run_pregate (and the other helper functions
# above) without triggering an actual gate run. Everything above this guard
# (repo root resolution, TEST_BED/ENGINE_LOG/BED_TOKEN setup, function
# definitions) is safe to execute on source — read-only or pure function
# definitions, no gate invocation. When executed directly (./run.sh or
# `bash run.sh`), BASH_SOURCE[0] equals $0, so this still dispatches exactly
# as before this guard existed.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  # Pre-gate FIRST (R1, #1454) — strictly before any bed preflight, build,
  # restart, or live GitHub/Claude call. See run_pregate's own comment for
  # why this ordering is the mechanism, not a runtime check.
  run_pregate

  # Bed preflight + optional --clean reset. Inside the guard so sourcing this
  # file (backoff_detection_test.sh / pregate_test.sh) never touches a bed —
  # or, in CI, aborts on the absence of one.
  prepare_bed_and_reset "$@"
  # --clean is consumed here, not inside the function: `shift` there would only
  # affect the function's own positional parameters, leaving --clean in "$@"
  # to be passed on to `go test` as an unknown flag.
  if [ "${1:-}" = "--clean" ]; then
    shift
  fi

  # Scenarios that deliberately exhaust a repo's merge-train state, and so
  # cannot share that repo with anything else under "on".
  #
  # TestMergeTrainRunawayGuardPausesBatch queues poison members on RepoBeta
  # until the runaway guard fires — that IS the scenario. The guard's state is
  # keyed per repo with a 1h window (ADR-059 D8), so for the next hour every
  # Queued member on Beta is paused before dispatch. TestCrossRepoSpawn puts
  # its child on Beta, so under "on" it inherits that pause, never closes, and
  # its parent blocks until the scenario times out ~52 minutes later.
  #
  # Measured 2026-08-13 across two independent runs (-parallel 4 and 2):
  # guard fired 16:47:46, cross-repo child queued 17:41:44 and was paused by
  # the already-tripped guard. The engine is correct at every step; the bed
  # scheduling is what's wrong.
  #
  # Only Alpha and Beta exist, and neither is free: Alpha hosts the other four
  # train scenarios (poisoning it would be strictly worse) and cross-repo
  # spawn structurally needs two distinct repos, so it cannot vacate Beta.
  # Temporal separation is therefore the available fix, not relocation — and a
  # different project board would not help, since the guard is keyed by repo
  # and one engine instance serves both.
  #
  # Running it last also lowers the peak concurrent Queued/awaiting-CI
  # population, which is what drives the ADR-1270/ADR-1208 settle scans'
  # GraphQL cost (#1527) — the same cost that invalidated both on-leg runs
  # that day with a rate-limit backoff.
  TRAIN_ISOLATED_RE='TestMergeTrainRunawayGuardPausesBatch'

  # A caller-supplied -run means they are targeting specific scenarios; honour
  # that exactly rather than forcing an isolated leg they did not ask for.
  caller_has_run=0
  for a in "$@"; do
    case "$a" in -run | -run=* | --run | --run=*) caller_has_run=1 ;; esac
  done

  if [ -n "${E2E_TRAIN_MODE:-}" ]; then
    # Single mode forced by the caller — one switch + one suite invocation.
    # Always uses E2E_PARALLEL (not E2E_PARALLEL_ON), unchanged from before
    # E2E_PARALLEL_ON existed — see the header comment.
    if [ "$E2E_TRAIN_MODE" = "on" ] && [ "$caller_has_run" -eq 0 ]; then
      switch_and_run on "$PARALLEL" -skip "$TRAIN_ISOLATED_RE" "$@"
      switch_and_run on "$PARALLEL" -run "^(${TRAIN_ISOLATED_RE})\$"
    else
      switch_and_run "$E2E_TRAIN_MODE" "$PARALLEL" "$@"
    fi
  else
    # Default: the full validation gate. "off" first — see header comment for
    # why. "on" gets the tighter E2E_PARALLEL_ON cap, and is split into a main
    # leg plus an isolated leg for the scenarios above. switch_and_run
    # restarts the bed each time, which also clears the in-memory guard state
    # — so the isolation is explicit, not merely dependent on ordering.
    switch_and_run off "$PARALLEL" "$@"
    if [ "$caller_has_run" -eq 0 ]; then
      switch_and_run on "$PARALLEL_ON" -skip "$TRAIN_ISOLATED_RE" "$@"
      switch_and_run on "$PARALLEL_ON" -run "^(${TRAIN_ISOLATED_RE})\$"
    else
      switch_and_run on "$PARALLEL_ON" "$@"
    fi
  fi
fi
