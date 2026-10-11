# ADR 2126: Overseer `fabrik_advance` — move a completed stage one column forward

Status: accepted (issue #2126; extends [ADR-1966-c](1966-c-fabrik-mcp-actions.md), parent initiative #1966)

## Context

The most common decision an overseer is asked to make is "this stage is finished and waits for a human — move it on". Specify, for example, does not advance by itself. `fabrik_promote` cannot make that move by design: it only takes an item out of an unmanaged parking column, and it refuses an item already in a pipeline column. The remaining options were a manual board drag or `fabrik:cruise`, which changes the item's autonomy for every remaining stage — a different decision from "advance this one stage".

ADR-1966-c fixed the rules an action follows: it performs an existing engine transition, writes first and then posts one audit comment, is attributable, and never lifts a pause or posts a free-form comment. `advance` is a fifth action under those rules. The decisions below are the ones a contributor would otherwise have to rediscover.

## Decisions

### 1. It calls the engine's own `advanceToNextStage`

The move is `advanceToNextStage(&gh.ProjectBoard{ProjectID: …}, item, currentStage)` — the function both auto-advance sites call — not `moveItemToStatus`, which is the overseer's generic status writer. That keeps the transition literally the engine's (including `clearStatusLine`, the cache write-through, the staleness baseline and the webhook echo). Two consequences:

- `advanceToNextStage` returns `nil` when there is no next stage, so the action decides "no next stage / next is holding or cleanup" itself before the call; otherwise a no-op would post a false audit comment.
- It reads `e.statusField` without the engine lock, as its other callers (worker goroutines) already do. `Advance` checks field and project presence under `e.mu` first. The project ID comes from the board cache, or from the last polled board (`Engine.boardProjectID`, set by `poll()`) when no cache is wired.

### 2. Refusals are structural and ordered

The column is judged against the stage configuration, not by name: the item must be in a configured, non-parking, non-holding, non-cleanup stage whose following stage (skipping parking columns) is neither a holding stage nor the cleanup stage. In the shipped order that makes Validate the refused stage ("Validate → use the PR merge"): its next column is Queued or Done, which is the engine's landing and the human's merge decision. A parking column points to `fabrik_promote`.

The refusal order is fixed so each case reports exactly one reason: board metadata, column, next stage's board column, worker, live column and labels, completion, pause, block, engine-owned wait labels, dependencies, feedback, review wait. A worker is "queued" if `ItemState.Worker` is set (it is set before the dispatch goroutine starts) or a live `fabrik:locked:*`, `fabrik:editing` or `stage:<X>:in_progress` label is present; retry cooldowns throttle the engine, not the decision, and are not refusal conditions.

### 3. No gate the engine applies to its own advance is skipped — and a refusal writes nothing

The engine holds its own advance on unresolved dependencies, unprocessed feedback and review waits, and a human drag has none of that. The overseer must not become a way around them, but the engine's gate functions write (`checkDependencies` adds `fabrik:blocked` and posts a comment, `checkReviewGate` and `reviewGateBlocksLanding` add `fabrik:awaiting-review`). So the read-only cores are used instead:

- `openDependencies`, extracted from `checkDependencies` (which now calls it), fed by a live `FetchItemDetails` copy;
- `feedbackGateBlocks` as is — its only side effect is an in-memory `feedback-pending` cooldown, which is what the engine's own held advance records too, so it is the one side effect of the refusal path;
- `reviewGateHold`, extracted from `reviewGateBlocksLanding` (which now wraps it and applies the label), so advance and landing cannot disagree on "review satisfied".

`stage:<X>:complete` is not enough on its own: for a `wait_for_reviews` stage it is added before the reviewers are recorded as outstanding, so a label-only check would leave a window. Both extractions are behaviour-preserving, and the existing tests run unchanged.

### 4. Live column and labels decide

As for `promote`, one ID-keyed `FetchProjectItemStatus` read confirms the column and the live column is what is advanced from; labels are read live (`FetchLabels`) and a failed read refuses. If the engine advanced the item first, the live column is already the next stage with no completion label, so the request is refused as "not complete" and the item can never end up two stages ahead. If both write at the same instant they target the same next stage, so the write is idempotent; the window is inherent, as for `promote`.

### 5. Autonomy is noted, not refused; no new wire shape

An item carrying `fabrik:cruise` or `fabrik:yolo` may be advanced: the engine would advance it anyway and a repeat is harmless, so the result carries a note and the label is untouched. Easy to reverse if that proves wrong. The result reuses `ActionResult` (`Summary`, `Writes`, `Notes`); `RefusalState.ValidTargets` is never set for `advance`. The protocol version stays 2: an older daemon answers `unknown_method` through the existing "does not support overseer actions" path.

### 6. `promote`'s refusal points to `advance`

An item already in a pipeline column is refused with a pointer to `fabrik_advance` and no `valid_targets` (#2113 had already removed the list); the unknown-column and unparked refusals do not mention it.

## Consequences

- The overseer can make the most common decision with a single audited call, without changing autonomy.
- `overseer_advance.go` is held to the same source scan as `overseer_actions.go`: no `postComment` of its own, no label removal, no `AddComment`/`pauseIssue`, and none of the writing gate wrappers.
- Two engine functions were split into a read-only core and a writing wrapper. A future change to what counts as a review wait or an open dependency lands in the core and applies to both the engine and the overseer.
- `advance` costs three live reads (column, labels, item details) plus the review-state reads on a `wait_for_reviews` stage, then one status write and one comment.
- Out of scope, unchanged: multi-stage jumps, merging, moving into a holding stage, moving backwards, and lifting a pause (ADR-1813).
