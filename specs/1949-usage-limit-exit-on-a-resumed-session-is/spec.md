# Feature Specification: Classify a usage-limit exit on a resumed session as a usage limit, not a resume failure

**Feature Branch**: `fabrik/issue-1949`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

When a Claude account hits its usage limit during a **resumed** invocation (`--resume <session>`), the engine does not recognise it as a usage limit. Instead it counts the exit as a session-resume failure, and after `maxResumeFailures` (2) it abandons a healthy session. The account-wide suspension (ADR-1119/1183/1815) starts only when a later **fresh** invocation hits the same limit.

Found in the 0.0.83 release gate on 2026-09-28 (bed on brett@v3rv.com, alpha#6829, `TestConvergenceRace`). Three consecutive rebase-reinvokes of Validate, all against the same exhausted account:

| time | session | engine outcome |
|---|---|---|
| 06:23:04Z | resume `3bc0258f…` | `error exit with unmatched terminal_reason=\"api_error\" (not classified as usage limit)`, then resume failure 1/2 |
| 06:24:17Z | resume `3bc0258f…` | the same, then resume failure 2/2, and the **session is abandoned** |
| 06:25:25Z | fresh `72364fd9…` | `usage-limit exit detected … api_error_status=429`, and dispatch is suspended until Oct 2 |

The CLI output was **identical and fully structured on all three** (per-issue logs under `.fabrik/logs/handarbeit-fabrik-test-alpha/issue-6829/Validate-comment-review-20260928-06{2304,2417,2525}-*.log`):
- `result`: `is_error: true`, `terminal_reason: \"api_error\"`, `api_error_status: 429`, `num_turns: 1`, text \"You've hit your weekly limit · resets Oct 2 at 4am\";
- `rate_limit_event`: `status: \"rejected\"`, `rateLimitType: \"seven_day\"`, with `resetsAt`.

## Root cause

`classifyUsageLimitExit` (`engine/claude.go`) matches `terminal_reason == \"api_error\" && api_error_status == 429`. It then declines when `usage.TurnsUsed > 0 && usage.CostUSD > 0`, a guard for \"real work happened, so this isn't a did-not-run limit exit\".

On a `--resume` invocation, the result's usage and cost are the **resumed session's cumulative** figures, not this invocation's. The two misclassified exits reported `used 1 turns, $0.6891` with token counts identical to the session's earlier Validate run, while the fresh session reported `$0.0000`.

So the guard sees prior work as this invocation's work. The 429 is not classified, `classifyAPIErrorExit` declines for the same reason, and the exit falls through to `classifyResumeFailure`.

## Consequences

- A healthy session's resume pointer is abandoned, and the next invocation cold-starts, losing context.
- The account-wide suspension starts one or more invocations late. Every dispatch in between burns an attempt against the exhausted account.
- The did-not-run refunds (#1812) and the `fabrik:claude-limit` comment/label do not fire for those exits.

## Requirements

- **R1** — A structured usage-limit exit is classified as one regardless of resume state. The signal is `terminal_reason == \"blocking_limit\"`, or `\"api_error\"` with `api_error_status == 429`, and/or a `rate_limit_event` with `status: \"rejected\"`. The \"real work happened\" guard must use this invocation's own usage, never the resumed session's cumulative totals. Plan decides how to obtain it, for example the delta against the session's pre-invocation totals, or treating a rejected `rate_limit_event` as decisive on its own.
- **R2** — A usage-limit exit never increments the resume-failure counter or abandons the session.
- **R3** — The same fix applies wherever the cumulative-usage guard is reused (`classifyAPIErrorExit`, and any did-not-run refund decision keyed on this invocation's usage).

## Acceptance

- Unit tests with a resumed-session fixture use the captured result and rate-limit lines: `api_error`/429, a rejected `rate_limit_event`, and cumulative cost > 0. The exit is classified as `claudeUsageLimitError`, the resume counter is untouched, and the session is not abandoned. The test fails with the fix neutralised.
- A fresh-session fixture shows the existing behaviour is unchanged.
- A genuine limit hit after real work in *this* invocation still follows the current path."

## Background

When a Claude account hits its usage limit, the engine is meant to recognise the exit structurally from the CLI's own result object and start an account-wide dispatch suspension. That suspension is what stops further invocations burning attempts against an exhausted account. The recognition contains a guard: an exit that shows "real work happened" is not treated as a did-not-run limit exit.

On a resumed invocation (`--resume <session>`) the CLI reports the resumed session's **cumulative** turns and cost, not this invocation's. In the 0.0.83 release gate on 2026-09-28 (alpha#6829, `TestConvergenceRace`), two consecutive resumed Validate invocations hit a weekly limit. Each reported `used 1 turns, $0.6891` with token counts identical to the session's earlier Validate run. The guard read that prior work as this invocation's work, declined to classify the 429, and the exit fell through to resume-failure handling. After two such exits the healthy session was abandoned. A third, fresh invocation (`$0.0000`) was the first to be classified as a usage limit, and only then did the suspension (until Oct 2) begin.

The CLI output was identical and fully structured on all three invocations:
- `result`: `is_error: true`, `terminal_reason: "api_error"`, `api_error_status: 429`, `num_turns: 1`, text "You've hit your weekly limit · resets Oct 2 at 4am";
- `rate_limit_event`: `status: "rejected"`, `rateLimitType: "seven_day"`, with `resetsAt`.

Consequences of the misclassification:
- A healthy session's resume pointer is abandoned and the next invocation cold-starts, losing context.
- The account-wide suspension (ADR-1119/1183/1815) starts one or more invocations late, and every dispatch in between burns an attempt against the exhausted account.
- The did-not-run refunds (#1812) and the `fabrik:claude-limit` comment/label do not fire for those exits.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A usage limit hit on a resumed session suspends dispatch immediately (Priority: P1)

An operator's account reaches its usage limit while a stage or comment-review invocation is resuming an existing session. The engine recognises the exit as a usage limit on that first occurrence and starts the account-wide suspension, instead of treating it as a session problem.

**Why this priority**: This is the defect. Each late classification wastes an attempt against an exhausted account and the delay compounds with every dispatch.

**Independent Test**: Feed the classifier a resumed-session result with `api_error`/429, a rejected `rate_limit_event`, and a non-zero cumulative cost and turn count. It must be classified as a usage-limit exit.

**Acceptance Scenarios**:

1. **Given** a resumed invocation whose result reports `terminal_reason: "api_error"`, `api_error_status: 429`, a rejected `rate_limit_event`, and cumulative cost > 0, **When** the engine interprets the exit, **Then** it is classified as a usage-limit exit and the account-wide suspension starts.
2. **Given** the same exit with `terminal_reason: "blocking_limit"`, **When** interpreted, **Then** it is classified as a usage-limit exit.
3. **Given** a usage-limit exit on a resumed invocation, **When** the engine applies the did-not-run handling, **Then** the attempt refunds and the `fabrik:claude-limit` label and comment apply as they do for a fresh invocation.

---

### User Story 2 - A usage-limit exit never damages the session (Priority: P1)

A usage-limit exit on a resumed invocation leaves the session's resume pointer intact and does not count toward session-resume failures, so the session is reused once the limit lifts.

**Why this priority**: Abandoning a healthy session loses context for no reason. The limit is an account condition, not a session condition.

**Independent Test**: Run a resumed-session usage-limit exit through the engine's exit handling and assert the resume-failure counter is unchanged and the session is not abandoned, including after two consecutive such exits.

**Acceptance Scenarios**:

1. **Given** a session with resume-failure count 0, **When** a resumed invocation exits on a usage limit, **Then** the count remains 0.
2. **Given** two consecutive resumed usage-limit exits, **When** the second is handled, **Then** the session is still not abandoned.

---

### User Story 3 - Behaviour for fresh sessions and genuine mid-work limits is unchanged (Priority: P1)

The "real work happened, so this is not a did-not-run limit exit" distinction keeps working, judged on this invocation's own usage rather than the session's cumulative totals.

**Why this priority**: The fix must not regress the case the guard exists for.

**Independent Test**: A fresh-session usage-limit fixture and a fixture where this invocation did real work before hitting the limit both follow today's path.

**Acceptance Scenarios**:

1. **Given** a fresh (non-resumed) invocation with `$0.0000` cost hitting the 429, **When** interpreted, **Then** the result is identical to today's.
2. **Given** an invocation (resumed or fresh) that performed real work in this invocation and then hit the limit, **When** interpreted, **Then** it follows today's "real work happened" path.

---

### User Story 4 - The same invocation-own-usage rule applies to every consumer of the guard (Priority: P2)

Other decisions that currently read the cumulative usage as if it were this invocation's, such as the API-error exit classification and any did-not-run refund decision, use this invocation's own usage.

**Why this priority**: Fixing only the usage-limit classifier would leave the same misreading in sibling classifiers and let the exit fall through to resume-failure handling by another route.

**Independent Test**: For each consumer, a resumed-session fixture with cumulative cost > 0 and no work in this invocation is judged as no work done.

**Acceptance Scenarios**:

1. **Given** a resumed-session API-error exit with cumulative cost > 0 and no work in this invocation, **When** the API-error classifier runs, **Then** it does not decline merely because of the cumulative totals.
2. **Given** a did-not-run refund decision keyed on usage, **When** the invocation is a resume with no work of its own, **Then** it is treated as did-not-run.

---

### Edge Cases

- A resumed invocation whose result reports a cost/turn count smaller than or equal to the session's pre-invocation totals (no new work).
- A resumed invocation that did real work and then hit the limit mid-way: must keep the current "real work happened" behaviour.
- A usage-limit result with no `rate_limit_event` present, or with a `rate_limit_event` whose status is not rejected: classification still follows the existing `terminal_reason`-based signal.
- A rejected `rate_limit_event` with a non-limit `terminal_reason`: behaviour is for Plan to decide under R1 and must be covered by a test either way.
- A fresh session whose first invocation reports `$0.0000`: unchanged.
- The pre-invocation totals for a resumed session are unavailable or unreliable (for example after a restart): the engine must fail toward the structured limit signal, not toward resume-failure handling.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A structured usage-limit exit is classified as a usage-limit exit regardless of whether the invocation is a fresh start or a resume. The structured signal is `terminal_reason == "blocking_limit"`, or `terminal_reason == "api_error"` with `api_error_status == 429`, and/or a `rate_limit_event` with `status: "rejected"`.
- **FR-002**: The "real work happened" guard in usage-limit classification is evaluated against this invocation's own usage, never the resumed session's cumulative totals. How this invocation's usage is obtained (for example the delta against the session's pre-invocation totals, or treating a rejected `rate_limit_event` as decisive on its own) is decided at Plan.
- **FR-003**: A usage-limit exit never increments the session-resume failure counter and never causes the session to be abandoned.
- **FR-004**: Every place that reuses the cumulative-usage guard (including the API-error exit classification and any did-not-run refund decision keyed on this invocation's usage) uses this invocation's own usage by the same rule as FR-002.
- **FR-005**: A usage-limit exit on a resumed invocation produces the same downstream effects as one on a fresh invocation: the account-wide suspension, the did-not-run refunds (#1812), and the `fabrik:claude-limit` comment and label.
- **FR-006**: The behaviour for fresh-session usage-limit exits, and for a genuine limit hit after real work in the current invocation, is unchanged.
- **FR-007**: Unit tests use a resumed-session fixture built from the captured result and `rate_limit_event` lines (`api_error`/429, rejected `rate_limit_event`, cumulative cost > 0). They assert the exit is a usage-limit error, the resume counter is untouched, and the session is not abandoned. A test with the fix neutralised must fail. A fresh-session fixture and a real-work-in-this-invocation fixture confirm FR-006.

### Key Entities *(if applicable)*

- **Resumed invocation**: a Claude invocation started with `--resume <session>`, whose reported usage and cost are the session's cumulative figures.
- **Invocation-own usage**: the turns and cost attributable to this invocation alone.
- **Usage-limit exit**: an invocation that ended because the account's usage limit was hit, recognised from the CLI's structured result and `rate_limit_event`, never from output prose.
- **Resume-failure counter**: the per-session count of consecutive resume failures, which triggers abandoning the session at `maxResumeFailures`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On the captured alpha#6829 scenario replayed in tests, the first resumed usage-limit exit is classified as a usage limit (previously the third invocation was the first to be).
- **SC-002**: After any number of consecutive resumed usage-limit exits, the session's resume-failure count is unchanged and the session is still resumable.
- **SC-003**: The new resumed-session test fails when the fix is neutralised, and the fresh-session and real-work-in-this-invocation tests pass both with and without the fix.
- **SC-004**: No attempt is burned against an exhausted account between the first resumed usage-limit exit and the start of the suspension.

## Assumptions

- The usage-limit signal in the CLI's result (`terminal_reason` plus `api_error_status`, and the `rate_limit_event`) is reliable and identical for fresh and resumed invocations, as observed in the three captured logs.
- The resumed result's usage and cost are the session's cumulative figures, as observed in the alpha#6829 logs; the spec does not require that to hold for every CLI version, only that the engine not depend on it.
- The existing downstream usage-limit handling (suspension, refunds, label and comment) is correct and only needs to be reached.
- The captured per-issue logs from the 0.0.83 release gate are available, or can be reconstructed, to build the test fixture.

## Out of Scope *(optional)*

- Changing how the account-wide suspension end time is derived (ADR-1815) or how it is cleared (ADR-1183).
- Changing the resume-failure policy itself (`maxResumeFailures`, cold-start-after-abandon) for genuine resume failures.
- Other exit classifications unrelated to the cumulative-usage guard.
- Fixing or re-running the 0.0.83 release-gate test; it is background evidence only.

## Source References *(optional)*

- Issue #1949 original report; 0.0.83 release gate, 2026-09-28, alpha#6829 (`TestConvergenceRace`).
- ADR-1119, ADR-1183, ADR-1815 (usage-limit suspension); #1812 (did-not-run refunds); `docs/state-machine.md` resume-failure handling.
