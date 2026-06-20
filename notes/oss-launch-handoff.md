# Fabrik OSS Launch — Session Handoff

**Purpose:** Load this into a fresh Claude Code session to get up to speed on Fabrik's open-source state, the identity migration that just shipped, the live distribution pipeline, what's queued, and the known gotchas. Written 2026-05-30 after a multi-day push to OSS-ready.

---

## 1. Quick orientation

**Fabrik** is a Go CLI that orchestrates Claude Code through an SDLC pipeline defined on a GitHub Project board. Issues are units of work; columns are stages (Specify → Research → Plan → Implement → Review → Validate → Done). The full architecture map is in `CLAUDE.md` at the repo root.

- **Repo:** `https://github.com/handarbeit/fabrik` — public
- **Working directory:** `/Users/bpja/dev/fabrik`
- **Docs site:** `https://fabrik.handarbeit.io`
- **Brand landing:** `https://www.handarbeit.io` (separate, not owned by this repo)
- **License:** Apache-2.0

**Bot identity:** `arbeithand` (display name on GitHub may show as such). All commits, releases, issue mutations, comments, and reactions on `handarbeit/fabrik` are now authored as this account. The PAT lives in `.env` as `FABRIK_TOKEN` (gitignored). Email is `handarbeit@handarbeit.io`.

---

## 2. The OSS migration (compressed timeline)

The repo started private under `tenaciousvc/fabrik`, briefly went through `shadoworg`, and ultimately settled at **`handarbeit/fabrik`**. The migration covered:

1. **Org transfer:** `tenaciousvc/fabrik` → `handarbeit/fabrik` (GitHub repo transfer, preserved issues/PRs/history).
2. **Personal-account rename:** `verveguy` (which held real PII — name, email, employer, location) → `arbeithand` (clean, no PII). The verveguy profile was scrubbed of all identifying fields before/during the rename.
3. **Git history rewrite:** Used `git filter-repo` with both `--replace-text` (blob content) and a `--commit-callback` (author/committer/message scrub). Rewrote 1773 of 1776 commits to `arbeithand <handarbeit@handarbeit.io>`. Preserved 3 commits by `jmatthewpryor` (legitimate other contributor). Stripped `Co-Authored-By: Claude …` trailers from 1224 commits. Force-pushed main + all 66 release tags.
4. **Issue tracker reset:** All 12 open verveguy-authored issues were re-filed under arbeithand in the Backlog column (#765–#776), then the originals were closed with "Superseded by #NEW" pointers and removed from the project board. Stale `fabrik/issue-NNN` branches on remote (17 of them) were deleted. 5 stale open PRs closed.
5. **Local cleanup:** 15 stale worktrees deleted, backup tag removed, `/tmp/fabrik-rewrite` cleaned up.
6. **Bare-clone committer-identity fix:** `engine/worktree.go:ensureBareClone` was patched (PR #780) to set `user.name = FABRIK_USER` and `user.email = <user>@users.noreply.github.com` on the bare clone after first clone. Closes the gap where Fabrik commits were inheriting the system-global git identity. Engine source code never set committer identity before this fix.
7. **OSS-readiness PR (#780, merged):** LICENSE (Apache-2.0), CONTRIBUTING.md, SECURITY.md, CODE_OF_CONDUCT.md, `.github/ISSUE_TEMPLATE/`, PR template, marketing-page (`docs/index.md`) update, residual identity scrub from tests/ADRs/specs/docs.
8. **Repo flipped to public** — final click in Settings.

---

## 3. Identity state — what's wired where

This is the surface area most likely to bite a fresh session if not understood.

### Local git identity (this repo only)

```bash
git config --local user.name   # arbeithand
git config --local user.email  # handarbeit@handarbeit.io
```

**The global git identity is intentionally NOT changed** — it's still `verveguy / bpjadam@gmail.com` for every other repo on this machine. Don't change it.

### Credential helper (this repo only)

The credential helper at `git config --local credential.helper` is a small inline shell function that:
1. Reads `.env` from the repo root
2. Extracts the `FABRIK_TOKEN=` value
3. Returns `username=arbeithand` + the token to git as HTTPS credentials

This means `git push` from this repo uses the arbeithand PAT, while pushes from any other repo continue using `gh`'s normal credentials (still verveguy). The helper config explicitly resets any inherited helpers (osxkeychain, gcm) for THIS repo only, so no inherited credential helper "wins" first.

### Fabrik runtime identity

`.env` (gitignored) sets:
- `FABRIK_TOKEN=ghp_…` — the arbeithand classic PAT (scopes: `repo`, `project`, `workflow`)
- `FABRIK_USER=arbeithand`

`.fabrik/config.yaml` (committed) sets `user: arbeithand`.

### Bare-clone identity (the gap that bit us)

The bare clone at `.fabrik/repos/handarbeit-fabrik.git/` has explicit `user.name = arbeithand` / `user.email = handarbeit@handarbeit.io` in its local config. **This must persist.** If you ever `rm -rf .fabrik/repos/` and let Fabrik re-clone, the new clone runs through `engine/worktree.go:ensureBareClone`'s `setCommitterIdentity` helper which now sets these automatically. But if a clone predates that fix, it inherits global git config (verveguy) and Fabrik's commits silently leak the wrong identity.

**Active worktrees inherit the bare clone's config.** If you observe verveguy-authored commits on a Fabrik branch, check `git config user.name` in the bare clone and in the specific worktree first.

### gh CLI

`gh auth status` shows `verveguy` as the active account (machine-wide). This is fine: `gh` operations done manually are authorized as verveguy, but git pushes from THIS repo route through the local credential helper as arbeithand, and Fabrik runtime authorizes as arbeithand via FABRIK_TOKEN. The split is intentional.

---

## 4. What's published / distributed

Two distinct distribution channels:

### Fabrik binary (`fabrik` CLI)

- Released via `scripts/cut-release.sh v0.0.XX` (currently at v0.0.66 in tags; main is past it).
- Build artifacts: `.tar.gz` per platform (darwin/arm64, darwin/amd64, linux/arm64, linux/amd64) attached to GitHub Releases.
- No Docker images, no Homebrew tap (yet — Homebrew was discussed and deferred; see `notes` below).
- Release authorship verified end-to-end as `@arbeithand` (cut-release.sh exits if the PAT identity, release author, or discussion author comes back as anyone else).

### Claude Code plugin (`fabrik` plugin)

- Lives at `plugin/fabrik/` in this repo.
- Marketplace manifest: `.claude-plugin/marketplace.json` (top-level) advertises one plugin (`fabrik`), `source: git-subdir`, `path: plugin/fabrik`, `ref: main`.
- Users install via `/plugin install fabrik` from a marketplace clone of this repo.
- Current published version: **0.1.1** (PR #790 bumped it from 0.1.0 on 2026-05-30 to force users' caches to refresh after the shadoworg → handarbeit URL migration; the prior 0.1.0 had stale URLs even though the source was clean).
- The separate `plugin/fabrik-workflows/` is **NOT** in marketplace.json — it's embedded in the Fabrik binary via `plugin/embed.go` and served to Claude workers locally. Its version field is internal-only.

### Docs site

- `docs/` is a Jekyll site published to GitHub Pages at `fabrik.handarbeit.io`.
- CNAME at `docs/CNAME`, custom domain + HTTPS enforced.
- Landing page is `docs/index.md` (hero, features, install snippet, "Learn more" cards including Discussions, Contributing, License).

---

## 5. Open issues and pipeline state

As of handoff:

**In active pipeline (Fabrik working it):**
- **#816** — `feat(release): auto-bump plugin versions on cut-release when source changed`. In **cruise mode**, currently at **Specify**. When it reaches Validate, **stop and inspect** before deciding to merge or close — issue body has open questions worth reviewing (testability, opt-out flag, behavior when jq missing, etc.). The original prototype for this work was PR #809 (closed). Acceptance criteria live in the issue body.

**Backlog (queued, no decision needed):**
- **#765** — Add deterministic hooks to Fabrik plugin for 100% compliance enforcement
- **#768** — `feat: decouple --filter-user from auth identity to enable personal-bot mode`
- **#770** — Convert Fabrik to use a GitHub App (bot) identity (the proper multi-instance fix; the rename to arbeithand was a partial workaround)
- **#771** — Custom workflow stage designer
- **#772** — Support ollama proxy for open weight models
- **#774** — `fabrik: re-enable Done-stage auto-archive with completion-anchored timing`

**Test-flake known issue:**
- **#764** — `engine: TestExtendTurns_PersistsAcrossMultipleStages hangs indefinitely`. Test in `engine/item_test.go:312` blocks `go test ./engine/` from running cleanly without a `-skip` filter. Pre-existing, not a regression. Workaround: `go test ./engine/ -timeout 30s -skip TestExtendTurns_PersistsAcrossMultipleStages`.

**Recently filed but not yet processed:**
- Other low-numbered Backlog items from before the migration may also be open — `gh issue list --repo handarbeit/fabrik --state open` for the current set.

---

## 6. Pending decisions / known issues

### From the v0.0.66 post-mortem (PR #826 merged, full text at `docs/postmortems/v0.0.66-broken-graphql-mutation.md`)

Two follow-ups identified but not adopted:
- **F-1: Schema-validation in CI** — introspect GitHub's GraphQL schema and validate every mutation/field in `github/*.go`. Requires a CI secret PAT. **Highest-leverage further prevention.** Not yet filed as its own issue.
- **F-2: Stricter `// Verified against live schema YYYY-MM-DD` comment convention** — lightweight, no tooling, but requires discipline. Not adopted.

### Fabrik-instance hygiene

Multiple `./fabrik --auto-upgrade` processes can pile up if you restart without killing the old one. Each one polls the board and races for the `fabrik:locked:arbeithand` label. If you see unexpected stalls, `ps aux | grep "[/ ]fabrik"` — should be exactly one running. Same for the `gh-webhook` forwarder.

### Review-stage stalls

Fabrik's Review stage has `wait_for_reviews: true`. If a PR opens with **zero review requests** (no GitHub Apps assigning reviewers, e.g. Copilot or Gemini), the gate has nothing to wait for and times out → issue gets `fabrik:awaiting-input + fabrik:paused`. To prevent this, **install Copilot Code Review and/or Gemini Code Assist on the `handarbeit` org**. When this hits, unpause with `gh api -X DELETE repos/handarbeit/fabrik/issues/<N>/labels/fabrik:paused` (and same for `fabrik:awaiting-input`).

### Homebrew tap

Not set up. Decision deferred. If desired later: create `handarbeit/homebrew-fabrik`, add a `brews:` section to `.goreleaser.yaml`. Existing release PAT (`PUBLIC_REPO_RELEASE_TOKEN`) has `repo` scope which is sufficient — no new permissions needed.

### Squash merging

**Disabled by default** on `handarbeit/fabrik` (`allow_squash_merge: false`). Merge commit and rebase are enabled. If you want to squash-merge a specific PR, temporarily enable via `gh api -X PATCH repos/handarbeit/fabrik -F allow_squash_merge=true`, merge, then turn off again.

### Branch protection on main

Requires the `Analyze (go)` CodeQL check + the new `Test and vet` check (added by PR #826). No force-pushes, no deletions. `enforce_admins: false` so an admin (you) can override in emergencies.

---

## 7. Key files and locations (cheat sheet)

| Concern | Location |
|---|---|
| Engine architecture & conventions | `CLAUDE.md` |
| Stage configs (YAML) | `.fabrik/stages/` |
| Per-project config | `.fabrik/config.yaml` (committed; sets `user: arbeithand`) |
| Secrets | `.env` (gitignored; FABRIK_TOKEN, FABRIK_USER) |
| Bare clone of managed repo | `.fabrik/repos/handarbeit-fabrik.git/` |
| Active worktrees | `.fabrik/worktrees/handarbeit-fabrik/issue-N/` |
| Fabrik logs | `.fabrik/logs/` |
| Engine source | `engine/*.go` |
| GitHub client | `github/*.go` |
| Claude Code marketplace plugin | `plugin/fabrik/` |
| Embedded worker plugin | `plugin/fabrik-workflows/` |
| Plugin manifest | `plugin/fabrik/.claude-plugin/plugin.json` (currently 0.1.1) |
| Marketplace manifest | `.claude-plugin/marketplace.json` |
| Release script | `scripts/cut-release.sh` |
| Release workflow | `.github/workflows/release.yml` |
| CI workflow | `.github/workflows/ci.yml` (added by PR #826) |
| Docs site landing | `docs/index.md` |
| Docs site bundle for LLMs | `docs/llms-full.txt` (regenerate via `bash scripts/generate-llms-full.sh`) |
| As-built state machine | `docs/state-machine.md` |
| As-built stage lifecycle | `docs/stage-lifecycle.md` |
| ADRs | `adrs/*.md` |
| Specs (per the new discipline) | `specs/<issue-number>-<slug>/spec.md` |
| Post-mortems | `docs/postmortems/` |

---

## 8. Gotchas / lessons learned

1. **The bare-clone committer-identity gap.** `engine/worktree.go` did not set `user.name`/`user.email` on the bare clone, so Fabrik commits inherited whatever global git identity was set. Fixed by `setCommitterIdentity` helper in PR #780. Watch for this if you ever blow away `.fabrik/repos/` — the next clone re-runs the helper, but any worktree already-created with the OLD identity needs explicit fixing.

2. **Git filter-repo's `verveguy/projects` rule was too greedy.** During the history rewrite, the substitution rule mapped `verveguy/projects` → `handarbeit/projects` for org project URLs. But it also matched test fixtures like `https://github.com/users/verveguy/projects/5` (a USER project), producing `users/handarbeit/projects/5` which is broken (handarbeit is an org, not a user). Fabrik's Validate stage caught this and fixed it. If you ever do another text rewrite, scope rules tightly and test against the spec/test fixtures before push.

3. **Claude Code's `/plugin update` keys on the version field.** Bumping plugin source without bumping `plugin.json` version means users never see the update. PR #816 will automate this on every release; until then, edit + commit the version bump alongside any plugin/fabrik change.

4. **`docs/llms-full.txt` is a generated bundle.** If you touch `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, or `docs/positioning.md`, run `bash scripts/generate-llms-full.sh` and commit the regenerated bundle in the same PR. CI's `docs-drift` check enforces this.

5. **Spec discipline is real.** New features need a spec under `specs/<issue-number>-<slug>/spec.md`. Look at `specs/sub-issue-decomposition/spec.md` (older convention without number prefix) and `specs/827-postmortem-v0.0.66/spec.md` (with number prefix) for format. The number-prefix convention is the new standard.

6. **Issue #799 was permanently deleted by mistake during cleanup**, then re-created as #827 from cached body. Comments on the original are lost. Lesson: `deleteIssue` GraphQL mutation is irreversible — close issues, don't delete them, unless absolutely sure.

---

## 9. Common operations

### Status check
```bash
# What's open and where it sits
gh issue list --repo handarbeit/fabrik --state open
gh pr list --repo handarbeit/fabrik --state open

# Fabrik process state
ps aux | grep -E "[/ ]fabrik" | grep -v grep
```

### Cut a release
```bash
# 1. Author release notes at release-notes/v0.0.XX.md (must start with `# Fabrik v0.0.XX`)
# 2. Run:
scripts/cut-release.sh v0.0.XX
# Pre-flight + identity check + build + race-tested suite + commit + tag + push + watch workflow + verify author + file doc-update issue
```

### Restart Fabrik cleanly
```bash
# Kill any running instances
pkill -f "[/ ]fabrik($| --auto-upgrade)"

# Rebuild
go build -o fabrik .

# Restart in a screen/tmux session
./fabrik --auto-upgrade --yolo  # or per your preferred flags
```

### Test changes locally
```bash
go build ./... && echo "build OK"
go vet ./...
# Avoid the known engine test hang:
go test ./engine/ -timeout 30s -skip TestExtendTurns_PersistsAcrossMultipleStages
go test ./... -timeout 60s -skip TestExtendTurns_PersistsAcrossMultipleStages
```

### Open a PR as arbeithand
With the local credential helper in place, normal `git push` and `gh pr create` work. To force the helper explicitly on push:
```bash
PAT=$(grep '^FABRIK_TOKEN=' .env | cut -d= -f2-)
git -c credential.helper="!f() { echo username=arbeithand; echo password=$PAT; }; f" push -u origin <branch>
GH_TOKEN="$PAT" gh pr create --repo handarbeit/fabrik --base main --head <branch> --title "..." --body "..."
```

---

## 10. What's NOT done yet

- **#816** has not reached Validate. When it does, inspect Fabrik's implementation against the prototype that was at PR #809, and against the issue's open questions (testability, opt-out flag, jq dependency, etc.).
- **F-1** (CI schema validation) and **F-2** (verified-comment convention) from the post-mortem are not filed as issues yet.
- **GitHub Apps** for Copilot Code Review / Gemini may not be installed on `handarbeit` org — without them, every Fabrik-opened PR stalls at Review until ReviewWaitTimeout. Worth checking and installing.
- **Homebrew tap** deferred indefinitely.

---

## How to use this document in a fresh session

Start the session in `/Users/bpja/dev/fabrik`, then either:
- Drop the full path into the prompt: "Read `notes/oss-launch-handoff.md` and use it as the context for our work on the OSS launch."
- Or paste relevant sections directly into the prompt.

The fresh session will have `CLAUDE.md` auto-loaded (architecture + conventions). This document adds the OSS-launch context that isn't in `CLAUDE.md`.
