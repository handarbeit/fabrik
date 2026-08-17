#!/usr/bin/env bash
# scripts/check-adr-references.sh — fail on ADR citations that name no ADR.
#
# Why this exists: ADRs are numbered after the issue they come from (see
# CLAUDE.md's "ADR numbering" section), so issue numbers and ADR numbers share
# one namespace. "ADR-1303" and "#1303" are one token apart and look equally
# plausible to a reader, but only one of them is true — #1303 is an issue with
# no ADR. That exact slip shipped in adrs/1408-*.md and stood undetected until
# a manual sweep found it.
#
# Nothing else in CI checks documentation claims. The docs-drift workflow
# regenerates docs/llms-full.txt and diffs it, which verifies the bundle
# matches its sources — a concatenation check, not a truth check. This script
# does not check truth either, but it does check the one property that is
# mechanically decidable: that every cited ADR exists.
#
# Scope: adrs/*.md, docs/*.md, and CLAUDE.md. Matches "ADR-N" and "ADR N"
# (any case). A reference is satisfied by any adrs/<N>-*.md, trying the number
# both as written and zero-padded to three digits, since legacy ADRs 001-073
# are zero-padded and issue-numbered ones are not.
#
# Usage: scripts/check-adr-references.sh
# Exit 0 if every cited ADR exists, 1 otherwise.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

sources=(CLAUDE.md)
while IFS= read -r f; do sources+=("$f"); done < <(ls adrs/*.md docs/*.md 2>/dev/null)

# Collect distinct referenced ADR numbers. The trailing sort -u keeps each
# number once no matter how many times or in how many files it appears.
refs=$(grep -ohiE "ADR[- ][0-9]{1,4}" "${sources[@]}" 2>/dev/null \
  | grep -oE "[0-9]{1,4}" \
  | sort -u || true)

if [ -z "$refs" ]; then
  echo "check-adr-references: no ADR references found — did the citation format change?" >&2
  exit 1
fi

dangling=""
count=0
for n in $refs; do
  count=$((count + 1))
  # 10# forces base-10: bash reads a leading-zero literal as octal, so a
  # citation like "ADR-008" would otherwise abort the script ("invalid octal").
  padded=$(printf "%03d" "$((10#$n))")
  if compgen -G "adrs/${n}-*.md" > /dev/null || compgen -G "adrs/${padded}-*.md" > /dev/null; then
    continue
  fi
  dangling="${dangling}${n} "
done

if [ -n "$dangling" ]; then
  echo "" >&2
  echo "ADR reference integrity check FAILED." >&2
  echo "" >&2
  echo "These ADR numbers are cited but no matching adrs/<N>-*.md exists:" >&2
  for n in $dangling; do
    echo "" >&2
    echo "  ADR-$n — cited at:" >&2
    grep -rniE "ADR[- ]$n([^0-9]|$)" "${sources[@]}" 2>/dev/null | sed 's/^/    /' >&2
  done
  echo "" >&2
  echo "Most often this means an *issue* number was written as an ADR number." >&2
  echo "ADRs are numbered after their originating issue, so the two share a" >&2
  echo "namespace: check whether the citation should read '#<N>' instead." >&2
  echo "" >&2
  exit 1
fi

echo "check-adr-references: OK — all $count cited ADRs exist"
