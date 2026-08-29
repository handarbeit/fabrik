#!/usr/bin/env bash
# scripts/e2e/pregate_test.sh — regression coverage for R1's (#1454) pre-gate
# in scripts/e2e/run.sh: the sim suite and github wire-contract tests must run
# — and must be seen to run — before any bed preflight, build, or live
# GitHub/Claude call, and a failure there must abort with a distinct exit
# code before any of that happens.
#
# This is what proves AC1 ("deliberately breaking a sim test and showing no
# live call is made") without paying for an actual multi-minute `go test`
# run or a real, temporary breakage of tests/sim: it exercises the real
# run_pregate function from run.sh (sourced, mirroring
# backoff_detection_test.sh's precedent) against a PATH-shadowed fake `go`
# binary that records every invocation and exits with a scripted status —
# so both "pass" and "fail" are simulated in milliseconds, and the fake
# being invoked at all is itself asserted (a marker file), not just trusted.
#
# Usage: scripts/e2e/pregate_test.sh
# Exit 0 if all assertions pass, 1 otherwise.

set -uo pipefail # no -e: assertions below intentionally continue past failures

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Source run.sh for its function definitions only — the sourcing guard at the
# bottom of run.sh (BASH_SOURCE[0] == $0) prevents this from triggering an
# actual gate run. run.sh's own `set -euo pipefail` becomes active in this
# shell too once sourced; `set +e` below turns errexit back off so this
# script's own assertions (several of which deliberately provoke a non-zero
# exit) don't abort it.
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

# fake_go_bin creates a directory on $FAKE_BIN_DIR containing a `go` script
# that appends one line to $MARKER_FILE per invocation (so callers can count
# and inspect calls) and exits with $FAKE_GO_EXIT (default 0). Prepending it
# to PATH shadows the real toolchain for both scripts/sim/run.sh's own `go
# test` call and run_pregate's direct `go test ./github/...` call — both are
# separate child processes, so PATH (not a shell function) is what's needed
# to shadow both.
FAKE_BIN_DIR="$(mktemp -d)"
MARKER_FILE="$(mktemp)"
# DIRTY_TREE_FILE: an untracked scratch file used by Case 6 below to make
# $REPO_ROOT's working tree dirty without touching any tracked file. Declared
# here (not created yet) so the EXIT trap can unconditionally clean it up
# even if a later case is added between here and Case 6 and aborts first.
DIRTY_TREE_FILE="$REPO_ROOT/.pregate_test_dirty_tree_scratch"
trap 'rm -rf "$FAKE_BIN_DIR" "$MARKER_FILE" "$DIRTY_TREE_FILE"' EXIT

cat > "$FAKE_BIN_DIR/go" <<'EOF'
#!/usr/bin/env bash
echo "FAKE_GO_CALLED: $*" >> "$MARKER_FILE"
exit "${FAKE_GO_EXIT:-0}"
EOF
chmod +x "$FAKE_BIN_DIR/go"

reset_marker() { : > "$MARKER_FILE"; }
marker_count() { wc -l < "$MARKER_FILE" | tr -d ' '; }

export PATH="$FAKE_BIN_DIR:$PATH"
export MARKER_FILE

# --- Case 1: E2E_SKIP_PREGATE=1 — the escape hatch skips entirely, no `go`
# invocation of any kind, and returns success. ---
reset_marker
( E2E_SKIP_PREGATE=1 FAKE_GO_EXIT=1 run_pregate ) >/dev/null 2>&1
rc=$?
assert_eq "E2E_SKIP_PREGATE=1 exits 0" "0" "$rc"
assert_eq "E2E_SKIP_PREGATE=1 makes no go call" "0" "$(marker_count)"

# --- Case 2: both layers "pass" (fake go exits 0) — run_pregate exits 0 and
# both the sim-suite go invocation and the github-tests go invocation
# actually happened (2 calls: one from scripts/sim/run.sh, one from
# run_pregate's own `go test ./github/...`). ---
reset_marker
( FAKE_GO_EXIT=0 run_pregate ) >/dev/null 2>&1
rc=$?
assert_eq "both layers passing exits 0" "0" "$rc"
assert_eq "both layers passing calls go twice (sim + github)" "2" "$(marker_count)"

# --- Case 3: the sim layer "fails" (fake go exits 1) — run_pregate aborts
# with PREGATE_FAILED_EXIT (5) after exactly ONE go call (scripts/sim/run.sh's
# own), and never reaches the github wire-contract tests call. This is the
# mechanism AC1 relies on: the ordering guarantees the second (and any live)
# call never happens once the first layer fails. ---
reset_marker
( FAKE_GO_EXIT=1 run_pregate ) >/dev/null 2>&1
rc=$?
assert_eq "sim-layer failure exits PREGATE_FAILED_EXIT (5)" "5" "$rc"
assert_eq "sim-layer failure stops after exactly one go call (github tests never run)" "1" "$(marker_count)"

# --- Case 4 (R5, #1624): FABRIK_PREGATE_VERIFIED_SHA matching the current
# HEAD skips the pre-gate entirely — zero go calls, exit 0 — even though the
# fake go is scripted to fail, proving the skip really does short-circuit
# before either layer runs rather than merely tolerating a pass. ---
reset_marker
CURRENT_SHA="$(git -C "$REPO_ROOT" rev-parse HEAD)"
( FAKE_GO_EXIT=1 FABRIK_PREGATE_VERIFIED_SHA="$CURRENT_SHA" run_pregate ) >/dev/null 2>&1
rc=$?
assert_eq "matching FABRIK_PREGATE_VERIFIED_SHA exits 0" "0" "$rc"
assert_eq "matching FABRIK_PREGATE_VERIFIED_SHA makes no go call" "0" "$(marker_count)"

# --- Case 5 (R5, #1624): a stale/mismatched FABRIK_PREGATE_VERIFIED_SHA falls
# through to the full pre-gate, same as if it had never been set — never a
# silent false skip. ---
reset_marker
( FAKE_GO_EXIT=0 FABRIK_PREGATE_VERIFIED_SHA="0000000000000000000000000000000000000dead" run_pregate ) >/dev/null 2>&1
rc=$?
assert_eq "mismatched FABRIK_PREGATE_VERIFIED_SHA exits 0 (full pre-gate ran and passed)" "0" "$rc"
assert_eq "mismatched FABRIK_PREGATE_VERIFIED_SHA runs the full pre-gate (2 go calls)" "2" "$(marker_count)"

# --- Case 6 (R5, #1624 — found during Review): a matching
# FABRIK_PREGATE_VERIFIED_SHA does NOT skip the pre-gate if the working tree
# has picked up an uncommitted change since the SHA was captured — a HEAD
# match alone can't detect that (a real gap: cut-release.sh's own step 4 can
# rewrite plugin/known_embedded_versions.go on disk between capturing the SHA
# and this check running). An untracked scratch file is enough to make the
# tree dirty without touching anything tracked. ---
reset_marker
: > "$DIRTY_TREE_FILE"
CURRENT_SHA="$(git -C "$REPO_ROOT" rev-parse HEAD)"
( FAKE_GO_EXIT=0 FABRIK_PREGATE_VERIFIED_SHA="$CURRENT_SHA" run_pregate ) >/dev/null 2>&1
rc=$?
rm -f "$DIRTY_TREE_FILE"
assert_eq "matching SHA + dirty tree exits 0 (full pre-gate ran and passed)" "0" "$rc"
assert_eq "matching SHA + dirty tree runs the full pre-gate (2 go calls), not a skip" "2" "$(marker_count)"

# --- Case 7 (REQ7, #1677): a matching FABRIK_PREGATE_VERIFIED_SHA DOES skip
# the pre-gate when the only dirty file matches a caller-declared
# FABRIK_PREGATE_ALLOWED_DIRTY_REGEX — this is the mechanism that makes
# cut-release.sh's own step 4 self-write (plugin/known_embedded_versions.go)
# stop defeating the dedup guard on every real release. Zero go calls, exit
# 0, same as Case 4's clean-tree skip. ---
reset_marker
: > "$DIRTY_TREE_FILE"
CURRENT_SHA="$(git -C "$REPO_ROOT" rev-parse HEAD)"
ALLOWED_REGEX='^\?\? \.pregate_test_dirty_tree_scratch$'
( FAKE_GO_EXIT=1 FABRIK_PREGATE_VERIFIED_SHA="$CURRENT_SHA" FABRIK_PREGATE_ALLOWED_DIRTY_REGEX="$ALLOWED_REGEX" run_pregate ) >/dev/null 2>&1
rc=$?
rm -f "$DIRTY_TREE_FILE"
assert_eq "matching SHA + only-allowlisted dirt exits 0 (skipped)" "0" "$rc"
assert_eq "matching SHA + only-allowlisted dirt makes no go call" "0" "$(marker_count)"

# --- Case 8 (REQ7, #1677 — preserves the TOCTOU protection): a matching
# FABRIK_PREGATE_VERIFIED_SHA does NOT skip when a dirty file does NOT match
# the declared allowlist, even though the same allowlist var is set — an
# unvetted change must still fall through to the full pre-gate exactly like
# Case 6's unscoped-dirty-tree case. This is what proves the allowlist
# narrows rather than replaces the dirty-tree check. ---
reset_marker
: > "$DIRTY_TREE_FILE"
CURRENT_SHA="$(git -C "$REPO_ROOT" rev-parse HEAD)"
UNRELATED_REGEX='^\?\? some/other/file\.go$'
( FAKE_GO_EXIT=0 FABRIK_PREGATE_VERIFIED_SHA="$CURRENT_SHA" FABRIK_PREGATE_ALLOWED_DIRTY_REGEX="$UNRELATED_REGEX" run_pregate ) >/dev/null 2>&1
rc=$?
rm -f "$DIRTY_TREE_FILE"
assert_eq "matching SHA + non-allowlisted dirt exits 0 (full pre-gate ran and passed)" "0" "$rc"
assert_eq "matching SHA + non-allowlisted dirt runs the full pre-gate (2 go calls), not a skip" "2" "$(marker_count)"

# --- Structural check: run_pregate must precede prepare_bed_and_reset inside
# the dispatch guard, textually — this is what makes "no live call is made"
# true by construction rather than by accident of today's implementation.
# Guards against a future edit reordering the two calls without any of the
# behavioral assertions above catching it (they only exercise run_pregate in
# isolation, not the guard's own call order). ---
GUARD_BLOCK="$(awk '/^if \[ "\$\{BASH_SOURCE\[0\]\}" = "\$\{0\}" \]; then$/,0' "$REPO_ROOT/scripts/e2e/run.sh")"
PREGATE_LINE="$(printf '%s\n' "$GUARD_BLOCK" | grep -n 'run_pregate' | head -1 | cut -d: -f1)"
PREPARE_LINE="$(printf '%s\n' "$GUARD_BLOCK" | grep -n 'prepare_bed_and_reset "\$@"' | head -1 | cut -d: -f1)"
if [ -n "$PREGATE_LINE" ] && [ -n "$PREPARE_LINE" ] && [ "$PREGATE_LINE" -lt "$PREPARE_LINE" ]; then
  echo "PASS: run_pregate precedes prepare_bed_and_reset in the dispatch guard (line $PREGATE_LINE < $PREPARE_LINE)"
else
  echo "FAIL: run_pregate does not precede prepare_bed_and_reset in the dispatch guard (pregate=$PREGATE_LINE, prepare=$PREPARE_LINE)"
  FAILED=1
fi

if [ "$FAILED" -ne 0 ]; then
  echo "=== pregate_test.sh: FAILED ==="
  exit 1
fi
echo "=== pregate_test.sh: all checks passed ==="
