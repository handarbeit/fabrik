#!/usr/bin/env bash
# List comments on an issue (or its linked PR) with author, timestamp, reactions.
#
# Usage: comments.sh <N> [--pr] [--last K] [--full] [--json] [-r owner/repo]
#   --pr           list comments on the issue's linked PR instead of the issue itself
#   --last K       show only last K comments (default 10; 0 = all)
#   --full         show full comment bodies (default: truncate to 500 chars)
#   --json         emit raw REST JSON
#   -r owner/repo  override repo lookup (skip project-board search)
#
# Reactions column shows: 👀 (eyes) 🚀 (rocket) 👍 (thumbs up).
# 👀→🚀 is Fabrik's durable "comment processed" state.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/lib.sh"

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  awk 'NR==1{next} /^#/{sub(/^# ?/, ""); print; next} {exit}' "$0"
  exit 0
fi

fabrik_load_config

N=${1:-}
if [ -z "$N" ]; then
  echo "usage: comments.sh <N> [--pr] [--last K] [--full] [--json] [-r owner/repo]" >&2
  exit 2
fi
shift

USE_PR=false; LAST=10; FULL=false; JSON=false
while [ $# -gt 0 ]; do
  case "$1" in
    --pr)   USE_PR=true ;;
    --last) shift; LAST=${1:-10} ;;
    --full) FULL=true ;;
    --json) JSON=true ;;
    -r)     shift; export FABRIK_ISSUE_REPO=${1:-} ;;
    -h|--help)
      awk 'NR==1{next} /^#/{sub(/^# ?/, ""); print; next} {exit}' "$0"
      exit 0 ;;
    *) echo "comments.sh: unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

fabrik_resolve_repo "$N"

TARGET=$N
if $USE_PR; then
  pr=$(gh api graphql -f query="
    query {
      repository(owner: \"$ISSUE_OWNER\", name: \"$ISSUE_REPO\") {
        issue(number: $N) {
          closedByPullRequestsReferences(first: 5, includeClosedPrs: true) {
            nodes { number }
          }
        }
      }
    }" --jq '.data.repository.issue.closedByPullRequestsReferences.nodes[0].number // empty')
  if [ -z "$pr" ]; then
    echo "no linked PR for issue #$N" >&2
    exit 1
  fi
  TARGET=$pr
fi

raw=$(gh api "repos/$ISSUE_OWNER/$ISSUE_REPO/issues/$TARGET/comments?per_page=100" --paginate)

if $JSON; then
  printf '%s\n' "$raw"
  exit 0
fi

# `gh api --paginate` concatenates one JSON array per page; slurp + flatten
# so --last operates across the whole comment history, not per page. Also
# handle deleted users (author == null) gracefully.
printf '%s\n' "$raw" | jq -s -r --argjson last "$LAST" --argjson full "$FULL" '
  add
  | (if $last > 0 then .[-$last:] else . end)
  | .[]
  | "── \(.created_at)  \(((.user | .login) // "ghost"))  [👀\(.reactions.eyes // 0) 🚀\(.reactions.rocket // 0) 👍\(.reactions["+1"] // 0)]",
    (if $full then (.body // "") else ((.body // "") | if length > 500 then .[0:500] + "…" else . end) end),
    ""
'
