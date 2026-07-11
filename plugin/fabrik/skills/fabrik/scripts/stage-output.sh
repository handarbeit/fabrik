#!/usr/bin/env bash
# Show the most recent Fabrik stage-output comment on an issue.
#
# Every Fabrik stage output comment starts with a marker line like:
#   🏭 **Fabrik — stage: Research**
# This script finds the last comment matching the marker for the given stage
# and prints its body — useful for "what did Research find on #N?".
#
# Usage: stage-output.sh <N> <StageName> [-r owner/repo]
# Example: stage-output.sh 852 Plan
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/lib.sh"

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  awk 'NR==1{next} /^#/{sub(/^# ?/, ""); print; next} {exit}' "$0"
  exit 0
fi

fabrik_load_config

N=${1:-}; STAGE=${2:-}
if [ -z "$N" ] || [ -z "$STAGE" ]; then
  echo "usage: stage-output.sh <N> <StageName> [-r owner/repo]" >&2
  exit 2
fi
shift 2
while [ $# -gt 0 ]; do
  case "$1" in
    -r) shift; export FABRIK_ISSUE_REPO=${1:-} ;;
    *)  echo "stage-output.sh: unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

fabrik_resolve_repo "$N"

marker="Fabrik — stage: $STAGE"

gh api "repos/$ISSUE_OWNER/$ISSUE_REPO/issues/$N/comments?per_page=100" --paginate \
  | jq -r --arg m "$marker" '
      map(select((.body // "") | contains($m)))
      | if length == 0 then
          "no comment matching \"\($m)\" on this issue"
        else
          .[-1] as $c
          | "── \($c.created_at)  \($c.user.login)\n\n\($c.body)"
        end
    '
