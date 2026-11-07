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

### 0. Run the release gate (before the cut)

The live e2e gate runs for hours, so start it on its own and cut afterwards. `scripts/cut-release.sh` then only checks the coverage ledger (`run.sh coverage`) and resumes whatever is missing.

**Ready?** Every non-`watch` issue on the milestone is closed and its PR merged: `gh issue list --repo handarbeit/fabrik --milestone <m> --state open`. Only `watch` items may stay open; they are handled at the cut (see step 4).

**Matrix.** Use `E2E_MATRIX=full` when the release changes the auth layer, the landing path, test classification, or **event ingestion** (webhook transports). Otherwise leave it unset (the sparse default).

**Daemons stay up.** Don't stop the dev daemon or any other daemon for the gate. With webhooks they barely compete for GraphQL, and CI runners live on the dedicated Linux box. Ask before stopping any daemon.

**Launch.** Run from `~/dev/fabrik-release`, never `~/dev/fabrik`: `cut-release.sh` fast-forwards itself, so a stale checkout runs stale code. Run it in tmux, with logs in a durable directory (not `/tmp`):

```bash
cd ~/dev/fabrik-release && git fetch origin && git merge --ff-only origin/main
SHA=$(git rev-parse origin/main)
L=~/dev/.scratch/fabrik/$(date +%F)-cut-<NNNN>; mkdir -p $L
cat > $L/gate.sh <<EOF
#!/bin/zsh
cd ~/dev/fabrik-release
export E2E_MATRIX=full E2E_SKIP_TOKEN_CHECK=1
export E2E_BEDS=\$HOME/dev/fabrik-test,\$HOME/dev/fabrik-test-2
export E2E_BED_REF=$SHA
export E2E_COVERAGE_DIR=\$HOME/dev/fabrik/.e2e-coverage
scripts/e2e/run.sh --clean --resume 2>&1 | tee -a $L/gate.log
echo "GATE EXIT: \${pipestatus[1]}" | tee -a $L/gate.log
EOF
chmod +x $L/gate.sh
tmux new-session -d -s fabrik-gate-<NNNN> "$L/gate.sh; exec zsh"
```

- `E2E_BEDS`: both test beds run concurrently. Bed 2 is gamma/delta, board #7, App `fabrik-bed-2`.
- `E2E_SKIP_TOKEN_CHECK=1` is required. The dev daemon runs as the `fabrik-dev-daemon` App, and the competing-token check (#1684) wrongly flags it.
- `E2E_COVERAGE_DIR` keeps one ledger across gate runs and the cut, whichever checkout ran them.
- `E2E_BED_REF` pins the engine SHA under test. If a tests-only fix (`tests/e2e/`, `tests/gate/`, `scripts/e2e/`) merges mid-gate, keep the pin: the ledger tolerates those paths, so recorded coverage still counts. An engine change invalidates the ledger and needs a new SHA.

**Monitor.** `tail -f $L/gate.log`. Exit codes: 3 budget exhausted, 4 preflight, 5 pre-gate, 6 post-suite watchdog, 7 precondition, 8 coverage incomplete. An interrupted gate is resumed by re-running `gate.sh`; `--resume` runs only the missing (test, leg) pairs.

**Cut.** Once coverage is complete, run step 3 from `~/dev/fabrik-release` with the **same** `E2E_*` exports (above all `E2E_COVERAGE_DIR`, `E2E_BED_REF` and `E2E_MATRIX`). Otherwise `run.sh coverage` looks at a different ledger or plan, and the release notes' coverage line names the wrong matrix.

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
- Write the notes to `release-notes/<version>.md` (one file per release, archived in-tree). The release workflow reads this path directly via `${{ github.ref_name }}` — there is no repo-root `release-notes.md` and you should not create one (it is `.gitignore`d).

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

**Both the `# Fabrik <version>` heading and the `## Summary` section are required.** The script validates both and refuses to proceed on either failure.

**Sizing the Summary.** This block IS the Discussions announcement body. Aim for somewhere between a short paragraph (50–80 words for a patch release) and a few themed bullets (~150–250 words for a release with multiple headline features). It should read as a standalone announcement — someone scanning the Discussions feed should immediately understand what shipped and roughly what it does.

What to include:
- The headline theme(s) — what this release is *about*. For a multi-feature release, list 2–4 themed bullets.
- Enough context that each theme is meaningful on its own (one sentence of "what" + one of "why it matters" is usually right per theme).

What NOT to include:
- The full Features/Fixes/Improvements bullets that already live below — the announcement links to the full release page for that.
- Internal/test/refactor details.
- Implementation specifics (issue numbers belong in the body sections, not the Summary).

A one-line tagline is too terse. A duplicate of the body sections is too long. Aim for "the elevator pitch."

### 3. Run the publish script

```bash
scripts/cut-release.sh v0.0.67
```

The script handles, in order:
1. **Pre-flight** — on main; ff'd to origin/main; tag `<version>` not already taken locally or on origin; `release-notes/<version>.md` exists, heading is exactly `# Fabrik <version>`, and contains a `## Summary` section. Working tree must be clean except for `release-notes/<version>.md` (either untracked or modified) — any other dirty file aborts.
2. **PAT identity check** — reads `FABRIK_TOKEN` from `.env`; aborts unless `gh api user` resolves to `@arbeithand`.
3. `go build ./...`. No local `go test -race ./...`: CI runs it on every PR (removed in v0.0.84).
4. Stages `release-notes/<version>.md` and commits it with `GIT_AUTHOR_*` + `GIT_COMMITTER_*` env vars so the commit is `arbeithand <handarbeit@handarbeit.io>`. Verifies the resulting `%an <%ae>` matches; aborts otherwise.
5. Pushes the release-notes commit, then tags and pushes the tag — both with `credential.helper=` and `credential.https://github.com.helper=` nuked, plus PAT-in-URL. This is the exact incantation needed on this machine to push as the bot rather than the default `gh auth` identity.
6. Polls for the release workflow run, watches it, and **explicitly re-checks** `conclusion == "success"` via `gh run view` (the `--exit-status` flag alone is not trusted).
7. **Hard identity verification** — fetches `release.author.login` and the latest Discussion's `author.login`. **Aborts loudly** if either is not `arbeithand`, with a pointer to rotate the `PUBLIC_REPO_RELEASE_TOKEN` repo secret (root cause of every wrong-identity recurrence so far).
8. Files the doc-update issue and adds it to project #1 at Status=Specify with `fabrik:yolo` — all authored by `arbeithand` via the PAT.

Flags:
- `--skip-tests` — accepted and ignored (the local race suite was removed).
- `--no-doc-issue` — skip the doc-update issue creation.

### 4. Report back

If the script exits 0, the release is live and verified. Report:
- "✅ Release v0.0.67 published to handarbeit/fabrik — release + announcement authored by @arbeithand. Doc-update issue #<n> filed at Specify."

After a successful cut:
- Comment on each open `watch` issue on the milestone: "quiet as of <version>" with the evidence, or the action taken. Move it to the next milestone if it stays open.
- Close each community report the release fixes **by hand**, with a comment naming the release, and move it to Shipped on the triage board (see CLAUDE.md "Handling Community Bug Reports").
- In `~/dev/fabrik`, delete any local `release-notes/<version>.md` draft before fast-forwarding main, because the committed copy would otherwise conflict.

If the script exits non-zero, surface the script's last error message verbatim. Do NOT attempt to repair the half-published state automatically — the script intentionally avoids destructive cleanup. The cleanup commands are commented at the bottom of the script for reference; relay them to the user and ask how they want to proceed.

## Important

- **Never edit `scripts/cut-release.sh` to bypass a check** — every guard exists because something failed once. The bot-identity guards in particular took three deletion-and-republish cycles to diagnose (the workflow secret `PUBLIC_REPO_RELEASE_TOKEN` is the root cause when the wrong-identity attribution bug recurs).
- **The script is bot-only** — only someone with the `arbeithand` PAT (i.e., the handarbeit/fabrik publisher) can use it. Fork maintainers cutting their own fork releases would need to parameterize the constants at the top.
- **Authoring release notes is the AI's job; everything else is the script's.** Don't reinvent the publication mechanics inline — past attempts at that produced wrong-identity releases that had to be cleaned up.
