# Feature Specification: Case-insensitive marker header parsing with diagnosable failures

**Feature Branch**: `fabrik/issue-2091`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

Report #2055: `FABRIK_PR_CREATE` parsing is case-sensitive. `engine/prcreate.go:104` requires the literal `TITLE:` prefix, and a model writing `title:` fails the parse. The error, \"missing TITLE line\" (`prcreate.go:80`), doesn't show what was found. `engine/item.go:1432-1437` then posts it, adds `fabrik:paused` and returns **before** `commitWIP`, the push and `handleStageComplete`. So a finished Implement stage is paused, and un-pausing re-runs the whole stage.

The same strict parser is duplicated for `FABRIK_SPAWN_CHILD` (`engine/spawn.go:243`), and it's worse there: a malformed block is skipped silently (`spawn.go:174-180`), so a lowercase `title:` means the child is never created and nothing says so. The \"PR body is empty\" error (`prcreate.go:83`) has the same \"doesn't show what was found\" problem.

## Requirements

**R1.** One shared helper parses `KEY: value` header lines for both markers. Keys match case-insensitively (`TITLE`, `DEPENDS_ON`, and any other header key), with surrounding whitespace tolerated.

**R2.** Every `FABRIK_PR_CREATE` parse error quotes, truncated and sanitised for inline rendering, the line the parser actually found, or says the block was empty.

**R3.** A `FABRIK_SPAWN_CHILD` block that still fails to parse is no longer dropped silently. It is logged, and it is surfaced the same way an unparseable `FABRIK_PR_CREATE` is (a comment naming the block index and the offending line). Plan decides whether that pauses, matching `FABRIK_PR_CREATE`'s existing behaviour unless there's a reason not to.

**R4.** A parse failure no longer skips the engine's own `commitWIP` and push of the stage's work. Plan confirms the safe ordering, so un-pausing doesn't redo committed work.

## Scope

- In scope: both marker parsers, error wording, the ordering fix, tests.
- Out of scope: changing the marker grammar beyond case and whitespace tolerance.
- Mention #2055 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2055. This issue's own number gets `Closes`.

## Acceptance

- Unit tests: `title:`, `Title:` and ` TITLE :` parse for both markers. Each parse error includes the found line. A malformed spawn block yields a surfaced error rather than silence.
- A test showing the stage's work is committed and pushed even when `FABRIK_PR_CREATE` fails to parse."

## Background

Stage agents (Implement for `FABRIK_PR_CREATE`; Plan, Review and Validate for `FABRIK_SPAWN_CHILD`) emit structured marker blocks whose first lines are `KEY: value` headers such as `TITLE:` and `DEPENDS_ON:`. The engine accepts only the exact upper-case spelling with no padding, so a model that writes `title:` or ` TITLE :` produces a block the engine rejects, even though the intent is unambiguous. The grammar is a convention the model is asked to follow, and models drift on case and spacing. The engine should tolerate that drift.

Three problems follow from the strictness, all reported in #2055:

1. **Brittle parsing.** A cosmetic difference in a header key fails the whole block.
2. **Undiagnosable failures.** The `FABRIK_PR_CREATE` error says only "missing TITLE line" or "PR body is empty". It does not show what the parser saw, so an operator cannot tell a casing slip from a truly absent title. For spawn blocks there is no error at all: a malformed block is skipped silently, so the child issue is never created and nothing says so. Today the Review/Validate stripping path also deliberately leaves such a block visible in the posted comment. That looks like leaked syntax rather than a diagnosed failure.
3. **Costly recovery.** On a `FABRIK_PR_CREATE` parse failure, the engine posts the error, applies `fabrik:paused` and returns before it commits and pushes the stage's work and before stage-complete handling. A finished Implement stage therefore ends up paused with its work uncommitted. Un-pausing re-runs the whole stage instead of retrying the one marker.

The two markers currently use separate copies of the same strict header-line parser. One shared parser fixes both and keeps them from drifting apart again.

Report #2055 is the originating community report. It stays open and human-owned until a release containing the fix ships.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Marker headers parse regardless of key case and padding (Priority: P1)

An agent emits a `FABRIK_PR_CREATE` or `FABRIK_SPAWN_CHILD` block whose header keys differ from the canonical spelling only in letter case or surrounding whitespace (`title:`, `Title:`, ` TITLE :`, `depends_on:`). The engine treats the block exactly as if the canonical spelling had been used.

**Why this priority**: This is the trigger of the reported incident. Without it, a cosmetic slip pauses a finished stage or silently loses a child issue.

**Independent Test**: Feed each header-case variant to the `FABRIK_PR_CREATE` parser and to the `FABRIK_SPAWN_CHILD` parser and compare the parsed result with the canonical-spelling result. No GitHub or Claude access is needed.

**Acceptance Scenarios**:

1. **Given** a `FABRIK_PR_CREATE` block headed `title: Add X`, **When** it is parsed, **Then** the title is `Add X` and the body is the remaining content.
2. **Given** a `FABRIK_PR_CREATE` block headed ` TITLE : Add X`, **When** it is parsed, **Then** the title is `Add X`.
3. **Given** a `FABRIK_SPAWN_CHILD` block headed `Title: Fix Y` followed by `depends_on: 3`, **When** it is parsed, **Then** the child has title `Fix Y` and the dependency `3`, identical to the upper-case form.
4. **Given** a header key the grammar does not define, **When** the block is parsed, **Then** it is handled exactly as it is today. Only case and whitespace tolerance is added.

---

### User Story 2 - Parse failures show what the parser found (Priority: P1)

When a `FABRIK_PR_CREATE` block cannot be parsed, the posted error quotes the offending line, truncated and sanitised so it renders safely inline, or says the block was empty. An operator can tell what went wrong without opening the raw worker output.

**Why this priority**: Even with tolerant parsing, genuine failures will occur. A diagnosable failure turns a debugging session into a one-glance fix.

**Independent Test**: Parse malformed `FABRIK_PR_CREATE` blocks (first line is not a header; header present but body empty; block entirely empty; very long or backtick-and-newline-laden first line) and assert that the error text contains the sanitised, truncated found line, or states the block was empty.

**Acceptance Scenarios**:

1. **Given** a block whose first non-empty line is `Summary of changes`, **When** parsed, **Then** the error quotes `Summary of changes` as the line found where a header was expected.
2. **Given** a block with a valid title and no body, **When** parsed, **Then** the error says the body is empty and quotes the title line that was found.
3. **Given** a block containing only whitespace, **When** parsed, **Then** the error says the block was empty.
4. **Given** a first line that is very long or contains backticks, newlines or other characters that would break inline rendering, **When** the error is built, **Then** the quoted line is truncated and sanitised.

---

### User Story 3 - A malformed spawn block is surfaced, not dropped (Priority: P1)

A `FABRIK_SPAWN_CHILD` block that still fails to parse after tolerant parsing is logged, and the operator is told through a comment on the item. The comment names the block's index and the offending line. Whether the item pauses follows Plan's decision, matching the `FABRIK_PR_CREATE` behaviour by default.

**Why this priority**: Silent loss of a child issue is the worst outcome in the report, because nothing indicates that anything went wrong.

**Independent Test**: Run the spawn-block parser over output containing a well-formed block, then a malformed block, then another well-formed block. Assert that both well-formed blocks are returned and that the malformed one yields a surfaced error carrying its 1-based index and the offending line. Then assert that the spawn-handling path posts the comment and logs.

**Acceptance Scenarios**:

1. **Given** output containing one spawn block with no recognisable header line, **When** spawn blocks are processed, **Then** a log line and a comment are produced naming block index 1 and the offending line, and no child is created for that block.
2. **Given** output containing two spawn blocks where only the second is malformed, **When** processed, **Then** the comment names block 2.
3. **Given** a block that parses only because of case or whitespace tolerance, **When** processed, **Then** no error is raised and the child is created.

---

### User Story 4 - A parse failure does not discard finished work (Priority: P1)

When the stage's `FABRIK_PR_CREATE` block fails to parse, the engine still commits and pushes the stage's work before it surfaces the error and pauses. Un-pausing therefore does not redo committed work.

**Why this priority**: This removes the costly consequence. Without it, even a diagnosable failure still forces a full stage re-run.

**Independent Test**: Run Implement-stage finalisation against a worktree with uncommitted changes and output containing an unparseable `FABRIK_PR_CREATE`. Assert that the work is committed and pushed, the error comment is posted and the item is paused.

**Acceptance Scenarios**:

1. **Given** an Implement run with uncommitted work and an unparseable `FABRIK_PR_CREATE`, **When** finalisation runs, **Then** the work is committed and pushed, the error comment is posted and the item is paused.
2. **Given** that same paused item is un-paused after the marker problem is addressed, **When** the stage is retried, **Then** the already-committed work is not lost or re-created.
3. **Given** the commit or push itself fails, **When** a parse failure is also being reported, **Then** the parse error is still posted and the item is still paused, and the push failure is logged.

---

### Edge Cases

- A header key matches more than one spelling in the same block (`TITLE:` and `title:` both present): the first occurrence wins. The behaviour is deterministic and not an error.
- A header line has no colon, or an empty value (`TITLE:` with nothing after it): treated as a missing title. The error quotes the line found.
- A value that itself contains a colon (`TITLE: fix: handle nil`): the key is the text before the first colon only, and the value keeps later colons.
- Windows line endings and tabs around keys are tolerated the same way as spaces.
- A body line that happens to look like a header (`Title: …` deep in the body) is not reinterpreted. Header parsing applies only to the header position, as today.
- Quoted offending lines that contain markdown (backticks, newlines, `@mentions`) must not break or alter the rendering of the posted comment.
- A block with no matching END marker keeps its existing error. This work adds no new behaviour there.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A single shared helper parses `KEY: value` header lines for both `FABRIK_PR_CREATE` and `FABRIK_SPAWN_CHILD`. Neither marker keeps its own copy of the header-line logic.
- **FR-002**: Header keys match case-insensitively for every header key either marker defines (including `TITLE` and `DEPENDS_ON`), and leading, trailing and around-colon whitespace is tolerated.
- **FR-003**: Parsing is otherwise unchanged. Marker names, block delimiters, defined header keys and the title/body split are as they are today. Case and whitespace tolerance is the only grammar change.
- **FR-004**: Every `FABRIK_PR_CREATE` parse error quotes the line the parser actually found, or states that the block was empty. This covers the missing-title error and the empty-body error.
- **FR-005**: Any line quoted in a parse error is truncated to a bounded length and sanitised for safe inline rendering (backticks, newlines and control characters neutralised), consistent with how other engine comments quote denied commands.
- **FR-006**: A `FABRIK_SPAWN_CHILD` block that fails to parse is logged and surfaced as a comment on the item. The comment names the block's 1-based index and the offending line, in the same way an unparseable `FABRIK_PR_CREATE` is surfaced.
- **FR-007**: Well-formed spawn blocks in the same output as a malformed one are still processed according to the pause decision recorded in the Assumptions. A malformed block never causes a well-formed sibling to be silently skipped without the operator being told.
- **FR-008**: Whether a malformed spawn block pauses the item is decided in Plan. The default is to match `FABRIK_PR_CREATE`'s existing behaviour (post the error comment and apply `fabrik:paused`) unless Plan documents a reason to differ.
- **FR-009**: When a `FABRIK_PR_CREATE` block fails to parse, the engine still performs its own commit of the stage's work and the push before it posts the error and pauses. The ordering is confirmed in Plan so that un-pausing does not redo committed work.
- **FR-010**: A failure of the commit or push in the parse-failure path does not suppress the parse-error comment or the pause. It is logged.
- **FR-011**: Unit tests cover `title:`, `Title:` and ` TITLE :` for both markers, each `FABRIK_PR_CREATE` parse error including the found line, and a malformed spawn block yielding a surfaced error.
- **FR-012**: A test shows the stage's work is committed and pushed when `FABRIK_PR_CREATE` fails to parse.
- **FR-013**: The PR body mentions #2055 by bare number only, with no closing keyword near it. The PR closes #2091 only. Docs describing the markers (state machine and stage lifecycle docs, if they cover this behaviour) are updated, and `docs/llms-full.txt` is regenerated if a canonical doc page changes.

### Key Entities

- **Marker block**: A `…_BEGIN`/`…_END` delimited region in stage output, made of header lines followed by free-form body content.
- **Header line**: A `KEY: value` line at the head of a marker block.
- **Parse failure report**: The comment and log line produced when a block cannot be parsed. It names the marker (and block index for spawn blocks) and quotes the sanitised found line.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For both markers, blocks whose header keys differ from the canonical spelling only by letter case or surrounding whitespace parse to results identical to the canonical form, with 100% of the tested variants passing.
- **SC-002**: Every `FABRIK_PR_CREATE` parse error message an operator can see contains either the quoted found line or the statement that the block was empty. No error remains that says only "missing TITLE line" or "PR body is empty".
- **SC-003**: Zero `FABRIK_SPAWN_CHILD` blocks are dropped without an operator-visible comment and a log line.
- **SC-004**: After a `FABRIK_PR_CREATE` parse failure, the stage's work is present on the remote branch, so un-pausing does not require re-running the stage's work.
- **SC-005**: No existing marker tests regress, and `go test -race ./...` and `go vet ./...` pass.

## Assumptions

- The grammar stays as is apart from case and whitespace tolerance, as the issue's scope states. New header keys, aliases and alternative delimiters are not added.
- "Surfaced the same way as `FABRIK_PR_CREATE`" means a self-authored `🏭 **Fabrik — …**` comment on the item plus a log line, using the engine's existing comment path, so it is never mistaken for human steering.
- The default for a malformed spawn block is to pause, mirroring `FABRIK_PR_CREATE`. A malformed declaration means an intended child was not created, and continuing silently would hide that. Plan may refine this, for example by deciding what happens to well-formed sibling blocks in the same output, and must record its reasoning.
- The safe ordering for R4 (commit and push, then error comment and pause) is what the issue expects. Plan confirms it against the existing finalisation order and the draft-PR creation sequencing, since PR creation needs the pushed branch.
- Quoting limits and sanitisation follow the existing convention for inline-quoted text elsewhere in the engine, rather than introducing a new one.
- The current behaviour of leaving a malformed spawn block visible in the posted Review/Validate comment is revisited by Plan together with this change, since the block is now explicitly reported.

## Out of Scope *(optional)*

- Any change to the marker grammar beyond case and whitespace tolerance (new keys, new delimiters, new value formats).
- Changing which stages may emit either marker.
- Changing the circuit-breaker, retry or cycle-limit behaviour of the pause.
- Closing or editing #2055. It remains human-owned and is closed by hand once a release containing the fix ships.
- Other markers (`FABRIK_STAGE_COMPLETE`, `FABRIK_ISSUE_UPDATE`, `FABRIK_SUMMARY`) and their parsing.

## Source References *(optional)*

- #2055 — originating community report (mentioned for context; not closed by this work).
- `engine/prcreate.go` — `FABRIK_PR_CREATE` parser and its error messages.
- `engine/spawn.go` — `FABRIK_SPAWN_CHILD` parser and block scanning.
- `engine/item.go` — handling of a `FABRIK_PR_CREATE` parse failure during stage finalisation.
