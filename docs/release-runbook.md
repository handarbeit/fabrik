# Fabrik Release Runbook

> **Audience.** A fresh Claude Code session that has been asked to cut a Fabrik release, diagnose a failed release, or recover from one published under the wrong identity. Self-contained: load this file and you have everything you need.

## TL;DR

To cut a release, **you write release notes; the script does everything else.**

1. Determine the version (next patch bump from `git describe --tags --abbrev=0`, or a version the user gave you).
2. Author `release-notes/<version>.md` (format below).
3. Run `scripts/cut-release.sh <version>`.
4. If exit code 0, report back: `✅ Release <version> published — release + announcement authored by @arbeithand. Doc-update issue #<n> filed at Specify.`
5. If non-zero, surface the error verbatim and consult the **Recovery** section below. Do not attempt to repair half-published state automatically.

Releases are user-triggered (the `/cut-release` slash command). You do not initiate one without an explicit operator request.

---

## Identity rules (non-negotiable)

**All release artifacts MUST be authored as `@arbeithand` (the bot).** The operator's default `gh auth` identity is `@verveguy`, which is wrong for releases. The script enforces this; do not bypass.

- **PAT**: `FABRIK_TOKEN` in `.env` at the repo root. This is an arbeithand-owned classic PAT.
- **Git commits**: `GIT_AUTHOR_NAME=arbeithand GIT_AUTHOR_EMAIL=handarbeit@handarbeit.io` (script sets this for the release-notes commit).
- **Tag push**: PAT-in-URL with credential helpers nuked — `git -c credential.helper= -c credential.https://github.com.helper= push https://x-access-token:${FABRIK_TOKEN}@github.com/handarbeit/fabrik.git <tag>`. Without nuking the helpers, the default `gh auth` identity wins and the push is attributed to verveguy.
- **GitHub release author + Discussions announcement author**: come from the GHA workflow, which uses the **repo secret `PUBLIC_REPO_RELEASE_TOKEN`** (not `GITHUB_TOKEN`). If this secret has been rotated to a non-arbeithand PAT, the release will publish under the wrong identity. The script catches this in step 7 and tells you to rotate the secret at https://github.com/handarbeit/fabrik/settings/secrets/actions/PUBLIC_REPO_RELEASE_TOKEN.

This identity bug recurred multiple times historically. Every recurrence root-caused to either (a) `PUBLIC_REPO_RELEASE_TOKEN` being wrong, or (b) someone pushed the tag without nuking credential helpers. The script's hard verification (step 7) is the canary.

---

## The single command

```bash
scripts/cut-release.sh v0.0.69
```

Flags:
- `--skip-tests` — skip `go test -race ./...`. Use only when the race-tested suite is known-green from a recent run (e.g. CI on the head-of-main commit just passed). Document the justification in the cut-release report.
- `--no-doc-issue` — skip filing the doc-update issue at the end. Rare; default is to file it.

---

## Authoring `release-notes/<version>.md`

This file is the source of truth for both the GitHub Release page and the Discussions announcement post. Path is required to match the tag: `release-notes/v0.0.69.md` for `v0.0.69`.

**Required heading**: exactly `# Fabrik v0.0.69` (the script validates the version match).

**Required section**: `## Summary` (the script validates this exists; the release workflow extracts the body of this section verbatim as the Discussions announcement post).

Schema:

```markdown
# Fabrik v0.0.69

## Summary

<This block IS the Discussions announcement body. Aim for 50-80 words for a
patch release, up to 150-250 words for a feature release with multiple themes.
Should read as standalone: someone scanning the Discussions feed should
immediately know what shipped and why it matters. Themed bullets work well for
multi-theme releases.>

## Features
- Description (#issue)

## Fixes
- Description (#issue)

## Improvements
- Description (#issue)

## Internal
- One-line summary of internal churn — do NOT enumerate refactors/tests/docs

## Upgrading

\```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
\```
```

**Summary section is what reaches users first.** Treat it as the elevator pitch. If the release has a headline theme (e.g. "post-Validate convergence is now atomic"), lead with it. Don't duplicate the Features/Fixes bullets in the Summary — those live below.

**Internal section**: one collapsed line. Do not enumerate every test addition, refactor, or doc fix.

**Drafting the notes**:
1. `git describe --tags --abbrev=0` — find the previous tag.
2. `git log <last-tag>..HEAD --oneline` — enumerate commits.
3. Skip merge commits and `Co-Authored-By` lines.
4. Group into Features / Fixes / Improvements / Internal. Omit empty sections.
5. For each user-facing change, link the issue: `(#nnn)`.
6. Write `## Summary` last, after you know what the themes actually are.

---

## What the script does (8 steps, in order)

Located at `scripts/cut-release.sh`. Each step exists because something broke once.

### 1. Pre-flight
- Must be on `main`.
- Working tree must be clean *except* for `release-notes/<version>.md` (added/modified) and `plugin/known_embedded_versions.go` (the script updates this itself in step 3).
- `git fetch origin main --tags`. Local main must be ff'd to origin (auto-pulls if behind, dies if diverged).
- Tag `<version>` must not exist locally or remotely.
- `release-notes/<version>.md` must exist, lead with `# Fabrik <version>`, and contain `## Summary`.

### 2. PAT identity check
- Reads `FABRIK_TOKEN` from `.env`.
- Aborts unless `gh api user` (with that token) returns `login: arbeithand`.

### 3. Build + record plugin hash
- `go build ./...` must succeed.
- Runs `go run ./tools/print-plugin-hash/` to compute the canonical hash of the embedded plugin tree.
- Appends the hash to `plugin/known_embedded_versions.go` if not already present. This list is consumed at runtime by `CheckPluginState` to distinguish a legitimate `.installed-version` from one written by the buggy v0.0.64 migration (which wrote a customised disk hash, breaking later customisation-detection logic). The list grows by one entry per release whose embedded plugin tree changed; identical embedded content reuses the same hash and adds nothing.

### 4. Race-tested suite
- `go test -race ./... > /tmp/cut-release-test.log`. On failure, last 40 lines of the log print to stderr and the script aborts.
- Skipped if `--skip-tests` was passed.

### 5. Commit release notes + plugin-version update
- Stages `release-notes/<version>.md` and `plugin/known_embedded_versions.go`.
- If nothing to commit (rare — both files already match), skips the commit and goes straight to tag. Otherwise commits as `arbeithand <handarbeit@handarbeit.io>` via `GIT_AUTHOR_*` and `GIT_COMMITTER_*` env vars, then verifies the resulting `%an <%ae>` matches.
- Pushes the commit to origin/main using PAT-in-URL with credential helpers nuked.

### 6. Tag + push
- Tags `HEAD` with `<version>`.
- Pushes the tag with the same PAT-in-URL + helper-nuking incantation.
- On failure, leaves the local tag in place and tells you to `git tag -d <version>` before retrying.

### 7. Watch the release workflow
- Polls `gh run list --workflow release.yml` up to 6 times (delays: 3, 6, 9, 12, 15, 18 seconds — total ~63s) for a `push` event on the version's ref.
- `gh run watch <run-id> --exit-status` — but does NOT trust the exit status alone. After watch returns, **explicitly re-queries** `gh run view --json conclusion --jq .conclusion`. Must equal `success`. On any other value: dumps `--log-failed` tail to stderr and aborts.

### 8. Identity verification (the canary)
- `gh api /repos/handarbeit/fabrik/releases/tags/<version> --jq .author.login` — must be `arbeithand`.
- GraphQL query for the most-recent matching Discussion's `author.login` — must be `arbeithand`.
- On mismatch: dies loudly with the rotation URL for `PUBLIC_REPO_RELEASE_TOKEN`. **This is the recurrence guard for the wrong-identity bug.**

### 9. (Optional) File doc-update issue
- Creates `Update docs for <version>` issue on handarbeit/fabrik with labels `documentation` and `fabrik:yolo`.
- Adds the issue to project #1 (Fabrik board) at `Status=Specify`.
- Skipped if `--no-doc-issue` was passed.

---

## What the release workflow does (`.github/workflows/release.yml`)

Triggered by `push` of any `v*.*.*` tag. Three jobs in one workflow:

1. **Verify tag is on main**: `git merge-base --is-ancestor <tag-commit> origin/main`. Tags that don't trace back to main are rejected.
2. **Verify `release-notes/<tag>.md` exists**: hard error if missing — file is required.
3. **`goreleaser release --release-notes release-notes/<tag>.md`** with `GITHUB_TOKEN=secrets.PUBLIC_REPO_RELEASE_TOKEN`. Builds darwin/linux × arm64/amd64 tarballs per `.goreleaser.yaml`, publishes the GitHub Release using the contents of the release-notes file.
4. **Post Discussions announcement** via GraphQL `createDiscussion`. Body is the `## Summary` section extracted from the release-notes file plus a link to the full release page. Posted to the `Announcements` category. Also uses `PUBLIC_REPO_RELEASE_TOKEN`.

If you ever need to change what GHA does on tag-push, this is the file.

---

## Plugin hash mechanics (why `known_embedded_versions.go` matters)

Fabrik's plugin tree (skills, workflows) is embedded in the binary via `//go:embed`. On first run, the binary extracts the embedded plugin into `.fabrik/plugin/` and writes a fingerprint to `.fabrik/plugin/.installed-version`. On subsequent runs, the binary compares the embedded fingerprint against the disk fingerprint to detect:
- Drift (operator customised the plugin → don't overwrite).
- Upgrade (new binary has a different embedded plugin → refresh).
- The historical v0.0.64 bug where the disk hash was a customised value, not an embedded one.

`KnownEmbeddedVersions` is the allow-list of every hash that has ever been an embedded-plugin fingerprint. `CheckPluginState` uses it to recognise legitimate disk hashes vs. corrupted v0.0.64-era values.

The release script appends the current build's plugin hash to this list **before** building and tagging, so the new binary already knows about its own hash. The `tools/print-plugin-hash/` binary is the canonical hasher.

If you ship a release whose embedded plugin changed but you forgot to update the list (impossible via the script, but possible if someone hand-rolls a release), upgrade detection on the next binary will misclassify the new disk fingerprint as a customisation.

---

## Verification checklist (what "shipped" actually means)

When the script exits 0, every one of the following is true. If you want to manually re-verify (e.g. someone disputes that the release actually happened):

1. **Tag exists on origin**: `git ls-remote --tags origin | grep <version>`.
2. **GitHub Release exists**: `gh release view <version> --repo handarbeit/fabrik`. Check `author: arbeithand`.
3. **Tarballs are attached** (4 of them: darwin-arm64, darwin-amd64, linux-arm64, linux-amd64) + `checksums.txt`.
4. **Discussions announcement exists**: open https://github.com/handarbeit/fabrik/discussions/categories/announcements — top post should be the new release, author `arbeithand`.
5. **Doc-update issue is on the project board at Specify** (unless `--no-doc-issue`).
6. **`plugin/known_embedded_versions.go` on main** contains the new hash.

---

## Recovery procedures

**The script does NOT auto-clean on failure.** It leaves the half-published state in place so the operator (you) can decide whether to scrub-and-retry or repair in place. Below are the recipes for the common failure modes.

### Wrong identity on release/announcement (step 7 fails)

This means `PUBLIC_REPO_RELEASE_TOKEN` repo secret is not an arbeithand PAT.

**Fix sequence**:
1. Rotate the secret at https://github.com/handarbeit/fabrik/settings/secrets/actions/PUBLIC_REPO_RELEASE_TOKEN. Use an arbeithand-owned PAT with `repo` + `discussion:write` scope.
2. Delete the bad release, announcement, and tag (commands below).
3. Re-run `scripts/cut-release.sh <version>`.

**Cleanup commands** (also at the bottom of the script for reference):

```bash
FABRIK_TOKEN=$(grep '^FABRIK_TOKEN=' .env | cut -d= -f2-)
VERSION=v0.0.69  # adjust

# Delete the Discussions announcement
DISC_ID=$(GH_TOKEN="$FABRIK_TOKEN" gh api graphql -f query='query {
    repository(owner: "handarbeit", name: "fabrik") {
      discussions(first: 5, orderBy: {field: CREATED_AT, direction: DESC}) {
        nodes { id title }
      }
    }
  }' --jq ".data.repository.discussions.nodes[] | select(.title | contains(\"$VERSION\")) | .id")
[ -n "$DISC_ID" ] && GH_TOKEN="$FABRIK_TOKEN" gh api graphql -f query="mutation { deleteDiscussion(input: {id: \"$DISC_ID\"}) { discussion { number } } }"

# Delete the release + tag
GH_TOKEN="$FABRIK_TOKEN" gh release delete "$VERSION" --repo handarbeit/fabrik --yes --cleanup-tag

# Delete the local tag (origin already cleaned by --cleanup-tag above)
git tag -d "$VERSION"
```

### Tag pushed but workflow failed (build error, test failure in CI, etc.)

`gh run view <run-id> --log-failed` and read the actual failure. The cleanup is the same as above (release will not have been created, but the tag exists on origin). After fixing the underlying issue, re-run the script.

### Tag push succeeded but `gh run watch` lost the run

Step 7 polls 6 times. If the workflow really hasn't started, check Actions UI for a queued/blocked run. If the workflow ran and finished while the script was sleeping, re-run identity verification manually:

```bash
gh api /repos/handarbeit/fabrik/releases/tags/v0.0.69 --jq .author.login   # should be arbeithand
```

### Release-notes commit pushed but tag push failed

The release-notes commit is now on main. Re-running the script will see the now-clean working tree, skip the commit step, and proceed to tag. If the failure was transient (network), re-running usually works. If it was non-transient (e.g. you discover the wrong version), revert the release-notes commit first.

### Local tag exists but origin doesn't have it

The script dies with `git tag -d <version>` as the hint. Run that, then retry.

### Working tree dirty with non-allowed files

The script's allow-list is exactly `release-notes/<version>.md` and `plugin/known_embedded_versions.go`. Anything else dirty (including untracked) aborts pre-flight. Stash or commit your other work first.

### Pre-existing `KnownEmbeddedVersions` entry

If the same hash is already in the list (no plugin change since last release), the script logs `hash already recorded` and proceeds. This is normal.

---

## Things that are NOT releases

- **`fabrik upgrade` / `--auto-upgrade`**: Fabrik checks GitHub Releases at poll cadence and self-restarts when a newer tag exists. Operator-side, not release-side. The release script's job ends at "release is on GitHub"; upgrade behavior is a runtime concern.
- **PR merges to main**: code lands on main throughout the day. A release packages a snapshot of main into a tagged, downloadable artifact. Merging a PR is not a release.
- **The doc-update issue**: filed at the end of every release to track follow-up doc work. Fabrik (with `fabrik:yolo`) processes it autonomously. Not part of the release artifact itself.

---

## Things you (the assistant) should never do during a release

- **Edit `scripts/cut-release.sh` to bypass a check.** Every guard exists because something failed once. If a guard is wrong, file an issue; don't reach around it.
- **Skip identity verification.** The wrong-identity bug recurs. The verification is the canary.
- **Auto-clean on script failure.** The script intentionally avoids destructive cleanup. Print the cleanup commands and ask the user how to proceed.
- **Use `gh` without `GH_TOKEN=$FABRIK_TOKEN` for release-related operations.** The default `gh auth` token is verveguy and will produce wrong-identity artifacts.
- **Initiate a release without the operator asking.** Releases are user-triggered. Read CLAUDE.md's "release identity (PAT)" memory rule if unsure — it codifies this.
- **Run release-notes drafting in parallel with other work.** Author the notes, get the operator's eye on them if anything is non-obvious, then run the script.

---

## File references (canonical paths)

- **Script**: `scripts/cut-release.sh`
- **Workflow**: `.github/workflows/release.yml`
- **goreleaser config**: `.goreleaser.yaml`
- **Plugin hasher**: `tools/print-plugin-hash/main.go`
- **Known-versions allow-list**: `plugin/known_embedded_versions.go`
- **Release notes**: `release-notes/v<X.Y.Z>.md` (per-version, archived in-tree)
- **Slash-command skill** (what `/cut-release` invokes): `.claude/skills/cut-release/SKILL.md`
- **Optional notes-drafting helper**: `scripts/draft-release-notes.sh` (AI-assisted draft to a scratch file; not used by the cut-release flow itself)

## Recent releases to study

Past `release-notes/v0.0.*.md` files are the best examples of the notes voice and structure. Particularly worth reading:

- `release-notes/v0.0.68.md` — multi-theme release (post-Validate convergence + auto-merge fallback + plugin customisation regression). Example of a Summary block with three themed paragraphs.

Older releases are concise patch bumps and show the single-theme Summary format.

---

## Glossary

- **arbeithand / @arbeithand**: the bot identity that owns release artifacts. PAT lives in `FABRIK_TOKEN`. Email `handarbeit@handarbeit.io`.
- **handarbeit**: the GitHub *organization* that owns the `fabrik` repo and project board. Note: it is an *organization*, not a user — GraphQL queries against it use `organization(login: "handarbeit")`, not `user(login: ...)`.
- **verveguy**: the operator's personal GitHub identity. Default `gh auth` user. **Must not** appear on any release artifact.
- **Project #1**: the handarbeit/fabrik project board (Fabrik's own SDLC pipeline). Status field has `Specify`, `Research`, `Plan`, `Implement`, `Review`, `Validate`, `Done` columns. Doc-update issues land at `Specify`.
- **Plugin tree**: `plugin/fabrik-workflows/skills/` — the embedded source-of-truth for all built-in Fabrik skills. Bumped via the plugin-version mechanism, fingerprinted via `print-plugin-hash`.
- **`PUBLIC_REPO_RELEASE_TOKEN`**: the repo secret used by the release workflow's GHA jobs to author the release + announcement. Independent of `FABRIK_TOKEN` (which is operator-side). Must also be an arbeithand PAT.
