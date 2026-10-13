# Feature Specification: TUI armed confirm prompts are always visible, self-cancelling, and cannot stop the daemon unseen

**Feature Branch**: `fabrik/issue-2092`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "tui: an armed confirm prompt is always visible, cancels on other keys or timeout, and can't stop the daemon unseen

## Problem

Report #1948: the TUI's `u` (upgrade skills) prompt is shown only through `header.SetStatusMsg` (`tui/model.go:522-534`). `HeaderComponent.View` (`tui/header.go:101-118`) truncates the status to whatever is left after the title, timer and badges, and drops it entirely when nothing is left.

At 80 columns with the stale badge, about 21 characters remain, so `[y/N]` is cut off. With the custom-workflow badge, the `[1]/[2]/[3]` reconcile options are never visible. Below about 60 columns the prompt disappears. The key appears to do nothing, yet it has armed a confirm.

The armed confirm also survives unrelated keys, and it is hazardous. Under `confirmReconcile`, a stray `1` (`model.go:538-542`) quits the TUI. `cmd/root.go:1719+` then sends SIGTERM to the engine, stopping the daemon and bypassing the active-workers quit confirmation that ctrl+c gets. The stop prompt (`model.go:472`) and the `OVERWRITE` prompt use the same truncatable channel.

## Requirements

**R1.** While any confirm is armed (`confirm*` flags), its prompt is always fully visible at 80 columns and is never truncated or dropped. Plan chooses between giving the status message priority over the timer and badges while armed, and rendering confirms in the footer or a dedicated line.

**R2.** An armed confirm is cancelled by any key that isn't one of its answers, and by a timeout (Plan picks a value, e.g. 10 s). It is visibly dismissed when cancelled.

**R3.** The reconcile action that quits the TUI goes through the same active-workers confirmation as ctrl+c, or does not stop the engine. Plan confirms which is correct.

**R4.** The same treatment applies to the stop and `OVERWRITE` prompts.

## Scope

- In scope: TUI prompt rendering and confirm lifecycle, tests.
- Out of scope: other header layout changes.
- Mention #1948 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1948. This issue's own number gets `Closes`.

## Acceptance

- Header tests at 80 and 60 columns with every badge combination show the full confirm text, including the answer keys.
- Tests showing that an unrelated key and a timeout cancel an armed confirm, and that `1` with no visible armed reconcile prompt does nothing."

## Background

Community report #1948 found that pressing `u` in the TUI arms a confirmation that the operator often cannot see. The prompt is written into the header's single status message. The header lays out its title, timer and badges first and gives the status only the space left over. At 80 columns with the stale-skills badge about 21 characters remain, so the answer hint `[y/N]` is cut off. With the custom-workflow badge the `[1]/[2]/[3]` reconcile options never appear. Below about 60 columns the prompt vanishes completely. To the operator the keypress looks like it did nothing, but a confirm has been armed.

Two problems follow from this:

1. **Invisibility.** The operator cannot tell a confirm is waiting, or what its answers are.
2. **Stale armed state is dangerous.** An armed confirm persists across unrelated keystrokes. Under the reconcile confirm, a later stray `1` quits the TUI. The launcher then sends SIGTERM to the engine, which stops the daemon without the active-workers confirmation that ctrl+c requires. A key the operator did not know was live can therefore stop work in progress. The stop prompt and the `OVERWRITE` prompt share the same truncatable channel and the same exposure.

The fix is to make armed confirms always visible, to make them short-lived and cancellable by any non-answer key, and to ensure no confirm answer can stop the daemon while bypassing the active-workers safeguard.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - See the confirm I just armed (Priority: P1)

An operator presses a key that arms a confirm (upgrade skills, reconcile, stop, overwrite) in a terminal 80 columns wide or narrower, with any combination of header badges showing. The full prompt, including its answer keys, appears.

**Why this priority**: An invisible confirm is the root defect of #1948. Without visibility the operator cannot answer or dismiss the prompt.

**Independent Test**: Render the header or footer at 80 and 60 columns for every badge combination with each confirm armed, and assert the full prompt text and answer keys are present.

**Acceptance Scenarios**:

1. **Given** the stale-skills badge is showing at 80 columns, **When** the operator presses `u`, **Then** the complete upgrade prompt including `[y/N]` is visible.
2. **Given** the custom-workflow badge is showing at 80 columns, **When** the reconcile confirm is armed, **Then** the `[1]/[2]/[3]` options are all visible.
3. **Given** a 60-column terminal with all badges showing, **When** any confirm is armed, **Then** its full prompt is visible and not dropped.

---

### User Story 2 - An armed confirm goes away on its own (Priority: P1)

An operator arms a confirm, then presses an unrelated key or does nothing. The confirm is cancelled and visibly dismissed, so a later keystroke cannot accidentally answer it.

**Why this priority**: A lingering armed confirm is what makes the hazard in Story 3 reachable.

**Independent Test**: Arm each confirm, send an unrelated key, and assert the confirm flag is cleared and the prompt is gone. Arm each confirm again, advance time past the timeout, and assert the same.

**Acceptance Scenarios**:

1. **Given** a confirm is armed, **When** the operator presses a key that is not one of its answers, **Then** the confirm is cancelled and the prompt is visibly dismissed.
2. **Given** a confirm is armed, **When** the timeout elapses with no answer, **Then** the confirm is cancelled and the prompt is visibly dismissed.
3. **Given** no reconcile prompt is visible or armed, **When** the operator presses `1`, **Then** nothing happens.

---

### User Story 3 - A confirm answer can't stop the daemon unseen (Priority: P1)

An operator answers the reconcile prompt with `1`. The TUI never stops the engine without the same active-workers confirmation ctrl+c requires.

**Why this priority**: Stopping the daemon and abandoning running workers is the most severe consequence of the defect.

**Independent Test**: With active workers present, drive the reconcile answer that currently quits and assert either the active-workers confirmation is shown first, or the engine is not stopped.

**Acceptance Scenarios**:

1. **Given** the reconcile confirm is armed and workers are active, **When** the operator presses the answer that currently quits the TUI, **Then** the engine is not stopped without the active-workers confirmation, or the engine is not stopped at all.
2. **Given** no workers are active, **When** the same answer is given, **Then** the behaviour matches what ctrl+c does in that state.

---

### User Story 4 - The stop and OVERWRITE prompts get the same treatment (Priority: P2)

The stop prompt and the `OVERWRITE` typed-confirmation prompt are always visible, cancel on a non-answer key or timeout, and are visibly dismissed.

**Why this priority**: They share the defective channel but are used less often than upgrade and reconcile.

**Independent Test**: Repeat the visibility and cancellation tests of Stories 1 and 2 for these two prompts.

**Acceptance Scenarios**:

1. **Given** the stop prompt is armed at 80 columns, **When** the header or footer is rendered, **Then** the full prompt is visible.
2. **Given** the `OVERWRITE` prompt is armed, **When** the timeout elapses, **Then** the typed input is discarded and the prompt is dismissed.

---

### Edge Cases

- A key that is a legitimate answer to the armed confirm is not treated as a cancelling "other" key.
- Arming a second confirm while one is armed replaces the first; only one prompt is ever shown, and the timeout restarts.
- The timeout fires after the confirm was already answered or cancelled: no effect and no stale message cleared.
- The `OVERWRITE` prompt collects typed characters; keys that are part of typing `OVERWRITE` must not cancel it, and a non-matching key or the timeout must.
- A terminal narrower than the longest prompt: the prompt wraps or is otherwise fully shown rather than truncated. Below 60 columns it is still not dropped.
- Terminal resize while a confirm is armed keeps the prompt fully visible.
- Active-workers count changes while a quit-type confirm is armed.
- The reconcile prompt is shown only with the custom-workflow badge. `1`, `2` and `3` do nothing when no reconcile confirm is armed.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: While any TUI confirm is armed, its complete prompt text, including every answer key, is visible at a terminal width of 80 columns and is never truncated or omitted.
- **FR-002**: The prompt remains fully visible for every combination of header badges (stale-skills, custom-workflow, and any others present), at 80 columns and at 60 columns.
- **FR-003**: Any key that is not one of an armed confirm's answers cancels that confirm.
- **FR-004**: An armed confirm cancels itself after a fixed timeout with no answer. The planning stage chooses the value (for example 10 seconds).
- **FR-005**: When a confirm is cancelled, whether by a non-answer key or by timeout, the prompt is visibly removed so the operator can see it is no longer armed.
- **FR-006**: The key `1` (and `2`, `3`) has no effect when no reconcile confirm is armed.
- **FR-007**: No confirm answer stops the engine without passing through the same active-workers confirmation that ctrl+c uses. Either the reconcile quit path goes through that confirmation, or it quits the TUI without stopping the engine. Planning confirms which is correct.
- **FR-008**: The stop prompt and the `OVERWRITE` prompt satisfy FR-001 through FR-005.
- **FR-009**: Automated header tests at 80 and 60 columns cover every badge combination with each confirm armed and assert the full prompt text and answer keys are present.
- **FR-010**: Automated tests show that an unrelated key cancels an armed confirm, that the timeout cancels an armed confirm, and that `1` with no armed reconcile prompt does nothing.

### Key Entities *(if applicable)*

- **Armed confirm**: A transient TUI state (upgrade, reconcile, stop, overwrite, and the existing quit and clear confirmations) that awaits a specific answer key or typed answer and carries a visible prompt.
- **Confirm prompt**: The text shown to the operator for an armed confirm, including its answer keys.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every confirm and every badge combination, the rendered prompt at 80 columns and at 60 columns contains the full prompt text and every answer key (100% of cases in the header test matrix).
- **SC-002**: After any non-answer key press or the timeout, no armed confirm remains and no prompt text is displayed, in every confirm type.
- **SC-003**: No sequence of keys beginning from a state with no visible armed confirm can stop the engine, and no confirm answer stops the engine while workers are active without the active-workers confirmation.
- **SC-004**: Pressing `1` with no armed reconcile prompt produces no state change in the TUI.

## Assumptions

- "Any confirm" in R1 and R2 covers every armed confirm in the TUI, including the quit and history-clear confirmations that already appear elsewhere. Planning verifies each is already compliant and brings any that are not into line.
- A single timeout value applies to all confirms. Planning picks it, in the range of roughly 5 to 15 seconds.
- Where the prompt is rendered (a priority status in the header, or the footer or a dedicated line) is a planning decision, provided FR-001 and FR-002 hold.
- If the reconcile quit path keeps stopping the engine, it must use the active-workers confirmation. If planning finds quitting the TUI should never stop the engine from this path, that is also acceptable (FR-007).
- Answers that are valid for a confirm (`y`, `1`/`2`/`3`, the typed `OVERWRITE` characters, existing cancel keys such as `n` and esc) keep their current meaning.

## Out of Scope *(optional)*

- Other header layout changes unrelated to confirm prompt visibility.
- Changing what the upgrade, reconcile, stop or overwrite actions themselves do, beyond the quit path in FR-007.
- Redesigning ctrl+c or the active-workers confirmation itself.
- Closing #1948. That report stays open until a release containing the fix ships and is closed by hand. This work only mentions #1948 by bare number.

## Source References *(optional)*

- Report #1948 (bare-number mention only; no closing keyword).
- `tui/model.go` (confirm flags, key handling), `tui/header.go` (status truncation), `cmd/root.go` (TUI exit stops the engine).
