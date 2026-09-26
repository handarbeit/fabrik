#!/usr/bin/env bash
# scripts/e2e/auth_mode_check_test.sh — regression coverage for run.sh's
# auth-mode legs (#1861): E2E_AUTH_MODE resolution, the auth-mode
# preconditions (auth_mode_problems) against fixture bed directories, and the
# competing-token check's downgrade to a warning when no pat leg is planned.
# No live processes, no network: the token check is driven by overriding
# discover_fabrik_process_dirs with fixture candidates.
#
# Usage: scripts/e2e/auth_mode_check_test.sh
# Exit 0 if all assertions pass, 1 otherwise.

set -uo pipefail # no -e: assertions below intentionally continue past failures

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

assert_contains() {
  local desc="$1" needle="$2" haystack="$3"
  case "$haystack" in
    *"$needle"*) echo "PASS: $desc" ;;
    *)
      echo "FAIL: $desc (missing '$needle' in: $haystack)"
      FAILED=1
      ;;
  esac
}

BED="$(mktemp -d)"
OTHER="$(mktemp -d)"
trap 'rm -rf "$BED" "$OTHER"' EXIT
mkdir -p "$BED/.fabrik"

# --- resolve_auth_modes ---
assert_eq "unset E2E_AUTH_MODE runs both, pat first" "pat app" "$(resolve_auth_modes "")"
assert_eq "E2E_AUTH_MODE=pat" "pat" "$(resolve_auth_modes pat)"
assert_eq "E2E_AUTH_MODE=app" "app" "$(resolve_auth_modes app)"
resolve_auth_modes both >/dev/null 2>&1
assert_eq "invalid E2E_AUTH_MODE fails" "1" "$?"

# --- auth_mode_problems ---
echo "owner: handarbeit" >"$BED/.fabrik/config.yaml"
echo "FABRIK_TOKEN=shared" >"$BED/.env"
assert_eq "pat-only with a neutral config: no problems" "" "$(auth_mode_problems "$BED" "pat")"

out="$(auth_mode_problems "$BED" "pat app")"
assert_contains "app leg without E2E_APP_ID is a problem" "E2E_APP_ID is not set" "$out"
assert_contains "app leg without E2E_APP_INSTALLATION_ID is a problem" "E2E_APP_INSTALLATION_ID is not set" "$out"

touch "$BED/.fabrik/key.pem"
cat >>"$BED/.env" <<'EOF'
E2E_APP_ID=4960842
E2E_APP_PRIVATE_KEY_PATH=.fabrik/key.pem
E2E_APP_INSTALLATION_ID=162085522
EOF
assert_eq "app leg with a full identity and readable key: no problems" "" "$(auth_mode_problems "$BED" "pat app")"

sed -i.bak 's#^E2E_APP_PRIVATE_KEY_PATH=.*#E2E_APP_PRIVATE_KEY_PATH=.fabrik/missing.pem#' "$BED/.env"
assert_contains "unreadable key file is a problem" "not a readable file" "$(auth_mode_problems "$BED" "app")"
assert_eq "unreadable key irrelevant to a pat-only run" "" "$(auth_mode_problems "$BED" "pat")"

printf 'owner: handarbeit\n# github_app_id: 1   (commented out is fine)\n' >"$BED/.fabrik/config.yaml"
assert_eq "commented-out github_app_id is not a problem" "" "$(auth_mode_problems "$BED" "pat")"
printf 'owner: handarbeit\ngithub_app_id: 4960842\n' >"$BED/.fabrik/config.yaml"
assert_contains "config.yaml github_app_id is a problem even for pat" "sets github_app_* keys" "$(auth_mode_problems "$BED" "pat")"

# --- check_competing_token_consumers: downgrade when no pat leg ---
echo "FABRIK_TOKEN=shared" >"$OTHER/.env"
TEST_BED="$BED"
BED_TOKEN="shared"
unset E2E_SKIP_TOKEN_CHECK
discover_fabrik_process_dirs() { printf '4242\t%s\n' "$OTHER"; }

out="$(AUTH_MODES="app" check_competing_token_consumers 2>&1)"
rc=$?
assert_eq "app-only run with a competitor: not refused" "0" "$rc"
assert_contains "app-only run with a competitor: warns naming it" "pid 4242" "$out"

out="$( (AUTH_MODES="pat app" check_competing_token_consumers) 2>&1)"
rc=$?
assert_eq "run with a pat leg and a competitor: refused" "$PRECONDITION_FAILED_EXIT" "$rc"
assert_contains "refusal names the competitor" "pid 4242" "$out"

if [ "$FAILED" -ne 0 ]; then
  echo "auth_mode_check_test: FAILED"
  exit 1
fi
echo "auth_mode_check_test: all passed"
