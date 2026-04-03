# ADR 011: Context Files as Claude Context Delivery Mechanism

## Status

Accepted

## Context

Fabrik invokes Claude for each pipeline stage by building a prompt that includes the issue body, prior comments, and stage instructions. As issues accumulate discussion — user answers, prior stage outputs, comment processing results — the inline prompt grows large and increasingly redundant.

There is also a blind-spot problem: when a user answers a question via a comment, `processComments` adds a rocket reaction and incorporates the answer. But when the stage re-invokes via `processItem`, `buildPrompt` only includes *new* (un-rocketed) comments. The user's answer is invisible to Claude because it was already processed.

## Decision

Before each Claude invocation (both stage runs and comment processing), Fabrik writes context files to `.fabrik/` in the worktree:

- `.fabrik/issue.md` — the issue body (the spec); always written
- `.fabrik/stage-{Name}.md` — the body of the most recent Fabrik stage comment for each relevant stage
- `.fabrik/pr-description.md` — the linked PR's description, for `post_to_pr` stage invocations only

**Scoping rules:**
- For stage invocations: write context for stages with `Order < currentStage.Order` (prior stages only). This supports rewinding — if an issue goes back to Plan, Claude only sees Specify and Research context.
- For comment processing: write context for all stages including the current one. Claude needs the current stage comment to build upon.

**Ordering:** Context files are written after any `git stash push -u` operation (for read-only stages) so they are present when Claude runs but not captured in the stash.

**Git exclusion:** `.fabrik/` is excluded from git tracking via `.git/info/exclude` (per-worktree, not `.gitignore`). This is written idempotently on every `EnsureWorktree` call.

**Error handling:** Context file write failures are non-fatal. Claude can still run without the files; the engine logs a warning and continues.

**Phased rollout:** Context files are shipped and validated first. Removing the inline fallback from `buildPrompt` / `buildCommentReviewPrompt` is deferred to a follow-up issue — confirm Claude reads the files reliably before removing the inline content.

## Alternatives Considered

**Stuffing everything into the inline prompt:** The current approach. Works but wastes tokens, grows unboundedly, and creates the processed-comment blind-spot described above.

**Passing content as tool definitions or system messages:** More complex to implement and no advantage over files that Claude can simply read with the Read tool.

**Removing inline context immediately:** Risky without validation. Files are a new mechanism; Claude's reliability in reading them should be confirmed before the fallback is removed.

## Consequences

- Claude can read prior stage outputs on demand without the engine predicting what it needs.
- The processed-comment blind-spot is eliminated: processed comments' effects are reflected in `.fabrik/stage-{Name}.md`, so stage retries see the incorporated answer.
- Context file content is always fresh (overwritten before each invocation) — no stale data risk.
- The inline prompt still contains the issue body and prior discussion during the transition period.
- Stage skills must instruct Claude to read `.fabrik/issue.md` and `.fabrik/stage-{Name}.md` at the start of each stage.
- Future contributors must keep `writeContextFiles` in sync with `formatOutputComment` in `engine/pr.go` (the header format used to identify stage comments).
