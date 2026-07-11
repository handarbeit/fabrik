#!/usr/bin/env bash
# Compact one-line-per-issue view of the Fabrik project board.
#
# By default, hides items in the Done column (usually noise) and hides the
# ubiquitous `stage:*:complete` labels — pass --all / --raw-labels to unhide.
#
# Usage: board.sh [--json] [--all] [--raw-labels] [--column NAME] [--label PATTERN] [--limit N]
#   --json           emit raw JSON from `gh project item-list`
#   --all            include items in the Done column
#   --raw-labels     don't hide `stage:*:complete` labels
#   --column NAME    filter to a single column (e.g. Implement)
#   --label PATTERN  filter to issues whose labels contain PATTERN (substring)
#   --limit N        max items to fetch from the project (default 300)
#
# Output columns:  <Column>  #N  <Title>  [<active fabrik/stage/model/effort/base labels>]
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/lib.sh"

fabrik_load_config

JSON=false
FILTER_COL=""
FILTER_LABEL=""
INCLUDE_DONE=false
RAW_LABELS=false
LIMIT=300
while [ $# -gt 0 ]; do
  case "$1" in
    --json)        JSON=true ;;
    --all)         INCLUDE_DONE=true ;;
    --raw-labels)  RAW_LABELS=true ;;
    --column)      shift; FILTER_COL=${1:-} ;;
    --label)       shift; FILTER_LABEL=${1:-} ;;
    --limit)       shift; LIMIT=${1:-300} ;;
    -h|--help)
      awk 'NR==1{next} /^#/{sub(/^# ?/, ""); print; next} {exit}' "$0"
      exit 0 ;;
    *) echo "board.sh: unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

# Push the Done filter server-side when possible; --column overrides.
query=""
if ! $INCLUDE_DONE && [ -z "$FILTER_COL" ]; then
  query="-status:Done"
elif [ -n "$FILTER_COL" ]; then
  query="status:\"$FILTER_COL\""
fi

if [ -n "$query" ]; then
  raw=$(gh project item-list "$FABRIK_PROJECT" --owner "$FABRIK_OWNER" \
    --limit "$LIMIT" --format json --query "$query")
else
  raw=$(gh project item-list "$FABRIK_PROJECT" --owner "$FABRIK_OWNER" \
    --limit "$LIMIT" --format json)
fi

if $JSON; then
  printf '%s\n' "$raw"
  exit 0
fi

printf '%s\n' "$raw" | jq -r \
  --arg lbl "$FILTER_LABEL" \
  --argjson raw "$RAW_LABELS" '
  .items // []
  | map(select(.content.number != null))
  | map({
      num:    .content.number,
      title:  (.content.title // ""),
      repo:   (.content.repository // ""),
      labels: (.labels // []),
      column: (.status // "-")
    })
  | (if $lbl != "" then map(select(.labels | any(contains($lbl)))) else . end)
  | sort_by(.column, .num)
  | .[]
  | (
      .labels
      | map(select(startswith("fabrik:") or startswith("stage:") or startswith("model:") or startswith("effort:") or startswith("base:")))
      | (if $raw then . else map(select(endswith(":complete") | not)) end)
      | join(",")
    ) as $flags
  | "\(.column)\t#\(.num)\t\(.title | .[0:70])\t[\($flags)]"
' | awk -F'\t' '{ printf "%-12s %-6s %-70s %s\n", $1, $2, $3, $4 }'
