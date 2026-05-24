---
name: cut-release
description: Cut a new Fabrik release — gathers changes since last tag, writes release notes, delegates publication to scripts/cut-release.sh
user_invocable: true
---

# Cut Release

Cut a new Fabrik release. Your job is to **author the release notes**; `scripts/cut-release.sh` does everything else (commit, tag, push as @arbeithand, watch workflow, verify identity, file doc-update issue).

## Usage

Invoked as `/cut-release` or `/cut-release v0.0.67`. If no version is provided, suggest the next patch bump from the current latest tag.

## Steps

### 1. Determine version

- Find the latest tag: `git describe --tags --abbrev=0`
- If a version arg was provided, validate semver with `v` prefix and that it's greater than the latest tag.
- If no version arg, suggest the next patch bump and proceed.

### 2. Author release notes

This is the part requiring judgment — do not delegate it.

- `git log <last-tag>..HEAD --oneline` to enumerate commits since the last release.
- Group user-facing changes by category: **Features**, **Fixes**, **Improvements**, **Internal**. Omit empty sections.
- Ignore merge commits and `Co-Authored-By` lines.
- Collapse internal churn into a single line under **Internal**. Don't enumerate refactors / test additions.
- Write the notes to `release-notes/<version>.md` (one file per release, archived in-tree). The script copies this to the repo-root `release-notes.md` automatically for goreleaser's input.

Use this schema:

```markdown
# Fabrik <version>

## Summary

<1-3 sentences. Punchy headline themes only. This block is what the Discussions
announcement post will contain — everything else lives on the GitHub Release page.>

## Features
- Description (#issue)

## Fixes
- Description (#issue)

## Improvements
- Description (#issue)

## Internal
- One-line summary of internal churn

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

**Both the `# Fabrik <version>` heading and the `## Summary` section are required.** The script validates both and refuses to proceed on either failure. The Summary should be tight (1–3 sentences) — that's literally the announcement post body, so don't pad it with detail that already lives below in Features/Fixes.

### 3. Run the publish script

```bash
scripts/cut-release.sh v0.0.67
```

The script handles, in order:
1. **Pre-flight** — must be on main, working tree clean (uncommitted `release-notes.md` allowed), ff'd to origin, tag not already taken locally or on origin.
2. **PAT identity check** — reads `FABRIK_TOKEN` from `.env`; aborts unless it belongs to `@arbeithand`.
3. `go build ./...` and `go test -race ./...`. Skippable only via `--skip-tests` (last resort).
4. Commits `release-notes.md` with `GIT_AUTHOR_*` env vars so the commit is attributed to `arbeithand <handarbeit@handarbeit.io>`.
5. Pushes the release-notes commit and the tag with `credential.helper=` and `credential.https://github.com.helper=` both nuked, plus PAT-in-URL — the exact incantation needed on this machine to push as the bot rather than the default `gh auth` identity.
6. Watches the release workflow and verifies `conclusion == "success"`.
7. **Hard identity verification** — fetches `release.author.login` and the latest Discussion's author. **Aborts loudly** if either is not `arbeithand`, with a pointer to rotate the `PUBLIC_REPO_RELEASE_TOKEN` repo secret.
8. Files the doc-update issue and adds it to project #1 at Status=Specify with `fabrik:yolo` — all authored by `arbeithand` via the PAT.

Flags:
- `--skip-tests` — skip the race-tested suite (use only if it's already known-green from a recent run).
- `--no-doc-issue` — skip the doc-update issue creation.

### 4. Report back

If the script exits 0, the release is live and verified. Report:
- "✅ Release v0.0.67 published to handarbeit/fabrik — release + announcement authored by @arbeithand. Doc-update issue #<n> filed at Specify."

If the script exits non-zero, surface the script's last error message verbatim. Do NOT attempt to repair the half-published state automatically — the script intentionally avoids destructive cleanup. The cleanup commands are commented at the bottom of the script for reference; relay them to the user and ask how they want to proceed.

## Important

- **Never edit `scripts/cut-release.sh` to bypass a check** — every guard exists because something failed once. The bot-identity guards in particular took three deletion-and-republish cycles to diagnose (the workflow secret `PUBLIC_REPO_RELEASE_TOKEN` is the root cause when the verveguy-attribution bug recurs).
- **The script is bot-only** — only someone with the `arbeithand` PAT (i.e., the handarbeit/fabrik publisher) can use it. Fork maintainers cutting their own fork releases would need to parameterize the constants at the top.
- **Authoring release notes is the AI's job; everything else is the script's.** Don't reinvent the publication mechanics inline — past attempts at that produced wrong-identity releases that had to be cleaned up.
