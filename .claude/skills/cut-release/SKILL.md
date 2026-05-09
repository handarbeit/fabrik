---
name: cut-release
description: Cut a new Fabrik release — gathers changes since last tag, writes release notes, commits, tags, and pushes
user_invocable: true
---

# Cut Release

Cut a new Fabrik release with curated release notes.

## Usage

Invoked as `/cut-release` or `/cut-release v0.0.12`. If no version is provided, suggest the next patch bump from the current latest tag.

## Steps

### 1. Pre-flight checks

- **Verify on main branch.** Run `git branch --show-current`. If the output is anything other than `main` (including empty output, which indicates a detached HEAD), check out main with `git checkout main` before proceeding. **Do NOT skip this** — this skill has caused off-branch tag pushes (v0.0.46) and detached-HEAD near-misses (v0.0.49) when assumed-on-main turned out to be wrong.
- Ensure working tree is clean (`git status --porcelain`). If dirty, stop and tell the user.
- Pull latest main: `git pull origin main`.
- Run `go build ./...` and `go test -race ./...`. If either fails, stop.

### 2. Determine version

- Find the latest tag: `git describe --tags --abbrev=0`
- If a version argument was provided, validate it:
  - Must be valid semver with `v` prefix (e.g. `v0.0.12`)
  - Must be greater than the current latest tag
- If no version provided, suggest next patch bump (e.g. `v0.0.11` → `v0.0.12`) and confirm with user

### 3. Gather changes

- Run `git log <last-tag>..HEAD --oneline` to get all commits since the last release
- Group changes into categories:
  - **Features** — new capabilities
  - **Fixes** — bug fixes
  - **Improvements** — enhancements to existing features
  - **Internal** — refactoring, test improvements, CI changes (summarize briefly, don't enumerate)
- Ignore merge commits and `Co-Authored-By` lines
- Focus on user-facing changes; collapse internal churn into a single line

### 4. Write release notes

Write `release-notes.md` in the repo root with this structure:

```markdown
# Fabrik <version>

## Features
- Description of feature (#issue)

## Fixes
- Description of fix (#issue)

## Improvements
- Description of improvement (#issue)

## Internal
- Summary of internal changes

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

Omit any empty category sections. Keep descriptions concise — one line each.

### 5. Commit, tag, and push

- `git add release-notes.md`
- Commit with message: `Release notes for <version>`
- Create tag: `git tag <version>`
- Push immediately: `git push origin main <version>`

Do NOT ask for confirmation before pushing. If builds and tests passed in step 1, proceed.

### 6. Verify the release workflow succeeded

**This step is not optional.** The release workflow does multiple things (build binaries, publish to handarbeit/fabrik, post Discussion announcement). Any of these can fail silently. A workflow that shows `completed` is NOT the same as `success` — only the **conclusion** tells you whether it actually worked.

```bash
# Get the run ID for the v<version> tag push
RUN_ID=$(gh run list --workflow release.yml --limit 1 -R handarbeit/fabrik \
  --json databaseId,headBranch --jq ".[] | select(.headBranch==\"v<version>\") | .databaseId")

# Wait for it to finish
gh run watch $RUN_ID -R handarbeit/fabrik

# CRITICAL: check the conclusion (not just the status)
CONCLUSION=$(gh run view $RUN_ID -R handarbeit/fabrik --json conclusion --jq .conclusion)
if [ "$CONCLUSION" != "success" ]; then
  echo "Release workflow FAILED with conclusion: $CONCLUSION"
  gh run view $RUN_ID -R handarbeit/fabrik --log-failed | tail -50
  # STOP — do not proceed. Tell the user the release failed and show the failing step.
fi
```

Also verify the release actually exists on the public repo:
```bash
gh release view v<version> --repo handarbeit/fabrik --json tagName
```

Report the result explicitly:
- ✅ "Release v<version> published to handarbeit/fabrik (run #<id>) — all steps green"
- ❌ "Release v<version> workflow FAILED at step X — <reason>" (and show the failing log lines)

Never report a release as successful unless `conclusion == "success"` AND the release exists on handarbeit/fabrik.

### 7. File documentation update issue

After a successful push, create a GitHub issue to update user documentation and move it to Specify:

```bash
# Create the issue
gh issue create -R handarbeit/fabrik \
  --title "Update docs for v<version>" \
  --label "documentation" \
  --label "fabrik:yolo" \
  --body "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v<version>.

## Changes to document

<paste the release notes summary here>

## Scope

- USER_GUIDE.md — update any sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current"

# Move the issue to Specify on the Fabrik PM board (org project #1)
# First, get the issue's project item ID, then update its status
```

Use `gh project item-add` and `gh project item-edit` to place the issue in the Specify column on the Fabrik PM project board. The `fabrik:yolo` label ensures it flows through the pipeline automatically.

## Important

- Never skip the build/test step — broken releases are worse than delayed releases
- Never force-push tags — if a tag exists, stop and ask the user
- The GitHub Actions workflow (`.github/workflows/release.yml`) uses `--release-notes release-notes.md` so the file must be committed before the tag push triggers the build
- Always file the doc update issue — documentation drift is how users get confused
