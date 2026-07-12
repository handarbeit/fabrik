#!/usr/bin/env bash
# scripts/e2e/reset.sh — clear state from prior e2e runs to a clean slate.
#
# Part of the test-prep cycle: run this before a clean suite so the bed starts
# from a known-empty state (stale board items and leftover branches otherwise
# pollute the next run's merge-train snapshots and confuse results).
#
# Default (no flags) resets the shared test bed to a clean slate:
#   - closes every OPEN pull request (deleting its head branch) in alpha + beta
#   - closes every OPEN issue in alpha + beta
#   - deletes any leftover fabrik/* branches in alpha + beta
#   - removes EVERY item from the "Fabrik Test" project board (closed issues
#     linger as board items otherwise — reset.sh used to leave these behind)
#
# Usage:
#   scripts/e2e/reset.sh             # full board/PR/issue/branch clean (run before a suite)
#   scripts/e2e/reset.sh --worktrees # ALSO removes Fabrik's worktrees + bare clones (destructive)
#
# The --worktrees mode is destructive — it deletes ~/dev/fabrik-test/.fabrik/worktrees/
# and ~/dev/fabrik-test/.fabrik/repos/. Use when the test bed itself is wedged;
# stop the Fabrik instance first (it will refuse otherwise).
#
# Overridable via env: FABRIK_TEST_DIR, FABRIK_TEST_REPO_ALPHA, FABRIK_TEST_REPO_BETA,
# FABRIK_TEST_PROJECT_OWNER, FABRIK_TEST_PROJECT_NUMBER.

set -euo pipefail

TEST_BED="${FABRIK_TEST_DIR:-$HOME/dev/fabrik-test}"
ALPHA="${FABRIK_TEST_REPO_ALPHA:-handarbeit/fabrik-test-alpha}"
BETA="${FABRIK_TEST_REPO_BETA:-handarbeit/fabrik-test-beta}"
PROJECT_OWNER="${FABRIK_TEST_PROJECT_OWNER:-handarbeit}"
PROJECT_NUMBER="${FABRIK_TEST_PROJECT_NUMBER:-2}"

if [ ! -f "$TEST_BED/.env" ]; then
  echo "test bed not found at $TEST_BED (expected .env)" >&2
  exit 1
fi

TOKEN=$(grep '^FABRIK_TOKEN=' "$TEST_BED/.env" | head -1 | cut -d= -f2-)
if [ -z "$TOKEN" ]; then
  echo "FABRIK_TOKEN missing from $TEST_BED/.env" >&2
  exit 1
fi

# All GitHub calls go through the test-bed PAT.
gh_() { GH_TOKEN="$TOKEN" gh "$@"; }

close_open_prs_in() {
  local repo="$1"
  local nums n
  nums=$(gh_ pr list -R "$repo" --state open --limit 200 --json number --jq '.[].number')
  if [ -z "$nums" ]; then
    echo "  $repo: no open PRs"
    return
  fi
  echo "  $repo: closing PRs: $(echo $nums | tr '\n' ' ')"
  for n in $nums; do
    # --delete-branch also removes the head branch; fall back to a plain close
    # if the branch is already gone.
    gh_ pr close "$n" -R "$repo" --delete-branch >/dev/null 2>&1 \
      || gh_ pr close "$n" -R "$repo" >/dev/null 2>&1 || true
  done
}

close_open_issues_in() {
  local repo="$1"
  local nums n
  nums=$(gh_ issue list -R "$repo" --state open --limit 500 --json number --jq '.[].number')
  if [ -z "$nums" ]; then
    echo "  $repo: no open issues"
    return
  fi
  echo "  $repo: closing issues: $(echo $nums | tr '\n' ' ')"
  for n in $nums; do
    gh_ issue close "$n" -R "$repo" \
      --reason completed --comment "Closed by scripts/e2e/reset.sh" >/dev/null 2>&1 || true
  done
}

delete_fabrik_branches_in() {
  local repo="$1"
  local refs r
  refs=$(gh_ api "repos/$repo/git/matching-refs/heads/fabrik/" --jq '.[].ref' 2>/dev/null || true)
  if [ -z "$refs" ]; then
    echo "  $repo: no leftover fabrik/* branches"
    return
  fi
  echo "  $repo: deleting $(echo "$refs" | grep -c . ) fabrik/* branch(es)"
  for r in $refs; do
    # matching-refs returns full refs (refs/heads/fabrik/...). The delete
    # endpoint is DELETE /repos/{repo}/git/refs/{ref}, where {ref} is the ref
    # WITHOUT the leading "refs/" (i.e. "heads/fabrik/..."). Strip only "refs/"
    # and append to git/refs/ → git/refs/heads/fabrik/... (verified working;
    # the two earlier variants both produced malformed paths that 404'd).
    gh_ api -X DELETE "repos/$repo/git/refs/${r#refs/}" >/dev/null 2>&1 || true
  done
}

resolve_project_id() {
  # Try org-owned first, then user-owned.
  local id
  id=$(gh_ api graphql -f query="query { organization(login:\"$PROJECT_OWNER\"){ projectV2(number:$PROJECT_NUMBER){ id } } }" \
        --jq '.data.organization.projectV2.id' 2>/dev/null || true)
  if [ -z "$id" ] || [ "$id" = "null" ]; then
    id=$(gh_ api graphql -f query="query { user(login:\"$PROJECT_OWNER\"){ projectV2(number:$PROJECT_NUMBER){ id } } }" \
          --jq '.data.user.projectV2.id' 2>/dev/null || true)
  fi
  [ "$id" = "null" ] && id=""
  echo "$id"
}

drain_board() {
  local pid ids item round remaining
  pid=$(resolve_project_id)
  if [ -z "$pid" ]; then
    echo "  could not resolve project $PROJECT_OWNER/#$PROJECT_NUMBER — skipping board drain" >&2
    return
  fi
  # Loop: fetch a page of item IDs and delete them, until the board is empty.
  # A few rounds absorb GitHub's eventual-consistency lag on deletes.
  for round in $(seq 1 12); do
    ids=$(gh_ api graphql -f query="query { node(id:\"$pid\"){ ... on ProjectV2 { items(first:50){ nodes { id } } } } }" \
          --jq '.data.node.items.nodes[].id' 2>/dev/null || true)
    [ -z "$ids" ] && break
    for item in $ids; do
      gh_ api graphql -f query="mutation { deleteProjectV2Item(input:{projectId:\"$pid\", itemId:\"$item\"}){ deletedItemId } }" \
        >/dev/null 2>&1 || true
    done
  done
  remaining=$(gh_ api graphql -f query="query { node(id:\"$pid\"){ ... on ProjectV2 { items(first:1){ totalCount } } } }" \
              --jq '.data.node.items.totalCount' 2>/dev/null || echo "?")
  echo "  project $PROJECT_OWNER/#$PROJECT_NUMBER drained (remaining items: $remaining)"
}

echo "== closing open PRs (with branch delete) =="
close_open_prs_in "$ALPHA"
close_open_prs_in "$BETA"

echo "== closing open issues =="
close_open_issues_in "$ALPHA"
close_open_issues_in "$BETA"

echo "== deleting leftover fabrik/* branches =="
delete_fabrik_branches_in "$ALPHA"
delete_fabrik_branches_in "$BETA"

echo "== draining project board =="
drain_board

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
