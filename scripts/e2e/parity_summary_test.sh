#!/usr/bin/env bash
# scripts/e2e/parity_summary_test.sh — coverage for R6's (#1933) sim-parity
# summary line in scripts/e2e/run.sh: print_sim_parity_summary reads
# tests/e2e/registry/registry.json with jq and prints
# "sim parity: N covered, M live-only, K gap". It is informational — it must
# always return 0 and degrade to "unavailable" rather than print a wrong count —
# and it is called from the dispatch guard outside run_pregate so it also prints
# under E2E_SKIP_PREGATE.
#
# Sources run.sh for function definitions only (the sourcing guard prevents a
# gate run), like pregate_test.sh.
#
# Usage: scripts/e2e/parity_summary_test.sh
# Exit 0 if all assertions pass, 1 otherwise.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
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

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat > "$TMP/registry.json" <<'JSON'
{"version":1,"tests":[
 {"name":"TestA","parity":"sim","sim":["TestSimA"]},
 {"name":"TestB","parity":"sim","sim":["TestSimB"]},
 {"name":"TestC","parity":"live-only","live_only_reason":"real-ci"},
 {"name":"TestD","parity":"gap"},
 {"name":"TestE","parity":"gap"},
 {"name":"TestF","parity":"gap"}
]}
JSON

# Case 1: counts come from the registry.
out="$(E2E_PARITY_REGISTRY="$TMP/registry.json" print_sim_parity_summary)"
assert_eq "counts line" "sim parity: 2 covered, 1 live-only, 3 gap" "$out"

# Case 2: prints under E2E_SKIP_PREGATE too (it is independent of run_pregate).
out="$(E2E_SKIP_PREGATE=1 E2E_PARITY_REGISTRY="$TMP/registry.json" print_sim_parity_summary)"
assert_eq "prints when the pre-gate is skipped" "sim parity: 2 covered, 1 live-only, 3 gap" "$out"

# Case 3: missing registry degrades, exit 0.
out="$(E2E_PARITY_REGISTRY="$TMP/nope.json" print_sim_parity_summary)"
rc=$?
assert_eq "missing registry exits 0" "0" "$rc"
case "$out" in
  "sim parity: unavailable (registry not found"*) assert_eq "missing registry degrades" ok ok ;;
  *) assert_eq "missing registry degrades" "sim parity: unavailable (registry not found...)" "$out" ;;
esac

# Case 4: unparseable registry degrades, exit 0.
echo 'not json' > "$TMP/bad.json"
out="$(E2E_PARITY_REGISTRY="$TMP/bad.json" print_sim_parity_summary)"
rc=$?
assert_eq "bad registry exits 0" "0" "$rc"
case "$out" in
  "sim parity: unavailable (cannot parse"*) assert_eq "bad registry degrades" ok ok ;;
  *) assert_eq "bad registry degrades" "sim parity: unavailable (cannot parse...)" "$out" ;;
esac

# Case 5: no jq on PATH degrades, exit 0 (PATH reduced to an empty dir; the
# function only needs builtins up to the jq probe).
mkdir "$TMP/empty"
out="$(PATH="$TMP/empty" E2E_PARITY_REGISTRY="$TMP/registry.json" print_sim_parity_summary)"
rc=$?
assert_eq "no jq exits 0" "0" "$rc"
assert_eq "no jq degrades" "sim parity: unavailable (jq not found)" "$out"

# Case 6: the real registry yields a well-formed line.
out="$(print_sim_parity_summary)"
case "$out" in
  "sim parity: "[0-9]*" covered, "[0-9]*" live-only, "[0-9]*" gap") assert_eq "real registry line shape" ok ok ;;
  *) assert_eq "real registry line shape" "sim parity: N covered, M live-only, K gap" "$out" ;;
esac

# Case 7: the dispatch guard calls the summary outside run_pregate and before it.
call_line="$(grep -n '^  print_sim_parity_summary$' "$REPO_ROOT/scripts/e2e/run.sh" | cut -d: -f1)"
pregate_line="$(grep -n '^  run_pregate$' "$REPO_ROOT/scripts/e2e/run.sh" | cut -d: -f1)"
if [ -n "$call_line" ] && [ -n "$pregate_line" ] && [ "$call_line" -lt "$pregate_line" ]; then
  assert_eq "summary is called before run_pregate in the dispatch guard" ok ok
else
  assert_eq "summary is called before run_pregate in the dispatch guard" "call before run_pregate" "call=$call_line pregate=$pregate_line"
fi

exit "$FAILED"
