# Feature Specification: Pruefer resumes the review until every staged entry is read (bounded coverage loop)

**Feature Branch**: `fabrik/issue-2375`
**Created**: 2026-10-17
**Status**: Specified
**Input**: User description: "## Problem

On a large PR, Pruefer's reviewer can leave staged diff entries unread and still post the review. #2362 fixed the cause where the CLI cut output to a preview. What remains is the reviewer choosing to stop.

On PR #2368 (28 entries, skill v0.1.1, Sonnet at `effort: medium`), three reviews of successive heads gave:

| Head | Unread | Turns | Output tokens | End |
|---|---|---|---|---|
| `d8e7f0ed` | 19 of 28 | 18 | 6k | `completed` |
| `562b763f` | 24 of 28 | 8 | 2k | `completed` |
| `ae274e3d` | 2 of 28 | 20 | 8.5k | `completed` |

- No turn or wall-time limit was hit.
- None of the partial results came from a `<persisted-output>` preview: the `coverage: partial` lines carry no preview count.
- Each partial review said up front which files it had read and which it had skipped (docs, ADR, spec, tests, and in one case the plugin-copy code).
- The `562b763f` review also said \"Earlier threads already cover many similar gaps\": it treated the pass as a delta on the core file and stopped after one new finding.

Three things lead to this:

1. **The instructions conflict.** The prompt (`renderDynamicContext`) and `INDEX.txt` say to open every entry. The review skill's \"Finish the review\" paragraph says to \"say so explicitly and name what you could not check… State what you examined\", which the model reads as permission to stop early as long as it says so.
2. **The model decides when it is done.** The same model, prompt and PR read 4 entries on one pass and 26 on another.
3. **Nothing pushes back.** `computeCoverage` knows exactly which entries are unread, but that only changes a log line and the posted coverage note.

Evidence: `.pruefer/transcripts/20261017T0419*`, `…T0421*` and `…T0426*` for `pr2368`.

## Requirements

- **R1. Coverage loop.** When a pass ends with partial coverage, Pruefer resumes the same Claude session with a follow-up message that:
  - lists the unread entry IDs (with their paths);
  - tells the reviewer to open them under the same reading rules;
  - asks it to emit the complete output contract again (summary, findings, coverage), revised for what it has now read.

  The output contract and its parser stay unchanged. The loop stops at the first of:
  - (a) coverage is complete;
  - (b) a pass opened no entry that had been unread before it;
  - (c) the extra-pass cap is reached. The cap is an operator setting with a default of 2; 0 disables the loop and gives today's behaviour byte for byte.
- **R2. One posted review per head.**
  - Only the final pass's output is posted. Earlier passes post nothing.
  - Coverage is computed over the whole session, so an entry read in any pass counts.
  - If the loop stops on (b) or (c), the review still names the unread entries as it does today.
- **R3. Resuming must be safe.**
  - The follow-up keeps exactly the same tool grant, plugin set, environment and permission mode as the first pass.
  - A resume failure (the session cannot be resumed, or the CLI errors) ends the loop and posts the best complete output so far. It never posts nothing because a later pass failed.
  - The skill-fallback re-run (ADR 2307) and the loop must not multiply: decide and document how they compose, and bound the total number of invocations per review.
- **R4. Session persistence.** Resuming needs the session saved for the length of the review. #2363 plans `--no-session-persistence` and a transcript purge. Make the two compatible:
  - either persist only until the review ends and then remove the profile session;
  - or have #2363's design leave room for the loop.

  Record which in the ADR.
- **R5. Skill wording.** Change the \"Finish the review\" paragraph so naming what you skipped is not a valid way to finish:
  - unread staged entries must be opened;
  - \"say what you could not check\" applies to analysis the reviewer could not complete, not to entries it chose not to open.

  Bump the skill version and keep `TestSkillBodyIsTheOriginalDefaultGuidance` meaningful.
- **R6. Record.** Log one line per review with:
  - the number of passes;
  - the entries opened in each pass;
  - the reason the loop stopped (complete / no progress / cap / resume failure / disabled);
  - the turns and cost summed across passes.

  The transcript saved under ADR 2303 holds the whole session.

## Scope

- Pruefer only (`pruefer/`), plus its README and an ADR numbered after this issue.
- Out of scope:
  - changing the model or effort defaults;
  - restarting with fresh context (keeping state on disk instead of resuming): note it as the fallback for a session too large to resume;
  - the patch-identity skip (#2230), whose behaviour should simply improve once coverage is more often complete.

## Acceptance

- Unit tests with a scripted CLI cover:
  - a partial first pass completed by one resume;
  - a no-progress stop;
  - a cap stop;
  - a resume failure posting the first pass's output;
  - cap 0 giving byte-identical arguments and output to today;
  - coverage counted across passes;
  - the skill-fallback interaction bounded.
- Each new test is shown to fail with its behaviour neutralised.
- Opt-in live probe: a resumed `claude` session for a review keeps its tool grant, and an unread entry opened in pass 2 is credited.
- After merge, verified on the dev daemon: the next PR with 25+ entries that is partial after pass 1 reaches complete, or logs why it stopped.

Mentioned for context: #2362, #2363, #2230, #2368."

## Background

Pruefer reviews large PRs by staging the diff as numbered entries (`INDEX.txt` plus one file per entry) and telling the reviewer to open every one. After a pass, `computeCoverage` compares the reviewer's report with what the stream-json transcript proves was read (ADR 2190, ADR 2303). Today a `partial` verdict changes only a log line and the coverage note in the posted review; the review is posted anyway.

#2362 removed one cause of partial coverage: the CLI cutting a Bash result to a 2 KB `<persisted-output>` preview. What is left is the reviewer choosing to stop. On PR #2368 (28 staged entries, review skill v0.1.1, Sonnet at `effort: medium`), three reviews of successive heads left 19, 24 and 2 of 28 entries unread:

| Head | Unread | Turns | Output tokens | End |
|---|---|---|---|---|
| `d8e7f0ed` | 19 of 28 | 18 | 6k | `completed` |
| `562b763f` | 24 of 28 | 8 | 2k | `completed` |
| `ae274e3d` | 2 of 28 | 20 | 8.5k | `completed` |

No turn or wall-time limit was hit. None of the partial results came from a preview: the `coverage: partial` lines carry no preview count. Each partial review said up front which files it had read and skipped (docs, ADR, spec, tests, in one case the plugin-copy code). The `562b763f` review also said "Earlier threads already cover many similar gaps", treating the pass as a delta on the core file and stopping after one new finding. Evidence is in `.pruefer/transcripts/20261017T0419*`, `…T0421*` and `…T0426*` for `pr2368`.

Three things lead to this:

1. **The instructions conflict.** The prompt and `INDEX.txt` say to open every entry, but the review skill's "Finish the review" paragraph says to "say so explicitly and name what you could not check… State what you examined". The model reads that as permission to stop early as long as it says so.
2. **The model decides when it is done.** The same model, prompt and PR read 4 entries on one pass and 26 on another.
3. **Nothing pushes back.** The unread set is known exactly but is not acted on.

The fix has two parts: make the skill wording stop excusing unread entries, and add a bounded loop in which Pruefer resumes the same session and asks the reviewer to open exactly the entries it missed.

Related work: #2362 (preview cause, done), #2363 (planned `--no-session-persistence` and a transcript purge, which interacts with resuming), #2230 (patch-identity skip, which should improve once coverage is more often complete), #2368 (the PR that showed the problem).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A partial first pass is completed by a resume (Priority: P1)

A maintainer's PR has many staged entries. The first review pass opens only some of them. Pruefer resumes the same session, names the unread entries, and the reviewer opens them. Pruefer posts one review whose coverage is complete.

**Why this priority**: This is the whole point of the issue. Without it, a large PR gets a review that says in effect "I did not look at most of this".

**Independent Test**: With a scripted CLI whose first pass opens some entries and whose resumed pass opens the rest, run one review and check that a single review is posted, that it is built from the final pass, and that coverage is complete.

**Acceptance Scenarios**:

1. **Given** a PR with 28 staged entries and a first pass that opened 9, **When** the review finishes pass 1, **Then** Pruefer resumes the same session with a message listing the 19 unread entry IDs and paths, and asks for the complete output contract again.
2. **Given** the resumed pass opened all 19, **When** it ends, **Then** coverage over the whole session is complete, the loop stops with reason `complete`, and exactly one review is posted from the final pass's output.
3. **Given** a first pass with complete coverage, **When** it ends, **Then** no resume happens and behaviour is the same as today.

---

### User Story 2 - The loop is bounded and never loses a review (Priority: P1)

An operator needs the loop to cost a known amount and never to leave a head without a review because a later pass misbehaved.

**Why this priority**: An unbounded loop spends model budget without limit, and a loop that can post nothing is worse than today's partial review.

**Independent Test**: With a scripted CLI, drive a no-progress pass, a cap stop, and a resume failure, and check the stop reason and the posted output in each.

**Acceptance Scenarios**:

1. **Given** a resumed pass that opens none of the previously unread entries, **When** it ends, **Then** the loop stops with reason `no progress` and the review is posted naming the still-unread entries as today.
2. **Given** an extra-pass cap of 2 and a reviewer that opens some, but not all, of the remaining entries each time, **When** the second extra pass ends, **Then** the loop stops with reason `cap` and no third pass is started.
3. **Given** a first pass with a complete output contract and partial coverage, **When** the resume fails (session cannot be resumed, or the CLI errors), **Then** the loop stops with reason `resume failure` and the first pass's output is posted.
4. **Given** the cap is set to 0, **When** a review runs, **Then** the CLI arguments and the posted output are byte-identical to today's and the log records reason `disabled`.

---

### User Story 3 - The resumed pass has exactly the same authority (Priority: P1)

A resumed session must not become a way to widen what the reviewer may do, or to lose what the first pass was given.

**Why this priority**: Pruefer reads untrusted PR content. A resume that dropped or widened the tool grant, changed the plugin set or environment, or changed the permission mode would be a security regression.

**Independent Test**: Compare the arguments, environment and plugin set of the first and resumed invocations in a scripted-CLI test; run the opt-in live probe against the real `claude` CLI.

**Acceptance Scenarios**:

1. **Given** a first pass and a resumed pass for one review, **When** their invocations are compared, **Then** the tool grant, plugin directories, environment and permission mode are identical.
2. **Given** the opt-in live probe, **When** a real resumed session reviews a PR, **Then** the resumed session still enforces its tool grant, and an entry first opened in pass 2 is credited in coverage.

---

### User Story 4 - The skill no longer excuses unread entries (Priority: P2)

The review skill's closing guidance stops presenting "name what you skipped" as a valid way to finish.

**Why this priority**: It removes the conflict with the prompt and should reduce how often a resume is needed, but the loop works without it, so it comes second.

**Independent Test**: Read the skill body and the tests that pin it.

**Acceptance Scenarios**:

1. **Given** the updated skill, **When** a reviewer reads "Finish the review", **Then** it says that unread staged entries must be opened, and that "say what you could not check" applies to analysis the reviewer could not complete and not to entries it chose not to open.
2. **Given** the skill change, **When** the tests run, **Then** the skill version has been bumped and `TestSkillBodyIsTheOriginalDefaultGuidance` still fails on an unrelated edit to the default guidance.

---

### User Story 5 - Each review leaves one record of what the loop did (Priority: P2)

An operator debugging a review can tell how many passes ran, what each opened, and why the loop stopped.

**Why this priority**: The acceptance check on the dev daemon depends on the loop explaining itself.

**Independent Test**: Run reviews that end for each stop reason and check the one log line.

**Acceptance Scenarios**:

1. **Given** any review, **When** it finishes, **Then** one log line names the number of passes, the entries opened in each pass, the stop reason (`complete`, `no progress`, `cap`, `resume failure` or `disabled`), and the turns and cost summed across passes.
2. **Given** a review that ran several passes and whose transcript is kept under ADR 2303, **When** the transcript is read, **Then** it holds the whole session, not only the last pass.

---

### Edge Cases

- A review with no staged diff, or one whose first pass is already complete, starts no resume.
- A review that fails on the first pass behaves exactly as today; the loop starts only after a pass that produced a complete, parseable output contract with partial coverage.
- A resumed pass that produces an unparseable or incomplete output contract: the best complete output so far (an earlier pass) is posted, and the stop reason is recorded as a resume failure.
- A resumed pass that opens only a subset of the unread entries counts as progress and continues, up to the cap.
- A pass that re-reads entries that were already read does not count as progress.
- The reviewer claims in text that it read the missing entries without opening them: coverage still comes from the transcript, so the entries stay unread.
- The skill-fallback re-run (ADR 2307) fires on a skill load failure: the total invocations per review stay bounded, however the fallback and the loop compose.
- A session too large to resume is treated as a resume failure; the first pass's output is posted.
- With `keep_transcripts: off`, the loop still works; only the saved file is absent.
- A forced re-review of the same head, or a re-review of a new head, starts its own loop and passes count.
- The patch-identity skip (#2230) is unaffected: it decides whether to review, not how many passes a review takes.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a review pass ends with partial coverage and a complete, parseable output contract, Pruefer MUST resume the same Claude session with a follow-up message that lists the unread staged entry IDs together with their paths, tells the reviewer to open them under the same reading rules as the first pass, and asks it to emit the complete output contract again, revised for what it has now read.
- **FR-002**: The output contract and its parser MUST be unchanged. A resumed pass emits the same contract as the first.
- **FR-003**: The loop MUST stop at the first of: (a) coverage over the whole session is complete; (b) a pass opened no entry that was unread before that pass; (c) the extra-pass cap has been reached.
- **FR-004**: The extra-pass cap MUST be an operator-level setting with a default of 2. A value of 0 MUST disable the loop and MUST give the same CLI arguments and the same posted output as before this change, byte for byte. A negative value MUST be refused at startup, as other operator numeric settings are. The setting MUST NOT be controllable from the repo-resident config of a reviewed repository (ADR 1642).
- **FR-005**: Exactly one review MUST be posted per head. Only the final pass's output is posted; earlier passes post nothing.
- **FR-006**: Coverage MUST be computed over the whole session, so an entry read in any pass counts as read.
- **FR-007**: When the loop stops on no-progress or on the cap, the posted review MUST still name the unread entries as it does today.
- **FR-008**: The resumed pass MUST keep exactly the same tool grant, plugin set, environment and permission mode as the first pass. A resume MUST NOT widen or drop any of them.
- **FR-009**: A resume failure (the session cannot be resumed, the CLI errors, or the resumed output has no complete output contract) MUST end the loop and MUST post the best complete output obtained so far. A later pass failing MUST NOT cause nothing to be posted.
- **FR-010**: The skill-fallback re-run (ADR 2307) and the coverage loop MUST compose in a documented way, and the total number of CLI invocations for one review MUST be bounded by a documented maximum that is stated in the ADR and pinned by a test.
- **FR-011**: The session MUST remain resumable for the length of a review. The design MUST be compatible with #2363's `--no-session-persistence` and transcript purge, either by keeping the session only until the review ends and then removing it from the Claude profile, or by recording what #2363's design must leave room for. The ADR MUST record which choice was made. In either case a session MUST NOT remain in the operator's Claude profile after the review ends because of this feature.
- **FR-012**: The review skill's "Finish the review" paragraph MUST be reworded so that naming what was skipped is not a valid way to finish: unread staged entries must be opened, and "say what you could not check" applies to analysis the reviewer could not complete, not to entries it chose not to open.
- **FR-013**: The embedded review skill's version MUST be bumped, and `TestSkillBodyIsTheOriginalDefaultGuidance` MUST remain a meaningful check that fails on an unintended change to the default guidance.
- **FR-014**: Each review MUST log one line recording the number of passes, the entries opened in each pass, the stop reason (`complete`, `no progress`, `cap`, `resume failure` or `disabled`), and the turns and cost summed across passes.
- **FR-015**: When transcripts are kept (ADR 2303), the saved transcript MUST contain the whole session across all passes.
- **FR-016**: The summed turns and cost reported for a review MUST include all passes, and the existing turn and wall-time limits MUST continue to apply to each invocation.
- **FR-017**: Behaviour MUST be unchanged for a review whose first pass has complete coverage, whose cap is 0, or which has no staged entries.
- **FR-018**: Unit tests with a scripted CLI MUST cover: a partial first pass completed by one resume; a no-progress stop; a cap stop; a resume failure posting the first pass's output; cap 0 giving byte-identical arguments and output; coverage counted across passes; and the skill-fallback interaction bounded. Each new test MUST be shown to fail when the behaviour it covers is neutralised.
- **FR-019**: An opt-in live probe (like the existing live probes) MUST show that a resumed `claude` session for a review keeps its tool grant and that an entry first opened in pass 2 is credited.
- **FR-020**: The Pruefer README (`cmd/pruefer/README.md`) MUST document the new setting, its default, and how it relates to the transcript and session settings. An ADR numbered `2375` MUST record the decisions, including the fallback of keeping state on disk instead of resuming for a session too large to resume.

### Key Entities *(if applicable)*

- **Review pass**: One CLI invocation within a review. The first pass is the existing review; later passes are resumes of the same session.
- **Session coverage**: The set of staged entries the transcript proves were opened, taken across all passes of one review.
- **Extra-pass cap**: The operator setting bounding how many resume passes may follow the first.
- **Stop reason**: One of `complete`, `no progress`, `cap`, `resume failure`, `disabled`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the scripted-CLI tests, a review whose first pass leaves entries unread and whose resumed pass opens them posts exactly one review with complete coverage.
- **SC-002**: For every review, the number of CLI invocations is at most the documented maximum, regardless of the reviewer's behaviour or the skill-fallback path.
- **SC-003**: With the cap at 0, the CLI arguments and posted output are byte-identical to the pre-change behaviour in the test that pins it.
- **SC-004**: In every scripted failure of a resumed pass, a review is still posted, built from the best complete earlier output.
- **SC-005**: After merge, on the dev daemon, the next PR with 25 or more staged entries that is partial after pass 1 either reaches complete coverage or logs the reason the loop stopped.
- **SC-006**: Each review produces exactly one log line carrying passes, entries per pass, stop reason, and summed turns and cost.
- **SC-007**: Each new test fails when its behaviour is neutralised, shown in the PR.

## Assumptions

- Resuming a session with the Claude CLI preserves the tool grant, plugin set and permission mode when the same arguments are passed again; the opt-in live probe verifies this and the spec does not rely on it without that check.
- A resume that carries the full session context costs more per pass than the first, but the default cap of 2 bounds the extra cost; the default is an operator-adjustable setting.
- Only the final pass's output is posted, so a final pass that drops findings from an earlier pass would lose them; the follow-up asks for the complete contract "revised for what it has now read", and this risk is accepted. Merging findings across passes is not attempted.
- Coverage is judged from the transcript as it is today (ADR 2190, ADR 2303); how entries are credited is unchanged.
- The follow-up message is built from the same staged-entry data the first pass was given; the exact wording is for the Plan stage.
- The loop applies only to passes that end with a parseable output contract; passes that fail outright keep today's handling.
- Which of the two R4 options is chosen is a design decision for the Plan stage; either satisfies FR-011 as long as the ADR records it.
- The setting follows the existing operator-setting convention for Pruefer (flag, environment variable, YAML, validated fail-loud); exact names are chosen in the Plan stage.

## Out of Scope *(optional)*

- Changing the model or effort defaults.
- Restarting with fresh context (keeping state on disk instead of resuming) for a session too large to resume; it is noted in the ADR as the fallback only.
- The patch-identity skip (#2230); its behaviour should improve once coverage is more often complete.
- Merging findings from earlier passes into the final pass's output.
- Changing how coverage credits reads, the staged-entry batch limit (#2362), or the transcript retention policy (#2363), apart from keeping them compatible with resuming.

## Source References *(optional)*

- #2362: staged-entry batch limit and preview diagnostic (ADR 2362).
- #2363: planned `--no-session-persistence` and transcript retention.
- #2230: patch-identity skip (ADR 2230).
- #2368: the PR on which the problem was observed.
- ADR 2190 (diff coverage contract), ADR 2303 (transcript retention), ADR 2307 (operator and base-ref review plugins, skill fallback), ADR 1642 (reviewed repo must not control operator settings).
- `.pruefer/transcripts/20261017T0419*`, `…T0421*`, `…T0426*` for `pr2368`.
