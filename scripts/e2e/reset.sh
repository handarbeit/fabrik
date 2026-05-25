#!/usr/bin/env bash
# scripts/e2e/reset.sh — clear state from prior e2e runs.
#
# Closes every OPEN issue in handarbeit/fabrik-test-alpha and fabrik-test-beta.
# Useful before a clean test session, or to reclaim a half-stuck test bed.
#
# Usage:
#   scripts/e2e/reset.sh             # closes issues only
#   scripts/e2e/reset.sh --worktrees # also removes Fabrik's worktrees + bare clones
#
# The --worktrees mode is destructive — it deletes ~/dev/fabrik-test/.fabrik/worktrees/
# and ~/dev/fabrik-test/.fabrik/repos/. Use when the test bed itself is wedged.

set -euo pipefail

TEST_BED="${FABRIK_TEST_DIR:-$HOME/dev/fabrik-test}"
ALPHA="${FABRIK_TEST_REPO_ALPHA:-handarbeit/fabrik-test-alpha}"
BETA="${FABRIK_TEST_REPO_BETA:-handarbeit/fabrik-test-beta}"

if [ ! -f "$TEST_BED/.env" ]; then
  echo "test bed not found at $TEST_BED (expected .env)" >&2
  exit 1
fi

TOKEN=$(grep '^FABRIK_TOKEN=' "$TEST_BED/.env" | head -1 | cut -d= -f2-)
if [ -z "$TOKEN" ]; then
  echo "FABRIK_TOKEN missing from $TEST_BED/.env" >&2
  exit 1
fi

close_open_in() {
  local repo="$1"
  local nums
  nums=$(GH_TOKEN="$TOKEN" gh issue list -R "$repo" --state open --json number --jq '.[].number')
  if [ -z "$nums" ]; then
    echo "  $repo: no open issues"
    return
  fi
  echo "  $repo: closing: $(echo $nums | tr '\n' ' ')"
  for n in $nums; do
    GH_TOKEN="$TOKEN" gh issue close "$n" -R "$repo" \
      --reason completed --comment "Closed by scripts/e2e/reset.sh" >/dev/null
  done
}

echo "== closing open issues =="
close_open_in "$ALPHA"
close_open_in "$BETA"

if [ "${1:-}" = "--worktrees" ]; then
  echo "== removing Fabrik worktrees + bare clones from $TEST_BED =="
  # Stop the Fabrik instance first if running — otherwise it'll keep using the
  # deleted dirs and produce confusing errors.
  pid=$(ps ax -o pid,command | grep "$TEST_BED/fabrik" | grep -v grep | awk '{print $1}' | head -1)
  if [ -n "$pid" ]; then
    echo "  fabrik-test pid $pid is running — stop it before --worktrees" >&2
    exit 1
  fi
  rm -rf "$TEST_BED/.fabrik/worktrees" "$TEST_BED/.fabrik/repos"
  rm -f  "$TEST_BED/.fabrik/fabrik.lock"
  echo "  worktrees + bare clones removed; restart fabrik-test to re-init"
fi

echo "done."
