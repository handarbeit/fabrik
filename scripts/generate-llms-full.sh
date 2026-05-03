#!/usr/bin/env bash
# generate-llms-full.sh — Generate docs/llms-full.txt from canonical Fabrik doc pages.
#
# Usage: scripts/generate-llms-full.sh
#   Run from the repo root or any directory — uses paths relative to this script.
#
# Output: docs/llms-full.txt — committed concatenated bundle checked by CI
#
# Workflow:
#   1. Run this script after modifying any canonical doc page listed in ORDERED below
#   2. Commit docs/llms-full.txt alongside your doc changes
#   3. CI (docs-drift.yml) verifies the committed file matches what this script produces

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DOCS_DIR="${REPO_ROOT}/docs"
OUT="${DOCS_DIR}/llms-full.txt"

TMPFILE="$(mktemp)"
trap 'rm -f "${TMPFILE}"' EXIT

# Strip YAML front matter (---...---) from a file into TMPFILE; pass through unchanged if none.
# The done flag prevents --- horizontal rules in the body (e.g., USER_GUIDE.md) from
# being misidentified as the front matter closing delimiter.
strip_front_matter_to_tmp() {
  awk 'BEGIN{fm=0; done=0} /^---$/ && !done { fm++; if(fm==2){done=1}; next } done || fm==0 {print}' "$1" > "${TMPFILE}"
}

# Pages in fixed order — do not reorder; CI drift checks require bitwise-identical output.
# Format: "filename:canonical-url"
ORDERED=(
  "USER_GUIDE.md:https://fabrik.handarbeit.io/USER_GUIDE"
  "state-machine.md:https://fabrik.handarbeit.io/state-machine"
  "stage-lifecycle.md:https://fabrik.handarbeit.io/stage-lifecycle"
  "positioning.md:https://fabrik.handarbeit.io/positioning"
)

> "$OUT"

for entry in "${ORDERED[@]}"; do
  file="${DOCS_DIR}/${entry%%:*}"
  url="${entry#*:}"

  strip_front_matter_to_tmp "$file"

  # Extract the first H1 heading from the body (reads from file, no SIGPIPE risk).
  title=$(awk '/^# /{sub(/^# /, ""); print; exit}' "${TMPFILE}")

  printf '# %s\n\nSource: %s\n\n' "$title" "$url" >> "$OUT"

  # Output body content, skipping leading blank lines and the first H1 heading
  # so the H1 appears exactly once (in the section header above).
  awk '
    BEGIN { skipping=1 }
    skipping && /^[[:space:]]*$/ { next }
    skipping && /^# /            { skipping=0; next }
    { skipping=0; print }
  ' "${TMPFILE}" >> "$OUT"

  printf '\n---\n\n' >> "$OUT"
done
