#!/usr/bin/env bash
# scripts/wire-contract/record-fixtures.sh — re-record the golden fixtures
# under github/testdata/recordings/ against a live GitHub account.
#
# This is the "R2/R4" half of the wire-contract test layer (see issue #1453 /
# github/testdata/README.md). Existing httptest-based tests for the R5
# priority set serve these recordings instead of hand-authored JSON literals
# — this script is how they get regenerated when GitHub's actual response
# shape changes.
#
# Refuses to run in CI (see the CI guard below) — this hits real GitHub
# endpoints, including real mutations, and must never run unattended.
#
# ── R3a: mutation safety ──────────────────────────────────────────────────
# Read operations (board fetch, PR review fetch, check-run fetch) are safe
# to record against the live private handarbeit/fabrik board — nothing is
# mutated.
#
# Mutation operations (addBlockedBy, project item status change, PR
# create/mark-ready/merge, label add) are NEVER run against handarbeit/fabrik.
# They run against the disposable handarbeit/fabrik-test-alpha sandbox repo
# and its "Fabrik Test" project board (project #2), creating throwaway
# issues/PRs/branches for the sole purpose of capturing a real response, then
# cleaning up via scripts/e2e/reset.sh. See ADR references in
# github/testdata/README.md for why this exists.
#
# Usage:
#   scripts/wire-contract/record-fixtures.sh              # record everything
#   scripts/wire-contract/record-fixtures.sh reads-only    # skip mutation ops
#
# Requires a `gh` CLI already authenticated with a token that has at least
# `repo` and `project` scope on both handarbeit/fabrik (read) and
# handarbeit/fabrik-test-alpha (read/write), and admin rights on
# fabrik-test-alpha sufficient to bypass its branch-protection required
# status checks for the disposable merge (the same account already used by
# scripts/e2e/reset.sh satisfies this). Prefers a test-bed token from
# $FABRIK_TEST_DIR/.env (FABRIK_TOKEN), mirroring reset.sh's convention;
# falls back to the ambient `gh auth` session if that file doesn't exist.
#
# Cadence: run this whenever refresh-schema.sh surfaces a schema change that
# touches one of the R5 priority operations, or whenever a live run surfaces
# a wire-format gap this suite should have caught (i.e. treat a production
# incident traced to github/ as a trigger to re-record, not just to patch
# the code).

set -euo pipefail

if [ -n "${CI:-}" ] || [ -n "${GITHUB_ACTIONS:-}" ]; then
  echo "record-fixtures.sh: refusing to run under CI/GITHUB_ACTIONS — this hits live GitHub, including real mutations." >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RECORDINGS_DIR="$REPO_ROOT/github/testdata/recordings"
MODE="${1:-all}"

TEST_BED="${FABRIK_TEST_DIR:-$HOME/dev/fabrik-test}"
ALPHA="${FABRIK_TEST_REPO_ALPHA:-handarbeit/fabrik-test-alpha}"
LIVE_REPO="${FABRIK_LIVE_REPO:-handarbeit/fabrik}"
LIVE_PROJECT_OWNER="${FABRIK_LIVE_PROJECT_OWNER:-handarbeit}"
LIVE_PROJECT_NUMBER="${FABRIK_LIVE_PROJECT_NUMBER:-1}"
SANDBOX_PROJECT_NUMBER="${FABRIK_TEST_PROJECT_NUMBER:-2}"

if [ -f "$TEST_BED/.env" ]; then
  TOKEN=$(grep '^FABRIK_TOKEN=' "$TEST_BED/.env" | head -1 | cut -d= -f2- || true)
fi
gh_() {
  if [ -n "${TOKEN:-}" ]; then
    GH_TOKEN="$TOKEN" gh "$@"
  else
    gh "$@"
  fi
}

mkdir -p "$RECORDINGS_DIR"
SCRUBCMD=(go run "$REPO_ROOT/scripts/wire-contract/scrubcmd")

# write_recording OPERATION ENDPOINT SOURCE_REPO RAW_RESPONSE_FILE
# Scrubs raw_response_file, checks it's clean, and writes the
# provenance-wrapped recording. Aborts the whole run if scrubbing still
# finds something after redaction — that means a pattern this script relies
# on to be redactable actually wasn't, which needs a human, not a silent
# partial fixture.
write_recording() {
  local operation="$1" endpoint="$2" source_repo="$3" raw_file="$4"
  local scrubbed_file
  scrubbed_file="$(mktemp)"
  "${SCRUBCMD[@]}" -in "$raw_file" -out "$scrubbed_file"
  if ! "${SCRUBCMD[@]}" -check -in "$scrubbed_file"; then
    echo "record-fixtures.sh: $operation still contains secret-shaped content after redaction — aborting, not writing a fixture" >&2
    rm -f "$scrubbed_file"
    exit 1
  fi
  python3 - "$operation" "$endpoint" "$source_repo" "$scrubbed_file" "$RECORDINGS_DIR/$operation.json" <<'PY'
import json, sys, datetime
operation, endpoint, source_repo, scrubbed_file, out_file = sys.argv[1:6]
with open(scrubbed_file) as f:
    response = json.load(f)
out = {
    "provenance": {
        "operation": operation,
        "endpoint": endpoint,
        "recorded_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "source_repo": source_repo,
        "scrubbed": True,
    },
    "response": response,
}
with open(out_file, "w") as f:
    json.dump(out, f, indent=2)
PY
  rm -f "$scrubbed_file"
  echo "  wrote $RECORDINGS_DIR/$operation.json"
}

echo "== read operations (safe against live $LIVE_REPO board) =="

echo "-- fetch_project_board --"
BOARD_QUERY='query($owner: String!, $projectNum: Int!, $cursor: String) {
  organization(login: $owner) {
    projectV2(number: $projectNum) {
      id
      title
      items(first: 5, after: $cursor) {
        totalCount
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          updatedAt
          fieldValueByName(name: "Status") {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
          content {
            __typename
            ... on Issue {
              id number title state updatedAt
              repository { nameWithOwner }
              labels(first: 30) { nodes { name } }
              closedByPullRequestsReferences(first: 5) {
                nodes {
                  updatedAt number headRefOid isMergeQueueEnabled isInMergeQueue
                  mergeQueueEntry { state position enqueuer { login } }
                }
              }
            }
            ... on PullRequest {
              id number title updatedAt
              repository { nameWithOwner }
              labels(first: 30) { nodes { name } }
            }
          }
        }
      }
    }
  }
}'
gh_ api graphql -f query="$BOARD_QUERY" -f owner="$LIVE_PROJECT_OWNER" -F projectNum="$LIVE_PROJECT_NUMBER" > /tmp/wc-board.json
write_recording fetch_project_board "https://api.github.com/graphql" "$LIVE_REPO (project #$LIVE_PROJECT_NUMBER, read-only)" /tmp/wc-board.json

echo "-- fetch_pr_reviews / fetch_check_runs (from the most recently merged $LIVE_REPO PR) --"
REC_PR=$(gh_ pr list -R "$LIVE_REPO" --state merged --limit 1 --json number --jq '.[0].number')
REC_SHA=$(gh_ pr view "$REC_PR" -R "$LIVE_REPO" --json headRefOid --jq .headRefOid)
gh_ api "repos/$LIVE_REPO/pulls/$REC_PR/reviews?per_page=100" > /tmp/wc-reviews.json
write_recording fetch_pr_reviews "GET /repos/{owner}/{repo}/pulls/{pull_number}/reviews" "$LIVE_REPO#$REC_PR (read-only)" /tmp/wc-reviews.json
gh_ api "repos/$LIVE_REPO/commits/$REC_SHA/check-runs?per_page=100" > /tmp/wc-checkruns.json
write_recording fetch_check_runs "GET /repos/{owner}/{repo}/commits/{sha}/check-runs" "$LIVE_REPO#$REC_PR (read-only)" /tmp/wc-checkruns.json

if [ "$MODE" = "reads-only" ]; then
  echo "reads-only mode: skipping mutation recordings."
  echo "done."
  exit 0
fi

echo "== mutation operations (sandbox $ALPHA only — never $LIVE_REPO) =="

echo "-- add_blocked_by --"
ISSUE_A=$(gh_ issue create -R "$ALPHA" --title "wire-contract fixture: blocker issue" --body "Disposable issue for #1453 wire-contract fixture recording. Safe to delete." | grep -oE '[0-9]+$')
ISSUE_B=$(gh_ issue create -R "$ALPHA" --title "wire-contract fixture: blocked issue" --body "Disposable issue for #1453 wire-contract fixture recording. Safe to delete." | grep -oE '[0-9]+$')
NODE_IDS=$(gh_ api graphql -f query="query { repository(owner:\"${ALPHA%%/*}\", name:\"${ALPHA#*/}\") { a: issue(number:$ISSUE_A){ id } b: issue(number:$ISSUE_B){ id } } }")
BLOCKER_ID=$(echo "$NODE_IDS" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["repository"]["a"]["id"])')
BLOCKED_ID=$(echo "$NODE_IDS" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["repository"]["b"]["id"])')
gh_ api graphql -f query='
mutation($issueId: ID!, $blockingIssueId: ID!) {
  addBlockedBy(input: {issueId: $issueId, blockingIssueId: $blockingIssueId}) {
    issue { id }
  }
}' -f issueId="$BLOCKED_ID" -f blockingIssueId="$BLOCKER_ID" > /tmp/wc-addblockedby.json
write_recording add_blocked_by "https://api.github.com/graphql" "$ALPHA#$ISSUE_A,#$ISSUE_B (disposable sandbox issues)" /tmp/wc-addblockedby.json

echo "-- update_project_item_status --"
PROJECT_ID=$(gh_ api graphql -f query="query { organization(login:\"${ALPHA%%/*}\") { projectV2(number: $SANDBOX_PROJECT_NUMBER) { id } } }" --jq '.data.organization.projectV2.id')
STATUS_FIELD=$(gh_ api graphql -f query="query { organization(login:\"${ALPHA%%/*}\") { projectV2(number: $SANDBOX_PROJECT_NUMBER) { fields(first: 20) { nodes { ... on ProjectV2SingleSelectField { id name options { id name } } } } } } }")
FIELD_ID=$(echo "$STATUS_FIELD" | python3 -c 'import json,sys
d=json.load(sys.stdin)["data"]["organization"]["projectV2"]["fields"]["nodes"]
f=next(n for n in d if n.get("name")=="Status")
print(f["id"])')
OPTION_ID=$(echo "$STATUS_FIELD" | python3 -c 'import json,sys
d=json.load(sys.stdin)["data"]["organization"]["projectV2"]["fields"]["nodes"]
f=next(n for n in d if n.get("name")=="Status")
opt=next(o for o in f["options"] if o["name"]=="Implement")
print(opt["id"])')
ITEM=$(gh_ api graphql -f query='mutation($projectId: ID!, $contentId: ID!) { addProjectV2ItemById(input: {projectId: $projectId, contentId: $contentId}) { item { id } } }' -f projectId="$PROJECT_ID" -f contentId="$BLOCKER_ID" --jq '.data.addProjectV2ItemById.item.id')
gh_ api graphql -f query='
mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
  updateProjectV2ItemFieldValue(input: {
    projectId: $projectId, itemId: $itemId, fieldId: $fieldId,
    value: { singleSelectOptionId: $optionId }
  }) {
    projectV2Item { id }
  }
}' -f projectId="$PROJECT_ID" -f itemId="$ITEM" -f fieldId="$FIELD_ID" -f optionId="$OPTION_ID" > /tmp/wc-updatestatus.json
write_recording update_project_item_status "https://api.github.com/graphql" "$LIVE_PROJECT_OWNER (project #$SANDBOX_PROJECT_NUMBER \"Fabrik Test\", disposable sandbox item)" /tmp/wc-updatestatus.json

echo "-- add_label_to_issue --"
gh_ api -X POST "repos/$ALPHA/issues/$ISSUE_B/labels" -f "labels[]=wire-contract-fixture-test" > /tmp/wc-addlabel.json
write_recording add_label_to_issue "POST /repos/{owner}/{repo}/issues/{issue_number}/labels" "$ALPHA#$ISSUE_B (disposable sandbox issue)" /tmp/wc-addlabel.json

echo "-- create_draft_pr / mark_pr_ready / merge_pr --"
WORKDIR=$(mktemp -d)
GIT_TOKEN="${TOKEN:-$(gh_ auth token)}"
git -C "$WORKDIR" clone -q "https://x-access-token:${GIT_TOKEN}@github.com/$ALPHA.git" repo
BRANCH="wire-contract-fixture-pr-$$"
git -C "$WORKDIR/repo" checkout -q -b "$BRANCH"
echo "wire-contract fixture PR — disposable, safe to delete" > "$WORKDIR/repo/WIRE_CONTRACT_FIXTURE.md"
git -C "$WORKDIR/repo" add WIRE_CONTRACT_FIXTURE.md
git -C "$WORKDIR/repo" -c user.email="wire-contract-fixture@handarbeit.io" -c user.name="wire-contract-fixture" commit -q -m "chore: wire-contract fixture PR (disposable)"
git -C "$WORKDIR/repo" push -q -u origin "$BRANCH"

gh_ api -X POST "repos/$ALPHA/pulls" \
  -f title="chore: wire-contract fixture PR (disposable)" \
  -f head="$BRANCH" -f base="main" \
  -f body="Disposable PR for #1453 wire-contract fixture recording. Safe to close/delete." \
  -F draft=true > /tmp/wc-createpr.json
write_recording create_draft_pr "POST /repos/{owner}/{repo}/pulls" "$ALPHA (disposable sandbox PR)" /tmp/wc-createpr.json
PR_NUM=$(python3 -c 'import json; print(json.load(open("/tmp/wc-createpr.json"))["number"])')

PR_NODE_ID=$(gh_ api graphql -f query="query { repository(owner:\"${ALPHA%%/*}\", name:\"${ALPHA#*/}\") { pullRequest(number: $PR_NUM) { id } } }" --jq '.data.repository.pullRequest.id')
gh_ api graphql -f query='
mutation($prId: ID!) {
  markPullRequestReadyForReview(input: { pullRequestId: $prId }) {
    pullRequest { id }
  }
}' -f prId="$PR_NODE_ID" > /tmp/wc-markready.json
write_recording mark_pr_ready "https://api.github.com/graphql" "$ALPHA#$PR_NUM (disposable sandbox PR)" /tmp/wc-markready.json

# fabrik-test-alpha's branch protection requires status checks that never
# run for a throwaway branch; the merge only succeeds because the recording
# account has admin rights on the sandbox repo and branch protection there
# has enforce_admins=false. This is sandbox-only behavior — Fabrik's own
# MergePR in production code self-gates on CI status before ever attempting
# this call, unrelated to whether the account merging could bypass protection.
gh_ api -X PUT "repos/$ALPHA/pulls/$PR_NUM/merge" -f merge_method="merge" > /tmp/wc-mergepr.json
write_recording merge_pr "PUT /repos/{owner}/{repo}/pulls/{pull_number}/merge" "$ALPHA#$PR_NUM (disposable sandbox PR)" /tmp/wc-mergepr.json

rm -rf "$WORKDIR"
gh_ api -X DELETE "repos/$ALPHA/git/refs/heads/$BRANCH" >/dev/null 2>&1 || true

echo "== cleaning up sandbox state via scripts/e2e/reset.sh =="
"$REPO_ROOT/scripts/e2e/reset.sh" || echo "  (reset.sh failed or test bed not configured — clean up $ALPHA manually)"

echo "done. Review 'git diff github/testdata/recordings/' before committing."
