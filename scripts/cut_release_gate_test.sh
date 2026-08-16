#!/usr/bin/env bash
# scripts/cut_release_gate_test.sh — regression coverage for cut-release.sh's
# flag parsing, in particular the mandatory-by-default live e2e integration
# gate's --skip-integration handling (R2/AC7, #1454), without ever touching a
# real bed, network, git remote, or publish path.
#
# Sources cut-release.sh for parse_args() (and insert_notes_line()) only —
# the BASH_SOURCE[0]==$0 dispatch guard at the bottom (mirroring
# scripts/e2e/run.sh's own precedent) prevents this from triggering main()
# (the real publish sequence: tag, push, watch workflow, file issue) on
# source. This is how AC7 ("cut-release.sh's new flags exercised on a
# no-publish path") is satisfied: parse_args is called directly and its
# resulting globals are asserted against; main() is never reached.
#
# Usage: scripts/cut_release_gate_test.sh
# Exit 0 if all assertions pass, 1 otherwise.

set -uo pipefail # no -e: assertions below intentionally continue past failures

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Source cut-release.sh for its function definitions only. Its own
# `set -euo pipefail` becomes active in this shell too once sourced; the
# `set +e` right after turns errexit back off so this script's own
# assertions (several of which deliberately provoke parse_args's non-zero
# exit) don't abort it.
# shellcheck source=/dev/null
source "$REPO_ROOT/scripts/cut-release.sh"
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

# assert_exits runs parse_args in a SUBSHELL (so its `exit` on a validation
# failure terminates only the subshell, not this test script) and asserts
# the resulting exit code.
assert_exits() {
  local desc="$1" expected_rc="$2"
  shift 2
  local rc=0
  ( parse_args "$@" ) >/dev/null 2>&1
  rc=$?
  assert_eq "$desc" "$expected_rc" "$rc"
}

# --- default: no --skip-integration, no other flags ---
parse_args v0.0.99
assert_eq "default VERSION" "v0.0.99" "$VERSION"
assert_eq "default SKIP_INTEGRATION" "0" "$SKIP_INTEGRATION"
assert_eq "default INTEGRATION_SKIP_REASON" "" "$INTEGRATION_SKIP_REASON"
assert_eq "default SKIP_TESTS" "0" "$SKIP_TESTS"
assert_eq "default NO_DOC_ISSUE" "0" "$NO_DOC_ISSUE"
assert_eq "default NO_PLUGIN_BUMP" "0" "$NO_PLUGIN_BUMP"

# --- --skip-integration=<reason> sets the flag and records the reason verbatim ---
parse_args v0.0.99 --skip-integration=testing-the-flag-itself
assert_eq "--skip-integration=<reason> sets SKIP_INTEGRATION" "1" "$SKIP_INTEGRATION"
assert_eq "--skip-integration=<reason> records the reason" "testing-the-flag-itself" "$INTEGRATION_SKIP_REASON"

# A reason with spaces (the real-world shape, quoted on the command line).
parse_args v0.0.99 --skip-integration="bed is mid-reset, rerunning tomorrow"
assert_eq "--skip-integration=<reason with spaces> records it verbatim" "bed is mid-reset, rerunning tomorrow" "$INTEGRATION_SKIP_REASON"

# --- bare --skip-integration (no reason at all) is a hard usage error — the
# live suite is mandatory by default and the one sanctioned escape hatch
# must be self-documenting, never silent. ---
assert_exits "bare --skip-integration exits 2" "2" v0.0.99 --skip-integration

# --- --skip-integration= (present but empty reason) is also a hard usage error ---
assert_exits "--skip-integration= (empty reason) exits 2" "2" v0.0.99 --skip-integration=

# --- pre-existing flags still parse correctly post-refactor (no behavior
# change from the parse_args()/main() split) ---
parse_args v0.0.99 --skip-tests --no-doc-issue --no-plugin-bump
assert_eq "--skip-tests still parses" "1" "$SKIP_TESTS"
assert_eq "--no-doc-issue still parses" "1" "$NO_DOC_ISSUE"
assert_eq "--no-plugin-bump still parses" "1" "$NO_PLUGIN_BUMP"

# --- flags can combine with --skip-integration ---
parse_args v0.0.99 --skip-tests --skip-integration=combo-check
assert_eq "--skip-tests + --skip-integration: SKIP_TESTS" "1" "$SKIP_TESTS"
assert_eq "--skip-tests + --skip-integration: SKIP_INTEGRATION" "1" "$SKIP_INTEGRATION"

# --- missing version is a hard usage error (pre-existing behavior) ---
assert_exits "missing version exits 2" "2"

# --- malformed version is a hard usage error (pre-existing behavior) ---
assert_exits "malformed version exits 2" "2" not-a-version

# --- unknown flag is a hard usage error (pre-existing behavior) ---
assert_exits "unknown flag exits 2" "2" v0.0.99 --not-a-real-flag

# --- interpret_e2e_exit_code: each known scripts/e2e/run.sh exit code gets
# its own distinct, non-misleading message and success/failure classification
# — in particular exit 4 (PREFLIGHT_FAILED_EXIT, a bed/infrastructure
# problem) must NOT be conflated with a fidelity-drift regression, which is
# what a prior version of this script did by falling through to the generic
# "*)" branch for it. ---
VERSION="v0.0.99" # interpret_e2e_exit_code's exit-3/4/5 messages interpolate $VERSION

msg="$(interpret_e2e_exit_code 0)"; rc=$?
assert_eq "exit 0 message" "live e2e integration suite passed" "$msg"
assert_eq "exit 0 classified as success" "0" "$rc"

msg="$(interpret_e2e_exit_code 3)"; rc=$?
case "$msg" in
  *"budget exhausted"*) echo "PASS: exit 3 message mentions budget exhaustion" ;;
  *) echo "FAIL: exit 3 message does not mention budget exhaustion: $msg"; FAILED=1 ;;
esac
assert_eq "exit 3 classified as failure" "1" "$rc"

msg="$(interpret_e2e_exit_code 4)"; rc=$?
case "$msg" in
  *"infrastructure problem"*"NOT a regression"*"NOT a fidelity-drift case"*)
    echo "PASS: exit 4 message is distinct and explicitly disclaims fidelity-drift" ;;
  *) echo "FAIL: exit 4 message does not distinctly disclaim fidelity-drift: $msg"; FAILED=1 ;;
esac
case "$msg" in
  *"FIDELITY-DRIFT"*) echo "FAIL: exit 4 message wrongly mentions the fidelity-drift procedure"; FAILED=1 ;;
  *) echo "PASS: exit 4 message does not mention the fidelity-drift procedure" ;;
esac
assert_eq "exit 4 classified as failure" "1" "$rc"

msg="$(interpret_e2e_exit_code 5)"; rc=$?
case "$msg" in
  *"own sim/wire-contract pre-gate failed"*) echo "PASS: exit 5 message mentions the live script's own pre-gate" ;;
  *) echo "FAIL: exit 5 message does not mention the live script's own pre-gate: $msg"; FAILED=1 ;;
esac
assert_eq "exit 5 classified as failure" "1" "$rc"

msg="$(interpret_e2e_exit_code 1)"; rc=$?
case "$msg" in
  *"FIDELITY-DRIFT"*) echo "PASS: exit 1 (real regression) message mentions the fidelity-drift procedure" ;;
  *) echo "FAIL: exit 1 message does not mention the fidelity-drift procedure: $msg"; FAILED=1 ;;
esac
assert_eq "exit 1 classified as failure" "1" "$rc"

# --- insert_notes_line: exercised directly against a scratch file, proving
# the helper both call sites (plugin-bump changelog, --skip-integration
# recorded-skip line) now share behaves identically to the pre-refactor
# inline logic it replaced. No network, no real release-notes file touched. ---
SCRATCH="$(mktemp)"
trap 'rm -f "$SCRATCH"' EXIT

cat > "$SCRATCH" <<'EOF'
# Fabrik v0.0.99

## Summary

Test fixture.

## Upgrading

```bash
echo hi
```
EOF
insert_notes_line "$SCRATCH" "- ⚠️ Live e2e integration suite SKIPPED for this release: fixture reason"
if grep -Fq '## Internal' "$SCRATCH" && grep -Fq 'SKIPPED for this release: fixture reason' "$SCRATCH"; then
  echo "PASS: insert_notes_line creates ## Internal section and inserts the line (no pre-existing section)"
else
  echo "FAIL: insert_notes_line did not create ## Internal / insert the line as expected"
  FAILED=1
  cat "$SCRATCH" >&2
fi

insert_notes_line "$SCRATCH" "- second line, existing ## Internal section"
if [ "$(grep -c 'second line, existing' "$SCRATCH")" -eq 1 ]; then
  echo "PASS: insert_notes_line appends into an already-existing ## Internal section"
else
  echo "FAIL: insert_notes_line did not correctly append to an existing ## Internal section"
  FAILED=1
fi

if [ "$FAILED" -ne 0 ]; then
  echo "=== cut_release_gate_test.sh: FAILED ==="
  exit 1
fi
echo "=== cut_release_gate_test.sh: all checks passed ==="
