#!/usr/bin/env bash
# scripts/e2e/reviewer_reachable_check_test.sh — regression coverage for R2's
# (#1684) review-bot-reachable preflight in scripts/e2e/run.sh.
#
# This is what proves AC2 ("starting a run with the review bot unreachable
# fails fast with a diagnostic") and AC4 ("neither check produces false
# positives on a correctly-prepared run") without needing a real Pruefer
# deployment: check_reviewer_reachable's liveness signal is a PID written
# into $PRUEFER_DIR/.pruefer/pruefer.lock (mirrors pruefer/daemon.go's
# acquireLock and run.sh's own stop_bed_instance idiom for fabrik.lock), so
# both "alive" and "dead" are simulated with real PIDs against a fixture
# directory — no fake binaries, no live Pruefer process needed. $$ (this
# script's own PID) proves "alive"; a definitely-unused high PID proves
# "dead".
#
# Usage: scripts/e2e/reviewer_reachable_check_test.sh
# Exit 0 if all assertions pass, 1 otherwise.

set -uo pipefail # no -e: assertions below intentionally continue past failures

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Source run.sh for its function definitions only — the sourcing guard at the
# bottom of run.sh (BASH_SOURCE[0] == $0) prevents this from triggering an
# actual gate run, mirroring pregate_test.sh/backoff_detection_test.sh's
# precedent exactly.
# shellcheck source=/dev/null
source "$REPO_ROOT/scripts/e2e/run.sh"
set +e

FAILED=0

assert_eq() {
  local desc="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "PASS: $desc"
  else
    echo "FAIL: $desc (expected '$expected', got '$actual')"
    FAILED=1
  fi
}

FIXTURE_DIR="$(mktemp -d)"
trap 'rm -rf "$FIXTURE_DIR"' EXIT
mkdir -p "$FIXTURE_DIR/.pruefer"
LOCK="$FIXTURE_DIR/.pruefer/pruefer.lock"

# A PID essentially guaranteed to be unused on any real system.
DEAD_PID=999999

# --- Case 1: a live PID ($$, this script's own) in the lock file passes. ---
echo "$$" >"$LOCK"
( PRUEFER_DIR="$FIXTURE_DIR" check_reviewer_reachable ) >/dev/null 2>&1
rc=$?
assert_eq "live PID in lock file: exits 0" "0" "$rc"

# --- Case 2: a missing lock file refuses with PRECONDITION_FAILED_EXIT (7).
# ---
rm -f "$LOCK"
( PRUEFER_DIR="$FIXTURE_DIR" check_reviewer_reachable ) >/dev/null 2>&1
rc=$?
assert_eq "missing lock file: exits PRECONDITION_FAILED_EXIT (7)" "7" "$rc"

# --- Case 3: a lock file naming a definitely-unused PID refuses with
# PRECONDITION_FAILED_EXIT (7). ---
echo "$DEAD_PID" >"$LOCK"
( PRUEFER_DIR="$FIXTURE_DIR" check_reviewer_reachable ) >/dev/null 2>&1
rc=$?
assert_eq "dead PID in lock file: exits PRECONDITION_FAILED_EXIT (7)" "7" "$rc"

# --- Case 4: E2E_SKIP_REVIEWER_CHECK=1 short-circuits to a pass even against
# the same dead-PID fixture from Case 3. ---
( PRUEFER_DIR="$FIXTURE_DIR" E2E_SKIP_REVIEWER_CHECK=1 check_reviewer_reachable ) >/dev/null 2>&1
rc=$?
assert_eq "E2E_SKIP_REVIEWER_CHECK=1 exits 0 despite dead PID" "0" "$rc"

# --- Case 5: a nonexistent PRUEFER_DIR warns and passes (undeterminable, not
# confirmed-down — never blocks a genuinely remote Pruefer topology). ---
( PRUEFER_DIR="$FIXTURE_DIR/does-not-exist" check_reviewer_reachable ) >/dev/null 2>&1
rc=$?
assert_eq "nonexistent PRUEFER_DIR: exits 0 (warn only)" "0" "$rc"

# --- Case 6: a -run argument in "$@" short-circuits to a pass even against
# the same dead-PID fixture from Case 3 — a narrowed subset is the operator's
# call, not inferred. ---
( PRUEFER_DIR="$FIXTURE_DIR" check_reviewer_reachable -run TestSomeScenario ) >/dev/null 2>&1
rc=$?
assert_eq "-run supplied: exits 0 despite dead PID" "0" "$rc"

# --- Case 7: a --run argument (long form) is recognized the same way. ---
( PRUEFER_DIR="$FIXTURE_DIR" check_reviewer_reachable --run=TestSomeScenario ) >/dev/null 2>&1
rc=$?
assert_eq "--run= supplied: exits 0 despite dead PID" "0" "$rc"

if [ "$FAILED" -ne 0 ]; then
  echo "=== reviewer_reachable_check_test.sh: FAILED ==="
  exit 1
fi
echo "=== reviewer_reachable_check_test.sh: all checks passed ==="
