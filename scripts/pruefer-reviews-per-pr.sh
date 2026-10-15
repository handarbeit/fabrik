#!/usr/bin/env bash
# Count Pruefer reviews per merged PR over a window (#2230, SC-001).
#
#   scripts/pruefer-reviews-per-pr.sh <owner/repo> <since> <until> [reviewer-login]
#
# <since>/<until> are dates (YYYY-MM-DD, merged-at, inclusive). The reviewer is
# the login Pruefer posts as (default: $PRUEFER_LOGIN). Prints one line per
# merged PR and a summary — reviews, merged PRs and reviews per merged PR — so
# the same command can be run over a window before and after a rollout and the
# two figures quoted. Read-only; uses `gh api` and is not part of CI.
set -euo pipefail

repo="${1:?usage: $0 <owner/repo> <since> <until> [reviewer-login]}"
since="${2:?missing <since>}"
until_="${3:?missing <until>}"
login="${4:-${PRUEFER_LOGIN:-}}"
[ -n "$login" ] || { echo "reviewer login required (4th arg or PRUEFER_LOGIN)" >&2; exit 2; }

prs=$(gh pr list --repo "$repo" --state merged --limit 1000 \
  --search "merged:${since}..${until_}" --json number --jq '.[].number')

total_reviews=0
total_prs=0
for n in $prs; do
  c=$(gh api --paginate "repos/${repo}/pulls/${n}/reviews" \
    --jq "[.[] | select(.user.login == \"${login}\")] | length" | paste -sd+ - | bc)
  printf 'PR #%s\t%s review(s)\n' "$n" "${c:-0}"
  total_reviews=$((total_reviews + ${c:-0}))
  total_prs=$((total_prs + 1))
done

echo "window ${since}..${until_} repo ${repo} reviewer ${login}"
echo "merged PRs: ${total_prs}  reviews: ${total_reviews}"
if [ "$total_prs" -gt 0 ]; then
  echo "reviews per merged PR: $(echo "scale=2; ${total_reviews} / ${total_prs}" | bc)"
fi
