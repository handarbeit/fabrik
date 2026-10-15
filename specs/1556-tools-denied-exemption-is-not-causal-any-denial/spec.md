# Feature Specification: Make the tools-denied exemption causal

**Feature Branch**: `fabrik/issue-1556`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "tools-denied exemption is not causal: any denial in a non-completing run exempts it from max_retries

`classifyToolsDenied` (`engine/claude.go`, shipped in #1523 / ADR-1523) exempts an invocation from `max_retries` whenever the CLI's `permission_denials` array is non-empty. `interpretClaudeResult` gates this only on `!completed`. Neither establishes that the denial is *why* the stage failed to complete — only that a denial occurred at some point during the run.

Consequence: a model that hits one denied tool early, works around it, and then fails to complete for a wholly unrelated reason — a real bug in the change, a stall, an underspecified issue — has the entire invocation misclassified as an environmental permission problem. It is exempted from `max_retries`, and the operator is handed a pause message directing them to hunt for a `PreToolUse` hook or settings misconfiguration that does not exist.

Narrow the tools-denied exemption so it applies when a permission denial plausibly *caused* the non-completion, rather than whenever any denial occurred anywhere in the run — without regressing the real case #1523 fixed (a genuine misconfiguration denying every mutating call)."

## Background

#1523 / ADR-1523 taught Fabrik that a stage which cannot complete because the CLI's permission layer denied its tool calls is blocked, not failed. Such a run does not count against `max_retries`; it is bounded instead by its own `MaxToolsDeniedRetries` counter (default 3), and at that bound the item is paused with a message pointing the operator at a permission misconfiguration. ADR-1704 extended the bound to the reinvoke paths and ADR-1775 made the denial command-scoped in its messaging.

The detection signal is **structural**: a parsed CLI field (`permission_denials`), not output prose. This matches the `fabrik:claude-limit` precedent (ADR-1119 / ADR-1183), which keys on `terminal_reason`. The signal is reliable. The gap is its **causal scope**: today any non-empty `permission_denials` on a non-completing run triggers the exemption, whether or not the denial had anything to do with the non-completion.

Cost of the gap, bounded but real:

- A run that hit one denial early, worked around it, then failed for an unrelated reason (a genuine bug, a stall, an underspecified issue) is exempted from `max_retries`.
- It is retried up to `MaxToolsDeniedRetries` times outside the `max_retries` budget.
- It ends in a pause whose message sends the operator to look for a permission misconfiguration that does not exist. This is Fabrik asserting something false about the cause of a failure, the same defect class as the other "trust the instruments" issues in the 0.1.0 milestone.

Settled and out of scope here: the `--disallowedTools` interaction was checked empirically during review of #1549. On CLI 2.1.228 a tool blocked via `--disallowedTools` yields `permission_denials: []` even on a non-completing run, so Fabrik's unconditional `--disallowedTools ScheduleWakeup Workflow` (ADR-1365) cannot trigger the exemption. This issue concerns causality only.

**Known future input (triage note, milestone 0.1.0).** This issue blocks #1372 (stage tool restrictions and worktree boundary enforcement). #1372's leading enforcement candidate is a `PreToolUse` hook, which *produces denials by design and routinely*, every time a stage bumps against the worktree boundary. Once that lands:

- Denials stop being rare, so a non-causal exemption would misfire constantly instead of occasionally.
- A Fabrik-originated boundary denial is the system working correctly, not a misconfiguration. The current logic cannot tell the two apart, so the regression guard for #1523's incident would be corrupted.

The narrowed rule must therefore be designed to stay correct when Fabrik's own enforcement becomes a routine source of denials. Of the four candidate directions in the issue, denied-tool relevance and progress correlation survive this most cleanly; proportion is the most threatened, since routine boundary denials inflate the denominator. Choosing the mechanism is deferred to Research/Plan.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Incidental denial does not mask an ordinary failure (Priority: P1)

An operator runs a stage whose model hits one denied tool call early, works around it, and then fails to complete for an unrelated reason. The invocation is treated as an ordinary failed attempt and counts against `max_retries`. If the stage keeps failing, it escalates through the normal failure path rather than the tools-denied path.

**Why this priority**: This is the defect. Each wrongly exempted run costs extra invocations and sends the operator to the wrong place.

**Independent Test**: Simulate an invocation whose response has a single early denial plus a non-completing outcome unrelated to it, and assert `StageRetryIncremented` is recorded and the tools-denied label, counter and pause are not applied.

**Acceptance Scenarios**:

1. **Given** a non-completing run containing exactly one denial early in the run and otherwise successful tool use, **When** the invocation result is interpreted, **Then** the run is classified as an ordinary failure and `StageRetryIncremented` is recorded.
2. **Given** the same run, **When** the item eventually exhausts `max_retries`, **Then** it escalates through the ordinary failure path, and no message tells the operator to look for a permission misconfiguration.

---

### User Story 2 - Genuine permission misconfiguration stays exempt (Priority: P1)

An operator has a settings or hook misconfiguration that denies the stage's essential tools, as in #1523's original incident. The stage cannot make progress, and the run is exempt from `max_retries`, exactly as today.

**Why this priority**: This is the regression guard. The feature exists for this case, and narrowing must not break it.

**Independent Test**: Simulate an invocation reproducing #1523's shape (denials on the stage's essential mutating tools, no progress) and assert `StageAttempted` is recorded without `StageRetryIncremented`, and that the tools-denied path is taken.

**Acceptance Scenarios**:

1. **Given** a non-completing run in which the stage's essential tools were denied and the stage made no progress, **When** the result is interpreted, **Then** the run is exempt from `max_retries` and counts against `MaxToolsDeniedRetries`.
2. **Given** that run reaches `MaxToolsDeniedRetries`, **When** the bound is hit, **Then** the item is paused with the permission-misconfiguration guidance as today.

---

### User Story 3 - Messages match the classification actually made (Priority: P2)

An operator reading a pause or explanatory comment is told only what Fabrik actually established. A permission-misconfiguration message appears only for runs classified as denial-caused.

**Why this priority**: A false cause is worse than a missing one because it misdirects the operator's investigation.

**Independent Test**: Drive one run through each classification and assert the comment text for the tools-denied path is never produced for the ordinary-failure classification.

**Acceptance Scenarios**:

1. **Given** a run classified as an ordinary failure despite containing a denial, **When** Fabrik posts any comment about it, **Then** the comment does not direct the operator to a `PreToolUse` hook or a settings misconfiguration as the cause.
2. **Given** a run classified as denial-caused, **When** Fabrik posts its comment, **Then** the existing guidance (ADR-1775 wording) is unchanged.

---

### User Story 4 - Rule stays correct once Fabrik's own enforcement denies routinely (Priority: P2)

When #1372 later makes Fabrik itself a routine source of denials (for example, boundary hooks), an ordinary failing run that merely brushed the boundary is not exempted just because denials are frequent.

**Why this priority**: The narrowing is a prerequisite for #1372. A rule that only works while denials are rare would have to be redone immediately.

**Independent Test**: Simulate a non-completing run with several incidental denials of non-essential or worked-around calls, in which the stage made progress (for example, produced a commit), and assert it is treated as an ordinary failure.

**Acceptance Scenarios**:

1. **Given** a non-completing run with multiple denials where the stage produced a commit, **When** the result is interpreted, **Then** the run is not exempt and `StageRetryIncremented` is recorded.

---

### Edge Cases

- A run with denials on its final turn, where the stage then cannot proceed: this is the plausible-cause shape and should stay exempt unless the chosen rule shows the stage progressed.
- A run with several denials of the same essential tool, interleaved with some successful calls of other tools.
- A run whose only denials are on non-mutating or peripheral tools, with the stage failing for another reason.
- The comment/reinvoke paths (`processComments`, `ci-fix-reinvoke`, review-reinvoke), which share the classification via `recordToolsDeniedDetection` (ADR-1704): the narrowed rule must apply identically on every invocation type.
- Missing, malformed or truncated `permission_denials` entries or run data: the rule must not exempt on insufficient evidence (see FR-005).
- A run that completes (`FABRIK_STAGE_COMPLETE`) despite denials is unaffected, since the exemption already requires non-completion.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A non-completing run containing a single incidental denial followed by unrelated non-completion MUST be treated as an ordinary failure and MUST increment the stage retry count against `max_retries`.
- **FR-002**: A non-completing run in which a genuine misconfiguration denies the stage's essential tools, reproducing #1523's incident, MUST remain exempt from `max_retries` and bounded by `MaxToolsDeniedRetries`, exactly as before.
- **FR-003**: The exemption MUST apply only when a permission denial plausibly caused the non-completion, not merely when a denial occurred anywhere in the run.
- **FR-004**: The tools-denied explanatory and pause comments MUST be produced only for runs classified as denial-caused, and the text MUST NOT direct an operator to look for a permission misconfiguration for any other classification.
- **FR-005**: When the available evidence cannot establish causality, the classification MUST bias toward the ordinary-failure path (under-exemption). A genuine failure wrongly exempted costs several wasted invocations and misleads the operator; a permission failure wrongly counted costs one retry.
- **FR-006**: Detection MUST remain structural (parsed CLI fields and run data). No string-matching on stage output prose may be introduced.
- **FR-007**: The narrowed rule MUST apply identically on the stage-dispatch path and on the comment/reinvoke paths that share the classification.
- **FR-008**: The rule MUST NOT depend on denials being rare. It must remain correct if Fabrik's own enforcement (#1372) later produces routine denials, which are not misconfigurations.
- **FR-009**: The chosen heuristic MUST be documented at the decision point in code, stating its false-positive and false-negative direction explicitly.
- **FR-010**: ADR-1523 MUST be amended to record the narrowed rule, its rationale and its error-direction bias.
- **FR-011**: Tests MUST show the new behavior is exercised rather than vacuous: the single-incidental-denial test (FR-001) MUST be shown failing against current `main` before it is shown passing, and MUST go red again when the narrowing is neutralized in isolation.

### Key Entities *(if applicable)*

- **Permission denial record**: an entry in the CLI result's `permission_denials` array, carrying the denied tool and, for Bash, the denied command's input.
- **Denial-caused classification**: the engine's determination that a denial plausibly prevented the stage from completing, as opposed to an incidental denial in a run that failed for another reason.
- **`max_retries` budget vs `MaxToolsDeniedRetries` budget**: the two separate counters that the classification selects between.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A simulated run with one early denial and unrelated non-completion records a stage retry increment (AC1), and that test is red on current `main`.
- **SC-002**: A simulated run reproducing #1523's shape (denials on the stage's essential tools) records a stage attempt without a retry increment (AC2).
- **SC-003**: Neutralizing the narrowing in isolation turns the SC-001 test red, demonstrating it is not vacuous (AC3).
- **SC-004**: In 100% of runs classified as ordinary failures, no comment posted by Fabrik attributes the failure to a permission misconfiguration.
- **SC-005**: ADR-1523 contains the narrowed rule and its stated error-direction bias (AC4).
- **SC-006**: A simulated run with several incidental denials and a produced commit is classified as an ordinary failure, showing the rule does not depend on denial rarity.

## Assumptions

- `permission_denials` remains the right and sufficient structural source of denial facts; any extra inputs the rule needs (turn ordering, tool identity, progress such as commits) come from already-parsed run data, not new prose parsing.
- The mechanism is chosen in Research/Plan. The four candidate directions (recency/adjacency, proportion, denied-tool relevance, progress correlation) are non-prescriptive, and a combination is acceptable.
- Because the tie-break favors under-exemption, a mixed or ambiguous run is treated as an ordinary failure. This is a deliberate, documented trade-off.
- `MaxToolsDeniedRetries` (default 3) and its flags are unchanged.
- Existing ADR-1704 and ADR-1775 behavior for runs that remain denial-caused (counter bound, command-scoped messaging) is preserved.

## Out of Scope *(optional)*

- Changing the structural detection signal itself (`permission_denials` is the right source).
- Changing `MaxToolsDeniedRetries`'s value or configuration.
- The `--disallowedTools` interaction, which is settled empirically and not a false-positive source.
- Implementing #1372's enforcement hooks, or classifying Fabrik-originated denials as a distinct category. The rule only needs to be robust to them.
- Any change to the `fabrik:claude-limit` or other non-permission exemptions.

## Source References

- #1523 / ADR-1523: original exemption and detection design
- ADR-1704: tools-denied bound covers reinvoke paths
- ADR-1775: command-scoped tools-denied messaging
- ADR-1365: `--disallowedTools`, emitted unconditionally
- ADR-1119 / ADR-1183: `fabrik:claude-limit`, the structural-detection precedent
- #1372: tool-restriction enforcement, which this issue blocks
- Found during independent review of PR #1549: https://github.com/handarbeit/fabrik/pull/1549#issuecomment-5259935626
