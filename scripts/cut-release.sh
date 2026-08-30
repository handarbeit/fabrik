#!/usr/bin/env bash
# cut-release.sh — Publish a Fabrik release as the @arbeithand bot.
#
# Usage:
#   scripts/cut-release.sh v0.0.67
#   scripts/cut-release.sh v0.0.67 --skip-tests       # skip race-tested suite (last-resort)
#   scripts/cut-release.sh v0.0.67 --no-doc-issue     # skip filing the doc-update issue
#   scripts/cut-release.sh v0.0.67 --no-plugin-bump   # skip the marketplace plugin version auto-bumps
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
#   3. Sim suite + github wire-contract tests — unconditional pre-gate (R1/R2, #1454).
#      Exports FABRIK_PREGATE_VERIFIED_SHA on success so step 5 doesn't pay for the
#      identical pre-gate a second time (R5, #1624) — see export_pregate_verified_sha.
#   4. go build + go test -race, -parallel capped via scripts/lib/parallel.sh
#      (REQ3, #1677 — same cap scripts/sim/run.sh uses, since this run covers
#      the same git-forking tests/sim package; skippable with --skip-tests,
#      the pre-gate above never is)
#   5. Live e2e integration gate — mandatory by default; --skip-integration=<reason> is the
#      one sanctioned, loud, release-notes-recorded escape hatch (R2, #1454). Always
#      passes --clean (REQ1, #1677) to reset the shared bed first — a release cut and
#      concurrent manual e2e testing on the bed can no longer safely overlap.
#      FIDELITY-DRIFT CHECK (R4, #1454): if this step fails on something step 3's
#      pre-gate passed, that's a fidelity bug in the sim, not just a live regression —
#      file it and fix it in tests/sim too (procedure: tests/sim/README.md's
#      "Fidelity-drift policy" section). This is a release-checklist step, not
#      something left to memory.
#   6. Commit release-notes.md (if dirty) as arbeithand
#   6b. For each plugin listed in .claude-plugin/marketplace.json whose source
#       changed since the previous tag, patch-bump its .claude-plugin/plugin.json
#       and commit them together as arbeithand (skippable with --no-plugin-bump)
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

# shellcheck source=lib/parallel.sh
source "$REPO_ROOT/scripts/lib/parallel.sh"

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

# allowed_dirty_regex prints the single grep -E pattern of tracked-file
# changes this script itself is expected to leave uncommitted at various
# points in a run, parameterized on $1 (VERSION). Two call sites share this
# one definition rather than maintaining two separate lists (#1677):
#   - step 1's own pre-flight DIRTY check (below) — unchanged behavior,
#     just no longer a second, hand-copied regex.
#   - run_pregate's dirty-tree check (scripts/e2e/run.sh), via the exported
#     FABRIK_PREGATE_ALLOWED_DIRTY_REGEX — see export_pregate_verified_sha.
#
# release-notes/<version>.md is written before step 1 even runs (a
# prerequisite, not a self-write) but is still expected dirty on a repeat
# invocation; plugin/known_embedded_versions.go is step 4's own conditional
# write (only when a new plugin hash isn't already recorded); the
# plugin/*/.claude-plugin/plugin.json pattern covers step 6b's marketplace
# version bumps (not yet run when this fires for run_pregate's purposes,
# but a retry after a partial step 6b could legitimately see it dirty too).
# Nothing outside this declared, named set is ever treated as expected —
# any other dirty file still fails both checks exactly as before this was
# extracted (see run_pregate's own comment on the TOCTOU gap this
# preserves).
allowed_dirty_regex() {
  local version="$1"
  printf '%s' "^\\?\\? release-notes/${version}\\.md\$| M release-notes/${version}\\.md\$|^M  release-notes/${version}\\.md\$| M plugin/known_embedded_versions\\.go\$|^M  plugin/known_embedded_versions\\.go\$| M plugin/[^/]+/\\.claude-plugin/plugin\\.json\$|^M  plugin/[^/]+/\\.claude-plugin/plugin\\.json\$"
}

# export_pregate_verified_sha exports FABRIK_PREGATE_VERIFIED_SHA (and
# FABRIK_PREGATE_ALLOWED_DIRTY_REGEX, REQ7 #1677 — see below) as the exact
# commit this invocation's own sim + wire-contract pre-gate (step 3, below)
# just verified. Called right after that step passes, while HEAD is still
# guaranteed unchanged (this script commits nothing until step 6, well
# after step 5's live e2e gate runs) — so the value names the exact tree
# that was checked, not merely "some release."
#
# scripts/e2e/run.sh's own pre-gate (run_pregate) checks FABRIK_PREGATE_VERIFIED_SHA
# against a freshly-resolved `git rev-parse HEAD` of its own before deciding
# whether to re-run the identical sim + wire-contract checks (R5, #1624) —
# see that function's own comment. An exported env var, not a file marker:
# run.sh is invoked as this script's direct child process (step 5), so
# there is no on-disk staleness/cleanup concern, and the value is naturally
# scoped to exactly this invocation (env vars do not outlive the child
# process) — unlike E2E_SKIP_PREGATE, a blanket opt-out this script
# deliberately never sets (see step 5's own comment), this is a narrow,
# tree-scoped signal that a standalone `scripts/e2e/run.sh` invocation
# never sees and so still pays the full pre-gate price, exactly as before
# this existed.
#
# REQ7 (#1677): the SHA match alone was never sufficient on a real release —
# step 4's "Record embedded plugin hash" write to
# plugin/known_embedded_versions.go lands on disk, uncommitted, between
# this function running (end of step 3) and run_pregate's own dirty-tree
# check (step 5), so the tree was dirty on essentially every real release
# and the SHA guard never actually engaged. FABRIK_PREGATE_ALLOWED_DIRTY_REGEX
# tells run_pregate which specific, already-known-benign self-writes to
# disregard when deciding "dirty" — the same allowlist step 1's own
# preflight already trusts, not a new or wider one. A standalone
# scripts/e2e/run.sh invocation never sets this var, so its dirty-tree
# check remains exactly as strict as before this existed.
export_pregate_verified_sha() {
  FABRIK_PREGATE_VERIFIED_SHA="$(git rev-parse HEAD)"
  export FABRIK_PREGATE_VERIFIED_SHA
  FABRIK_PREGATE_ALLOWED_DIRTY_REGEX="$(allowed_dirty_regex "$VERSION")"
  export FABRIK_PREGATE_ALLOWED_DIRTY_REGEX
}

# interpret_e2e_exit_code prints the operator-facing message for a given
# scripts/e2e/run.sh exit code ($1) and returns 0 if that code means success,
# 1 otherwise — main() calls `ok`/`die` with the result accordingly. Extracted
# into its own function (R2, #1454) so scripts/cut_release_gate_test.sh can
# assert on the message for each known exit code — in particular that `4`
# (PREFLIGHT_FAILED_EXIT, a bed/infrastructure problem where the suite never
# actually ran) gets its own distinct, non-misleading message rather than
# falling through to the generic "real regression, check fidelity-drift"
# branch, which would wrongly send an operator investigating a stuck lock or
# an unreachable SSH remote down the fidelity-drift procedure instead.
# Mirrors scripts/e2e/run.sh's own exit-code convention (3/4/5/7) exactly —
# see that script's header comment for where each code originates. (Exit 6,
# POST_SUITE_WATCHDOG_EXIT (#1676), has no dedicated case here yet — it falls
# through to the generic branch below; a pre-existing gap from #1676, not
# introduced by this issue and left as-is.)
interpret_e2e_exit_code() {
  local rc="$1"
  case "$rc" in
    0)
      echo "live e2e integration suite passed"
      ;;
    3)
      echo "live e2e integration suite aborted: GraphQL budget exhausted mid-run (exit 3) — the verdict cannot be trusted. Wait for the budget window to reset and re-run scripts/cut-release.sh $VERSION."
      ;;
    4)
      echo "live e2e integration suite aborted: bed preflight failed (exit 4) inside scripts/e2e/run.sh — the suite never ran. This is an infrastructure problem (stuck lock, unreachable SSH remote, dirty tracked files in the bed checkout, etc.), NOT a regression and NOT a fidelity-drift case — see scripts/e2e/run.sh's own preflight_bed output above for the specific cause, fix it, and re-run scripts/cut-release.sh $VERSION."
      ;;
    5)
      echo "live e2e integration suite aborted: its own sim/wire-contract pre-gate failed (exit 5) inside scripts/e2e/run.sh. Unexpected: this script's own pre-gate step (step 3) already passed against the same tree, and R5's FABRIK_PREGATE_VERIFIED_SHA (#1624) should have made run.sh skip re-running it rather than fail it a second time — investigate the discrepancy (different ref? dirty tree in the bed? the SHA guard itself misfiring) before retrying."
      ;;
    7)
      echo "live e2e integration suite aborted: an operational precondition was not met (exit 7, PRECONDITION_FAILED_EXIT) inside scripts/e2e/run.sh — either a competing local Fabrik instance is sharing the bed's @arbeithand GraphQL token, or the review bot (Pruefer) is confidently unreachable. This is an operational problem, NOT a regression and NOT a fidelity-drift case — see scripts/e2e/run.sh's own output above for which precondition failed and what it found, fix it (stop the competing instance, or start Pruefer), and re-run scripts/cut-release.sh $VERSION. See tests/e2e/README.md's 'Operational up/down contract' (#1684) for the full mechanism."
      ;;
    *)
      echo "live e2e integration suite FAILED (exit $rc) — see scripts/e2e/run.sh output above. This is a real regression; do not retry with --skip-integration to work around it. FIDELITY-DRIFT CHECK (R4, #1454): this script's own sim + wire-contract pre-gate (step 3) already passed against this same tree, so whatever the live suite just caught is exactly the case that policy covers — file a fidelity issue, add the scenario to tests/sim, and update tests/sim/simgh/FIDELITY.md (see tests/sim/README.md's 'Fidelity-drift policy' section) once the underlying regression itself is fixed."
      ;;
  esac
  [ "$rc" -eq 0 ]
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
# and any plugin/*/.claude-plugin/plugin.json (all are updated by this script
# itself, after the build step and in step 6b). The manifest pattern is
# deliberately not pinned to one plugin: step 6b bumps every marketplace-listed
# plugin, so a run that dies partway through can leave any of them modified but
# uncommitted, and a retry must not abort on the mess its own predecessor made.
#
# Allowlist disposition (reviewed for #1070, extended for #816): all three
# files are committed by this script's own step 6 / step 6b, *before* the tag
# is created and pushed in step 7. By the time release.yml's CI job checks
# out the tag, it's a fresh clone — it never sees this local working tree,
# dirty or not. So this allowlist cannot affect the CI-built artifact and
# does not need to be tightened; the built-artifact VCS check lives in
# .goreleaser.yaml/release.yml instead (see
# adrs/071-release-artifact-vcs-verification.md).
DIRTY=$(git status --porcelain | grep -Ev "$(allowed_dirty_regex "$VERSION")" || true)
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
#
# Once this passes, export_pregate_verified_sha (R5, #1624) records HEAD so
# step 5's invocation of scripts/e2e/run.sh can skip re-running the
# identical checks against the same, unchanged tree — see that function's
# own comment. This does NOT weaken the check: it still runs here, in full,
# exactly as before; it only stops step 5 from paying for it a second time.
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
export_pregate_verified_sha

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
  # -parallel capped (REQ3, #1677): this repo-wide invocation includes the
  # same git-forking tests/sim package that scripts/sim/run.sh's own
  # SIM_PARALLEL caps — left uncapped here, it inherits GOMAXPROCS (28 on
  # the host that produced #1677) and reliably reproduces the TSan
  # fork/exec SIGSEGV #1624's cap was meant to prevent, just via a
  # different entry point. FABRIK_RACE_PARALLEL is a narrow override (for
  # experimentation); default_race_parallel (scripts/lib/parallel.sh) is
  # the same helper scripts/sim/run.sh uses, so the two never drift into
  # incoherent caps for overlapping concurrency. -timeout matches
  # scripts/sim/run.sh's own SIM_TIMEOUT rationale — see that script's
  # header comment.
  RACE_PARALLEL="${FABRIK_RACE_PARALLEL:-$(default_race_parallel)}"
  if ! go test -race -parallel "$RACE_PARALLEL" -timeout 20m ./... >/tmp/cut-release-test.log 2>&1; then
    tail -40 /tmp/cut-release-test.log >&2
    die "go test -race ./... failed (full log: /tmp/cut-release-test.log)"
  fi
  ok "all tests pass with -race (-parallel $RACE_PARALLEL)"
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
#
# --clean (REQ1, #1677): every invocation now resets the shared bed via
# scripts/e2e/reset.sh before the gate runs — this was previously never
# passed, so bed state (closed-but-still-labeled board items, stale
# branches, accumulated sessions/logs) accrued release over release until
# TestSwitchTrainMode's lock-timeout check failed against a bed slow enough
# from that accumulated backlog to blow its wait window (#1677's own
# ten-attempt v0.0.81 incident). scripts/e2e/reset.sh is already destructive
# by design (closes all open PRs/issues, deletes fabrik/* branches, drains
# the board) — this is not new destructive capability, just the missing
# call site — but it does mean a release cut and any concurrent manual e2e
# testing on this shared bed can no longer safely overlap. Do not run this
# script while someone else is using ~/dev/fabrik-test by hand.
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
  echo "   running scripts/e2e/run.sh --clean against origin/main (this can take hours — see tests/e2e/README.md)"
  E2E_RC=0
  "$REPO_ROOT/scripts/e2e/run.sh" --clean || E2E_RC=$?
  E2E_MSG="$(interpret_e2e_exit_code "$E2E_RC")"
  if [ "$E2E_RC" -eq 0 ]; then
    ok "$E2E_MSG"
  else
    die "$E2E_MSG"
  fi
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

# ─── 6b. auto-bump marketplace plugin versions if source changed ─────────────
# Claude Code's /plugin update compares plugin.json's version field against
# marketplace.json@main; same version number means no refresh fires even if the
# plugin's source changed. Patch-bump the manifest of every plugin whose source
# changed since the previous tag, so users always get fresh content.
#
# The plugin list is derived from marketplace.json rather than hardcoded: a
# plugin listed there is bumpable from that moment on, with no matching edit
# here. That coupling is deliberate — marketplace.json membership is exactly
# what makes /plugin update's version comparison apply to a plugin, so the two
# cannot drift apart. plugin/fabrik-workflows is out of scope for the same
# reason: it isn't listed, isn't served by /plugin update, and has its own
# content-hash-based change detection.
step "Plugin version bump"
if [ "$NO_PLUGIN_BUMP" -eq 1 ]; then
  warn "--no-plugin-bump was passed; skipping plugin version bumps"
elif [ -z "$PREV_TAG" ]; then
  warn "no previous release tag found — skipping plugin version bumps (first release?)"
else
  MARKETPLACE_JSON=".claude-plugin/marketplace.json"
  [ -f "$MARKETPLACE_JSON" ] || die "$MARKETPLACE_JSON not found"

  # Only git-subdir entries carry a source.path pointing at a directory in this
  # repo; any other source type is distributed from elsewhere and is not ours to
  # bump. Enumerated by a Go helper rather than jq, so the release path depends
  # on nothing the repo doesn't already require.
  if ! PLUGIN_DIRS="$(go run ./tools/list-marketplace-plugins/ "$MARKETPLACE_JSON")"; then
    die "failed to enumerate plugins from $MARKETPLACE_JSON"
  fi

  BUMPED_MANIFESTS=""
  BUMPED_SUMMARY=""

  for PLUGIN_DIR in $PLUGIN_DIRS; do
    PLUGIN_MANIFEST="$PLUGIN_DIR/.claude-plugin/plugin.json"
    [ -f "$PLUGIN_MANIFEST" ] \
      || die "$PLUGIN_MANIFEST not found (listed in $MARKETPLACE_JSON)"

    if ! BUMP_OUTPUT="$(go run ./tools/bump-plugin-version/ "$PLUGIN_DIR" "$PLUGIN_MANIFEST" "$PREV_TAG")"; then
      die "bump-plugin-version failed for $PLUGIN_DIR (prev tag: $PREV_TAG)"
    fi
    if [ -z "$BUMP_OUTPUT" ]; then
      ok "$PLUGIN_DIR unchanged since $PREV_TAG — no bump needed"
      continue
    fi

    OLD_PLUGIN_VER="$(printf '%s' "$BUMP_OUTPUT" | cut -d' ' -f1)"
    NEW_PLUGIN_VER="$(printf '%s' "$BUMP_OUTPUT" | cut -d' ' -f2)"
    PLUGIN_NAME="$(basename "$PLUGIN_DIR")"
    ok "bumped $PLUGIN_DIR $OLD_PLUGIN_VER -> $NEW_PLUGIN_VER (source changed since $PREV_TAG)"

    BUMPED_MANIFESTS="$BUMPED_MANIFESTS $PLUGIN_MANIFEST"
    BUMPED_SUMMARY="$BUMPED_SUMMARY $PLUGIN_NAME@$NEW_PLUGIN_VER"

    # Append a changelog line so each bump is visible in the GitHub Release
    # notes. release-notes/<version>.md was already committed and pushed above,
    # so this lands in a later commit alongside the manifest bumps rather than
    # amending the already-pushed one.
    insert_notes_line "$NOTES_FILE" "- Auto-bumped $PLUGIN_NAME plugin to $NEW_PLUGIN_VER (source changed since $PREV_TAG)"
  done

  if [ -z "$BUMPED_MANIFESTS" ]; then
    ok "no marketplace plugin source changed since $PREV_TAG — nothing to bump"
  else
    # Intentionally unquoted: BUMPED_MANIFESTS is a space-separated path list.
    # Plugin paths come from marketplace.json and contain no spaces.
    git add $BUMPED_MANIFESTS "$NOTES_FILE"
    GIT_AUTHOR_NAME="$BOT_LOGIN" \
    GIT_AUTHOR_EMAIL="$BOT_EMAIL" \
    GIT_COMMITTER_NAME="$BOT_LOGIN" \
    GIT_COMMITTER_EMAIL="$BOT_EMAIL" \
    git commit -m "Bump plugin versions:$BUMPED_SUMMARY" --quiet
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
