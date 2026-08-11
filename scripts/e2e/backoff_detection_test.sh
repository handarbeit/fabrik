#!/usr/bin/env bash
# scripts/e2e/backoff_detection_test.sh — regression coverage for R2's
# (#1527) rate-limit-backoff detection in scripts/e2e/run.sh.
#
# R2 is a release-gating guarantee ("a throttled run must fail loudly and
# explicitly") with no automated coverage prior to this script — see PR
# #1547's review thread, where a byte-offset-based version of the detector
# shipped, was defeated in the field by fabrik.log's O_TRUNC-on-startup
# behavior, and was fixed to a whole-file scan (see run.sh's
# detect_rate_limit_backoff for the full account). This script exists so
# that regression can't recur silently: it exercises the *actual*
# detect_rate_limit_backoff function from run.sh against fixtures, plus a
# deliberately-reintroduced copy of the historical broken approach, so a
# future edit that reintroduces byte-offset tracking (or otherwise narrows
# the scan) fails this script instead of only being caught by a real,
# multi-hour bed run.
#
# Usage: scripts/e2e/backoff_detection_test.sh
# Exit 0 if all assertions pass, 1 otherwise.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Source run.sh for its function definitions only — the sourcing guard at
# the bottom of run.sh (BASH_SOURCE[0] == $0) prevents this from triggering
# an actual gate run. Some of run.sh's top-level setup still executes on
# source (TEST_BED/BED_TOKEN resolution) but is read-only and degrades
# gracefully (a missing $TEST_BED/.env just warns), so it's safe here.
# shellcheck source=/dev/null
source "$REPO_ROOT/scripts/e2e/run.sh"

FAILED=0
TMP_LOG="$(mktemp)"
trap 'rm -f "$TMP_LOG"' EXIT

check() {
  local desc="$1"
  local expect_match="$2" # "match" or "no-match"
  if detect_rate_limit_backoff "$TMP_LOG"; then
    local actual="match"
  else
    local actual="no-match"
  fi
  if [ "$actual" = "$expect_match" ]; then
    echo "PASS: $desc (expected $expect_match, got $actual)"
  else
    echo "FAIL: $desc (expected $expect_match, got $actual)"
    FAILED=1
  fi
}

# --- Case 1: no backoff activity at all -> no match ---
{
  echo "2026-08-11T09:00:00Z [info] poll cycle complete"
  echo "2026-08-11T09:00:30Z [info] poll cycle complete"
} > "$TMP_LOG"
check "no backoff activity" "no-match"

# --- Case 2: only the companion per-poll "is low" line -> no match ---
# (R2 deliberately excludes this spammy per-poll line, which fires on every
# poll while low even without ever crossing the 20% activation threshold —
# see run.sh's header comment.)
{
  echo "2026-08-11T09:00:00Z [warn] GraphQL rate limit is low (25% remaining) — consider reducing poll frequency"
} > "$TMP_LOG"
check "per-poll 'is low' line alone (not the activation line)" "no-match"

# --- Case 3: the real one-shot activation line, no truncation -> match ---
{
  echo "2026-08-11T09:00:00Z [warn] GraphQL rate limit low (19% remaining) — activating rate-limit backoff"
} > "$TMP_LOG"
check "activation line present, no truncation" "match"

# --- Case 4: the actual field regression — a restart truncates the log
# between "before" and "after", and the activation line is logged only
# after truncation. This is the exact scenario that defeated the
# byte-offset version of the detector (a pre-restart offset measured
# against a file the restart then truncates out from under it). The
# CURRENT detector (whole-file scan) must still catch this. ---
{
  # Simulate a substantial pre-restart log (mirrors the ~640KB real bed log
  # cited in PR #1547's review thread) ...
  for _ in $(seq 1 50); do
    echo "2026-08-11T08:00:00Z [info] poll cycle complete, nothing to do"
  done
} > "$TMP_LOG"
# ... then simulate the engine restart's O_TRUNC (engine/poll.go's Run())
# truncating the file to empty, and the freshly-restarted engine logging the
# activation line as one of its very first polls.
: > "$TMP_LOG"
echo "2026-08-11T09:15:16Z [warn] GraphQL rate limit low (19% remaining) — activating rate-limit backoff" >> "$TMP_LOG"
check "activation line logged only after a mid-leg truncation (the field regression)" "match"

# --- Case 5 (neutralization): the SAME fixture as Case 4, run through the
# historical broken byte-offset approach instead of the current
# detect_rate_limit_backoff. This proves the fixture actually exercises the
# regression (not just a fixture that happens to always match), and that
# the byte-offset approach a future edit might reintroduce would indeed be
# caught failing here. ---
{
  # log_offset captured BEFORE the restart, against the pre-truncation file
  # (mirrors PR #1547's original, defeated implementation).
  pre_restart_log="$(mktemp)"
  for _ in $(seq 1 50); do
    echo "2026-08-11T08:00:00Z [info] poll cycle complete, nothing to do"
  done > "$pre_restart_log"
  log_offset=$(wc -c < "$pre_restart_log")
  rm -f "$pre_restart_log"

  # The restart truncates the real log out from under that offset, then the
  # freshly-restarted engine logs the activation line — same as Case 4's
  # $TMP_LOG, which is still in that post-truncation state here.
  if tail -c "+$((log_offset + 1))" "$TMP_LOG" 2>/dev/null | grep -q 'activating rate-limit backoff'; then
    legacy_result="match"
  else
    legacy_result="no-match"
  fi
}
if [ "$legacy_result" = "no-match" ]; then
  echo "PASS: neutralization — historical byte-offset approach fails to detect the same fixture Case 4 caught (confirms Case 4 exercises the real regression, and that this approach was correctly replaced)"
else
  echo "FAIL: neutralization — historical byte-offset approach unexpectedly detected the fixture; Case 4 may not be exercising the regression it claims to"
  FAILED=1
fi

if [ "$FAILED" -ne 0 ]; then
  echo "=== backoff_detection_test.sh: FAILED ==="
  exit 1
fi
echo "=== backoff_detection_test.sh: all checks passed ==="
