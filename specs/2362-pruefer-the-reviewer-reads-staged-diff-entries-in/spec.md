# Feature Specification: Pruefer reviewer reads staged diff entries in amounts the CLI returns whole

**Feature Branch**: `fabrik/issue-2362`
**Created**: 2026-10-16
**Status**: Specified
**Input**: User description: "pruefer: the reviewer reads staged diff entries in batches the CLI truncates to a 2 KB preview

## Problem

The reviewer opens the staged diff by batching many entries into one shell command (`cat 0001.diff 0002.diff …`, often `cd <dir>; cat …`). When the output of one Bash call is larger than about 30 KB, the Claude CLI does not return it to the model. Instead it saves the full output to a file in the session directory and returns only `<persisted-output> Output too large (62.1KB) … Preview (first 2KB)`. The reviewer therefore sees about 2 KB of a 20–60 KB batch and never sees the rest of the entries in it.

Evidence from the transcripts #2303 now keeps (dev Pruefer, `.pruefer/transcripts/`, 2026-10-16):

- 22 of the 50 saved partial-coverage transcripts contain `<persisted-output>` results.
- On #2360 at head `4faf9c9e`, five of the reviewer's 13 tool calls were multi-entry `cat` batches. Four of them came back as 2 KB previews, and coverage reported 22 of 30 entries unread. That review admitted it was partial. On #2191 the review claimed full coverage under the same conditions.

The coverage check (ADR 2190) is right in these cases: the entries were not seen. The fault is in how the reviewer is told to read them.

It also limits #2230. Of 53 patch-identity checks since that change landed, 44 reviewed again only because the previous review was partial. A partial review blocks the skip by design (FR-008).

## Requirements

- R1. The staging instructions in `renderDynamicContext` (`pruefer/claude.go`) and the review skill (`pruefer/reviewplugin/plugin/skills/review/SKILL.md`) must lead the reviewer to open entries in amounts the CLI returns whole: one entry per `Read`, or a `cat` of entries whose combined size, as listed in INDEX.txt, stays under a stated limit. Pick the limit with a margin below the CLI's threshold and record it in one place.
- R2. Tell the reviewer that a `<persisted-output>` result means the content was not seen, and that it must open those entries again in smaller pieces. The persisted file is outside the allowed read paths and must stay so.
- R3. The `coverage: partial` log line names how many unread entries were part of a shell result that came back as a preview, so an operator can see this cause without opening the transcript. Detection must be structural: a `tool_result` for a staged-entry command whose content starts with `<persisted-output>`. Do not match on prose.
- R4. `deriveOpened`'s crediting rules stay unchanged: a preview never credits an entry.

## Acceptance

- A unit test with a recorded stream-json fixture that has a `<persisted-output>` result for a multi-entry `cat` shows those entries uncredited and counted under R3's cause.
- A prompt/skill test pins the new reading instruction, including the size limit.
- Check on a live review of a PR with 30+ entries, using the dev daemon after merge: no `<persisted-output>` in its transcript, and coverage complete or partial for some other reason.

## Out of scope

- How `deriveOpened` credits reads.
- `--no-session-persistence` and the session folders left behind (separate issue)."

## Background

Pruefer stages a PR's diff as numbered entries on disk, with an INDEX.txt listing them and their sizes, and the reviewer opens them with its tools. The diff-coverage contract (ADR 2190) checks, from the reviewer's own transcript, which entries it actually opened, and marks the review `coverage: partial` when some were not seen.

The reviewer tends to open many entries in one shell command (`cat 0001.diff 0002.diff …`, often prefixed with `cd <dir>;`). When one Bash call's output exceeds roughly 30 KB, the Claude CLI does not hand it to the model. It writes the full output to a file in the session directory and returns a `<persisted-output>` notice with only the first 2 KB as a preview. The reviewer sees a small slice of a 20–60 KB batch and never sees the remaining entries in it.

Observed impact (transcripts kept by #2303, dev Pruefer, 2026-10-16):

- 22 of 50 saved partial-coverage transcripts contain `<persisted-output>` results.
- On #2360 at head `4faf9c9e`, 5 of the reviewer's 13 tool calls were multi-entry `cat` batches. 4 returned 2 KB previews, and coverage reported 22 of 30 entries unread. That review admitted it was partial. On #2191, the review claimed full coverage under the same conditions.
- The patch-identity skip from #2230 is blocked by a partial review by design (its FR-008). Of 53 patch-identity checks since that change landed, 44 re-reviewed only because the previous review was partial.

The coverage check is behaving correctly in these cases, because the entries were genuinely not seen. The defect is in the instructions that tell the reviewer how to read them. Today an operator also cannot tell from the log that truncated previews were the cause of a partial review without opening the transcript.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The reviewer reads staged entries in amounts the CLI returns whole (Priority: P1)

A reviewer working on a PR with many staged entries is told, in both the generated staging instructions and the review skill, to open entries one per `Read`, or in a `cat` whose combined listed size stays under a stated limit. As a result, no shell result is replaced by a preview and every entry the reviewer opens is actually seen.

**Why this priority**: This removes the cause. Without it, large PRs keep getting partial reviews, and the patch-identity skip cannot apply to them.

**Independent Test**: Render the staging instructions and read the review skill. Both state the batching rule and the same numeric limit. On a live review of a PR with 30 or more entries, the transcript contains no `<persisted-output>`.

**Acceptance Scenarios**:

1. **Given** a review request with staged entries, **When** the dynamic staging context is rendered, **Then** it tells the reviewer to read one entry per `Read`, or to `cat` entries whose combined size as listed in INDEX.txt is under the stated limit.
2. **Given** the review skill, **When** it describes reading the staged diff, **Then** it states the same rule and the same limit.
3. **Given** the limit is changed in its single recorded location, **When** the instructions are rendered again, **Then** the prompt reflects the new value without further edits.

---

### User Story 2 - The reviewer recovers from a preview result (Priority: P1)

If a shell result does come back as `<persisted-output>` anyway, the reviewer knows the content was not seen and opens those entries again in smaller pieces, without trying to read the persisted file.

**Why this priority**: The size rule can still be broken, for instance by a long `cd … ; cat …` chain. Recovery limits the damage to one extra read instead of a partial review.

**Independent Test**: The staging instructions and the skill both state that a `<persisted-output>` result means the content was not seen, and that the entries must be reopened in smaller pieces. The persisted file's location remains outside the allowed read paths.

**Acceptance Scenarios**:

1. **Given** the staging instructions, **When** they are rendered, **Then** they say a `<persisted-output>` result means the entries in that command were not seen and must be opened again in smaller pieces.
2. **Given** the reviewer's allowed read paths, **When** this change is applied, **Then** the CLI's persisted-output file is still not readable by the reviewer.

---

### User Story 3 - An operator sees truncation as the cause of a partial review (Priority: P2)

When a review is logged as `coverage: partial`, the log line also says how many of the unread entries were part of a shell result that came back as a preview, so the operator can name the cause without opening the transcript.

**Why this priority**: It makes the problem visible and measurable after the fix. It is diagnostic only and changes no review behaviour.

**Independent Test**: Run coverage derivation on a recorded stream-json fixture with a `<persisted-output>` result for a multi-entry `cat`. The affected entries are not credited and are counted under the truncation cause in the log line.

**Acceptance Scenarios**:

1. **Given** a transcript where a `cat` of several staged entries produced a `tool_result` whose content starts with `<persisted-output>`, **When** coverage is evaluated, **Then** none of those entries is credited as opened, and the `coverage: partial` line reports how many unread entries were in such truncated results.
2. **Given** a partial review with no `<persisted-output>` results, **When** the line is logged, **Then** the truncation count is zero or absent and the rest of the line is unchanged.
3. **Given** a `tool_result` whose text merely mentions `<persisted-output>` but does not start with it, **When** coverage is evaluated, **Then** it is not counted as a truncated result.

---

### Edge Cases

- A multi-entry command is truncated, and the reviewer later reads some of those entries individually. Entries later opened properly are credited by the existing rules and are not counted as unread, so they are not in the truncation count.
- A single staged entry larger than the limit cannot be split by batching. It is read on its own, with whatever the existing single-`Read` behaviour is, and this change does not alter how it is handled.
- A command mixes staged-entry paths with other paths, and its result is a preview. Only the staged entries named in it are affected.
- A `tool_result` that is a preview but belongs to a command that names no staged entry does not count toward the cause.
- The transcript cannot be used as authoritative (the existing `authoritative=false` case). The truncation count is reported only from what the transcript shows, and the existing authoritativeness reporting is unchanged.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The generated staging instructions (`renderDynamicContext`) must tell the reviewer to open staged entries either one per `Read`, or in a `cat` of entries whose combined size, as listed in INDEX.txt, stays under a stated limit.
- **FR-002**: The review skill must give the same reading rule and the same limit as FR-001.
- **FR-003**: The limit must sit below the CLI's truncation threshold with a stated margin and be defined in exactly one place that both the generated instructions and the skill text derive from or are tested against.
- **FR-004**: The staging instructions and the review skill must state that a `<persisted-output>` result means the content was not seen, and that the reviewer must open those entries again in smaller pieces.
- **FR-005**: The CLI's persisted-output file must remain outside the reviewer's allowed read paths. No read permission is added for it.
- **FR-006**: The `coverage: partial` log line must state how many unread entries were named by a shell command whose result came back as a preview.
- **FR-007**: A preview result is identified structurally: a `tool_result` for a command naming staged entries whose content starts with `<persisted-output>`. Detection must not rely on matching prose elsewhere in the output.
- **FR-008**: The rules by which `deriveOpened` credits an entry as opened are unchanged. A preview result never credits any entry.
- **FR-009**: A unit test using a recorded stream-json fixture with a `<persisted-output>` result for a multi-entry `cat` must show that those entries are uncredited and are counted under the truncation cause.
- **FR-010**: A prompt/skill test must pin the new reading instruction, including the numeric limit.

### Key Entities

- **Staged diff entry**: One numbered file of the staged PR diff, listed with its size in INDEX.txt.
- **Preview result**: A shell tool result that the CLI replaced with a `<persisted-output>` notice and the first 2 KB of output.
- **Batch limit**: The maximum combined listed size of entries in one `cat`, defined once.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a live review of a PR with 30 or more staged entries, run on the dev daemon after merge, the saved transcript contains no `<persisted-output>`, and coverage is complete or partial for a reason other than truncation.
- **SC-002**: For a fixture containing a preview result for a multi-entry `cat`, the entries it named are reported unread and are attributed to truncation in the `coverage: partial` line.
- **SC-003**: The share of saved partial-coverage transcripts that contain `<persisted-output>` results, 22 of 50 at the time of filing, falls close to zero on reviews after the change.
- **SC-004**: Fewer patch-identity checks re-review solely because the previous review was partial; 44 of 53 did so before this change.

## Assumptions

- The CLI returns a Bash result whole only below a threshold of about 30 KB; the batch limit is chosen with a margin below that, and its exact value is a planning decision recorded in one place.
- INDEX.txt already lists each entry's size, so the reviewer can work out a batch's combined size without reading the entries.
- The stream-json transcript marks a preview result with a `tool_result` whose content begins with `<persisted-output>`, as seen in the saved transcripts.
- Diagnostic reporting under FR-006 is additive to the existing log line and does not change whether a review is partial or complete.

## Out of Scope *(optional)*

- How `deriveOpened` credits reads (R4 and FR-008 only require that it is unchanged).
- `--no-session-persistence` and the leftover session folders (a separate issue).
- Making the CLI's truncation threshold configurable or reading the persisted-output file.
- Changing how a single entry larger than the batch limit is staged or read.

## Source References *(optional)*

- ADR 2190 — Pruefer diff coverage contract (`adrs/2190-pruefer-diff-coverage-contract.md`)
- #2303 — saved reviewer transcripts used as evidence
- #2230 — patch-identity skip, blocked by partial reviews (FR-008 of that change)
- #2360, #2191 — reviews cited as examples
