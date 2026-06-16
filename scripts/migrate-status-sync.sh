#!/usr/bin/env bash
# Sync project board membership and Status from tenaciousvc/projects/1 to
# handarbeit/projects/1.
#
# Issue/PR global node IDs are stable across repo transfers, so this script can
# run before OR after the repo transfer — the IDs resolve to the same content
# either way.
#
# Idempotent: addProjectV2ItemById returns the existing item if the content is
# already on the destination project, so re-running is safe.
#
# Usage:
#   scripts/migrate-status-sync.sh [--dry-run]

set -euo pipefail

SRC_OWNER="tenaciousvc"
SRC_REPO="fabrik"
SRC_PROJECT_NUMBER=1
DST_PROJECT_ID="PVT_kwDOENB0Ac4BW0n_"        # handarbeit/projects/1
DST_STATUS_FIELD_ID="PVTSSF_lADOENB0Ac4BW0n_zhSF0ww"
DONE_OPTION_ID="cee12096"

# Destination Status option IDs (captured at project creation time).
status_option_id() {
  case "$1" in
    Backlog)   echo f2731a62 ;;
    Specify)   echo 949d7c9d ;;
    Research)  echo 3e2db473 ;;
    Plan)      echo d6b797c1 ;;
    Implement) echo 43826d52 ;;
    Review)    echo 24700f0c ;;
    Validate)  echo acf7f2f9 ;;
    Done)      echo cee12096 ;;
    *)         return 1 ;;
  esac
}

DRY_RUN=0
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=1
  echo "(dry-run mode — no mutations)"
fi

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }
}
require gh
require jq

fetch_page() {
  local q='
    query($cursor: String) {
      organization(login: "'"$SRC_OWNER"'") {
        projectV2(number: '"$SRC_PROJECT_NUMBER"') {
          items(first: 100, after: $cursor) {
            pageInfo { hasNextPage endCursor }
            nodes {
              id
              content {
                __typename
                ... on Issue        { id number title state url }
                ... on PullRequest  { id number title state url }
                ... on DraftIssue   { id title }
              }
              fieldValues(first: 30) {
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
    }
  '
  if [[ -n "${1:-}" ]]; then
    gh api graphql -f query="$q" -f cursor="$1"
  else
    gh api graphql -f query="$q"
  fi
}

add_item() {
  local content_id="$1"
  gh api graphql -f query='
    mutation($projectId: ID!, $contentId: ID!) {
      addProjectV2ItemById(input: {projectId: $projectId, contentId: $contentId}) {
        item { id }
      }
    }
  ' -f projectId="$DST_PROJECT_ID" -f contentId="$content_id" \
    --jq '.data.addProjectV2ItemById.item.id'
}

set_status() {
  local item_id="$1" option_id="$2"
  gh api graphql -f query='
    mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
      updateProjectV2ItemFieldValue(input: {
        projectId: $projectId,
        itemId: $itemId,
        fieldId: $fieldId,
        value: { singleSelectOptionId: $optionId }
      }) {
        projectV2Item { id }
      }
    }
  ' -f projectId="$DST_PROJECT_ID" -f itemId="$item_id" \
    -f fieldId="$DST_STATUS_FIELD_ID" -f optionId="$option_id" \
    --jq '.data.updateProjectV2ItemFieldValue.projectV2Item.id' >/dev/null
}

declare -i total=0 synced=0 skipped_draft=0 skipped_unknown=0 errored=0
cursor=""

while :; do
  page=$(fetch_page "$cursor")
  has_next=$(jq -r '.data.organization.projectV2.items.pageInfo.hasNextPage' <<<"$page")
  cursor=$(jq -r '.data.organization.projectV2.items.pageInfo.endCursor // ""' <<<"$page")

  while IFS=$'\t' read -r typename content_id title status; do
    total+=1
    [[ "$status" == "null" ]] && status=""

    case "$typename" in
      DraftIssue)
        echo "skip draft: $title"
        skipped_draft+=1
        continue
        ;;
      Issue|PullRequest)
        ;;
      *)
        echo "skip unknown type=$typename: $title"
        skipped_unknown+=1
        continue
        ;;
    esac

    label="$typename: $title"
    if [[ -z "$status" ]]; then
      echo "+ $label (no status)"
    else
      echo "+ $label [$status]"
    fi

    if [[ $DRY_RUN -eq 1 ]]; then
      synced+=1
      continue
    fi

    if ! item_id=$(add_item "$content_id" 2>&1); then
      echo "  ERROR adding to dest project: $item_id" >&2
      errored+=1
      continue
    fi

    if [[ -n "$status" ]]; then
      if ! option_id=$(status_option_id "$status"); then
        echo "  WARN unmapped status '$status' — leaving unset" >&2
        option_id=""
      fi
      if [[ -n "$option_id" ]]; then
        if ! set_status "$item_id" "$option_id" 2>err; then
          echo "  ERROR setting status: $(cat err)" >&2
          rm -f err
          errored+=1
          continue
        fi
        rm -f err
      fi
    fi

    synced+=1
  done < <(jq -r '
    .data.organization.projectV2.items.nodes[]
    | . as $n
    | ($n.fieldValues.nodes[] | select(.field.name == "Status") | .name) as $status
    | [
        ($n.content.__typename // ""),
        ($n.content.id // ""),
        ($n.content.title // "(no title)" | gsub("\t"; " ")),
        ($status // "")
      ] | @tsv
  ' <<<"$page")

  [[ "$has_next" == "true" ]] || break
done

echo
echo "=== phase 2: sweep all closed issues → Done ==="

fetch_closed_issues_page() {
  local q='
    query($cursor: String) {
      repository(owner: "'"$SRC_OWNER"'", name: "'"$SRC_REPO"'") {
        issues(states: CLOSED, first: 100, after: $cursor,
               orderBy: {field: CREATED_AT, direction: ASC}) {
          pageInfo { hasNextPage endCursor }
          nodes { id number title }
        }
      }
    }
  '
  if [[ -n "${1:-}" ]]; then
    gh api graphql -f query="$q" -f cursor="$1"
  else
    gh api graphql -f query="$q"
  fi
}

declare -i closed_total=0 closed_synced=0 closed_errored=0
cursor=""
while :; do
  page=$(fetch_closed_issues_page "$cursor")
  has_next=$(jq -r '.data.repository.issues.pageInfo.hasNextPage' <<<"$page")
  cursor=$(jq -r '.data.repository.issues.pageInfo.endCursor // ""' <<<"$page")

  while IFS=$'\t' read -r issue_id number title; do
    closed_total+=1
    echo "+ #$number $title → Done"

    if [[ $DRY_RUN -eq 1 ]]; then
      closed_synced+=1
      continue
    fi

    if ! item_id=$(add_item "$issue_id" 2>&1); then
      echo "  ERROR adding to dest project: $item_id" >&2
      closed_errored+=1
      continue
    fi
    if ! set_status "$item_id" "$DONE_OPTION_ID" 2>err; then
      echo "  ERROR setting status: $(cat err)" >&2
      rm -f err
      closed_errored+=1
      continue
    fi
    rm -f err
    closed_synced+=1
  done < <(jq -r '
    .data.repository.issues.nodes[]
    | [.id, (.number|tostring), (.title // "(no title)" | gsub("\t"; " "))]
    | @tsv
  ' <<<"$page")

  [[ "$has_next" == "true" ]] || break
done

echo
echo "=== summary ==="
echo "phase 1 — board items"
echo "  total processed:     $total"
echo "  synced (or would):   $synced"
echo "  skipped draft:       $skipped_draft"
echo "  skipped unknown:     $skipped_unknown"
echo "  errored:             $errored"
echo "phase 2 — closed issues → Done"
echo "  total processed:     $closed_total"
echo "  synced (or would):   $closed_synced"
echo "  errored:             $closed_errored"
[[ $DRY_RUN -eq 1 ]] && echo "(dry-run — no changes made)"
