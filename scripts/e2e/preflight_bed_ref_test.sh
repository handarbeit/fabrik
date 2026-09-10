#!/usr/bin/env bash
# scripts/e2e/preflight_bed_ref_test.sh — regression coverage for #1693:
# preflight_bed (scripts/e2e/run.sh) must resolve any E2E_BED_REF the origin
# remote actually has, not just origin/main, even though the bed checkout is
# a single-branch clone (fetch refspec
# +refs/heads/main:refs/remotes/origin/main). A bare `git fetch origin`
# obeys that refspec and only ever updates origin/main, so any other ref
# used to die with git's raw `fatal: ambiguous argument` on the following
# rev-parse — outside any preflight wrapping.
#
# This exercises the real preflight_bed function from run.sh (sourced,
# mirroring pregate_test.sh's precedent) against two local scratch git
# repos standing in for the remote and the bed: a bare "origin" with a
# branch never fetched into the bed, and a "bed" that is a single-branch
# clone of it, matching the real bed's refspec shape exactly (verified
# below via `git config --get-all remote.origin.fetch`, the same check the
# issue's own repro used). No network, no real ~/dev/fabrik-test involved —
# FABRIK_TEST_DIR points preflight_bed at the scratch bed instead.
#
# Every scenario runs with E2E_BED_NO_BUILD=1 so preflight_bed never
# reaches the checkout/build/`./fabrik --version` steps (R3's guard is
# untouched code and out of scope for this regression test) — it stops
# right after the fetch/resolve step this issue changes, either at the
# "bed is at X, want Y ... and E2E_BED_NO_BUILD is set" refusal (a
# pre-existing, correct outcome for a mismatched bed) or at the "already at"
# no-op path when the bed already matches.
#
# Usage: scripts/e2e/preflight_bed_ref_test.sh
# Exit 0 if all assertions pass, 1 otherwise.

set -uo pipefail # no -e: assertions below intentionally continue past failures

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# --- Build the scratch fixture BEFORE sourcing run.sh: TEST_BED is derived
# from FABRIK_TEST_DIR once, at source time (scripts/e2e/run.sh:352), not
# read lazily inside preflight_bed. ---
SCRATCH_DIR="$(mktemp -d)"
ORIGIN_DIR="$SCRATCH_DIR/origin.git"
SEED_DIR="$SCRATCH_DIR/seed"
BED_DIR="$SCRATCH_DIR/bed"
trap 'rm -rf "$SCRATCH_DIR"' EXIT

git init -q --bare "$ORIGIN_DIR"
git init -q "$SEED_DIR"
git -C "$SEED_DIR" config user.email "test@example.com"
git -C "$SEED_DIR" config user.name "test"
git -C "$SEED_DIR" commit -q --allow-empty -m "main commit"
git -C "$SEED_DIR" branch -M main
git -C "$SEED_DIR" remote add origin "$ORIGIN_DIR"
git -C "$SEED_DIR" push -q origin main

# A branch that exists on origin but is never fetched into the bed's
# single-branch clone below — this is the exact shape of the issue's repro
# (origin/fabrik/my-branch, never fetched, single-branch bed clone).
git -C "$SEED_DIR" checkout -q -b feature
git -C "$SEED_DIR" commit -q --allow-empty -m "feature commit"
git -C "$SEED_DIR" push -q origin feature
git -C "$SEED_DIR" checkout -q main

MAIN_SHA="$(git -C "$SEED_DIR" rev-parse main)"
MAIN_SHORT="${MAIN_SHA:0:7}"
FEATURE_SHA="$(git -C "$SEED_DIR" rev-parse feature)"
FEATURE_SHORT="${FEATURE_SHA:0:7}"

git clone -q --single-branch --branch main "$ORIGIN_DIR" "$BED_DIR"

# Verify the fixture actually reproduces the real bed's single-branch
# refspec shape — a fixture that drifts from this would silently test the
# wrong thing.
FIXTURE_REFSPEC="$(git -C "$BED_DIR" config --get-all remote.origin.fetch)"
EXPECTED_REFSPEC="+refs/heads/main:refs/remotes/origin/main"

export FABRIK_TEST_DIR="$BED_DIR"
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

assert_contains() {
  local desc="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then
    echo "PASS: $desc"
  else
    echo "FAIL: $desc (expected output to contain '$needle')"
    FAILED=1
  fi
}

assert_not_contains() {
  local desc="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then
    echo "FAIL: $desc (expected output NOT to contain '$needle')"
    FAILED=1
  else
    echo "PASS: $desc"
  fi
}

assert_eq "scratch bed reproduces the real bed's single-branch refspec" "$EXPECTED_REFSPEC" "$FIXTURE_REFSPEC"

# --- Scenario R1 (#1693): a branch pushed to origin but never fetched into
# the bed resolves — the fetch/resolve step must reach it despite the bed's
# single-branch clone refspec. preflight_bed still exits non-zero here (bed
# HEAD is at main, E2E_BED_NO_BUILD is set, so the pre-existing
# mismatch-and-refuse-to-build path fires) — that outcome is correct and
# unrelated to this fix; what this asserts is that resolution itself
# succeeded (the target short SHA appears) and no fetch-failure occurred. ---
OUTPUT="$( ( E2E_BED_REF=origin/feature E2E_BED_NO_BUILD=1 preflight_bed ) 2>&1 )"
assert_contains "never-fetched branch resolves to its short SHA" "$OUTPUT" "$FEATURE_SHORT"
assert_not_contains "never-fetched branch: no fetch-failure message" "$OUTPUT" "git fetch failed in"

# --- Scenario R2 (#1693): a ref that doesn't exist on origin at all fails
# with a preflight-context message naming the ref and $TEST_BED, exiting via
# PREFLIGHT_FAILED_EXIT — not git's raw, unwrapped message escaping under
# set -e (the pre-fix failure mode: a bare fetch always "succeeds" since it
# only ever touches origin/main, so resolution used to die later at an
# unwrapped `git rev-parse` with a bare `fatal: ambiguous argument`). ---
OUTPUT="$( ( E2E_BED_REF=origin/does-not-exist E2E_BED_NO_BUILD=1 preflight_bed ) 2>&1 )"
RC=$?
assert_eq "bogus ref exits PREFLIGHT_FAILED_EXIT ($PREFLIGHT_FAILED_EXIT)" "$PREFLIGHT_FAILED_EXIT" "$RC"
assert_contains "bogus ref: wrapped message names the ref" "$OUTPUT" "does-not-exist"
assert_contains "bogus ref: wrapped message names \$TEST_BED" "$OUTPUT" "$BED_DIR"
assert_contains "bogus ref: wrapped message reassures the bed is untouched" "$OUTPUT" "bed is untouched"
assert_not_contains "bogus ref: no bare unwrapped rev-parse failure" "$OUTPUT" "ambiguous argument"

# --- Scenario R4 (#1693): E2E_BED_REF unset resolves to origin/main exactly
# as before — the bed (already cloned at main's tip) is reported already
# up to date, with no fetch-failure message. ---
unset E2E_BED_REF
OUTPUT="$( ( E2E_BED_NO_BUILD=1 preflight_bed ) 2>&1 )"
assert_not_contains "default ref (unset): no fetch-failure message" "$OUTPUT" "git fetch failed in"
assert_contains "default ref (unset): resolves to main's short SHA" "$OUTPUT" "$MAIN_SHORT"

if [ "$FAILED" -ne 0 ]; then
  echo "=== preflight_bed_ref_test.sh: FAILED ==="
  exit 1
fi
echo "=== preflight_bed_ref_test.sh: all checks passed ==="
