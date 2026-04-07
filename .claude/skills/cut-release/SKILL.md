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

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*<os>_<arch>*' -O - | tar xz
\```
```

Omit any empty category sections. Keep descriptions concise — one line each.

### 5. Commit and tag

- `git add release-notes.md`
- Commit with message: `Release notes for <version>`
- Create tag: `git tag <version>`

### 6. Push

- Show the user the release notes and ask for confirmation before pushing
- Push: `git push origin main <version>`
- Report the GitHub Actions release URL: `gh run list --limit 1 -R handarbeit/fabrik`

## Important

- Never skip the build/test step — broken releases are worse than delayed releases
- Never force-push tags — if a tag exists, stop and ask the user
- The GitHub Actions workflow (`.github/workflows/release.yml`) uses `--release-notes release-notes.md` so the file must be committed before the tag push triggers the build
