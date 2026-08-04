#!/usr/bin/env bash
# ghes-probe.sh — READ-ONLY capability probe for running Fabrik against GitHub Enterprise Server.
#
# What it does:  three GraphQL introspection queries + one REST version read.
# What it sends: nothing but the queries below. No repo, issue, org or user data is read.
# What it emits: a capability matrix on stdout. Review before sharing — it names your
#                GHES host and version, and nothing else about your installation.
#
# Usage:
#   export GHES_HOST=github.example.com        # host only, no scheme, no trailing slash
#   export GHES_TOKEN=<a PAT with read scope>  # classic PAT; read-only use here
#   bash ghes-probe.sh
#
# Exit 0 always; failures are reported in the matrix rather than aborting.

set -uo pipefail

: "${GHES_HOST:?set GHES_HOST (e.g. github.example.com — host only)}"
: "${GHES_TOKEN:?set GHES_TOKEN (classic PAT)}"

REST="https://${GHES_HOST}/api/v3"
GQL="https://${GHES_HOST}/api/graphql"

say() { printf '%s\n' "$*"; }
hr()  { printf '%s\n' "----------------------------------------------------------------"; }

gql() { # $1 = query string -> raw JSON response
  curl -sS -X POST "$GQL" \
    -H "Authorization: bearer ${GHES_TOKEN}" \
    -H "Content-Type: application/json" \
    --data "$(printf '{"query":%s}' "$(printf '%s' "$1" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')")" \
    2>&1
}

# ---------------------------------------------------------------- version
hr; say "GHES VERSION AND ENDPOINTS"; hr
VER=$(curl -sS -H "Authorization: bearer ${GHES_TOKEN}" "${REST}/meta" 2>&1 \
      | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin); print(d.get("installed_version","(field absent)"))
except Exception as e: print("(could not parse /meta: %s)" % e)')
say "installed_version : ${VER}"
say "REST base         : ${REST}"
say "GraphQL endpoint  : ${GQL}"

# Prove the endpoint split empirically — this is the assumption worth testing.
PROBE_WRONG=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${REST}/graphql" \
  -H "Authorization: bearer ${GHES_TOKEN}" -H "Content-Type: application/json" \
  --data '{"query":"{__typename}"}' 2>/dev/null)
PROBE_RIGHT=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${GQL}" \
  -H "Authorization: bearer ${GHES_TOKEN}" -H "Content-Type: application/json" \
  --data '{"query":"{__typename}"}' 2>/dev/null)
say ""
say "POST ${REST}/graphql  -> HTTP ${PROBE_WRONG}   (expected: 404 — confirms one baseURL is insufficient)"
say "POST ${GQL}           -> HTTP ${PROBE_RIGHT}   (expected: 200)"

# ---------------------------------------------------------------- type fields
check_type_fields() { # $1 = GraphQL type, rest = field names
  local type="$1"; shift
  local resp fields
  resp=$(gql "{ __type(name: \"${type}\") { fields { name } } }")
  fields=$(printf '%s' "$resp" | python3 -c '
import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("__PARSE_FAIL__"); raise SystemExit
t = (d.get("data") or {}).get("__type")
if not t:
    print("__NO_TYPE__"); raise SystemExit
print(" ".join(f["name"] for f in (t.get("fields") or [])))')

  if [ "$fields" = "__PARSE_FAIL__" ]; then
    say "  ${type}: could not parse response (auth or network?)"; return
  fi
  if [ "$fields" = "__NO_TYPE__" ]; then
    say "  ${type}: TYPE ABSENT  <-- significant"; return
  fi
  for f in "$@"; do
    case " $fields " in
      *" $f "*) say "  [ok ]  ${type}.${f}" ;;
      *)        say "  [MISS] ${type}.${f}" ;;
    esac
  done
}

hr; say "QUERY FIELDS FABRIK DEPENDS ON"; hr
check_type_fields Issue closedByPullRequestsReferences blockedBy trackedIssues subIssues projectItems
check_type_fields PullRequest mergeQueueEntry reviewDecision isInMergeQueue autoMergeRequest latestReviews
check_type_fields Repository mergeQueue projectV2
check_type_fields ProjectV2Item fieldValueByName

# ---------------------------------------------------------------- mutations
hr; say "MUTATIONS FABRIK DEPENDS ON"; hr
MUTS=$(gql '{ __schema { mutationType { fields { name } } } }' | python3 -c '
import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("__PARSE_FAIL__"); raise SystemExit
m = ((d.get("data") or {}).get("__schema") or {}).get("mutationType")
if not m:
    print("__PARSE_FAIL__"); raise SystemExit
print(" ".join(f["name"] for f in (m.get("fields") or [])))')

if [ "$MUTS" = "__PARSE_FAIL__" ]; then
  say "  could not enumerate mutations (auth or network?)"
else
  for m in addBlockedBy removeBlockedBy addSubIssue \
           enqueuePullRequest dequeuePullRequest \
           addProjectV2ItemById updateProjectV2ItemFieldValue archiveProjectV2Item \
           resolveReviewThread unresolveReviewThread \
           markPullRequestReadyForReview enablePullRequestAutoMerge disablePullRequestAutoMerge \
           requestReviews addLabelsToLabelable removeLabelsFromLabelable; do
    case " $MUTS " in
      *" $m "*) say "  [ok ]  ${m}" ;;
      *)        say "  [MISS] ${m}" ;;
    esac
  done
fi

hr
say "Done. Send the output above back — every [MISS] is a capability gate Fabrik"
say "will need, and the two HTTP codes settle the endpoint-split question."
hr
