#!/usr/bin/env bash
# Shared helpers for fabrik plugin scripts.
# Source from a sibling script:
#   HERE=$(cd "$(dirname "$0")" && pwd) && . "$HERE/lib.sh"
#
# After sourcing, call `fabrik_load_config` to populate:
#   FABRIK_ROOT         — dir containing .fabrik/
#   FABRIK_OWNER        — GitHub org/user that owns the project
#   FABRIK_PROJECT      — project number (integer, as string)
#   FABRIK_OWNER_TYPE   — "organization" (default) or "user"
#   FABRIK_REPO         — repo name, or empty in multi-repo mode
#
# `fabrik_resolve_repo <N>` sets ISSUE_OWNER / ISSUE_REPO for issue N:
# uses config's repo if set, otherwise looks it up from the project board.

set -euo pipefail

fabrik_find_root() {
  local d="$PWD"
  while [ "$d" != "/" ]; do
    if [ -f "$d/.fabrik/config.yaml" ]; then
      printf '%s\n' "$d"
      return 0
    fi
    d=$(dirname "$d")
  done
  echo "fabrik: no .fabrik/config.yaml found from $PWD upward" >&2
  return 1
}

# Extract a top-level `key: value` from a simple YAML file.
# Ignores commented lines; strips trailing comments and surrounding quotes.
fabrik_config_get() {
  local key=$1 file=$2
  awk -v k="$key" '
    BEGIN { pat = "^" k "[[:space:]]*:" }
    /^[[:space:]]*#/ { next }
    $0 ~ pat {
      sub(/^[^:]*:[[:space:]]*/, "")
      sub(/[[:space:]]+#.*$/, "")
      sub(/[[:space:]]+$/, "")
      gsub(/^"|"$/, "")
      gsub(/^'\''|'\''$/, "")
      if ($0 != "") { print; exit }
    }
  ' "$file"
}

fabrik_load_config() {
  FABRIK_ROOT=$(fabrik_find_root)
  local f="$FABRIK_ROOT/.fabrik/config.yaml"
  FABRIK_OWNER=$(fabrik_config_get owner "$f")
  FABRIK_PROJECT=$(fabrik_config_get project "$f")
  FABRIK_OWNER_TYPE=$(fabrik_config_get owner_type "$f")
  FABRIK_REPO=$(fabrik_config_get repo "$f")
  : "${FABRIK_OWNER_TYPE:=organization}"
  if [ -z "${FABRIK_OWNER:-}" ] || [ -z "${FABRIK_PROJECT:-}" ]; then
    echo "fabrik: config.yaml at $f is missing owner or project" >&2
    return 1
  fi
  export FABRIK_ROOT FABRIK_OWNER FABRIK_PROJECT FABRIK_OWNER_TYPE FABRIK_REPO
}

fabrik_scope_field() {
  case "${FABRIK_OWNER_TYPE:-organization}" in
    user) printf 'user' ;;
    *)    printf 'organization' ;;
  esac
}

fabrik_resolve_repo() {
  local n=${1:-}
  if [ -z "$n" ]; then
    echo "fabrik_resolve_repo: issue number required" >&2
    return 2
  fi
  # Env override lets callers skip the board lookup entirely.
  if [ -n "${FABRIK_ISSUE_REPO:-}" ]; then
    ISSUE_OWNER=${FABRIK_ISSUE_REPO%/*}
    ISSUE_REPO=${FABRIK_ISSUE_REPO#*/}
    export ISSUE_OWNER ISSUE_REPO
    return 0
  fi
  if [ -n "${FABRIK_REPO:-}" ]; then
    ISSUE_OWNER=$FABRIK_OWNER
    ISSUE_REPO=$FABRIK_REPO
    export ISSUE_OWNER ISSUE_REPO
    return 0
  fi
  # Multi-repo mode: look the issue up on the project. `gh project item-list`
  # paginates via --limit, so this works for large boards.
  # Two issues with the same number can exist in different repos on one project;
  # take the first match. Don't swallow gh errors — let auth/network/missing-gh
  # failures surface with their real message.
  local nw
  nw=$(gh project item-list "$FABRIK_PROJECT" --owner "$FABRIK_OWNER" \
        --limit 500 --format json \
        --jq "[.items[] | select(.content.number == $n) | .content.repository][0] // empty")
  if [ -z "$nw" ]; then
    echo "fabrik: issue #$n not found on project $FABRIK_OWNER/$FABRIK_PROJECT (set repo: in config.yaml or FABRIK_ISSUE_REPO=owner/repo)" >&2
    return 1
  fi
  ISSUE_OWNER=${nw%/*}
  ISSUE_REPO=${nw#*/}
  export ISSUE_OWNER ISSUE_REPO
}
