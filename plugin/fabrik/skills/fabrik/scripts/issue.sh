#!/usr/bin/env bash
# Details for a single issue on the Fabrik project board.
#
# Usage: issue.sh <N> [--body] [--json] [-r owner/repo]
#   --body        include the issue body in the output
#   --json        emit raw GraphQL JSON
#   -r owner/repo override repo lookup (skip project-board search)
#
# Fields shown: title, state, repo, column, labels, URL, updated time,
# comment count + last-comment metadata, linked PRs.
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
  echo "usage: issue.sh <N> [--body] [--json] [-r owner/repo]" >&2
  exit 2
fi
shift

BODY=false; JSON=false
while [ $# -gt 0 ]; do
  case "$1" in
    --body) BODY=true ;;
    --json) JSON=true ;;
    -r)     shift; export FABRIK_ISSUE_REPO=${1:-} ;;
    -h|--help)
      awk 'NR==1{next} /^#/{sub(/^# ?/, ""); print; next} {exit}' "$0"
      exit 0 ;;
    *) echo "issue.sh: unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

fabrik_resolve_repo "$N"

query='
query($owner: String!, $repo: String!, $n: Int!) {
  repository(owner: $owner, name: $repo) {
    issue(number: $n) {
      number title state url body createdAt updatedAt
      labels(first: 30) { nodes { name } }
      comments(last: 1) { totalCount nodes { author { login } createdAt } }
      closedByPullRequestsReferences(first: 5, includeClosedPrs: true) {
        nodes { number url state isDraft }
      }
      projectItems(first: 10) {
        nodes {
          project {
            number
            owner {
              ... on Organization { login }
              ... on User         { login }
            }
          }
          fieldValues(first: 20) {
            nodes {
              ... on ProjectV2ItemFieldSingleSelectValue {
                name
                field { ... on ProjectV2SingleSelectField { name } }
              }
            }
          }
        }
      }
    }
  }
}'

raw=$(gh api graphql \
  -F owner="$ISSUE_OWNER" -F repo="$ISSUE_REPO" -F n="$N" \
  -f query="$query")

# Extract just the issue node, augmented with the resolved column for
# this specific project (from projectItems).
issue=$(printf '%s\n' "$raw" | jq --arg proj_owner "$FABRIK_OWNER" --arg proj_num "$FABRIK_PROJECT" '
  .data.repository.issue // null
  | if . == null then null
    else . + {
      column: (
        [.projectItems.nodes[]?
         | select((.project.number | tostring) == $proj_num and .project.owner.login == $proj_owner)
         | .fieldValues.nodes[]?
         | select(.field.name == "Status")
         | .name
        ][0] // "-"
      )
    }
    end
')

if [ "$issue" = "null" ]; then
  echo "issue #$N not found in $ISSUE_OWNER/$ISSUE_REPO" >&2
  exit 1
fi

if $JSON; then
  printf '%s\n' "$issue"
  exit 0
fi

printf '%s\n' "$issue" | jq -r --argjson body "$BODY" \
  --arg repo "$ISSUE_OWNER/$ISSUE_REPO" '
  def prstr:
    "#\(.number) [\(.state)\(if .isDraft then ",draft" else "" end)] \(.url)";
  def lastcomm:
    (.comments.nodes[0]) as $c
    | if $c == null then "none"
      else "last by \((($c.author | .login) // "ghost")) at \($c.createdAt)"
      end;
  "#\(.number)  \(.title)",
  "State:      \(.state)",
  "Repo:       \($repo)",
  "Column:     \(.column)",
  "Labels:     \(.labels.nodes | map(.name) | join(", "))",
  "URL:        \(.url)",
  "Updated:    \(.updatedAt)",
  "Comments:   \(.comments.totalCount) (\(lastcomm))",
  "Linked PRs: \((.closedByPullRequestsReferences.nodes | map(prstr) | if length == 0 then ["none"] else . end | join("; ")))",
  (if $body then "\n--- BODY ---\n\(.body // "(empty)")" else empty end)
'
