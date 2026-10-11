# Feature Specification: MCP overseer `fabrik_advance` — move a completed stage one column forward

**Feature Branch**: `fabrik/issue-2126`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

An overseer cannot carry out the most common decision it is asked to make: **advance an item whose stage has completed but which does not auto-advance.**

- `fabrik_promote` refuses, by design, any item already in a pipeline column. Example (in-situ test, 2026-10-11): `promote #1209 (Specify, stage:Specify:complete) → Research` gives \"already in the pipeline column Specify; promote only moves items out of an unmanaged parking column\".
- The refusal still lists `valid_targets: [Specify … Validate]`, which suggests a move the tool will not make, and it does not say what to do instead.
- The only remaining options are a manual board move or setting cruise. Cruise changes the item's autonomy for every remaining stage, which is a different decision from \"advance this one stage\".

## Requirements

- **R1 — `fabrik_advance(issue)`.** Moves an item exactly one stage forward, from its current pipeline column to the next configured stage, when:
  - the item is a managed, open, non-PR issue;
  - its current stage is complete (`stage:<X>:complete`);
  - no worker is in flight or queued for it;
  - it is not paused, awaiting input, or blocked.

  Validate → Done (landing) and any move into a holding stage (Queued) are refused. Those are the engine's own transitions and the human's merge decision.
- **R2 — Reuse the engine's own advance.** It uses the engine's existing advance transition (the same one auto-advance uses) and none of its own. It confirms the column with one live ID-keyed read, failing closed, like `promote`. Requester, sanitisation and audit comment follow ADR-1966-c:
  - a `🏭 **Fabrik — overseer action: advance**` comment;
  - written first, then the comment.
- **R3 — Refusals say what to do.** Every refusal carries the item's current state and the specific reason, e.g.:
  - \"stage not complete — a worker is running\";
  - \"paused — a human comment resumes it\";
  - \"Validate → use the PR merge\".

  `fabrik_promote`'s refusal for an item already in a pipeline column points to `fabrik_advance`, and no longer lists `valid_targets` as if the move were available.
- **R4 — Static scans.** The engine side lives beside `overseer_actions.go`. The read-path no-GitHub scans stay unweakened.
- **R5 — Docs.**
  - `docs/state-machine.md` §7.15.
  - The `docs/USER_GUIDE.md` tool table.
  - Regenerate `docs/llms-full.txt`.
  - Add an ADR, or extend ADR-1966-c with a dated section.

## Scope

In scope: the advance action and the promote refusal text.

Out of scope:
- multi-stage jumps;
- merging;
- lifting pauses (deliberately absent, ADR-1813).

## Acceptance

All of the following are shown non-vacuous:
- **Success:** an engine test with an item in Specify carrying `stage:Specify:complete` and no autonomy runs `Advance`. The Status is moved to Research through the engine's advance path, and one audit comment is posted. A tests/sim run shows the engine dispatches Research on the next poll.
- **Refusals:** each R1 precondition violation is refused with the specific reason, and nothing is written.
- **Promote's pipeline-column refusal:** it names `fabrik_advance` and lists no `valid_targets`.
- **Audit comment is not steering:** the audit comment is not treated as steering. Pinned through `findNewComments`/`resumeAuthorised`/`feedbackGateBlocks` as for the existing actions.
- `go test -race ./...` is clean.

Mentioned for context: #1966. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.
"

## Background

An overseer session (a Claude Code session driving the pipeline through the Fabrik MCP tools) is routinely asked to decide whether a finished stage should move on. Many stages do not advance by themselves: Specify, for example, waits for a human to approve the spec. Today the overseer has no tool for that decision.

- `fabrik_promote` only moves items out of an unmanaged parking column (such as Backlog) and refuses an item that is already in a pipeline column. In an in-situ test on 2026-10-11, promoting #1209 (in Specify, `stage:Specify:complete`) to Research was refused with "already in the pipeline column Specify; promote only moves items out of an unmanaged parking column".
- The remaining options are a manual board move, or setting `fabrik:cruise`. Cruise changes the item's autonomy for every remaining stage, which is a different decision from "advance this one stage".
- The refusal used to list `valid_targets` for a move it would not make. #2113 (now closed) removed the list from the refusal for an item not in a parking column. What remains is that the refusal does not say what to do instead.

This feature gives the overseer a narrow, audited action for that one decision. It adds no new engine transition: it performs the move the engine itself makes when a stage that auto-advances completes. The action set it joins (`promote`, `set_autonomy`, `revalidate`, `clear_claude_limit`) is described in ADR-1966-c and `docs/state-machine.md` §7.15, and the rules those actions follow (an attributable requester, write first and then an audit comment, no pause lifting, no free-form comments) apply here unchanged. The parent initiative is #1966.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Advance a completed stage by one column (Priority: P1)

An overseer finds an item whose current stage is complete and which is waiting for a human to advance it (for example #1209 in Specify with `stage:Specify:complete`, no autonomy label). It calls `fabrik_advance` for that issue. The item moves exactly one stage forward to the next configured stage, one audit comment records who asked, and the engine runs the next stage on its next poll as it would have after an ordinary advance.

**Why this priority**: This is the whole reason for the feature and the most common overseer decision. It is the only way to move one stage without changing the item's autonomy for the rest of the pipeline.

**Independent Test**: With an open managed issue in Specify carrying `stage:Specify:complete`, no autonomy label, no paused/blocked labels and no worker, call advance. The board Status becomes Research, exactly one `🏭 **Fabrik — overseer action: advance**` comment exists, and the next poll dispatches Research.

**Acceptance Scenarios**:

1. **Given** an open issue in Specify with `stage:Specify:complete` and nothing blocking it, **When** the overseer requests an advance, **Then** its Status becomes Research, the result reports the move, and one audit comment naming the requester is posted.
2. **Given** the advance above succeeded, **When** the engine next polls, **Then** it dispatches the Research stage for the item.
3. **Given** a successful advance, **When** the audit comment is later examined by the engine's comment logic, **Then** it is not treated as human steering (it cannot resume a paused item, start a comment-review cycle, or hold an advance or landing).

---

### User Story 2 - Understand and act on a refusal (Priority: P1)

An overseer asks to advance an item that cannot be advanced right now. The refusal says exactly why, in terms the overseer can act on, and carries the item's current state. Nothing is written to the board, labels or comments.

**Why this priority**: The action is only safe if it declines every case where the engine's own advance would not proceed, and only useful if the overseer then knows what to do next.

**Independent Test**: For each precondition (not a managed issue, closed, pull request, stage not complete, worker in flight or queued, paused or awaiting input, blocked, Validate, next stage is a holding stage), set up the item and request an advance. Each is refused with its own specific reason, and no label, Status or comment write occurs.

**Acceptance Scenarios**:

1. **Given** an item whose current stage has not completed, **When** an advance is requested, **Then** it is refused with a reason that says the stage is not complete, and says a worker is running when one is.
2. **Given** a paused or awaiting-input item, **When** an advance is requested, **Then** it is refused with a reason that says a human comment resumes it, and the pause is not lifted.
3. **Given** an item in Validate, **When** an advance is requested, **Then** it is refused with a reason that says to use the PR merge, because Validate → Done is the engine's landing and the human's merge decision.
4. **Given** an item whose next stage is a holding stage (such as Queued), **When** an advance is requested, **Then** it is refused with a reason that says that move is the engine's own.
5. **Given** a blocked item, **When** an advance is requested, **Then** it is refused with a reason that names the block.

---

### User Story 3 - Promote points to advance (Priority: P2)

An overseer who tries `fabrik_promote` on an item already in a pipeline column is told the move is impossible for promote and which tool to use instead.

**Why this priority**: It closes the discoverability gap that led to this issue. It is small and independent of the advance action itself.

**Independent Test**: Request a promote on an item in a pipeline column. The refusal names `fabrik_advance` and carries no `valid_targets`.

**Acceptance Scenarios**:

1. **Given** an item in a pipeline column, **When** promote is requested, **Then** the refusal names `fabrik_advance` as the way to move it forward and lists no `valid_targets`.
2. **Given** an item in a parking column, **When** promote is requested with a missing or invalid target, **Then** the refusal is unchanged (it still lists the valid targets).

---

### Edge Cases

- The cached column lags GitHub: the live read decides, never the cache, and the live column is what is advanced from. A live read error refuses.
- The cache says the stage is complete but a worker is in flight or the engine is about to dispatch one: refused as not safe to advance.
- The item carries `fabrik:cruise` or `fabrik:yolo` (the engine advances it on its own): the request still works and the result notes the autonomy mode (see Assumptions).
- Two advances for one item arrive close together, or the engine's own advance races the overseer's: the item must end up in the next stage once, never two stages ahead.
- The next stage exists in the stage configuration but has no board column: the request fails with that specific reason and nothing is written.
- The item is in the last pipeline stage, the cleanup stage, or an unmanaged parking column: refused with a reason (Validate points to the PR merge; a parking column points to `fabrik_promote`).
- The stage is complete only in the cache, because a CI wait or review wait still holds the engine's own advance: refused rather than skipping a gate the engine would apply.
- The audit comment fails to post after the move: the move stands and the result reports the audit comment as failed, as for the existing actions.
- The requester name is missing or contains unsafe characters: the missing name is refused, unsafe characters are sanitised, as for the existing actions.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The overseer action set offers an `advance` action, surfaced as the `fabrik_advance` MCP tool, taking an issue reference and the requester's name like the existing actions.
- **FR-002**: A successful advance moves the item's board Status from its current pipeline column to the next configured stage, exactly one stage forward, never more.
- **FR-003**: An advance proceeds only for an item that is a managed, open, non-PR issue whose current stage carries `stage:<X>:complete`.
- **FR-004**: An advance is refused while a worker is in flight or queued for the item.
- **FR-005**: An advance is refused for an item that is paused, awaiting input, or blocked. It never removes `fabrik:paused` or `fabrik:awaiting-input` (ADR-1813).
- **FR-006**: An advance is refused where the engine's own advance would hold the item: while a CI wait, a review wait, an unprocessed comment/review feedback or an unresolved dependency stands between the completed stage and the next. The overseer cannot skip a gate the engine applies.
- **FR-007**: An advance from Validate (Validate → Done) is refused, with a reason pointing to the PR merge.
- **FR-008**: An advance whose next stage is a holding stage (such as Queued) is refused. So is any item whose current column is a parking column or the cleanup stage, with a reason that points to the correct path (`fabrik_promote` for a parking column).
- **FR-009**: The move uses the engine's existing stage-advance transition (the one auto-advance uses) and introduces no transition of its own.
- **FR-010**: Before writing, the action confirms the item's current column with one live, ID-keyed read and fails closed on a read error. The live column, not the cache, decides which stage is "current" and which is next.
- **FR-011**: Every refusal carries the item's current state (as the existing refusals do) and a specific, actionable reason. Examples: "stage not complete — a worker is running", "paused — a human comment resumes it", "Validate → use the PR merge".
- **FR-012**: A refused advance writes nothing: no Status move, no label change and no audit comment.
- **FR-013**: A successful advance posts exactly one audit comment, `🏭 **Fabrik — overseer action: advance**`, naming the sanitised requester, written after the move. A failed comment leaves the move in place and is reported as such.
- **FR-014**: The audit comment is not treated as steering. It cannot resume a paused item, start a comment-review cycle, or hold a stage advance or landing, under both PAT and App authentication.
- **FR-015**: After a successful advance, the engine dispatches the next stage on its normal schedule exactly as it would after its own advance (no extra labels or hand-offs from the overseer path).
- **FR-016**: `fabrik_promote`'s refusal for an item already in a pipeline column names `fabrik_advance` as the way to move it forward and lists no `valid_targets`.
- **FR-017**: The engine side of the action lives beside the existing overseer actions, outside the files covered by the read-path no-GitHub scans, and those scans are not weakened.
- **FR-018**: `docs/state-machine.md` §7.15 and the tool table in `docs/USER_GUIDE.md` describe the new action and the changed promote refusal, `docs/llms-full.txt` is regenerated, and the decision is recorded in an ADR (or a dated section added to ADR-1966-c).

### Key Entities *(if applicable)*

- **Overseer action**: a mutating request over the local socket (`promote`, `set_autonomy`, `revalidate`, `clear_claude_limit`, and now `advance`) that performs an existing engine transition and posts an audit comment naming the requester.
- **Refusal**: a structured response carrying the item's current state and the reason the action was declined, with nothing written.
- **Completed stage**: an item whose current stage carries `stage:<X>:complete`, the signal that the stage's own work (including any CI wait) is done.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An item in Specify with `stage:Specify:complete` and no autonomy label moves to Research with one advance request, one Status write and one audit comment, and the next poll dispatches Research. A test would fail if the move or the dispatch did not happen.
- **SC-002**: Every FR-003 to FR-008 precondition violation has a test showing the specific refusal reason and zero writes (no Status, label or comment).
- **SC-003**: Promote's refusal for a pipeline-column item names `fabrik_advance` and has no `valid_targets`; promote behaviour for parking-column items is unchanged.
- **SC-004**: Tests against the real comment predicates (`findNewComments`, `resumeAuthorised`, `feedbackGateBlocks`) and a sim poll run show the advance audit comment does not resume a paused item, start a comment-review cycle or hold an advance, including when posted under an operator's own login.
- **SC-005**: The existing method-set and source scans (no pause lift, no free-form comment, no GitHub client in the read-path files) still pass with the new action present.
- **SC-006**: `go test -race ./...` passes, the docs-bundle drift check passes, and the new action appears in the §7.15 table and the `docs/USER_GUIDE.md` tool table.

## Assumptions

- "The next configured stage" is the stage after the item's live current column in configured stage order, as the engine's own advance defines it. A stage with no board column is a refusal, not a skip.
- The `stage:<X>:complete` label is the completion signal. For stages that wait on CI it is deferred until CI passes, so the label alone covers the CI wait; review waits and unprocessed feedback are checked separately (FR-006).
- "Queued for it" in R1 means the item is claimed by an in-flight worker or the engine has it marked for dispatch, and is treated the same as "in flight". The exact check is a design question for Plan.
- An item carrying `fabrik:cruise` or `fabrik:yolo` is not refused. The engine would advance it anyway, a repeated move to the same next stage is harmless, and the result notes the autonomy mode as `promote` does. Whether cruise/yolo should instead be refused is easy to reverse.
- Advancing into the next stage never touches the item's autonomy labels, and there is no multi-stage option.
- The audit action name is `advance` and the requester rules (self-asserted `--subscriber` name, sanitised, 64 characters) are those of ADR-1966-c, unchanged.
- The ADR is numbered after this issue (`adrs/2126-...`), or the existing ADR-1966-c gets a dated section; either satisfies FR-018 and is Plan's choice.

## Out of Scope *(optional)*

- Multi-stage jumps.
- Merging a PR, or moving Validate → Done (the engine's landing and the human's merge decision).
- Moving an item into a holding stage such as Queued (the engine's own transition).
- Lifting a pause or answering awaiting-input (deliberately absent, ADR-1813); there is still no tool that posts a free-form comment.
- Moving an item backwards or sideways, or changing the item's autonomy.
- Any change to when or how the engine's own auto-advance decides.

## Source References *(optional)*

- Issue #1966 (parent MCP overseer initiative; mentioned for context, by bare number only), ADR-1966-c, `docs/state-machine.md` §7.15.
- Issue #2113 (the earlier change to `fabrik_promote`'s refusal shape).
- ADR-1813 (pauses are lifted only by a human comment).
