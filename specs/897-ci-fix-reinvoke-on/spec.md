# Feature Specification: E2E Test — CI-Fix Reinvoke on a Real CI Failure

**Feature Branch**: `fabrik/issue-897`
**Created**: 2026-06-18
**Status**: Draft
**Input**: User description: "test(e2e): CI-fix reinvoke on a real CI failure — failure→fix→recover (and bound at MaxCiFixCycles)"

## Background

No end-to-end scenario drives a real CI **failure** through the CI gate and out the CI-fix reinvoke loop. Issue #888 (settling primitive) reinterprets exactly the signals this loop consumes (`mergeable_state` / `check_runs` / HEAD SHA). Without a test that covers a `wait_for_ci` failure being classified, fix-reinvoked, and recovered, the consolidation in #888 could silently break the loop with no regression guard. This test is the guard.

The CI-fix reinvoke loop is a non-trivial engine path:
1. `checkCIGate` detects a CI failure and returns `(_, true, _)`.
2. The catch-up loop in `poll.go` checks the worker guard and cycle count, then calls `dispatchCIFixReinvoke`.
3. `dispatchCIFixReinvoke` spawns a goroutine that passes a synthetic CI-fix comment to `processComments()`.
4. Claude pushes a fix commit; CI re-runs; if it passes, `checkCIGate` returns clean on the next poll tick and `stage:<X>:complete` is applied.
5. If CI fails again and `CIFixCycles` ≥ `MaxCiFixCycles`, `pauseForCIFixCycleLimit` fires.

No current e2e test exercises steps 1–5. The existing unit tests in `engine/ci_test.go` cover the gate logic in isolation; they do not exercise a real CI check run, a real Claude fix commit, or the post-fix CI re-evaluation.

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Happy Path: CI Failure Detected, Fixed, and Recovered (Priority: P1)

A developer files a `wait_for_ci` issue whose implementation produces a PR that fails a required CI check. Fabrik detects the failure, dispatches a CI-fix reinvoke, Claude pushes a corrective commit, CI passes on the new HEAD, and the stage advances normally.

**Why this priority**: This is the primary regression guard for ADR-056 root cause 2 and the #888 settling primitive consolidation. Without it, a bug in the CI-fix loop would not be caught until a production incident.

**Independent Test**: `TestCIFixReinvokeHappyPath` — files a single issue, waits for the CI failure observation, waits for the fix commit, waits for CI pass, asserts label transitions and stage completion.

**Acceptance Scenarios**:

1. **Given** a `wait_for_ci` issue is filed with a body that instructs Implement to leave the CI sentinel in a failing state, **When** Implement completes and the PR is pushed, **Then** `fabrik:awaiting-ci` is applied to the issue and `stage:<X>:complete` is withheld.

2. **Given** `fabrik:awaiting-ci` is present on the issue and CI has failed, **When** the engine's catch-up loop next evaluates the issue, **Then** exactly one CI-fix reinvoke goroutine is dispatched (observable as a new commit on the PR branch or a new comment on the PR within the polling window).

3. **Given** the CI-fix reinvoke has run and Claude has pushed a corrective commit, **When** CI passes on the new HEAD, **Then** `stage:<X>:complete` is added, `fabrik:awaiting-ci` is cleared, and the issue advances to the next stage.

---

### User Story 2 — Negative Variant: Unfixable CI Failure Pauses at Cycle Limit (Priority: P2)

An unfixable CI failure (one Claude cannot repair) causes the engine to exhaust `MaxCiFixCycles` and pause the issue rather than looping forever.

**Why this priority**: The cycle limit is a safety bound. Without testing it, an off-by-one or missing increment could cause infinite loops in production.

**Independent Test**: `TestCIFixReinvokeCycleLimit` — files an issue seeded with an unfixable CI failure, waits for the issue to reach `fabrik:paused` + `fabrik:awaiting-input`, asserts the PR has received at most `MaxCiFixCycles` fix-cycle commits (no infinite loop).

**Acceptance Scenarios**:

1. **Given** a `wait_for_ci` issue with a CI sentinel Claude cannot fix, **When** `MaxCiFixCycles` CI-fix reinvoke cycles have fired, **Then** `fabrik:paused` and `fabrik:awaiting-input` are applied and no further CI-fix reinvokes are dispatched.

2. **Given** the issue is paused at the cycle limit, **When** the operator manually removes `fabrik:paused` and intervenes, **Then** the cycle counter resets (`EngineCyclesCleared`) and fresh CI-fix attempts can begin.

---

### Edge Cases

- **Exactly one productive cycle**: The fix commit must appear exactly once, not as a storm of rapid re-invocations. The test should count fix-cycle commits/comments and assert the count equals 1 (positive path) or `MaxCiFixCycles` (negative path) — not more.
- **Worker guard idempotency**: If a CI-fix goroutine is already in-flight when the catch-up loop re-evaluates, no second goroutine is dispatched. The test implicitly covers this by asserting a single fix commit.
- **Stage completion deferred**: `stage:<X>:complete` must NOT be present while `fabrik:awaiting-ci` is present. The test should assert this ordering: `fabrik:awaiting-ci` is applied before `stage:<X>:complete`, and `stage:<X>:complete` is never present concurrently with `fabrik:awaiting-ci`.

## Requirements *(mandatory)*

### Test-bed Prerequisite: Deterministic CI Failure Sentinel

The test repo (`handarbeit/fabrik-test-alpha`) needs a new required CI job that produces a deterministic, Claude-fixable failure. This must be specced and implemented as part of the work — it is the critical prerequisite.

Design constraints:
- **Reliable**: The failure must trigger on every run when the sentinel is present and pass reliably when it is absent. No flakiness.
- **Claude-fixable in one cycle**: The fix must be mechanically obvious from the CI failure output so a single Claude session reliably corrects it. Example shapes:
  - A required CI job that checks for the presence of a specific file (e.g. `CI_OK`) and fails if it is absent. The issue body instructs Implement NOT to create the file; the CI-fix reinvoke adds it.
  - A required CI job that reads a sentinel value from a file (e.g. a checksum in `ci-sentinel.txt`) and fails if the value is wrong. The issue body instructs Implement to write the wrong value; the CI-fix reinvoke writes the correct one.
- **Unfixable variant for negative test**: A second sentinel (e.g. `CI_UNFIXABLE`) that causes the same CI job to fail but the issue body explicitly instructs Claude to leave the file in a state that cannot satisfy the check. This makes the negative-path test deterministic without depending on Claude being unable to fix a real bug.
- **Mirror the `slow-ci-required` pattern**: The sentinel is encoded in the issue body, not in the branch — consistent with the existing `slow-ci-required` trick in `convergence_race_test.go`.

The Research stage must design the exact sentinel mechanism before any coding begins.

### Functional Requirements

- **FR-001**: A new required CI job in `handarbeit/fabrik-test-alpha` implements the CI failure sentinel. The job's name must be unique enough to be unambiguous in harness assertions.
- **FR-002**: A new `TestCIFixReinvokeHappyPath` e2e test covers the positive scenario (failure → fix → recover).
- **FR-003**: A new `TestCIFixReinvokeCycleLimit` e2e test covers the negative scenario (unfixable failure → cycle limit → pause). This test is marked as optional-but-recommended in the issue; the spec treats it as required for complete regression coverage.
- **FR-004**: New harness helpers are added to `tests/e2e/harness.go`:
  - `PRCheckRunConclusions(t, env, repo, prNumber)` — returns a map of check run name → conclusion for the current HEAD of the PR.
  - `PRCommitCount(t, env, repo, prBranch, baseBranch)` — returns the number of commits on the PR branch ahead of the base branch.
  - (Or equivalent helpers sufficient to assert "exactly one fix-cycle commit" and "CI check passed".)
- **FR-005**: The e2e README is updated with new rows in the Scenarios table and Regression coverage map, referencing #855, #871, and ADR-056.
- **FR-006**: The test issue body used by `TestCIFixReinvokeHappyPath` must not contain `slow-ci-required` (the fix cycle must complete without the 6-minute slow gate, keeping wall-clock reasonable).
- **FR-007**: Assertions on label ordering: `fabrik:awaiting-ci` must be observed present before `stage:<X>:complete` appears. `stage:<X>:complete` must never be observed simultaneously with `fabrik:awaiting-ci`.

### Key Entities

- **CI failure sentinel**: A string or file presence checked by the new required CI job in the test repo. Controls whether the job passes or fails.
- **Fix-cycle commit**: A commit pushed by the CI-fix reinvoke goroutine after Claude processes the synthetic CI-fix comment.
- **`MaxCiFixCycles`**: Engine constant (default 5). The cycle limit for the negative test. The test should use the default value and assert it is not exceeded by more than 1.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `TestCIFixReinvokeHappyPath` passes: the issue closes (or advances to the next stage) within the wall-clock budget, with `fabrik:awaiting-ci` having been applied and cleared exactly once, and `stage:<X>:complete` appearing only after `fabrik:awaiting-ci` is cleared.
- **SC-002**: `TestCIFixReinvokeCycleLimit` passes: the issue reaches `fabrik:paused` + `fabrik:awaiting-input` within the wall-clock budget, with the number of fix-cycle commits on the PR branch equal to `MaxCiFixCycles` (no infinite loop).
- **SC-003**: The new required CI job in `handarbeit/fabrik-test-alpha` does not break the existing test suite. All other e2e tests continue to pass with the new CI job present.
- **SC-004**: The harness additions compile without errors under `go build -tags e2e ./tests/e2e/...`.

## Assumptions

- `handarbeit/fabrik-test-alpha` CI workflow files are writable and the test bed has permissions to add new required jobs.
- The test bed's `fabrik:yolo` label is used in tests where auto-advance is needed; the CI-fix reinvoke path applies regardless of yolo (Phase 1 runs unconditionally per the state-machine spec).
- The `wait_for_ci` setting is applied via the stage YAML in the test-bed's `.fabrik/stages/` config, not per-issue. The test must be designed to work with however `wait_for_ci` is configured in the test bed — or the test must set the label on the issue to trigger the CI gate path.
- `MaxCiFixCycles` defaults to 5. The negative test should hardcode the expected commit count as `5` (or read it from the harness if exposed) rather than guessing.
- The settling primitive (#888) is merged before this test lands. The test asserts post-#888 behavior (specifically, that `settle.CheckRuns` is correctly populated and drives the CI-fix classification).

## Out of Scope

- Changes to the CI-fix reinvoke engine logic itself (this is a test-only issue).
- Modifying `engine/ci.go`, `engine/poll.go`, or any production engine code.
- Adding a new e2e test for the rebase-reinvoke path (separate issue; this test covers only the CI-fix path).
- Auto-merge behavior post-CI-pass (the convergence race test covers that separately).
- CI integration of the e2e suite into GitHub Actions (tracked separately per the README).

## Source References

- `tests/e2e/convergence_race_test.go` — existing test that uses the `slow-ci-required` sentinel trick; CI sentinel design should follow this pattern.
- `tests/e2e/harness.go` — add new helpers here.
- `engine/ci.go:340` — `dispatchCIFixReinvoke` (the function being tested end-to-end).
- `engine/ci.go:458` — `pauseForCIFixCycleLimit` (the function being tested in the negative variant).
- `engine/poll.go:1380–1392` — catch-up loop where cycle count is checked and reinvoke dispatched.
- `docs/state-machine.md` (CI-fix reinvoke section, lines ~609–611 and ~1136–1148) — authoritative spec for the loop.
- `adrs/056-consolidate-convergence-gate-recovery.md` — the ADR whose root cause 2 this test guards.
- Issues #855 and #871 — the signal-window bugs consolidated by #888 that this test regression-guards.
