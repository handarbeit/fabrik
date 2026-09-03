#!/usr/bin/env bash
# scripts/e2e/token_consumer_check_test.sh — regression coverage for R1's
# (#1684) competing-token-consumer preflight in scripts/e2e/run.sh.
#
# This is what proves AC1 ("starting a gate run with a competing poller on
# the same token produces an explicit warning or refusal naming it, before
# any live API spend") and AC4 ("neither check produces false positives on a
# correctly-prepared run") without needing a real second Fabrik instance:
# find_competing_token_consumers is a pure function over "PID<TAB>DIR" lines
# and fixture directories with their own .env files, so both the positive
# (competitor found) and negative (bed's own directory excluded, no
# candidates, mismatched token) cases run in milliseconds against real
# temp-dir fixtures — no fake binaries, no live processes.
#
# check_competing_token_consumers's own orchestration (E2E_SKIP_TOKEN_CHECK,
# the unreadable-BED_TOKEN degrade-to-warning path) is exercised too, run in
# a subshell each time since a match calls `exit`.
#
# Usage: scripts/e2e/token_consumer_check_test.sh
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

BED_DIR="$(mktemp -d)"
OTHER_DIR="$(mktemp -d)"
NO_ENV_DIR="$(mktemp -d)"
trap 'rm -rf "$BED_DIR" "$OTHER_DIR" "$NO_ENV_DIR"' EXIT

echo "FABRIK_TOKEN=shared-token-abc" >"$BED_DIR/.env"
echo "FABRIK_TOKEN=shared-token-abc" >"$OTHER_DIR/.env"
# $NO_ENV_DIR deliberately has no .env at all.

# --- Case 1: a candidate with a matching token, in a different directory,
# is reported as a competitor. ---
candidates="$(printf '1111\t%s\n' "$OTHER_DIR")"
out="$(find_competing_token_consumers "$BED_DIR" "shared-token-abc" "$candidates")"
rc=$?
assert_eq "matching-token competitor: exit 0" "0" "$rc"
assert_eq "matching-token competitor: reported" "$(printf '1111\t%s' "$OTHER_DIR")" "$out"

# --- Case 2: a candidate whose token differs is not reported. ---
echo "FABRIK_TOKEN=a-different-token" >"$OTHER_DIR/.env"
candidates="$(printf '1111\t%s\n' "$OTHER_DIR")"
out="$(find_competing_token_consumers "$BED_DIR" "shared-token-abc" "$candidates")"
rc=$?
assert_eq "mismatched-token candidate: exit 1 (no match)" "1" "$rc"
assert_eq "mismatched-token candidate: no output" "" "$out"
echo "FABRIK_TOKEN=shared-token-abc" >"$OTHER_DIR/.env" # restore for later cases

# --- Case 3 (AC4): the bed's OWN directory, even carrying the identical
# token, is excluded — it's already budgeted into the ~4,000/5,000 estimate,
# never a "competitor". ---
candidates="$(printf '2222\t%s\n' "$BED_DIR")"
out="$(find_competing_token_consumers "$BED_DIR" "shared-token-abc" "$candidates")"
rc=$?
assert_eq "bed's own directory excluded: exit 1 (no match)" "1" "$rc"
assert_eq "bed's own directory excluded: no output" "" "$out"

# --- Case 4: a candidate directory with no .env at all is silently skipped,
# not an error. ---
candidates="$(printf '3333\t%s\n' "$NO_ENV_DIR")"
out="$(find_competing_token_consumers "$BED_DIR" "shared-token-abc" "$candidates")"
rc=$?
assert_eq "missing .env candidate: exit 1 (no match)" "1" "$rc"
assert_eq "missing .env candidate: no output" "" "$out"

# --- Case 5: an empty candidate list is a no-op, no match. ---
out="$(find_competing_token_consumers "$BED_DIR" "shared-token-abc" "")"
rc=$?
assert_eq "empty candidate list: exit 1 (no match)" "1" "$rc"
assert_eq "empty candidate list: no output" "" "$out"

# --- Case 6: multiple candidates — bed's own dir excluded, no-env dir
# skipped, only the genuine competitor reported. ---
candidates="$(printf '2222\t%s\n3333\t%s\n1111\t%s\n' "$BED_DIR" "$NO_ENV_DIR" "$OTHER_DIR")"
out="$(find_competing_token_consumers "$BED_DIR" "shared-token-abc" "$candidates")"
rc=$?
assert_eq "mixed candidates: exit 0 (one match)" "0" "$rc"
assert_eq "mixed candidates: only the genuine competitor reported" "$(printf '1111\t%s' "$OTHER_DIR")" "$out"

# --- Case 7: check_competing_token_consumers honors E2E_SKIP_TOKEN_CHECK,
# never even discovering candidates. ---
( E2E_SKIP_TOKEN_CHECK=1 TEST_BED="$BED_DIR" BED_TOKEN="shared-token-abc" check_competing_token_consumers ) >/dev/null 2>&1
rc=$?
assert_eq "E2E_SKIP_TOKEN_CHECK=1 exits 0" "0" "$rc"

# --- Case 8: check_competing_token_consumers degrades to a warning (not a
# block) when BED_TOKEN is empty/unreadable. ---
( TEST_BED="$BED_DIR" BED_TOKEN="" check_competing_token_consumers ) >/dev/null 2>&1
rc=$?
assert_eq "empty BED_TOKEN degrades to a pass (warning only)" "0" "$rc"

# --- Case 9 (#1687, R3): wiring-level coverage of
# check_competing_token_consumers' match->refuse path itself. Cases 1-6
# above only prove find_competing_token_consumers (the pure detection unit)
# works in isolation; cases 7-8 only exercise check_competing_token_consumers
# paths that never reach the match/refuse logic. Nothing before this proved
# the orchestrator actually calls discover_fabrik_process_dirs, feeds its
# output through find_competing_token_consumers, and exit()s
# PRECONDITION_FAILED_EXIT on a positive match — replacing
# check_competing_token_consumers' body with `return 0` left every prior
# case green. Here discover_fabrik_process_dirs (the untestable,
# OS-dependent half) is overridden with a canned candidate so the
# orchestrator's own match-handling can be exercised deterministically,
# without a real competing process.
#
# Left intentionally un-restored: this is the last case in the file, so the
# override can't leak into a later one — a future editor appending Case 10+
# after this should redefine discover_fabrik_process_dirs back to its
# original body first (or move this case to the end again).
discover_fabrik_process_dirs() { printf '9999\t%s\n' "$OTHER_DIR"; }
out="$( ( TEST_BED="$BED_DIR" BED_TOKEN="shared-token-abc" check_competing_token_consumers ) 2>&1 )"
rc=$?
assert_eq "orchestrator match: exits PRECONDITION_FAILED_EXIT" "$PRECONDITION_FAILED_EXIT" "$rc"
if ! printf '%s' "$out" | grep -q "PRECONDITION FAILED"; then
  echo "FAIL: orchestrator match: output names PRECONDITION FAILED"
  FAILED=1
else
  echo "PASS: orchestrator match: output names PRECONDITION FAILED"
fi
if ! printf '%s' "$out" | grep -q "9999"; then
  echo "FAIL: orchestrator match: output names competing PID"
  FAILED=1
else
  echo "PASS: orchestrator match: output names competing PID"
fi
if ! printf '%s' "$out" | grep -qF "$OTHER_DIR"; then
  echo "FAIL: orchestrator match: output names competing dir"
  FAILED=1
else
  echo "PASS: orchestrator match: output names competing dir"
fi

if [ "$FAILED" -ne 0 ]; then
  echo "=== token_consumer_check_test.sh: FAILED ==="
  exit 1
fi
echo "=== token_consumer_check_test.sh: all checks passed ==="
