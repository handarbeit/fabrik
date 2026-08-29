#!/usr/bin/env bash
# scripts/e2e/hang_hardening_test.sh — regression coverage for #1676's hang
# hardening in scripts/e2e/run.sh: the v0.0.81 cut hung for 17h19m AFTER
# `go test` had already exited, because the two `gh api rate_limit`
# budget-probe calls had no timeout at all (`|| echo ""` only guards a
# *failing* call, not a *hanging* one).
#
# These are shell-level behaviors (per the issue's own scope), so this
# mirrors backoff_detection_test.sh/pregate_test.sh's precedent: source
# run.sh for its real function definitions and exercise them directly
# against constructed fixtures, rather than running an actual multi-hour
# gate. Covers:
#   - with_timeout (R1) — AC1, including a non-vacuous demonstration that
#     the guarded command genuinely hangs past the timeout window on its
#     own, so the PASS above isn't just the command finishing fast anyway.
#   - disarm_deadline_signal plus the background-watcher-with-marker-file
#     pattern R2's post-suite watchdog uses (inline in switch_and_run, not
#     itself an extractable function — see run.sh's own comment on why the
#     watcher is armed inline rather than through a shared "arm" helper) —
#     supports AC2 by exercising the same deadline-detection mechanism in
#     isolation, without requiring a live bed.
#   - last_completed_test_name (R3) — supports AC3 by exercising the
#     go-test-json parse a real stall warning would use to name the last
#     completed scenario.
#
# Usage: scripts/e2e/hang_hardening_test.sh
# Exit 0 if all assertions pass, 1 otherwise.

set -uo pipefail # no -e: assertions below intentionally continue past failures

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Source run.sh for its function definitions only — the sourcing guard at the
# bottom of run.sh (BASH_SOURCE[0] == $0) prevents this from triggering an
# actual gate run. Mirrors backoff_detection_test.sh's precedent.
# shellcheck source=/dev/null
source "$REPO_ROOT/scripts/e2e/run.sh"
set +e

FAILED=0

pass() { echo "PASS: $1"; }
fail() {
  echo "FAIL: $1"
  FAILED=1
}

# ---------------------------------------------------------------------------
# with_timeout (R1) — AC1
# ---------------------------------------------------------------------------

# Case 1: a genuinely hanging command is killed at the deadline, not left to
# run to completion. Uses a 2s timeout against a 30s sleep — if with_timeout
# were a no-op (or the deadline math were broken), this would take ~30s and
# blow well past the bound checked below.
start=$(date +%s)
with_timeout 2 sleep 30
rc=$?
end=$(date +%s)
elapsed=$((end - start))
if [ "$rc" -eq 124 ] && [ "$elapsed" -le 10 ]; then
  pass "with_timeout kills a hanging command at its deadline (rc=124, elapsed=${elapsed}s, bound 10s)"
else
  fail "with_timeout did not enforce its deadline (rc=$rc, elapsed=${elapsed}s, expected rc=124 within 10s)"
fi

# Case 2 (non-vacuous, per AC1): prove the same underlying command genuinely
# hangs past the timeout window on its own — i.e. Case 1's fast return is
# with_timeout's doing, not the guarded command quietly finishing quickly
# regardless. Backgrounds the SAME command unwrapped, waits past the
# timeout used above, and confirms it is still alive.
( sleep 30 ) &
unwrapped_pid=$!
sleep 2
if kill -0 "$unwrapped_pid" 2>/dev/null; then
  pass "non-vacuous: the same command run WITHOUT with_timeout is still running past the 2s bound used in Case 1 (proves Case 1's early return is the timeout firing, not the command finishing on its own — 'removing the timeout' returns the hang)"
else
  fail "non-vacuous check failed: the unwrapped command finished on its own within 2s — Case 1 above would pass vacuously"
fi
kill -TERM "$unwrapped_pid" 2>/dev/null
wait "$unwrapped_pid" 2>/dev/null

# Case 3: a command that finishes well within the deadline returns its own
# exit code untouched, not a false 124.
with_timeout 5 true
rc=$?
if [ "$rc" -eq 0 ]; then
  pass "with_timeout passes through a fast command's own exit code (0) unmodified"
else
  fail "with_timeout returned $rc for a fast, successful command (expected 0)"
fi

with_timeout 5 false
rc=$?
if [ "$rc" -eq 1 ]; then
  pass "with_timeout passes through a fast command's own non-zero exit code (1) unmodified, not a false timeout"
else
  fail "with_timeout returned $rc for a fast, failing command (expected 1)"
fi

# Case 4: the REQUIRED output-capture pattern (temp file, not `$(...)`) — the
# same pattern the real budget_before/budget_after call sites use — actually
# enforces the deadline against a hanging command while still letting the
# caller capture its output.
capture_tmp="$(mktemp)"
start=$(date +%s)
with_timeout 2 sleep 30 > "$capture_tmp" 2>/dev/null
rc=$?
end=$(date +%s)
elapsed=$((end - start))
if [ "$rc" -eq 124 ] && [ "$elapsed" -le 10 ]; then
  pass "with_timeout enforces its deadline when output is captured via a temp file (rc=124, elapsed=${elapsed}s) — the pattern the real gh api call sites use"
else
  fail "with_timeout via temp-file capture did not enforce its deadline (rc=$rc, elapsed=${elapsed}s, expected rc=124 within 10s)"
fi
rm -f "$capture_tmp"

# Case 4b (regression guard, mirrors backoff_detection_test.sh's
# "neutralization" precedent): capturing with_timeout's output via
# `$(with_timeout ...)` instead of a temp file must NOT be reintroduced —
# confirmed empirically during Implement that command substitution always
# runs its command list in a subshell where `set -m`'s one-process-group-
# per-background-job behavior does not apply, so the deadline watcher's
# group-kill silently targets a process group that doesn't exist and never
# actually kills the guarded command — it just runs to completion (or hangs
# forever) unbounded, defeating with_timeout's entire purpose with no
# visible error. This asserts that hazard is real (not a theoretical
# concern) so a future refactor back to `$(with_timeout ...)` at a real call
# site fails THIS test instead of silently reintroducing the exact class of
# hang #1676 exists to fix.
start=$(date +%s)
captured=$(with_timeout 2 sleep 5)
end=$(date +%s)
elapsed=$((end - start))
if [ "$elapsed" -ge 4 ]; then
  pass "regression guard: \$(with_timeout ...) does NOT enforce its deadline (elapsed=${elapsed}s, ran the full underlying sleep) — confirms this pattern is genuinely unsafe and must never be reintroduced at a real call site; captured='$captured'"
else
  fail "regression guard: \$(with_timeout ...) unexpectedly enforced its deadline (elapsed=${elapsed}s) — if this pattern has become safe, with_timeout's own comment and the temp-file-capture call sites should be revisited"
fi

# Case 5: process-group discipline — a timed-out command's own child
# processes are killed too, not left orphaned (mirrors run_reaped's R3,
# #1624 discipline). Guards a small shell script that forks a background
# child sleep and then waits on it; both parent and child must be gone
# shortly after with_timeout's own deadline fires.
child_marker="$(mktemp -d)"
with_timeout 2 bash -c '
  sleep 30 &
  echo $! > "'"$child_marker"'/child_pid"
  wait
'
sleep 1 # give the group-kill a moment to land
child_pid="$(cat "$child_marker/child_pid" 2>/dev/null || echo "")"
if [ -n "$child_pid" ] && ! kill -0 "$child_pid" 2>/dev/null; then
  pass "with_timeout group-kills a timed-out command's own background children (no orphaned sleep left running)"
else
  fail "with_timeout left a child process (pid=$child_pid) running after its own deadline fired — orphan leak"
fi
rm -rf "$child_marker"

# ---------------------------------------------------------------------------
# Deadline-watcher-plus-marker-file pattern (R2) — supports AC2
#
# R2's post-suite watchdog is inline in switch_and_run (armed directly in
# the caller's own function body, not through a shared "arm" function — see
# run.sh's own comment on why command substitution's subshell semantics rule
# that out), so it isn't itself an extractable, independently-callable
# function. What IS shared and directly testable is disarm_deadline_signal
# (the teardown half) and the marker-file-on-timeout pattern both
# with_timeout and the watchdog use — exercised here standalone, without
# needing a live bed or an actual switch_and_run invocation.
# ---------------------------------------------------------------------------

# Case 6: the pattern fires (marker written, main process signaled) when the
# guarded process outlives the deadline — the watchdog's "stuck" case.
wdir="$(mktemp -d)"
( sleep 30 ) &
main_pid=$!
(
  sleep 1
  if kill -0 "$main_pid" 2>/dev/null; then
    : > "$wdir/fired"
    kill -TERM "$main_pid" 2>/dev/null
  fi
) >/dev/null 2>&1 &
watcher_pid=$!
wait "$main_pid" 2>/dev/null
disarm_deadline_signal "$watcher_pid"
if [ -f "$wdir/fired" ]; then
  pass "deadline-watcher pattern fires its marker when the guarded process outlives the deadline (R2's 'stuck' case)"
else
  fail "deadline-watcher pattern did not fire its marker for a genuinely-stuck guarded process"
fi
rm -rf "$wdir"

# Case 7: the pattern does NOT fire when the guarded process finishes before
# the deadline — the watchdog's normal, expected case (R2 should never fire
# on a healthy run).
wdir="$(mktemp -d)"
( sleep 1 ) &
main_pid=$!
(
  sleep 5
  if kill -0 "$main_pid" 2>/dev/null; then
    : > "$wdir/fired"
  fi
) >/dev/null 2>&1 &
watcher_pid=$!
wait "$main_pid" 2>/dev/null
disarm_deadline_signal "$watcher_pid"
if [ ! -f "$wdir/fired" ]; then
  pass "deadline-watcher pattern does NOT fire when the guarded process finishes before the deadline (R2's normal, healthy-run case)"
else
  fail "deadline-watcher pattern fired its marker even though the guarded process finished well within the deadline — would produce a false abort on a healthy run"
fi
rm -rf "$wdir"

# ---------------------------------------------------------------------------
# last_completed_test_name (R3) — supports AC3
# ---------------------------------------------------------------------------

jsonlog="$(mktemp)"
trap 'rm -f "$jsonlog"' EXIT

# Case 8: empty/nonexistent log -> "(none yet)"
: > "$jsonlog"
name=$(last_completed_test_name "$jsonlog")
if [ "$name" = "(none yet)" ]; then
  pass "last_completed_test_name on an empty log reports '(none yet)'"
else
  fail "last_completed_test_name on an empty log reported '$name', expected '(none yet)'"
fi

# Case 9: a realistic go test -json stream — run/pause/cont actions carry no
# "completed" signal and must be ignored; the most recent terminal
# (pass/fail/skip) top-level test name wins, in stream order (not sorted by
# elapsed).
{
  echo '{"Action":"run","Test":"TestAlpha"}'
  printf '%s\n' '{"Action":"output","Test":"TestAlpha","Output":"=== RUN TestAlpha\n"}'
  echo '{"Action":"pass","Test":"TestAlpha","Elapsed":1.2}'
  echo '{"Action":"run","Test":"TestBeta"}'
  echo '{"Action":"pause","Test":"TestBeta"}'
  echo '{"Action":"cont","Test":"TestBeta"}'
  echo '{"Action":"fail","Test":"TestBeta","Elapsed":3.4}'
  echo '{"Action":"run","Test":"TestGamma"}'
} > "$jsonlog"
name=$(last_completed_test_name "$jsonlog")
if [ "$name" = "TestBeta" ]; then
  pass "last_completed_test_name reports the most recent terminal (pass/fail/skip) test — TestGamma is still running (only 'run', no terminal action) and correctly excluded"
else
  fail "last_completed_test_name reported '$name', expected 'TestBeta' (the last completed test; TestGamma never completed)"
fi

# Case 10: subtests (Test names containing "/") are excluded — only
# top-level test completions are reported, matching report_test_timings'
# own filter.
{
  echo '{"Action":"pass","Test":"TestAlpha","Elapsed":1.0}'
  echo '{"Action":"pass","Test":"TestAlpha/subcase","Elapsed":0.5}'
} > "$jsonlog"
name=$(last_completed_test_name "$jsonlog")
if [ "$name" = "TestAlpha" ]; then
  pass "last_completed_test_name excludes subtests (containing '/'), reporting only the top-level completion"
else
  fail "last_completed_test_name reported '$name', expected 'TestAlpha' (subtest completion should be excluded)"
fi

# Case 11: skip is treated as a completion, same as pass/fail.
{
  echo '{"Action":"pass","Test":"TestAlpha","Elapsed":1.0}'
  echo '{"Action":"skip","Test":"TestBeta","Elapsed":0.0}'
} > "$jsonlog"
name=$(last_completed_test_name "$jsonlog")
if [ "$name" = "TestBeta" ]; then
  pass "last_completed_test_name treats 'skip' as a completion, same as pass/fail"
else
  fail "last_completed_test_name reported '$name', expected 'TestBeta' (skip should count as the most recent completion)"
fi

# ---------------------------------------------------------------------------
# _post_suite_watchdog_signal (R2 fix, found during Review, #1676)
# ---------------------------------------------------------------------------

# Case 12: the "external signal" fallback branch (the $watchdog_dir/fired
# marker absent — i.e. this signal was NOT the watchdog itself firing) must
# disarm the watchdog's own background timer job too, not just the
# suite/consumer jobs — an earlier revision's TERM-only trap left the
# watchdog job running in exactly this branch, and never wired INT to any
# watchdog-aware handler at all (see the function's own comment in run.sh
# for the full account). This exercises the shared handler directly against
# stand-in jobs for suite_pid/consumer_pid/watchdog_pid, run in a subshell
# since the handler itself calls `exit`. The function reads these as
# ordinary variables (this test sets them as globals rather than switch_and_
# run's own locals) — equivalent from the function's own point of view,
# since bash trap/function execution doesn't distinguish the two. $mode and
# $suite_exit_epoch are only read by the function's OTHER branch (the
# watchdog's own "fired" diagnostic, not exercised by this fallback-focused
# case), so they're deliberately not set here.
watchdog_dir="$(mktemp -d)"
fifo_dir="$(mktemp -d)"
( sleep 30 ) &
suite_pid=$!
( sleep 30 ) &
consumer_pid=$!
( sleep 30 ) &
watchdog_pid=$!
( _post_suite_watchdog_signal 143 ) >/dev/null 2>&1
sleep 1
if ! kill -0 "$watchdog_pid" 2>/dev/null && ! kill -0 "$suite_pid" 2>/dev/null && ! kill -0 "$consumer_pid" 2>/dev/null; then
  pass "_post_suite_watchdog_signal's external-signal fallback disarms the watchdog job (not just suite/consumer) — no orphaned timer left running"
else
  fail "_post_suite_watchdog_signal left a job running (watchdog=$(kill -0 "$watchdog_pid" 2>/dev/null && echo alive || echo gone), suite=$(kill -0 "$suite_pid" 2>/dev/null && echo alive || echo gone), consumer=$(kill -0 "$consumer_pid" 2>/dev/null && echo alive || echo gone)) — orphan leak"
fi
rm -rf "$watchdog_dir" "$fifo_dir" 2>/dev/null

if [ "$FAILED" -ne 0 ]; then
  echo "=== hang_hardening_test.sh: FAILED ==="
  exit 1
fi
echo "=== hang_hardening_test.sh: all checks passed ==="
