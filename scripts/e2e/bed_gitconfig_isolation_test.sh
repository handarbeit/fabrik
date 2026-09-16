#!/usr/bin/env bash
# scripts/e2e/bed_gitconfig_isolation_test.sh — regression coverage for
# #1756/R5's bed git-config isolation in scripts/e2e/run.sh.
#
# The live e2e bed used to launch the fabrik daemon inheriting the operator's
# full environment, including any global url.*.insteadOf rewrite — the exact
# host-config accident that masks D2 (App-auth + default-HTTPS worker git
# 403, see ADR-1756) on an operator's own machine. write_isolated_bed_gitconfig
# generates a bed-local git config that keeps only credential.* entries
# (so PAT-mode HTTPS git keeps working) and drops everything else,
# especially any insteadOf rewrite. This script exercises that function
# directly against a synthetic host config — no live bed or network needed
# — mirroring backoff_detection_test.sh's sourced-fixture shape.
#
# Usage: scripts/e2e/bed_gitconfig_isolation_test.sh
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
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

SYNTHETIC_HOST_CONFIG="$WORKDIR/host-gitconfig"
OUT_CONFIG="$WORKDIR/bed-isolated-gitconfig"

check_contains() {
  local desc="$1" needle="$2"
  if grep -qF "$needle" "$OUT_CONFIG"; then
    echo "PASS: $desc"
  else
    echo "FAIL: $desc — expected to find $(printf '%q' "$needle") in $OUT_CONFIG"
    FAILED=1
  fi
}

check_not_contains() {
  local desc="$1" needle="$2"
  if grep -qF "$needle" "$OUT_CONFIG" 2>/dev/null; then
    echo "FAIL: $desc — did not expect to find $(printf '%q' "$needle") in $OUT_CONFIG"
    FAILED=1
  else
    echo "PASS: $desc"
  fi
}

# --- Case: a host config with both a credential helper and an insteadOf
# rewrite (the exact combination that masks D2 on an operator's own
# machine) — the generated bed config must keep the former and drop the
# latter. ---
cat >"$SYNTHETIC_HOST_CONFIG" <<'EOF'
[user]
	name = Test Operator
	email = operator@example.com
[credential]
	helper = osxkeychain
[credential "https://github.com"]
	helper = !gh auth git-credential
[url "git@github.com:"]
	insteadOf = https://github.com/
EOF

export GIT_CONFIG_GLOBAL="$SYNTHETIC_HOST_CONFIG"
export GIT_CONFIG_NOSYSTEM=1
write_isolated_bed_gitconfig "$OUT_CONFIG"
unset GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM

check_contains "generic credential.helper preserved" "osxkeychain"
check_contains "host-scoped credential.helper preserved" "gh auth git-credential"
check_not_contains "insteadOf rewrite dropped" "insteadOf"
check_not_contains "insteadOf rewrite dropped (key form)" "git@github.com"
check_not_contains "unrelated user.* config dropped" "Test Operator"

# --- Case: a host config with no credential.* entries at all -> the
# generated file must exist and be empty (not an error). ---
: >"$SYNTHETIC_HOST_CONFIG"
: >"$OUT_CONFIG.stale" # sentinel so we can confirm OUT_CONFIG is overwritten, not left stale
export GIT_CONFIG_GLOBAL="$SYNTHETIC_HOST_CONFIG"
export GIT_CONFIG_NOSYSTEM=1
write_isolated_bed_gitconfig "$OUT_CONFIG"
unset GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM

if [ -s "$OUT_CONFIG" ]; then
  echo "FAIL: expected an empty generated config when the host has no credential.* entries, got:"
  cat "$OUT_CONFIG"
  FAILED=1
else
  echo "PASS: empty host credential config produces an empty generated config"
fi

if [ "$FAILED" -ne 0 ]; then
  echo "one or more assertions failed"
  exit 1
fi
echo "all assertions passed"
