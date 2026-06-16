#!/usr/bin/env bash
# Update local references from tenaciousvc/fabrik to handarbeit/fabrik.
# Run this AFTER the GitHub repo transfer has completed.
#
# This updates:
#   - .fabrik/config.yaml `owner:` field
#   - git origin remote URL
#   - tenaciousvc/fabrik → handarbeit/fabrik in tracked code/docs
#   - `--owner tenaciousvc` → `--owner handarbeit` in skills
#
# Usage:
#   scripts/migrate-update-refs.sh [--dry-run]

set -euo pipefail

DRY_RUN=0
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=1
  echo "(dry-run mode — no files modified)"
fi

run() {
  if [[ $DRY_RUN -eq 1 ]]; then
    echo "  WOULD: $*"
  else
    eval "$@"
  fi
}

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "WARN: working tree has uncommitted changes — review before running for real" >&2
  echo "      (continuing in dry-run mode is safe)" >&2
  if [[ $DRY_RUN -eq 0 ]]; then
    read -r -p "continue anyway? [y/N] " ans
    [[ "$ans" =~ ^[Yy] ]] || { echo "aborted"; exit 1; }
  fi
fi

# 1. Files that need `tenaciousvc/fabrik` → `handarbeit/fabrik`. Discovered
# dynamically so the script keeps working as the plugin layout evolves; the
# excludes filter out runtime/cache paths and the script itself.
files=()
while IFS= read -r f; do
  files+=("$f")
done < <(
  grep -rl 'tenaciousvc/fabrik' \
    --include='*.md' --include='*.yaml' --include='*.yml' \
    --include='*.go' --include='*.json' --include='*.sh' \
    . 2>/dev/null \
  | sed 's|^\./||' \
  | grep -v -E '^\.fabrik/(repos|worktrees|debug|logs|sessions|history|plugin)' \
  | grep -v -E '^scripts/migrate-' \
  | sort
)

echo "=== updating tenaciousvc/fabrik → handarbeit/fabrik ==="
if [[ ${#files[@]} -eq 0 ]]; then
  echo "  (no matches found)"
fi
for f in "${files[@]}"; do
  count=$(grep -c 'tenaciousvc/fabrik' "$f" || true)
  echo "  $f ($count match(es))"
  run "sed -i '' 's|tenaciousvc/fabrik|handarbeit/fabrik|g' '$f'"
done

# 2. The `--owner tenaciousvc` form (audit-documentation skill).
echo
echo "=== updating --owner tenaciousvc → --owner handarbeit ==="
for f in .claude/skills/audit-documentation/SKILL.md .claude/skills/cut-release/SKILL.md; do
  [[ -f "$f" ]] || continue
  if grep -q 'owner tenaciousvc' "$f"; then
    echo "  $f"
    run "sed -i '' 's|owner tenaciousvc|owner handarbeit|g' '$f'"
  fi
done

# 3. The orgs/tenaciousvc/hooks fixture in webhook_test.go.
echo
echo "=== updating /orgs/tenaciousvc/hooks → /orgs/handarbeit/hooks ==="
if [[ -f engine/webhook_test.go ]] && grep -q 'orgs/tenaciousvc/hooks' engine/webhook_test.go; then
  echo "  engine/webhook_test.go"
  run "sed -i '' 's|orgs/tenaciousvc/hooks|orgs/handarbeit/hooks|g' engine/webhook_test.go"
fi

# 4. .fabrik/config.yaml owner.
echo
echo "=== updating .fabrik/config.yaml owner ==="
if grep -qE '^owner:\s*tenaciousvc\s*$' .fabrik/config.yaml; then
  run "sed -i '' 's|^owner:[[:space:]]*tenaciousvc[[:space:]]*$|owner: handarbeit|' .fabrik/config.yaml"
  echo "  .fabrik/config.yaml"
fi

# 5. Git remote.
echo
echo "=== updating git remote ==="
current=$(git remote get-url origin)
echo "  current: $current"
case "$current" in
  *tenaciousvc/fabrik*)
    new="${current/tenaciousvc\/fabrik/handarbeit/fabrik}"
    echo "  new:     $new"
    run "git remote set-url origin '$new'"
    ;;
  *handarbeit/fabrik*)
    echo "  already pointing at handarbeit/fabrik — skipping"
    ;;
  *)
    echo "  unrecognized remote — leaving alone" >&2
    ;;
esac

# 6. Bare clone in .fabrik/repos/ — unrelated to this script's scope but worth
# flagging since the engine caches under <owner>-<repo>.git.
if [[ -d .fabrik/repos/tenaciousvc-fabrik.git ]]; then
  echo
  echo "NOTE: .fabrik/repos/tenaciousvc-fabrik.git still exists." >&2
  echo "      The engine will re-clone as handarbeit-fabrik.git on first run." >&2
  echo "      You can rm -rf the old cache once the new run is healthy." >&2
fi

echo
echo "=== verifying remaining tenaciousvc references ==="
remaining=$(grep -rln 'tenaciousvc' \
  --include='*.md' --include='*.yaml' --include='*.yml' \
  --include='*.go' --include='*.json' --include='*.sh' \
  . 2>/dev/null \
  | grep -v -E '\.fabrik/(repos|worktrees|debug|logs|sessions|history|plugin)' \
  | grep -v '^./scripts/migrate-' || true)
if [[ -n "$remaining" ]]; then
  echo "  remaining files (review manually):"
  echo "$remaining" | sed 's|^|    |'
else
  echo "  (none)"
fi

echo
echo "Done."
[[ $DRY_RUN -eq 1 ]] && echo "(dry-run — no changes made)"
