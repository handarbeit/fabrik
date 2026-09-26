#!/usr/bin/env bash
# scripts/release_gate_full_suite_test.sh — regression coverage for R3/AC5 (#1857):
# the release gate runs the FULL test suite, unconditionally, and never consults
# any per-PR test-selection mechanism.
#
# #1857 evaluated selecting CI tests from the dependency graph and decided not to
# build it (adrs/1857-no-per-pr-test-selection.md). Nothing is narrowed today, but
# the reasoning of adrs/1454-sim-pre-gate-not-replacement.md — a cheap layer never
# replaces the full one at the release gate — must survive any future selector.
# This test pins that: if a later change makes scripts/cut-release.sh or
# scripts/e2e/run.sh's run_pregate narrow what they run, or wires a selector into
# either, it fails here instead of silently shipping a release on a subset.
#
# It is a static content check (grep over the two scripts' text). It never
# executes either script, so it needs no bed, token, network or git remote.
#
# Usage: scripts/release_gate_full_suite_test.sh
# Exit 0 if all assertions pass, 1 otherwise.

set -uo pipefail # no -e: assertions below intentionally continue past failures

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CUT_RELEASE="$REPO_ROOT/scripts/cut-release.sh"
E2E_RUN="$REPO_ROOT/scripts/e2e/run.sh"

FAILED=0

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILED=1; }

# code_lines prints $1's non-comment lines, so a pattern quoted only in an
# explanatory comment never satisfies (or trips) an assertion.
code_lines() { grep -vE '^[[:space:]]*#' "$1"; }

# func_body prints the body of shell function $2 in file $1 (from its
# "name() {" line to the first column-0 closing brace), comments removed.
func_body() {
  awk -v fn="$2" '
    $0 ~ "^" fn "\\(\\) *\\{" { in_fn = 1 }
    in_fn { print }
    in_fn && /^}/ { exit }
  ' "$1" | grep -vE '^[[:space:]]*#'
}

# assert_has DESC REGEX TEXT — TEXT must contain a line matching REGEX.
assert_has() {
  local desc="$1" regex="$2" text="$3"
  if printf '%s\n' "$text" | grep -qE -- "$regex"; then
    pass "$desc"
  else
    fail "$desc (no line matching: $regex)"
  fi
}

# assert_lacks DESC REGEX TEXT — no line of TEXT may match REGEX.
assert_lacks() {
  local desc="$1" regex="$2" text="$3" hit
  hit="$(printf '%s\n' "$text" | grep -nEi -- "$regex" || true)"
  if [ -z "$hit" ]; then
    pass "$desc"
  else
    fail "$desc (found: $hit)"
  fi
}

# Anything that would indicate a test-selection mechanism has been wired in.
SELECTOR_RE='scripts/ci/|select[-_]tests|test[-_]selection|affected[-_]packages|changed[-_]packages'

# The two full-suite invocations cut-release.sh must keep: the never-skippable
# ./github/... pre-gate step, and the full `./...` run. The latter is anchored
# on ` ./...` as the final word so a narrowed package list can't satisfy it.
CUT_RELEASE_GITHUB_RE='go test -race .*\./github/\.\.\.'
CUT_RELEASE_FULL_RE='go test -race .*[[:space:]]\./\.\.\.([[:space:]>]|$)'

cut_code="$(code_lines "$CUT_RELEASE")"
pregate_body="$(func_body "$E2E_RUN" run_pregate)"
e2e_code="$(code_lines "$E2E_RUN")"

# --- self-checks: the extraction and patterns must be able to fail ---
[ -n "$cut_code" ] && pass "cut-release.sh has code lines to inspect" || fail "cut-release.sh code extraction is empty"
[ -n "$pregate_body" ] && pass "run_pregate body extracted from e2e/run.sh" || fail "run_pregate body extraction is empty"

narrowed_fixture='  if ! go test -race -parallel "$P" -timeout 20m ./engine/... ; then'
if printf '%s\n' "$narrowed_fixture" | grep -qE -- "$CUT_RELEASE_FULL_RE"; then
  fail "self-check: full-suite pattern wrongly accepts a narrowed package list"
else
  pass "self-check: full-suite pattern rejects a narrowed package list"
fi
full_fixture='  if ! go test -race -parallel "$P" -timeout 20m ./... >/tmp/x.log 2>&1; then'
if printf '%s\n' "$full_fixture" | grep -qE -- "$CUT_RELEASE_FULL_RE"; then
  pass "self-check: full-suite pattern accepts the real invocation shape"
else
  fail "self-check: full-suite pattern rejects the real invocation shape"
fi
if printf '%s\n' 'source scripts/ci/select-tests.sh' | grep -qEi -- "$SELECTOR_RE"; then
  pass "self-check: selector pattern detects a wired-in selector"
else
  fail "self-check: selector pattern misses a wired-in selector"
fi

# --- scripts/cut-release.sh ---
assert_has "cut-release.sh runs the never-skippable go test -race ./github/... pre-gate" \
  "$CUT_RELEASE_GITHUB_RE" "$cut_code"
assert_has "cut-release.sh runs the full go test -race ... ./... suite" \
  "$CUT_RELEASE_FULL_RE" "$cut_code"
assert_lacks "cut-release.sh references no test-selection mechanism" \
  "$SELECTOR_RE" "$cut_code"

# --- scripts/e2e/run.sh's run_pregate ---
assert_has "run_pregate runs scripts/sim/run.sh --all" \
  'scripts/sim/run\.sh"? +--all' "$pregate_body"
assert_has "run_pregate runs go test -race ./github/..." \
  "$CUT_RELEASE_GITHUB_RE" "$pregate_body"
assert_lacks "e2e/run.sh references no test-selection mechanism" \
  "$SELECTOR_RE" "$e2e_code"

if [ "$FAILED" -ne 0 ]; then
  echo "release_gate_full_suite_test: FAILED"
  exit 1
fi
echo "release_gate_full_suite_test: all assertions passed"
exit 0
