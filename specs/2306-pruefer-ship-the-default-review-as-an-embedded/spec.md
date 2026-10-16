# Feature Specification: Ship Pruefer's default review as an embedded plugin skill (`pruefer-review:review`) and invoke it

**Feature Branch**: `fabrik/issue-2306`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: "Second in the chain replacing #1859.

## Problem

Pruefer's default review is a Go string constant, `defaultReviewGuidance` (`pruefer/claude.go:437`). Operator and repo guidance are composed onto it as text (`composeGuidanceLayer`). There is nothing to invoke, so there is no foundation for review plugins. Fabrik has the same shape solved: an embedded base plugin, `fabrik-stages`, deployed and loaded with `--plugin-dir`, and stages that name skills from it.

## Requirements

- **R1:** Pruefer embeds a base plugin, `pruefer-review`, with one skill, `review`, whose instructions are today's default guidance. At review time the plugin is written to a per-review directory, never a shared mutable one, and passed with `--plugin-dir`.
- **R2:** The reviewer gets the `Skill` tool. The prompt tells it to carry out the review by invoking the resolved review skill, here `pruefer-review:review`. Resolution uses `internal/skillref` (previous issue) against exactly the plugin directories Pruefer passes.
- **R3:** Plugins Pruefer did not pass are never visible: none from the daemon's `CLAUDE_CONFIG_DIR` profile and none from `.claude/` in the reviewed tree. Keep `--setting-sources \"\"`, and add a test that asserts it directly, under two different `CLAUDE_CONFIG_DIR` values.
- **R4:** Pruefer keeps ownership of the output format. The summary block, the findings JSON and the coverage block from #2190 stay in the Go-owned part of the prompt, outside any skill. A review run through the skill must parse exactly as one run today.
- **R5:** Existing guidance keeps working. Operator `review_guidance`/`review_guidance_mode` and a repo's `.pruefer/skills/review/SKILL.md` (append/replace) are still composed. They are passed to the skill as additional guidance, so a repo using only those sees the same review behaviour as before.
- **R6:** Every review's log line names the skill it ran and the plugin it came from.

## Out of scope

- Operator or repo plugins, and selecting a skill other than the default (next issue).
- Any change to the tool grant beyond adding `Skill`.

## Acceptance

- With no operator or repo customization, a review invokes `pruefer-review:review` and posts a review in the same format as today.
- A skill planted in the reviewed tree's `.claude/skills/`, or in the daemon's profile, is not available to the reviewer. The test fails if `--setting-sources \"\"` is removed.
- Coverage is still credited, including reads made by subagents, and `PRUEFER_COVERAGE` behaves as before.
- New tests are shown to be non-vacuous; `go test -race ./...` passes."

## Background

Pruefer, Fabrik's review bot, builds its reviewer prompt from a Go string constant (`defaultReviewGuidance`) with operator guidance and repo guidance (`.pruefer/skills/review/SKILL.md`) concatenated or substituted onto it as text. Nothing in that arrangement can be invoked, named or swapped, so it gives no foundation for review plugins. Fabrik already solved the same shape for its stages: an embedded base plugin (`fabrik-stages`) is written to disk, loaded with `--plugin-dir`, and stages name skills from it as `plugin:skill`.

This issue is the second in the chain that replaces #1859. The previous issue (#2305, closed) extracted the plugin catalogue, reference resolver and skill-load-failure detector into the shared leaf package `internal/skillref`, which Pruefer may import. This issue uses it: the default review moves out of Go source into an embedded skill that the reviewer is told to invoke. The plugin is the foundation; operator/repo plugins and choosing a different skill are the next issue.

Pruefer's security posture constrains the design. Reviews pass `--setting-sources ""` so that neither the reviewed tree's `.claude/` nor the daemon's `CLAUDE_CONFIG_DIR` profile can change what the reviewer sees or may do; reviews had silently degraded fleet-wide when a profile's settings leaked in. Adding plugins must not reopen that door: the only plugins visible to a reviewer are the ones Pruefer itself passes.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Default review runs through an embedded skill (Priority: P1)

An operator running Pruefer with no `review_guidance` and a reviewed repo with no `.pruefer/skills/review/SKILL.md` gets a review whose criteria come from the embedded `pruefer-review:review` skill, invoked by the reviewer, and posted in exactly the format used today.

**Why this priority**: This is the core change; every other story builds on it.

**Independent Test**: Run a review with a stubbed Claude invoker; assert the invocation passes a `--plugin-dir` for the per-review plugin, the tool grant includes `Skill`, the prompt names `pruefer-review:review`, and a canned output that invokes the skill parses into the same review (summary, findings, coverage) as before.

**Acceptance Scenarios**:

1. **Given** no operator or repo guidance, **When** a review runs, **Then** the reviewer is told to carry out the review by invoking `pruefer-review:review`, and the skill's instructions are today's default guidance.
2. **Given** a review that ran through the skill, **When** its output is parsed, **Then** the summary block, findings JSON and coverage block parse exactly as for a review today.
3. **Given** two reviews running concurrently, **When** each writes the plugin, **Then** each uses its own directory and neither can observe or alter the other's copy.
4. **Given** a review has finished (success or error), **When** cleanup runs, **Then** its plugin directory is gone.

---

### User Story 2 - Only Pruefer's plugin is visible to the reviewer (Priority: P1)

A security-minded operator needs assurance that a skill planted in a reviewed PR's `.claude/skills/`, or present in the daemon's Claude profile, cannot be offered to or invoked by the reviewer.

**Why this priority**: Pruefer reviews untrusted code; this isolation is the reason `--setting-sources ""` exists and must not regress.

**Independent Test**: A test builds the review arguments under two different `CLAUDE_CONFIG_DIR` values and asserts `--setting-sources` is passed with an empty value in both, and that the only plugin directory passed is Pruefer's own; the test fails when that flag pair is removed.

**Acceptance Scenarios**:

1. **Given** a reviewed tree containing `.claude/skills/evil/SKILL.md`, **When** a review runs, **Then** that skill is not among the plugins the reviewer can see.
2. **Given** the daemon's profile contains a plugin or skill, **When** a review runs, **Then** it is not available to the reviewer, whichever `CLAUDE_CONFIG_DIR` is in effect.
3. **Given** the empty `--setting-sources` argument is removed from the review arguments, **When** the new test runs, **Then** it fails.

---

### User Story 3 - Existing operator and repo guidance still apply (Priority: P2)

An operator who sets `review_guidance` (append or replace), or a repo that ships `.pruefer/skills/review/SKILL.md` (append or replace), sees the same review behaviour as before this change.

**Why this priority**: Prevents a regression for every existing customised deployment.

**Independent Test**: With operator and/or repo guidance in each mode, assert the prompt carries the composed additional guidance for the skill, in the same layer order and modes as `resolveGuidance` today, and that the reviewer is still told to invoke the skill.

**Acceptance Scenarios**:

1. **Given** operator guidance in append mode, **When** the prompt is built, **Then** that guidance is passed to the skill as additional guidance and applies on top of the skill's default criteria.
2. **Given** repo guidance in replace mode, **When** the prompt is built, **Then** the review criteria are the repo guidance alone, as today, and the default criteria no longer apply.
3. **Given** operator and repo guidance both present, **When** the prompt is built, **Then** the repo layer folds onto the operator layer exactly as today.

---

### User Story 4 - The output contract and coverage stay Pruefer's (Priority: P2)

Findings parsing, anchoring and the `PRUEFER_COVERAGE` check keep working unchanged, including for reads made by subagents.

**Why this priority**: These feed submission and the patch-skip logic; a regression would silently corrupt reviews.

**Independent Test**: Existing coverage and contract tests pass unmodified; a new test confirms the contract text is produced by Go code and appears outside the skill, and that coverage is credited for a stream that includes a `Skill` call and subagent reads.

**Acceptance Scenarios**:

1. **Given** the embedded skill's text, **When** it is inspected, **Then** it contains no summary-block, findings-JSON or coverage-block format instructions.
2. **Given** a stream with a `Skill` invocation followed by `Read` calls in the main thread and in subagents, **When** coverage is derived, **Then** credit is identical to the same reads without the `Skill` call.
3. **Given** `PRUEFER_COVERAGE` set to each existing value, **When** a review runs, **Then** behaviour matches that of today.

---

### User Story 5 - Operators can see which skill ran (Priority: P3)

An operator reading Pruefer's log can tell, for any review, which skill ran and which plugin supplied it.

**Why this priority**: Observability foundation for the plugin work that follows.

**Independent Test**: Run a review and assert the log line carries the skill reference and the plugin name.

**Acceptance Scenarios**:

1. **Given** a default review, **When** it is logged, **Then** the line names `pruefer-review:review` and the plugin `pruefer-review`.

---

### Edge Cases

- The embedded plugin cannot be written (disk full, unwritable temp directory): the review fails loudly with that cause; it does not silently fall back to the old inline prompt.
- The reference `pruefer-review:review` does not resolve against the plugin directories passed: the review does not run; the error names the reference and the directories searched.
- The reviewer never invokes the skill, or the skill fails to load at runtime: the failure is detectable and reported (reusing `internal/skillref` detection) rather than posting a review as though the default criteria had run.
- The reviewed tree contains its own `.pruefer/skills/review/SKILL.md`: it is read as repo guidance only, as today; it is never loaded as a plugin.
- The per-review plugin directory must not sit inside the reviewed tree or the staged diff, so reviewed content cannot influence it and it does not appear in the reviewer's path-limited scope as reviewable content.
- A subagent (Task) spawned by the reviewer: coverage still credits its reads; subagents get no new tools beyond what they have today other than what `Skill` implies.
- Review of a merge-train trial PR and the patch-skip path behave as today.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Pruefer MUST embed a base plugin named `pruefer-review` containing exactly one skill, `review`, whose instructions are today's `defaultReviewGuidance`.
- **FR-002**: For each review, Pruefer MUST write that plugin to a directory created for that review alone (never shared or reused across reviews), pass it to the reviewer with `--plugin-dir`, and remove it when the review ends.
- **FR-003**: The reviewer's tool grant MUST include `Skill` and no other change; the existing read-only tool and path limits are unchanged.
- **FR-004**: The prompt MUST direct the reviewer to carry out the review by invoking the resolved review skill reference, which for the default is `pruefer-review:review`.
- **FR-005**: The reference MUST be resolved with `internal/skillref` against exactly the plugin directories Pruefer passes with `--plugin-dir`, and a reference that does not resolve MUST fail the review with an error naming the reference and the directories searched.
- **FR-006**: Pruefer MUST keep `--setting-sources ""` on every review invocation, and no plugin outside those Pruefer passes (from the daemon's `CLAUDE_CONFIG_DIR` profile or from `.claude/` in the reviewed tree) may be visible to the reviewer.
- **FR-007**: A test MUST assert directly that the review arguments carry an empty `--setting-sources` under two different `CLAUDE_CONFIG_DIR` values, and MUST fail if that argument is removed.
- **FR-008**: The summary block, findings JSON and coverage block (#2190) instructions MUST remain in Go-owned prompt text outside any skill, and the embedded skill MUST NOT contain them.
- **FR-009**: Output from a review run through the skill MUST parse into summary, findings and coverage exactly as output from a review today.
- **FR-010**: Operator `review_guidance`/`review_guidance_mode` and the repo's `.pruefer/skills/review/SKILL.md` (append/replace) MUST still be composed in today's order and modes, and the composed result MUST be passed to the skill as additional guidance in the prompt.
- **FR-011**: With only operator and/or repo guidance configured (no plugin features), review behaviour MUST match today's, including replace mode, where the supplied guidance alone defines the review criteria.
- **FR-012**: Coverage crediting (`PRUEFER_COVERAGE` and the opened-entries derivation) MUST be unchanged, including credit for reads made by subagents, and MUST NOT be disturbed by `Skill` tool calls in the stream.
- **FR-013**: Every review's log line MUST name the skill reference that ran and the plugin it came from.
- **FR-014**: The default review path MUST NOT fall back silently to the inline Go prompt if the plugin cannot be written or the skill cannot be resolved or loaded.
- **FR-015**: New tests MUST be shown to be non-vacuous (each fails when the behaviour it covers is removed), and `go test -race ./...` MUST pass.

### Key Entities *(if applicable)*

- **Base review plugin (`pruefer-review`)**: The embedded plugin carrying the default review skill; written per review and passed to the reviewer.
- **Review skill (`pruefer-review:review`)**: The default review criteria, invoked by the reviewer.
- **Additional guidance**: The composed operator and repo guidance, supplied to the skill in the prompt.
- **Output contract**: The Go-owned summary/findings/coverage format text, outside any skill.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With no customisation, 100% of reviews invoke `pruefer-review:review` and post a review in the same format as before this change; existing parsing and contract tests pass unmodified.
- **SC-002**: A skill planted in the reviewed tree's `.claude/skills/` or the daemon's profile is never available to the reviewer under any `CLAUDE_CONFIG_DIR`; the isolation test fails when `--setting-sources ""` is removed.
- **SC-003**: For an identical set of reads, coverage credit and the `PRUEFER_COVERAGE` outcome are identical before and after this change, subagent reads included.
- **SC-004**: A repo or operator using only guidance (append or replace) gets the same review criteria as before in every combination of layers and modes.
- **SC-005**: Every review's log line names the skill and plugin; concurrent reviews never share a plugin directory.
- **SC-006**: Each new test is shown to fail when the behaviour it covers is removed, and `go test -race ./...` passes.

## Assumptions

- `internal/skillref` (delivered by #2305) is available on the base branch; this branch is behind `main` and must be rebased onto it.
- "The plugin it came from" in the log line means the plugin's name (`pruefer-review`) and the per-review directory it was loaded from.
- In replace mode, the skill is still invoked (so the log line stays truthful and the mechanism is uniform), and the prompt states that the supplied guidance replaces the skill's own review criteria. Matching today's replace-mode behaviour exactly is the requirement; how the prompt expresses it is a planning decision.
- The reviewer's `Skill` grant may be scoped to the one resolved skill if the CLI supports it; whether it does is a research/plan question and does not change the "only add `Skill`" boundary.
- The per-review directory is created outside the reviewed tree and outside the staged diff, alongside the other per-review temporary material, and cleaned up with it.
- No change to configuration surface (flags, env, YAML) is needed for this issue.

## Out of Scope *(optional)*

- Operator-supplied or repo-supplied plugins, and selecting any skill other than `pruefer-review:review` (next issue in the chain).
- Any change to the reviewer's tool grant beyond adding `Skill`.
- Changing the content of the default review guidance (it moves verbatim) or the output contract.
- Changes to the engine or Fabrik stages' use of `internal/skillref`.

## Source References *(optional)*

- Previous issue in the chain: #2305 (`internal/skillref`); the chain replaces #1859
- `pruefer/claude.go`: `defaultReviewGuidance`, `resolveGuidance`, `composeGuidanceLayer`, `buildReviewArgs`, `buildReviewToolRules`
- `pruefer/coverage.go`: `deriveOpened`
- Fabrik's stage-skill model: ADR 2137/2138 and `plugin/names.go`
