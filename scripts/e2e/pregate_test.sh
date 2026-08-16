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
trap 'rm -rf "$FAKE_BIN_DIR" "$MARKER_FILE"' EXIT

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
