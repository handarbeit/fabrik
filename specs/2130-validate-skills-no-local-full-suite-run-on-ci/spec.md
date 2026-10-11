# Feature Specification: Validate skills: no local full-suite run on CI-gated stages; targeted tests, 5m cap, diagnose before retry

**Feature Branch**: `fabrik/issue-2130`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: the original issue body, reproduced verbatim below.

> ## Problem
>
> The Validate stage reruns the full test suite locally even when CI is about to run it, or already has, on the same commit. On a loaded host that alone can use up the whole 60-minute `max_wall_time`.
>
> **Observed on #2097, 2026-10-11.** The transcript is `.fabrik/logs/handarbeit-fabrik/issue-2097/Validate-20261011-122644-*.log`.
> - A comment-review pass pushed the steered fix as `a542cf46` at 12:26Z. Validate started immediately, and `ci-status.md` read `verdict: pending`, `ci_gated: true`.
> - The skill's skip rule ("Skipping a redundant full-suite run") covers only `verdict: green`. In every other case it says to "run the step exactly as written", which is the full suite.
> - Over 19 turns the worker ran overlapping full or near-full `go test -race` invocations about seven times:
>   - the full module across `./engine ./cmd ./internal/... ./tests/...`
>   - `./tests/sim/` alone
>   - `./engine ./cmd` at `-timeout 14m`
>   - a targeted `-run` retry
>   - a final batch at `-timeout 20m`
> - Five of those commands exceeded the Bash tool's 590 s limit and were backgrounded, and the worker spent its turns polling their output.
> - One run panicked at `test timed out after 9m0s`. The worker retried more broadly instead of asking why; a later standalone `tests/sim` run passed in 26 s, which points to host load.
> - The stage hit `max_wall_time: 60m` (`terminal_reason: aborted_tools`, 3597 s) without completing.
>
> Meanwhile CI ran the same full suite on `a542cf46` in about 7–8 minutes. That is the normal time: recent green runs on #2096, #2089 and #2110 took 6.6–7.9 minutes per job. CI is the stage's own gate: Validate has `wait_for_ci: true`, and the engine's CI gate decides on the pushed head.
>
> The skill already says `go test -timeout 5m` and "run synchronously in the foreground". The worker used 8–20 minute timeouts instead, which is exactly what pushed every command past the tool limit.
>
> 60 minutes is far too long for any test in this repo apart from the live e2e bed. A Validate stage should finish its local verification in minutes.
>
> ## Requirements
>
> **R1: a pending CI on a CI-gated stage means targeted tests only.** In `fabrik-validate` (and `fabrik-validate-comment`), this rule applies when `ci-status.md` has `ci_gated: true` and `verdict` is `pending` or `unknown`. It also applies when `HEAD != head_sha` because this run pushed or rebased. In those cases, run the build plus the tests for the packages the change touches, once, and do not run the full suite locally. The full-suite verdict is CI's job, and the engine's `wait_for_ci` gate is the backstop. This extends the existing "After you change code" rule, which already does this when HEAD moves, to the case where the head was pushed by the *previous* invocation.
>
> **R2: no local full suite on a CI-gated stage at all.** Unless `ci_gated` is `false`, Validate never runs `go test ./...` (or an equivalent full-module invocation) locally. With `verdict: red`, it reads the failing checks (`gh pr checks`, or the run log) and reproduces only the failing tests locally. It never reruns the whole suite to find them. With `ci_gated: false`, the existing full-suite step stays.
>
> **R3: bounded test commands.** Every local test command carries a framework timeout that keeps the whole command inside the Bash tool's limit. For Go, `-timeout` must be no more than 5m on a targeted run. Keep the skill's existing 5m guidance and make it a hard rule: never raise it to fit a broader run, and narrow the run instead. Never let a test command be moved to the background and then poll it.
>
> **R4: diagnose before retrying.** If a test run times out or panics on a timeout, check the host (`uptime`, and the failing test's own elapsed time) and rerun only the failing test, once, before concluding anything. Never retry with a broader set or a longer timeout. A timeout that does not reproduce in isolation is reported as host load in the stage output. It is not a fix trigger and does not count as a failure of the change.
>
> **R5: report what ran.** The stage output's Test Suite line says which of these happened, and the PR description audit and final summary use the same wording:
> - `TARGETED — <packages>; full suite left to CI on <sha>`
> - `SKIPPED — CI green on <sha>`
> - `PASSED` (full, `ci_gated: false` only)
>
> **R6: apply the same rules wherever a stage runs tests.** Apply R1–R5 to every built-in skill that runs the suite, as far as it is CI-gated: `fabrik-review` and `fabrik-implement` with their comment variants. Edit the embedded source `plugin/fabrik-workflows/skills/*/SKILL.md`, never `.fabrik/plugin/`.
>
> ## Scope
>
> - In scope:
>   - The `plugin/fabrik-workflows/skills/` SKILL.md files above.
>   - Any stage-prompt line in `stages/examples/*.yaml` / `.fabrik/stages/*.yaml` that contradicts the new rules.
>   - `docs/stage-lifecycle.md`, where it describes the ci-status skip.
>   - A skill-content test in the style of the existing embedded-skill tests that pins the new rules. It must assert that the pending/CI-gated rule and the 5m cap are present, and that no CI-gated skill instructs an unconditional `go test ./...`.
>   - An ADR.
> - Out of scope:
>   - Engine changes. `ci-status.md` already carries every fact this needs.
>   - `max_wall_time` values.
>   - Orphaned test processes (#2128).
>   - Making the suite itself faster.
>
> ## Acceptance
>
> 1. With `ci-status.md` `verdict: pending` and `ci_gated: true`, the Validate skill directs targeted tests only and names CI as the full-suite verdict.
> 2. With `verdict: red` and `ci_gated: true`, it directs reading the failing checks and reproducing only those tests.
> 3. With `ci_gated: false`, the full-suite step is unchanged.
> 4. No CI-gated built-in skill tells the worker to run the full module locally, or to use a test timeout above 5m. The skill-content test fails if either instruction is reintroduced, demonstrated by neutralising the new wording.
> 5. The diagnose-before-retry rule (R4) is present in every skill that runs tests.

## Background

The Validate stage reruns the full test suite locally even when CI is about to run it, or has already run it, on the same commit. On a loaded host that alone can consume the entire 60-minute `max_wall_time`.

The trigger was #2097 on 2026-10-11:

- A comment-review pass pushed a fix and Validate started at once. `.fabrik-context/ci-status.md` read `verdict: pending`, `ci_gated: true`.
- The current skip rule covers only `verdict: green`. In every other case the skill says to run the step as written, which is the full suite.
- The worker ran about seven overlapping full or near-full `go test -race` invocations with 8–20 minute timeouts, even though the skill already says `-timeout 5m` and "run synchronously in the foreground".
- Five of those commands exceeded the Bash tool's limit and were backgrounded, and the worker spent its turns polling output.
- One run timed out and the worker retried more broadly instead of diagnosing it. A later standalone run of the same package passed in 26 s, which points to host load, not a defect.
- The stage hit `max_wall_time` (60m) without completing, while CI ran the same suite on the same commit in about 7–8 minutes, the normal duration.

CI is the stage's own gate: Validate has `wait_for_ci: true`, and the engine's CI gate decides on the pushed head. The existing `ci-status.md` context file (#1997) already carries every fact needed to decide what to run locally, so no engine change is required. This is a skill-instruction fix.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Pending CI means targeted tests only (Priority: P1)

An operator's issue reaches Validate right after a push, so CI on the pushed head is still `pending` (or `unknown`), or the worker has moved HEAD itself. The Validate worker builds and runs only the tests for the packages the change touches, once, and leaves the full-suite verdict to CI and the engine's `wait_for_ci` gate.

**Why this priority**: This is the exact failure observed on #2097, which burned a full 60-minute stage without completing.

**Independent Test**: Read the `fabrik-validate` and `fabrik-validate-comment` skills against a `ci-status.md` with `ci_gated: true` and `verdict: pending`. They must direct targeted tests only and name CI as the full-suite verdict.

**Acceptance Scenarios**:

1. **Given** `ci_gated: true` and `verdict: pending`, **When** Validate reaches its test step, **Then** it runs the build plus tests for the touched packages once, and does not run the full suite.
2. **Given** `ci_gated: true` and `verdict: unknown`, **When** Validate reaches its test step, **Then** the same targeted-only behavior applies.
3. **Given** `ci_gated: true` and `HEAD != head_sha` because the run pushed or rebased, **When** Validate reaches its test step, **Then** the same targeted-only behavior applies.

---

### User Story 2 - Red CI is reproduced narrowly (Priority: P1)

When CI on a CI-gated stage is `red`, the worker reads which checks failed and reproduces only those tests locally, never rerunning the whole suite to find them.

**Why this priority**: A red verdict is the other common reason a worker reaches for the full suite; the same cost problem applies.

**Independent Test**: With `ci_gated: true` and `verdict: red`, the skills direct reading the failing checks (`gh pr checks` or the run log) and reproducing only the failing tests.

**Acceptance Scenarios**:

1. **Given** `ci_gated: true` and `verdict: red`, **When** Validate investigates, **Then** it reads the failing checks first and runs only the failing tests locally.
2. **Given** `ci_gated: true` and `verdict: red`, **When** Validate investigates, **Then** it does not run `go test ./...` or an equivalent full-module invocation.

---

### User Story 3 - Test commands are bounded and failures are diagnosed (Priority: P1)

Every local test command carries a framework timeout of at most 5 minutes, is never backgrounded and polled, and a timeout is diagnosed (host load, isolated rerun of the one failing test) before any conclusion, never answered by a broader or longer rerun.

**Why this priority**: Unbounded commands and reflexive broadening turned one slow run into an hour-long stage.

**Independent Test**: Every skill that runs tests contains the 5m hard cap, the no-background rule and the diagnose-before-retry rule.

**Acceptance Scenarios**:

1. **Given** a local test command, **When** the skill instructs it, **Then** the command carries `-timeout` of 5m or less, and the skill says never to raise it and to narrow the run instead.
2. **Given** a run that times out or panics on a timeout, **When** the worker responds, **Then** it checks the host (`uptime`, the failing test's elapsed time), reruns only the failing test once, and never retries broader or longer.
3. **Given** a timeout that does not reproduce in isolation, **When** the worker reports, **Then** it records it as host load; it is not a fix trigger and not a failure of the change.

---

### User Story 4 - Full suite is unchanged when the stage is not CI-gated (Priority: P2)

When `ci_gated` is `false`, nothing else will run the suite, so the existing full-suite step stays.

**Why this priority**: The change must not lose verification where CI is not the gate.

**Independent Test**: With `ci_gated: false`, the Validate skill still directs the full-suite step as before, within the bounded-command rules.

**Acceptance Scenarios**:

1. **Given** `ci_gated: false`, **When** Validate reaches its test step, **Then** the full-suite step runs as before.

---

### User Story 5 - The Test Suite line says what ran (Priority: P2)

A reader of the stage output, PR description audit and final summary can tell which kind of verification happened, from fixed wording.

**Why this priority**: Targeted runs are now the norm on CI-gated stages, so "PASSED" alone would be misleading.

**Independent Test**: The skills prescribe exactly the three phrasings for the Test Suite line, the PR description audit and the final summary.

**Acceptance Scenarios**:

1. **Given** a targeted run on a CI-gated stage, **When** the stage reports, **Then** the line reads `TARGETED — <packages>; full suite left to CI on <sha>`.
2. **Given** a skipped run because CI is green on the current head, **When** the stage reports, **Then** the line reads `SKIPPED — CI green on <sha>`.
3. **Given** a full run on a stage with `ci_gated: false`, **When** the stage reports, **Then** the line reads `PASSED`.

---

### User Story 6 - The same rules apply in every built-in skill that runs tests (Priority: P2)

`fabrik-review`, `fabrik-implement` and their comment variants follow the same rules as Validate, as far as they are CI-gated, so the problem cannot recur in a sibling stage.

**Why this priority**: The same unconditional full-suite instruction exists across the other skills.

**Independent Test**: A skill-content test over the embedded skills asserts the rules in each skill that runs tests.

**Acceptance Scenarios**:

1. **Given** each built-in skill that runs the suite, **When** its content is inspected, **Then** it carries the CI-gated targeted-only rule, the 5m cap and the diagnose-before-retry rule.
2. **Given** the skill-content test, **When** the new wording is neutralised or an unconditional full-suite or over-5m instruction is reintroduced, **Then** the test fails.

---

### Edge Cases

- `ci-status.md` is absent (no linked PR): the stage cannot be shown to be CI-gated from that file. The skills keep their existing behavior for that case.
- `verdict: none` on a CI-gated stage is not named by this issue as a targeted-only trigger. Whether it is treated like `pending`/`unknown` is recorded as an assumption.
- `verdict: green` with `HEAD == head_sha` and a clean tree keeps the existing skip, reported as `SKIPPED — CI green on <sha>`.
- A change touching no Go package (docs or skill text only): the targeted run is the build plus any package whose tests read that content, such as the embedded-skill tests.
- A timeout that reproduces in isolation is a genuine failure of that test and is handled as a normal test failure, not as host load.
- The rules constrain the worker's *local* commands only. The engine's `wait_for_ci` gate still decides on the pushed head.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When `ci-status.md` has `ci_gated: true` and `verdict` is `pending` or `unknown`, `fabrik-validate` and `fabrik-validate-comment` direct the worker to run the build plus the tests for the touched packages, once, and not the full suite, naming CI and the engine's `wait_for_ci` gate as the full-suite verdict.
- **FR-002**: The same targeted-only rule applies when `HEAD != head_sha` because this run pushed or rebased, including when the head was pushed by a previous invocation.
- **FR-003**: Unless `ci_gated` is `false`, the skills never instruct `go test ./...` or any equivalent full-module invocation locally.
- **FR-004**: With `ci_gated: true` and `verdict: red`, the skills direct reading the failing checks (`gh pr checks` or the run log) and reproducing only the failing tests locally, never rerunning the whole suite to find them.
- **FR-005**: With `ci_gated: false`, the existing full-suite step is retained, subject to FR-006 and FR-007.
- **FR-006**: Every local test command in the skills carries a framework timeout that keeps the whole command inside the Bash tool's limit; for Go, `-timeout` is at most 5m. The skills state this as a hard rule: never raise the timeout to fit a broader run, narrow the run instead.
- **FR-007**: The skills forbid moving a test command to the background and then polling it; test commands run synchronously in the foreground.
- **FR-008**: When a test run times out or panics on a timeout, the skills direct the worker to check the host (`uptime` and the failing test's own elapsed time) and rerun only the failing test, once, before concluding anything, and forbid retrying with a broader set or a longer timeout.
- **FR-009**: The skills direct that a timeout which does not reproduce in isolation is reported as host load in the stage output, is not a fix trigger, and does not count as a failure of the change.
- **FR-010**: The skills prescribe the Test Suite line, PR description audit and final summary wording: `TARGETED — <packages>; full suite left to CI on <sha>`, `SKIPPED — CI green on <sha>`, or `PASSED` (full run, `ci_gated: false` only).
- **FR-011**: FR-001 through FR-010 are applied to every built-in skill that runs the suite, as far as it is CI-gated: `fabrik-validate`, `fabrik-validate-comment`, `fabrik-review`, `fabrik-review-comment`, `fabrik-implement` and `fabrik-implement-comment`. The diagnose-before-retry rule (FR-008, FR-009) is present in every one of them.
- **FR-012**: The edits are made to the embedded source `plugin/fabrik-workflows/skills/*/SKILL.md`, never to `.fabrik/plugin/`.
- **FR-013**: Any stage-prompt line in `stages/examples/*.yaml` and `.fabrik/stages/*.yaml` that contradicts these rules is corrected, and `docs/stage-lifecycle.md` is updated where it describes the ci-status skip (with `docs/llms-full.txt` regenerated in the same change).
- **FR-014**: A skill-content test in the style of the existing embedded-skill tests pins the rules: it asserts that the pending/CI-gated rule, the 5m cap and the diagnose-before-retry rule are present, and that no CI-gated skill instructs an unconditional `go test ./...` or a test timeout above 5m. It fails when the new wording is neutralised.
- **FR-015**: An ADR records the decision, numbered after this issue (`adrs/2130-…`).

### Key Entities *(if applicable)*

- **`ci-status.md`**: the context file written by the engine (#1997) carrying the PR head SHA, the engine's CI verdict for that head (`green`/`red`/`pending`/`none`/`unknown`) and `ci_gated`. It is the sole input to the rules; the skills read it, and the engine is unchanged.
- **CI-gated stage**: a stage whose engine gate waits for CI (`wait_for_ci: true`), flagged by `ci_gated: true`.
- **Test Suite line**: the line in stage output, PR description audit and final summary that states which verification happened.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a CI-gated stage with a pending, unknown or red verdict, or with a moved HEAD, a Validate run performs no full-module local test invocation.
- **SC-002**: No local test command in any built-in skill carries a timeout above 5m or is run in the background, and the skill-content test fails if either is reintroduced.
- **SC-003**: A run like #2097 completes local verification in minutes rather than exhausting the 60-minute `max_wall_time`.
- **SC-004**: Every skill that runs tests contains the diagnose-before-retry rule, verified by the skill-content test.
- **SC-005**: Every stage report on a CI-gated stage uses one of the three fixed Test Suite phrasings.
- **SC-006**: With `ci_gated: false`, the full-suite step is still present and unchanged in meaning.

## Assumptions

- `verdict: none` on a CI-gated stage (no checks present yet) is treated the same as `pending`/`unknown`: targeted tests only, since CI is still the stage's gate. The issue names only `pending`, `unknown` and `red`; this is the conservative reading and is reversible.
- "As far as it is CI-gated" means the rules apply whenever `ci-status.md` reports `ci_gated: true`. A skill run with no `ci-status.md` keeps its existing behavior, since nothing shows CI will run the suite.
- "Packages the change touches" is decided by the worker from the diff, and includes any package whose tests read the changed content (for example embedded skills and docs).
- Applying the rules to `fabrik-implement` and `fabrik-review` does not change their `ci_gated: false` behavior.
- The 5m cap is the Go `-timeout` for targeted runs; equivalent frameworks use an equivalent bound that keeps the command inside the Bash tool's limit.

## Out of Scope *(optional)*

- Engine changes; `ci-status.md` already carries every fact needed.
- `max_wall_time` values.
- Orphaned test processes (#2128).
- Making the test suite itself faster.

## Source References *(optional)*

- Observed failure: #2097, transcript `.fabrik/logs/handarbeit-fabrik/issue-2097/Validate-20261011-122644-*.log`.
- Existing mechanism being extended: `ci-status.md` context file (#1997, `adrs/1997-ci-status-context-skip-redundant-suite.md`).
- CI durations for comparison: #2096, #2089, #2110 (6.6–7.9 minutes per job).
