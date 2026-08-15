#!/usr/bin/env bash
# cut-release.sh — Publish a Fabrik release as the @arbeithand bot.
#
# Usage:
#   scripts/cut-release.sh v0.0.67
#   scripts/cut-release.sh v0.0.67 --skip-tests       # skip race-tested suite (last-resort)
#   scripts/cut-release.sh v0.0.67 --no-doc-issue     # skip filing the doc-update issue
#   scripts/cut-release.sh v0.0.67 --no-plugin-bump   # skip the plugin/fabrik version auto-bump
#   scripts/cut-release.sh v0.0.67 --skip-integration=<reason>
#       # Skip the mandatory live e2e integration gate (step 5 below). This is
#       # a LOUD, RECORDED escape hatch, not a quiet default — see step 5's own
#       # comment and adrs/1454-sim-pre-gate-not-replacement.md for why the
#       # live suite is mandatory by default and what this flag costs. A bare
#       # `--skip-integration` (no reason) is refused.
#
# Prereqs:
#   - On main, clean working tree, ff'd to origin/main
#   - release-notes.md in repo root, top heading matches the version
#   - .env contains FABRIK_TOKEN (an arbeithand PAT)
#   - Repo secret PUBLIC_REPO_RELEASE_TOKEN on handarbeit/fabrik is an arbeithand PAT
#     (script verifies release+discussion author after; will abort + tell you to rotate
#     if it comes back as anyone else).
#
# What it does:
#   1. Pre-flight: branch, clean tree, ff-pull, release-notes heading match
#   2. PAT identity check (must be arbeithand)
#   3. Sim suite + github wire-contract tests — unconditional pre-gate (R1/R2, #1454)
#   4. go build + go test -race (skippable with --skip-tests; the pre-gate above never is)
#   5. Live e2e integration gate — mandatory by default; --skip-integration=<reason> is the
#      one sanctioned, loud, release-notes-recorded escape hatch (R2, #1454)
#   6. Commit release-notes.md (if dirty) as arbeithand
#   6b. If plugin/fabrik's source changed since the previous tag, patch-bump
#       plugin/fabrik/.claude-plugin/plugin.json and commit as arbeithand
#       (skippable with --no-plugin-bump)
#   7. Tag, push tag with credential helpers nuked + PAT-in-URL
#   8. Watch the release workflow run; fail loudly on non-success
#   9. Verify the published release author and discussion author are both arbeithand
#   10. File doc-update issue and add to project at Status=Specify
#
# On failure after the tag is pushed, the script does NOT auto-clean. It prints the
# exact cleanup commands you'd need so you can decide whether to scrub and retry.
#
# parse_args() and main() are split, and both sit behind the same
# BASH_SOURCE[0]==$0 dispatch guard scripts/e2e/run.sh already uses (see that
# script's own precedent). This lets scripts/cut_release_gate_test.sh `source`
# this file and exercise parse_args()'s flag validation — including the
# mandatory-by-default live suite's --skip-integration handling — directly,
# without ever reaching main() (the real publish sequence: tag, push, watch
# workflow, file issue). This is a pure mechanical wrap: no step's logic,
# ordering, or behavior changed as part of the split, only where the code
# lives.

set -euo pipefail

# ─── constants ────────────────────────────────────────────────────────────────
REPO="handarbeit/fabrik"
PROJECT_NUM=1
PROJECT_OWNER="handarbeit"
BOT_LOGIN="arbeithand"
BOT_EMAIL="handarbeit@handarbeit.io"

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

step() { printf '\n\033[1;34m▶ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[1;32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[1;33m!\033[0m %s\n' "$*"; }
die()  { printf '\n\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# insert_notes_line appends $2 into $1's "## Internal" section — creating
# that section (just before "## Upgrading", the schema's always-last
# section) if it doesn't exist yet. Shared by the plugin-bump changelog line
# (step 6b, pre-existing) and the --skip-integration recorded-skip line
# (step 5, R2/#1454), so the two call sites can't drift into different
# insertion logic.
insert_notes_line() {
  local file="$1"
  local line="$2"
  if grep -Eq '^## Internal[[:space:]]*$' "$file"; then
    awk -v line="$line" '
      { print }
      /^## Internal[[:space:]]*$/ && !inserted { print line; inserted=1 }
    ' "$file" > "${file}.tmp"
    mv "${file}.tmp" "$file"
  elif grep -Eq '^## Upgrading[[:space:]]*$' "$file"; then
    # No existing Internal section: insert a new one directly before
    # Upgrading (always the last section in the notes schema) rather than
    # appending at the file's end, which would land after the closing
    # ```bash fence and break the canonical section order.
    awk -v line="$line" '
      /^## Upgrading[[:space:]]*$/ && !inserted {
        print "## Internal"
        print line
        print ""
        inserted=1
      }
      { print }
    ' "$file" > "${file}.tmp"
    mv "${file}.tmp" "$file"
  else
    {
      echo ""
      echo "## Internal"
      echo "$line"
    } >> "$file"
  fi
}

# ─── arg parsing ──────────────────────────────────────────────────────────────
parse_args() {
  VERSION="${1:-}"
  SKIP_TESTS=0
  NO_DOC_ISSUE=0
  NO_PLUGIN_BUMP=0
  SKIP_INTEGRATION=0
  INTEGRATION_SKIP_REASON=""
  shift || true
  while [ $# -gt 0 ]; do
    case "$1" in
      --skip-tests)     SKIP_TESTS=1 ;;
      --no-doc-issue)   NO_DOC_ISSUE=1 ;;
      --no-plugin-bump) NO_PLUGIN_BUMP=1 ;;
      --skip-integration=*)
        SKIP_INTEGRATION=1
        INTEGRATION_SKIP_REASON="${1#--skip-integration=}"
        [ -n "$INTEGRATION_SKIP_REASON" ] \
          || { echo "--skip-integration requires a non-empty reason: --skip-integration=<reason>" >&2; exit 2; }
        ;;
      --skip-integration)
        echo "--skip-integration requires a reason: --skip-integration=<reason> (a bare flag is not accepted — R2/#1454: the live suite is mandatory by default, and its one sanctioned escape hatch must be loud and self-documenting)" >&2
        exit 2
        ;;
      *) echo "Unknown flag: $1" >&2; exit 2 ;;
    esac
    shift
  done

  if [ -z "$VERSION" ]; then
    echo "Usage: $0 vX.Y.Z [--skip-tests] [--no-doc-issue] [--no-plugin-bump] [--skip-integration=<reason>]" >&2
    exit 2
  fi
  if ! printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "Version must look like v0.0.67 (got: $VERSION)" >&2
    exit 2
  fi
}

# main runs the real, side-effecting publish sequence (git commits/pushes, a
# real tag push, watching a real workflow run, filing a real issue) — never
# invoked on source, only from the dispatch guard at the bottom when this
# script is executed directly. Reads the globals parse_args sets.
main() {

# ─── 1. pre-flight ────────────────────────────────────────────────────────────
step "Pre-flight checks"

BRANCH="$(git branch --show-current)"
[ "$BRANCH" = "main" ] || die "must be on main (currently on '$BRANCH')"
ok "on main"

# Allow uncommitted release-notes/<version>.md, plugin/known_embedded_versions.go,
# and plugin/fabrik/.claude-plugin/plugin.json (all three are updated by this
# script itself, after the build step and in step 6b).
#
# Allowlist disposition (reviewed for #1070, extended for #816): all three
# files are committed by this script's own step 6 / step 6b, *before* the tag
# is created and pushed in step 7. By the time release.yml's CI job checks
# out the tag, it's a fresh clone — it never sees this local working tree,
# dirty or not. So this allowlist cannot affect the CI-built artifact and
# does not need to be tightened; the built-artifact VCS check lives in
# .goreleaser.yaml/release.yml instead (see
# adrs/071-release-artifact-vcs-verification.md).
DIRTY=$(git status --porcelain | grep -Ev "^\?\? release-notes/${VERSION}\.md$| M release-notes/${VERSION}\.md$|^M  release-notes/${VERSION}\.md$| M plugin/known_embedded_versions\.go$|^M  plugin/known_embedded_versions\.go$| M plugin/fabrik/\.claude-plugin/plugin\.json$|^M  plugin/fabrik/\.claude-plugin/plugin\.json$" || true)
[ -z "$DIRTY" ] || die "working tree dirty:
$DIRTY"
ok "working tree acceptable"

git fetch origin main --tags --quiet
LOCAL="$(git rev-parse HEAD)"
REMOTE="$(git rev-parse origin/main)"
if [ "$LOCAL" != "$REMOTE" ]; then
  if git merge-base --is-ancestor HEAD origin/main; then
    warn "local main is behind origin/main — fast-forwarding"
    git pull --ff-only origin main --quiet
  else
    die "local main has diverged from origin/main"
  fi
fi
ok "synced with origin/main"

if git rev-parse "$VERSION" >/dev/null 2>&1; then
  die "tag $VERSION already exists locally — remove it before retrying"
fi
if git ls-remote --tags origin "$VERSION" | grep -q "$VERSION"; then
  die "tag $VERSION already exists on origin — see cleanup notes at the bottom of this script"
fi
ok "tag $VERSION free locally and remotely"

NOTES_FILE="release-notes/${VERSION}.md"
[ -f "$NOTES_FILE" ] || die "$NOTES_FILE not found — author the release notes there before running this script"
HEAD_LINE="$(head -1 "$NOTES_FILE")"
if ! printf '%s' "$HEAD_LINE" | grep -Eq "^# Fabrik ${VERSION}( |$)"; then
  die "$NOTES_FILE heading mismatch:
  expected: '# Fabrik $VERSION'
  found:    '$HEAD_LINE'"
fi
if ! grep -Eq '^## Summary[[:space:]]*$' "$NOTES_FILE"; then
  die "$NOTES_FILE is missing a '## Summary' section — required for the Discussions announcement"
fi
ok "$NOTES_FILE heading + ## Summary present"

# ─── 2. PAT identity check ────────────────────────────────────────────────────
step "PAT identity check"

[ -f .env ] || die ".env not found — needed for FABRIK_TOKEN"
FABRIK_TOKEN="$(grep '^FABRIK_TOKEN=' .env | head -1 | cut -d= -f2-)"
[ -n "$FABRIK_TOKEN" ] || die "FABRIK_TOKEN not set in .env"

PAT_OWNER="$(GH_TOKEN="$FABRIK_TOKEN" gh api user --jq .login)"
[ "$PAT_OWNER" = "$BOT_LOGIN" ] \
  || die "FABRIK_TOKEN does not belong to $BOT_LOGIN (got: $PAT_OWNER)"
ok "FABRIK_TOKEN authenticated as @$BOT_LOGIN"

# ─── 3. sim + wire-contract pre-gate (unconditional — R1/R2, #1454) ──────────
# Unlike the full go test -race ./... below, this step is NEVER skippable —
# not even by --skip-tests. It's the same free, fast pre-gate
# scripts/e2e/run.sh runs ahead of its own live suite (see that script's
# run_pregate), re-run here so a release cut through this script pays the
# same "cheap layers first" ordering even if someone hand-invokes
# scripts/e2e/run.sh separately with E2E_SKIP_PREGATE set. Cheap (~107s
# total per tests/sim/README.md), so making it unconditional costs nothing
# real while removing an entire class of "the live suite failed for a
# reason the sim would have caught for free" incidents.
step "Sim + wire-contract pre-gate"
if ! "$REPO_ROOT/scripts/sim/run.sh" --all; then
  die "sim suite failed — fix it before cutting a release (scripts/sim/run.sh --all to reproduce)"
fi
ok "sim suite passed"
if ! go test -race -count=1 ./github/... >/tmp/cut-release-pregate-test.log 2>&1; then
  tail -40 /tmp/cut-release-pregate-test.log >&2
  die "github wire-contract tests failed (full log: /tmp/cut-release-pregate-test.log)"
fi
ok "github wire-contract tests passed"

# ─── 4. build + test ──────────────────────────────────────────────────────────
step "Build"
go build ./... >/dev/null || die "go build failed"
ok "go build clean"

step "Record embedded plugin hash for $VERSION"
PLUGIN_HASH="$(go run ./tools/print-plugin-hash/)"
[ -n "$PLUGIN_HASH" ] || die "print-plugin-hash produced empty output"
KNOWN_VERSIONS_FILE="plugin/known_embedded_versions.go"
if grep -qF "\"${PLUGIN_HASH}\"" "$KNOWN_VERSIONS_FILE"; then
  ok "hash already recorded in $KNOWN_VERSIONS_FILE (no change needed)"
else
  # Insert the new hash before the closing } of the KnownEmbeddedVersions slice.
  awk -v hash="$PLUGIN_HASH" -v version="$VERSION" '
    /^\}$/ { print "\t\"" hash "\", // " version }
    { print }
  ' "$KNOWN_VERSIONS_FILE" > "${KNOWN_VERSIONS_FILE}.tmp"
  mv "${KNOWN_VERSIONS_FILE}.tmp" "$KNOWN_VERSIONS_FILE"
  ok "appended $PLUGIN_HASH to $KNOWN_VERSIONS_FILE"
fi

if [ "$SKIP_TESTS" -eq 1 ]; then
  warn "--skip-tests was passed; go test -race ./... was NOT run (the sim + wire-contract pre-gate above still ran unconditionally)"
else
  step "Race-tested suite"
  if ! go test -race ./... >/tmp/cut-release-test.log 2>&1; then
    tail -40 /tmp/cut-release-test.log >&2
    die "go test -race ./... failed (full log: /tmp/cut-release-test.log)"
  fi
  ok "all tests pass with -race"
fi

# Capture the previous release tag now, before this release's tag exists,
# for the plugin-bump change-detection diff in step 6b. Tags were already
# fetched in pre-flight. Empty on a first-ever release (no v* tag yet).
PREV_TAG="$(git describe --tags --abbrev=0 --match='v*' 2>/dev/null || true)"

# ─── 5. live e2e integration gate (mandatory by default — R2, #1454) ────────
# The live suite is the only layer that exercises real Claude, real review
# bots, and real GitHub wire behaviour — nothing else in this pipeline can
# substitute for it, and it is never retired or reduced (see
# adrs/1454-sim-pre-gate-not-replacement.md). So unlike --skip-tests above,
# there is no quiet default here: either the suite runs, or a human
# explicitly said why not, and that "why not" ships in the release notes
# where anyone reading them can see it. A --skip-integration that silently
# produced an unvalidated release was rejected outright (R2's option (a) vs
# (b) decision — see the ADR).
#
# Positioned here — after build+test, before the release-notes commit below
# — so (a) a live-suite failure aborts before anything is committed or
# pushed, and (b) the skip-reason line (or nothing, on a pass) folds into
# the SAME release-notes commit rather than needing a second push, and (c)
# since scripts/e2e/run.sh fetches origin/main by default, this tests
# exactly the commit about to ship, right before it ships.
step "Live e2e integration gate"
if [ "$SKIP_INTEGRATION" -eq 1 ]; then
  cat >&2 <<EOF

################################################################################
## SKIPPING THE LIVE E2E INTEGRATION SUITE (--skip-integration)
## Reason: $INTEGRATION_SKIP_REASON
##
## This release has NOT been validated against real GitHub, real Claude, and
## real review bots. This is being recorded in $NOTES_FILE.
################################################################################

EOF
  insert_notes_line "$NOTES_FILE" "- ⚠️ Live e2e integration suite SKIPPED for this release: $INTEGRATION_SKIP_REASON"
  warn "live e2e integration suite SKIPPED — reason recorded in $NOTES_FILE"
else
  echo "   running scripts/e2e/run.sh against origin/main (this can take hours — see tests/e2e/README.md)"
  E2E_RC=0
  "$REPO_ROOT/scripts/e2e/run.sh" || E2E_RC=$?
  case "$E2E_RC" in
    0)
      ok "live e2e integration suite passed"
      ;;
    3)
      die "live e2e integration suite aborted: GraphQL budget exhausted mid-run (exit 3) — the verdict cannot be trusted. Wait for the budget window to reset and re-run scripts/cut-release.sh $VERSION."
      ;;
    5)
      die "live e2e integration suite aborted: its own sim/wire-contract pre-gate failed (exit 5) inside scripts/e2e/run.sh. Unexpected, since this script's own pre-gate step (step 3) already passed against the same tree — investigate the discrepancy (different ref? dirty tree in the bed?) before retrying."
      ;;
    *)
      die "live e2e integration suite FAILED (exit $E2E_RC) — see scripts/e2e/run.sh output above. This is a real regression; do not retry with --skip-integration to work around it."
      ;;
  esac
fi

# ─── 6. commit release notes as arbeithand ────────────────────────────────────
step "Commit release notes"
# Stage the per-version source-of-truth file and the updated known-versions list.
# The workflow reads release-notes/<version>.md directly — no copy step needed.
git add "$NOTES_FILE"
git add "$KNOWN_VERSIONS_FILE"
if git diff --cached --quiet; then
  warn "no release-notes changes to commit — skipping commit step"
else
  GIT_AUTHOR_NAME="$BOT_LOGIN" \
  GIT_AUTHOR_EMAIL="$BOT_EMAIL" \
  GIT_COMMITTER_NAME="$BOT_LOGIN" \
  GIT_COMMITTER_EMAIL="$BOT_EMAIL" \
  git commit -m "Release notes for $VERSION" --quiet
  COMMIT_AUTHOR="$(git log -1 --pretty=format:'%an <%ae>')"
  [ "$COMMIT_AUTHOR" = "$BOT_LOGIN <$BOT_EMAIL>" ] \
    || die "commit author wrong (got: $COMMIT_AUTHOR)"
  ok "committed as $COMMIT_AUTHOR"

  step "Push release-notes commit as @$BOT_LOGIN"
  git \
    -c credential.helper= \
    -c credential.https://github.com.helper= \
    push "https://x-access-token:${FABRIK_TOKEN}@github.com/${REPO}.git" main >/dev/null 2>&1 \
    || die "release-notes push failed"
  ok "release-notes commit pushed"
fi

# ─── 6b. auto-bump plugin/fabrik version if source changed ───────────────────
# Claude Code's /plugin update compares plugin.json's version field against
# marketplace.json@main; same version number means no refresh fires even if
# the plugin's source changed. Patch-bump plugin/fabrik's manifest whenever
# its source changed since the previous tag, so users always get fresh
# content. plugin/fabrik-workflows is out of scope — it's not listed in
# marketplace.json and has its own content-hash-based change detection.
step "Plugin version bump"
PLUGIN_MANIFEST="plugin/fabrik/.claude-plugin/plugin.json"
if [ "$NO_PLUGIN_BUMP" -eq 1 ]; then
  warn "--no-plugin-bump was passed; skipping plugin/fabrik version bump"
elif [ -z "$PREV_TAG" ]; then
  warn "no previous release tag found — skipping plugin/fabrik version bump (first release?)"
else
  if ! BUMP_OUTPUT="$(go run ./tools/bump-plugin-version/ plugin/fabrik "$PLUGIN_MANIFEST" "$PREV_TAG")"; then
    die "bump-plugin-version failed for plugin/fabrik (prev tag: $PREV_TAG)"
  fi
  if [ -z "$BUMP_OUTPUT" ]; then
    ok "plugin/fabrik unchanged since $PREV_TAG — no bump needed"
  else
    OLD_PLUGIN_VER="$(printf '%s' "$BUMP_OUTPUT" | cut -d' ' -f1)"
    NEW_PLUGIN_VER="$(printf '%s' "$BUMP_OUTPUT" | cut -d' ' -f2)"
    ok "bumped plugin/fabrik $OLD_PLUGIN_VER -> $NEW_PLUGIN_VER (source changed since $PREV_TAG)"

    # Append a changelog line so the version bump is visible in the GitHub
    # Release notes. release-notes/<version>.md was already committed and
    # pushed above, so this lands in a second commit alongside the manifest
    # bump rather than amending the already-pushed one.
    insert_notes_line "$NOTES_FILE" "- Auto-bumped fabrik plugin to $NEW_PLUGIN_VER (source changed since $PREV_TAG)"

    git add "$PLUGIN_MANIFEST" "$NOTES_FILE"
    GIT_AUTHOR_NAME="$BOT_LOGIN" \
    GIT_AUTHOR_EMAIL="$BOT_EMAIL" \
    GIT_COMMITTER_NAME="$BOT_LOGIN" \
    GIT_COMMITTER_EMAIL="$BOT_EMAIL" \
    git commit -m "Bump fabrik plugin to $NEW_PLUGIN_VER" --quiet
    COMMIT_AUTHOR="$(git log -1 --pretty=format:'%an <%ae>')"
    [ "$COMMIT_AUTHOR" = "$BOT_LOGIN <$BOT_EMAIL>" ] \
      || die "plugin-bump commit author wrong (got: $COMMIT_AUTHOR)"
    ok "committed as $COMMIT_AUTHOR"

    step "Push plugin-bump commit as @$BOT_LOGIN"
    git \
      -c credential.helper= \
      -c credential.https://github.com.helper= \
      push "https://x-access-token:${FABRIK_TOKEN}@github.com/${REPO}.git" main >/dev/null 2>&1 \
      || die "plugin-bump push failed — the release-notes commit for $VERSION was already pushed to main, but this plugin-bump commit ($(git rev-parse HEAD)) is still local only. Push it manually (git push origin main) or discard it (git reset --hard HEAD~1) before retrying."
    ok "plugin-bump commit pushed"
  fi
fi

# ─── 7. tag + push ────────────────────────────────────────────────────────────
step "Tag and push as @$BOT_LOGIN"
TAG_COMMIT="$(git rev-parse HEAD)"
git tag "$VERSION" "$TAG_COMMIT"

# Nuke credential helpers explicitly: the default gh-CLI helper points at the
# wrong user. PAT-in-URL alone is not enough on this machine.
git \
  -c credential.helper= \
  -c credential.https://github.com.helper= \
  push "https://x-access-token:${FABRIK_TOKEN}@github.com/${REPO}.git" "$VERSION" >/dev/null 2>&1 \
  || die "tag push failed — local tag $VERSION still present, remove with: git tag -d $VERSION"
ok "tag $VERSION pushed (commit $TAG_COMMIT)"

# ─── 8. watch workflow ────────────────────────────────────────────────────────
step "Locate release workflow run"
RUN_ID=""
for attempt in 1 2 3 4 5 6; do
  sleep $((attempt * 3))
  RUN_ID="$(GH_TOKEN="$FABRIK_TOKEN" gh run list \
    --workflow release.yml --limit 5 -R "$REPO" \
    --json databaseId,headBranch,event,createdAt \
    --jq "[.[] | select(.headBranch==\"$VERSION\" and .event==\"push\")] | sort_by(.createdAt) | last | .databaseId" 2>/dev/null || true)"
  [ -n "$RUN_ID" ] && [ "$RUN_ID" != "null" ] && break
done
[ -n "$RUN_ID" ] && [ "$RUN_ID" != "null" ] || die "release workflow run for $VERSION not found after retries"
ok "run id: $RUN_ID"

step "Watch workflow"
GH_TOKEN="$FABRIK_TOKEN" gh run watch "$RUN_ID" -R "$REPO" --exit-status >/dev/null \
  || warn "gh run watch exited non-zero (will recheck conclusion explicitly)"

CONCLUSION="$(GH_TOKEN="$FABRIK_TOKEN" gh run view "$RUN_ID" -R "$REPO" --json conclusion --jq .conclusion)"
if [ "$CONCLUSION" != "success" ]; then
  warn "workflow conclusion: $CONCLUSION"
  GH_TOKEN="$FABRIK_TOKEN" gh run view "$RUN_ID" -R "$REPO" --log-failed | tail -40 >&2 || true
  die "release workflow did not succeed — see logs above. To retry: delete release+tag+discussion (see cleanup at the bottom of this script), fix, and re-run."
fi
ok "workflow conclusion: success"

# ─── 9. identity verification ─────────────────────────────────────────────────
step "Verify release + discussion author = @$BOT_LOGIN"
RELEASE_AUTHOR="$(GH_TOKEN="$FABRIK_TOKEN" gh api "/repos/$REPO/releases/tags/$VERSION" --jq .author.login)"
[ "$RELEASE_AUTHOR" = "$BOT_LOGIN" ] || die "release author is '$RELEASE_AUTHOR', expected '$BOT_LOGIN'. The repo secret PUBLIC_REPO_RELEASE_TOKEN is wrong — rotate it to an arbeithand PAT at: https://github.com/${REPO}/settings/secrets/actions/PUBLIC_REPO_RELEASE_TOKEN, then delete the release+discussion+tag (see cleanup below) and re-run."
ok "release author: @$RELEASE_AUTHOR"

DISCUSSION_AUTHOR="$(GH_TOKEN="$FABRIK_TOKEN" gh api graphql -f query="
query {
  repository(owner: \"handarbeit\", name: \"fabrik\") {
    discussions(first: 5, orderBy: {field: CREATED_AT, direction: DESC}) {
      nodes { title author { login } }
    }
  }
}" --jq ".data.repository.discussions.nodes[] | select(.title | contains(\"$VERSION\")) | .author.login" | head -1)"
if [ -z "$DISCUSSION_AUTHOR" ]; then
  warn "no $VERSION discussion found (workflow may have skipped it)"
elif [ "$DISCUSSION_AUTHOR" != "$BOT_LOGIN" ]; then
  die "discussion author is '$DISCUSSION_AUTHOR', expected '$BOT_LOGIN'. Same root cause as above."
else
  ok "discussion author: @$DISCUSSION_AUTHOR"
fi

# ─── 10. file doc-update issue ─────────────────────────────────────────────────
if [ "$NO_DOC_ISSUE" -eq 1 ]; then
  warn "--no-doc-issue was passed; skipping doc-update issue + project placement"
else
  step "File doc-update issue + add to project at Specify"
  ISSUE_BODY="Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in $VERSION.

See release notes: https://github.com/${REPO}/releases/tag/${VERSION}

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate \`docs/llms-full.txt\` after any canonical-doc edits (per CLAUDE.md)"

  ISSUE_URL="$(GH_TOKEN="$FABRIK_TOKEN" gh issue create -R "$REPO" \
    --title "Update docs for $VERSION" \
    --label "documentation" \
    --label "fabrik:yolo" \
    --body "$ISSUE_BODY")"
  ok "issue created: $ISSUE_URL"

  PROJECT_ID="$(GH_TOKEN="$FABRIK_TOKEN" gh project view "$PROJECT_NUM" --owner "$PROJECT_OWNER" --format json --jq .id)"
  ITEM_ID="$(GH_TOKEN="$FABRIK_TOKEN" gh project item-add "$PROJECT_NUM" --owner "$PROJECT_OWNER" --url "$ISSUE_URL" --format json --jq .id)"
  STATUS_FIELD="$(GH_TOKEN="$FABRIK_TOKEN" gh project field-list "$PROJECT_NUM" --owner "$PROJECT_OWNER" --format json --jq '.fields[] | select(.name=="Status") | .id')"
  SPECIFY_OPT="$(GH_TOKEN="$FABRIK_TOKEN" gh project field-list "$PROJECT_NUM" --owner "$PROJECT_OWNER" --format json --jq '.fields[] | select(.name=="Status") | .options[] | select(.name=="Specify") | .id')"
  GH_TOKEN="$FABRIK_TOKEN" gh project item-edit \
    --id "$ITEM_ID" --project-id "$PROJECT_ID" \
    --field-id "$STATUS_FIELD" --single-select-option-id "$SPECIFY_OPT" >/dev/null
  ok "added to project at Status=Specify"
fi

# ─── done ─────────────────────────────────────────────────────────────────────
step "Release $VERSION published"
echo "  release:    https://github.com/${REPO}/releases/tag/${VERSION}"
echo "  workflow:   https://github.com/${REPO}/actions/runs/${RUN_ID}"
echo "  author:     @$BOT_LOGIN (verified)"

}

# Guarded so scripts/cut_release_gate_test.sh can `source` this file to reach
# parse_args() and insert_notes_line() (and every other function above)
# without triggering an actual publish. Everything above this guard is safe
# to execute on source — constants, cd to repo root, and pure function
# definitions, no publish side effects. When executed directly
# (./cut-release.sh or `bash cut-release.sh`), BASH_SOURCE[0] equals $0, so
# this still dispatches exactly as before this guard existed.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  parse_args "$@"
  main
fi

# ─── cleanup reference (not executed) ─────────────────────────────────────────
# If a release goes out under the wrong identity (or you need to redo it):
#
#   FABRIK_TOKEN=$(grep '^FABRIK_TOKEN=' .env | cut -d= -f2-)
#   DISC_ID=$(GH_TOKEN="$FABRIK_TOKEN" gh api graphql -f query='query {
#       repository(owner: "handarbeit", name: "fabrik") {
#         discussions(first: 5, orderBy: {field: CREATED_AT, direction: DESC}) {
#           nodes { id title }
#         }
#       }
#     }' --jq '.data.repository.discussions.nodes[] | select(.title | contains("VERSION_HERE")) | .id')
#   [ -n "$DISC_ID" ] && GH_TOKEN="$FABRIK_TOKEN" gh api graphql -f query="mutation { deleteDiscussion(input: {id: \"$DISC_ID\"}) { discussion { number } } }"
#   GH_TOKEN="$FABRIK_TOKEN" gh release delete VERSION_HERE --repo handarbeit/fabrik --yes --cleanup-tag
#   git tag -d VERSION_HERE
#
# Then fix whatever caused the wrong identity (commonly: the PUBLIC_REPO_RELEASE_TOKEN secret),
# and re-run this script.
