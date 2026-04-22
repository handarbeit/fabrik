# ADR 028: Merge-Conflict Gate and Rebase Reinvoke

**Status**: Accepted
**Date**: 2026-04-21

## Context

The CI gate (ADR 027) introduced a post-completion catch-up window in which the engine polls CI checks on the linked PR and re-invokes the stage agent to fix CI failures. During that window, a second PR on the same base branch may merge, moving `origin/main` forward. If the new base conflicts with this branch, GitHub flips the PR's `mergeable` flag to `false` — but neither the CI gate nor the review gate notices:

- The CI gate evaluates `FetchCheckRuns` against the PR **head SHA**. GitHub continues to run workflows on the branch commit regardless of merge state, so check runs remain green.
- The review gate only reacts to new inline review thread comments.
- The merge guard (`attemptMergeOnValidate`) has the right check (GitHub's `mergeable` field) but only fires in catch-up Phase 2, after CI has already passed — which can take 10+ minutes if CI is pending.

The observed failure mode (issue #654 on the develop project): after Validate completed, a different PR merged an `ADR-054.md` on `main`. Our branch had also added `ADR-054.md`. The branch now conflicted with main, but CI on the branch head still passed. For 30+ minutes the engine spun through re-invokes (CI-await polls and review-nit responses) that posted near-empty Fabrik comments while the real blocker — an unmergeable branch — went undetected. The stage eventually paused on a retry limit; the rebase only happened after human unpause triggered the comment-review path in Validate.

The gap is structural: Phase 1 of the catch-up loop has no sense of mergeability. Adding one before the CI gate preempts the wasteful polling.

## Decision

Introduce a third prong of the catch-up loop Phase 1: a merge-conflict gate that runs **before** the CI gate and, on a confirmed conflict, dispatches a rebase re-invocation of the stage agent.

### Prong 3 — Merge-Conflict Gate and Rebase Reinvoke

Controlled by the same `wait_for_ci: true` opt-in as the CI gate — these are the stages that enter the post-complete catch-up window in the first place, so the merge gate has no other stages to guard.

`checkMergeabilityGate(board, item, stage)` returns `(blocked, conflict bool)`:

- **Clear** `(false, false)`: stage does not opt in; no linked PR; `mergeable == true`; or transient API error (defer to next poll). Any stale `fabrik:rebase-needed` label is removed.
- **Blocked, not conflict** `(true, false)`: GitHub returned `mergeable == null` (not yet computed). No label applied — mirroring the CI gate's R10c rule that transient unknown states must not churn labels. Caller skips to the next item; the next poll re-evaluates with a definite answer.
- **Conflict** `(true, true)`: `mergeable == false`. `fabrik:rebase-needed` is applied idempotently. Caller dispatches the rebase reinvoke or pauses on the cycle limit.

Two REST calls: `FetchLinkedPR` (already cached on the client) for the PR number, then `FetchPRMergeable` (new — `/pulls/{number}`) for the single-PR `mergeable` field. The list endpoint used by `FetchLinkedPR` does not return `mergeable`, so a second targeted call is unavoidable; the cost is an extra REST call only for `wait_for_ci: true` stages in their post-complete window.

### Rebase Reinvoke

Mirrors `dispatchCIFixReinvoke` / `dispatchReviewReinvoke`:

1. **inFlight guard:** if a rebase goroutine from a previous poll is still running for this item, skip dispatch entirely (no cycle-limit check).
2. **Cycle check:** if `rebaseCycleCount[stageKey] >= MaxRebaseCycles`, pause with `pauseForRebaseCycleLimit`.
3. Otherwise: increment cycle count, call `dispatchRebaseReinvoke`.

`dispatchRebaseReinvoke` spawns a goroutine that:

- Marks `inFlight`; acquires semaphore; calls `ensureRepoReady`.
- Resolves the base branch via `baseBranchForItem` (for the instruction text).
- Calls `buildRebaseComment` to construct a synthetic `gh.Comment` (`DatabaseID: 0`) instructing the stage agent to `git fetch origin <base> && git rebase origin/<base>`, resolve conflicts conservatively (never drop code from the base), watch for semantic collisions (two PRs claiming the same ADR number, migration slot, etc.), run the project's build + tests, and force-push with `--force-with-lease`. It explicitly tells the agent not to emit `FABRIK_STAGE_COMPLETE`.
- Calls `processComments` with the synthetic comment and the `rebase_skill` (falls back to `comment_skill` when unset).
- Emits `JobStartedEvent` / `JobCompletedEvent` for the TUI.

The `DatabaseID: 0` guard skips 👀 / 🚀 reactions for synthetic comments — same pattern as the CI-fix and review-reinvoke paths.

### Cycle Limit: Default 3 (Lower Than Review/CI)

`MaxRebaseCycles` defaults to 3 versus 5 for review and CI. Rebase either works in one shot or needs human judgment. Three attempts is enough to survive a transient network blip during `git push`; beyond that, repeat failures almost always mean a semantic conflict that no automated rebase can resolve safely (and where additional attempts burn tokens without making progress).

Configurable via `--max-rebase-cycles` flag and `FABRIK_MAX_REBASE_CYCLES` env var, both following the same pattern as the other cycle limits.

### Label Left In Place on Pause

When `pauseForRebaseCycleLimit` fires, `fabrik:rebase-needed` is intentionally **not** removed alongside `fabrik:paused` and `fabrik:awaiting-input`. The human operator needs to see the reason Fabrik stopped. CI-await has the same property: `fabrik:awaiting-ci` is removed when the gate times out because the state it signals (CI failure) no longer cleanly describes the pause (which is a timeout, not a failure). Rebase cycle-limit pause describes a mergeability conflict, which remains true after the pause, so the label stays.

### Ordering: Before the CI Gate

The merge-conflict gate runs before the CI gate in Phase 1. A PR that cannot merge has no reason to wait for CI:

- Spinning on CI-await while the branch is unmergeable burns tokens on re-invokes that cannot possibly succeed.
- Claude on CI-fix reinvoke cannot productively act on a branch that must first be rebased — changes made during CI-fix would have to be re-rebased anyway.
- The observed failure mode on issue #654 shows exactly what happens when order is reversed: 7 empty "CI still running" re-invokes before the conflict is even detected.

The tradeoff: if both a conflict and a real CI failure exist, the conflict is resolved first. The rebase will likely change the head SHA, so CI will re-run against the new commit anyway; addressing CI on the stale head would be wasted work.

### Why Claude Rebases (Not the Engine)

The engine could run `git fetch && git rebase` directly from a worker goroutine. We deliberately do not.

Automatic rebase is right most of the time and catastrophically wrong some of the time: two PRs independently pick `adr-054.md`; both pick migration slot `0042`; both add a new line at the same point in a shared config. A mechanical rebase drops one side silently — git has no way to know that "054 on my branch" and "054 on main" are semantically *both valid and non-overlapping* if renumbered. A Claude-driven rebase can rename the ADR, update the index, and keep both contributions. The synthetic comment explicitly flags this concern ("watch for semantic collisions"), so the one judgment call only a human-like agent can make is applied in the right place.

The cost is a re-invocation (several minutes + Claude tokens) rather than an inline `exec.Cmd` call. That cost is why `MaxRebaseCycles` defaults to 3: if Claude cannot rebase cleanly in three attempts the conflict almost certainly needs a human.

### `itemMayNeedWork` Cache Bypass

Items with `fabrik:rebase-needed` bypass the `updatedAt` cache in `itemMayNeedWork`, same as `fabrik:awaiting-ci` and `fabrik:blocked`. Base-branch advances do not bump the item's `updatedAt`, so without this bypass the gate would only re-evaluate when some other event touched the issue.

## Alternatives Considered

### Engine-side `git rebase`

Save a round-trip to Claude by running `git fetch origin <base> && git rebase origin/<base> && git push --force-with-lease` directly. Rejected — see "Why Claude Rebases" above. Silent data loss on semantic conflicts is a worse failure mode than taking a minute longer on rebase.

### Extend the CI Gate Instead of a Separate Gate

Add a mergeability check inside `checkCIGate` as the first thing it does. Rejected because the gate returns a `(blocked, ciFailure, timedOut)` triple whose semantics are all about CI; piling a fourth concern onto it would produce a worse signature and conflate label management (different labels, different timeouts, different reinvoke skills). A separate function with its own return type mirrors the existing pattern and keeps each gate independently testable.

### Detect Mergeability in the Merge Guard

`attemptMergeOnValidate` already checks `mergeable` and returns `ErrNotMergeable`. We could simply run it earlier. Rejected because the merge guard is specific to yolo auto-merge and mixes several concerns (CI status tracking via `ciMergePendingSince`, `MergePR` RPC, rebase commit strategy). Teaching it to also drive re-invocations would duplicate the dispatch logic that `dispatchCIFixReinvoke` already owns.

### React to `mergeable == null` by Asking GitHub to Compute

A `null` response means GitHub has not computed mergeability yet; asking again forces computation. We currently block without a label and wait for the next poll, which effectively does this via the existing poll cadence. A more aggressive fetch loop (call twice within one poll, with a short sleep) would shave one poll interval off the detection latency in the worst case; we chose the simpler path because the latency gain is small and the current behavior is predictable.

### A `MaxRebaseCycles` Default of 5

Aligning with review and CI defaults (both 5). Rejected because rebase is a different kind of task: it either succeeds in one shot or it needs human judgment. Five attempts would waste tokens on unresolvable semantic conflicts. Three is enough to survive transient push failures without encouraging the agent to dig itself deeper.
