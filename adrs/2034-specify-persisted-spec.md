# ADR 2034: Specify persists its spec via an engine-written projection

## Status

Accepted

## Context

The Specify stage ran `read_only: true`, so the spec existed only in the issue body. Once Plan consumed it there was no committed spec: nothing to review in the PR beside the code and nothing discoverable in the repo afterwards. Issue #2034 (restating the proposal in #1630) asks Specify to author in Spec Kit's content structure and to persist the result as `specs/<issue>-<slug>/spec.md` on the issue branch.

The engine has two Specify entry points with different worktree behavior. Stage dispatch (`finalizeStageOutcome`) stashes, pops, may `commitWIP` (not for read-only stages) and pushes. Clarification rounds (`processCommentsClassified` / `publishCommentOutput`) do none of that: no stash, no commit, no push. The requirements need a write on **every** round that updates the body — including a round that ends in `FABRIK_BLOCKED_ON_INPUT` — with a slug locked at first commit, `## Open Questions` stripped, and no empty commits.

## Decision

1. **The engine writes the file, not Claude.** Both paths already hold the exact canonical text (`extractUpdatedBody`) right after `UpdateIssueBody`. `persistSpec` (`engine/spec_persist.go`) projects it to disk and commits it. Specify's allowed tools are unchanged (no `Write`, no git), so the stage gains no write reach.
2. **Keep `read_only: true`; add an opt-in `persist_spec: true` stage field.** `read_only` keeps describing the worker's posture (stash/restore, no `commitWIP`, no boundary rewrite). `persist_spec` names the one engine-performed write. Setting `read_only: false` was rejected: it drops the stash protection and enables `commitWIP`, which would commit a blocked round as `chore: partial Specify stage progress (incomplete)` using `git add -A` — not the `add`/`update` commit contract, and able to sweep unrelated dirty files in. Special-casing the stage *name* in the engine was rejected as less explicit than config.
3. **Opt-in field, not default-on for every config.** Custom or older stage YAMLs keep today's behavior. The drift check (`stages/drift.go`) reports a missing top-level key whose embedded default is not a no-op, so existing deployments are told at startup and `fabrik refresh-stages --apply` adds it. An old `specify.yaml` therefore produces no file rather than tool denials and a pause.
4. **Slug lock = the filesystem.** An existing `specs/<N>-*/` directory is reused only if its `spec.md` is absent, uncommitted, or was added by a `docs(spec): ` commit — a repo's own hand-written spec with the same number is skipped and never overwritten (if the derived path is itself foreign, nothing is persisted); otherwise the slug is derived once from the title (lowercase ASCII, ≤ 50 chars, `spec` fallback). `itemstate` is in-memory, so this is the only restart-safe lock; the directory is never renamed.
5. **Pathspec-scoped commit.** `git add -- <path>` then `git commit -- <path>` so other dirty or staged worktree state is never captured or misattributed. A projection identical to the file as committed in HEAD commits nothing; a file that matches on disk but is untracked or dirty (an earlier commit failed) is committed on the next round.
6. **Push.** The stage path rides the existing post-run push. The comment path has none, so `publishCommentOutput` pushes (`pushBranchUnlessQueued`) only when a spec commit was created.
7. **#921 interaction.** Every Specify branch now carries a spec commit, which would make `commitsAheadOfBase` never zero and stop fully-delegated coordinators self-healing to Done. `commitsAheadOfBase` now takes the issue number and does not count a commit whose only changes are `specs/<N>-*/spec.md` (`isSpecOnlyCommit`). It parses `git log --name-only` rather than using an exclude pathspec on `rev-list`, because a pathspec-limited `rev-list` silently drops empty commits, which the original semantics counted.
8. **`FABRIK_NO_WORK_NEEDED` writes no file** — the item goes to Done with no PR, so a committed spec would be orphaned.
9. **The spec template is the one downstream projects already use, inlined verbatim in the skill.** The shipped default promotes the Spec Kit feature-specification format (`# Feature Specification:` header fields, `## Background`, `## User Scenarios & Testing`, `## Requirements` with `FR-NNN`, `## Success Criteria` with `SC-NNN`, `## Assumptions`, optional `## Out of Scope`, `## Open Questions`, `## Source References`) rather than inventing a new shape, so a project switching to stock Fabrik keeps producing specs identical in shape to its existing ones. `fabrik-specify` carries the single canonical copy (no pointer to `.specify/` or an example path — managed repos may have neither); `fabrik-specify-comment` refers to it. `**Status**` is `Draft` while questions remain and `Specified` once none do, and both skills maintain it, so the committed file carries the same value. `## Open Questions` is emitted as exactly that heading (without the template's italic hint) because `stripOpenQuestions` matches the heading text; it removes the section up to the next `## ` heading, so a following `## Source References` is kept. The engine projection is unchanged.
10. **No new live e2e test.** Everything is reachable in the sim (`tests/sim/specify_persist_spec_test.go`, which configures Specify explicitly because the sim bed never sets `ReadOnly`) and engine tests; the live release gate's cost is not justified, and no registry entry is needed.

## Consequences

- The spec is reviewable in the PR diff and persists in the repo; the issue body stays canonical and the file is a one-way projection, never read back.
- Behavior no longer depends on the model following instructions, and needs no new tool permissions.
- New engine code in two places plus a slug rule in Go.
- The three downstream forks that replace the `fabrik-specify` skill see `fabrik upgrade` report a customization until they reconcile or use `--force`; this belongs in the release notes.
- Issues already past Specify are not backfilled.
