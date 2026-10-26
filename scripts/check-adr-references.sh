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
# It also checks that a citation names exactly one ADR, which is just as
# mechanical:
#
#   - No two unsuffixed ADR files share a number. The legacy numbers 014, 017,
#     028 and 032 each name two or three ADRs from before issue-numbered ADRs;
#     CLAUDE.md forbids renumbering legacy ADRs, so they are grandfathered and
#     any other shared number fails.
#   - A citation of one of those grandfathered numbers names its file on the
#     same line (a link or path such as 032-webhook-event-delivery.md), since
#     "ADR-032" alone could mean either ADR. Each file's own heading is exempt.
#   - A citation of a suffixed set (1953-a, 1953-b, ...) carries the suffix.
#   - Each ADR's "# ADR N:" heading carries its own file's number.
#
# Usage: scripts/check-adr-references.sh
# Exit 0 if every cited ADR exists and names one ADR, 1 otherwise.

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

# --- Each citation names exactly one ADR ---------------------------------

failed=0
fail() {
  if [ "$failed" -eq 0 ]; then
    echo "" >&2
    echo "ADR reference integrity check FAILED." >&2
  fi
  failed=1
  echo "" >&2
  echo "$1" >&2
}

grandfathered="14 17 28 32"

# Unsuffixed files sharing a number. Suffixed sets (1089-a-, 1089-b-) share one
# by design and are excluded.
dupes=$(ls adrs/*.md | grep -vE '^adrs/[0-9]+-[a-z]-' \
  | sed -nE 's|^adrs/0*([0-9]+)-.*|\1|p' | sort -n | uniq -d)
for n in $dupes; do
  case " $grandfathered " in *" $n "*) continue ;; esac
  fail "  ADR number $n is used by more than one file — number the new ADR after its own issue (CLAUDE.md, \"ADR numbering\"):"
  ls adrs/ | grep -E "^0*${n}-" | sed 's/^/    adrs\//' >&2
done

# A citation of a grandfathered number must name its file on the same line.
# The file-name test is applied to the line's text only: grep's "file:line:"
# prefix would otherwise satisfy it for every line of adrs/032-*.md itself.
for n in $grandfathered; do
  padded=$(printf "%03d" "$n")
  hits=$(grep -niE "ADR[- ]0*${n}([^0-9-]|\$)" "${sources[@]}" 2>/dev/null \
    | awk -v re="(^|[^0-9])0*${n}-[a-z]" '{ text = $0; sub(/^[^:]*:[^:]*:/, "", text); if (tolower(text) !~ re) print }' \
    | grep -vE "^adrs/${padded}-[^:]*:1:" || true)
  if [ -n "$hits" ]; then
    fail "  ADR-$padded names more than one ADR ($(ls adrs/ | grep -E "^${padded}-" | tr '\n' ' ')) — link the one you mean:"
    echo "$hits" | sed 's/^/    /' >&2
  fi
done

# A citation of a suffixed set must carry its suffix.
suffixed=$(ls adrs/*.md | sed -nE 's|^adrs/0*([0-9]+)-[a-z]-.*|\1|p' | sort -u)
for n in $suffixed; do
  hits=$(grep -niE "ADR[- ]0*${n}([^0-9-]|\$)" "${sources[@]}" 2>/dev/null || true)
  if [ -n "$hits" ]; then
    fail "  ADR-$n is a suffixed set ($(ls adrs/ | grep -E "^0*${n}-[a-z]-" | tr '\n' ' ')) — cite it as ADR-$n-a, ADR-$n-b, ...:"
    echo "$hits" | sed 's/^/    /' >&2
  fi
done

# Each ADR's heading carries its own file's number (and suffix).
for f in adrs/[0-9]*.md; do
  file_id=$(basename "$f" | sed -nE 's/^0*([0-9]+)(-([a-z])-)?.*/\1\2/p' | sed -E 's/-$//')
  got=$(grep -m1 -E '^# ' "$f" | sed -nE 's/^# *[Aa][Dd][Rr][- ]*0*([0-9]+(-[a-z])?)[^0-9a-z].*/\1/p')
  if [ -n "$got" ] && [ "$got" != "$file_id" ]; then
    fail "  $f is headed ADR $got — its file number is $file_id."
  fi
done

if [ "$failed" -ne 0 ]; then
  echo "" >&2
  exit 1
fi

echo "check-adr-references: OK — all $count cited ADRs exist and each names one ADR"
