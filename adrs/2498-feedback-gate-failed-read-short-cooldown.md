# ADR 2498: Feedback gate — a failed live read records a short admission cooldown

## Status

Accepted

## Context

`feedbackGateBlocks` (ADR 1953-a) fails closed: when its live read fails, the feedback state is unknown and the advance or landing is held. Until now that hold recorded the same `feedback-pending` admission cooldown as a hold on real pending feedback, `githubRecheckInterval()` = 10 × `PollSeconds`, and `feedback-pending` is not a short admission cooldown reason, so the idle poll could sleep past it. A transient failure (one 502) therefore cost the item a full periodic re-evaluation before the advance was retried (community report #2453, `totalslacker/chronicle` #536: the hold cleared about 4 minutes later and the operator moved the card by hand). This is the same class as ADR 2232: every engine-scheduled retry owns an admission cooldown sized to the retry it stands for.

## Decision

1. **A hold caused only by a failed live read** (`pf.empty() && !pf.Live`) records a new dedicated reason, `feedback-read-failed`, until `now + mergeGateRecheckInterval()` (`PollSeconds/2`, floor 1 s — the merge gate's `merge-unsettled` cadence). It records no `feedback-pending` entry.
2. **A hold on real pending feedback** keeps `feedback-pending` at `githubRecheckInterval()`, unchanged. `githubRecheckInterval` remains the API-cost knob (ADR 1831).
3. **A dedicated reason, not a reuse.** Reusing `feedback-pending` with a shorter value would overwrite an earlier real-feedback entry's deadline (the cooldown map holds one deadline per reason). Reusing `merge-unsettled` would trigger its merge-specific readers (`catch_up_handlers`, `stages.go`, `completion_terminal.go`, `validate_rerun.go`). The new reason is added to both `shortAdmissionCooldownReasons` (so `PollWithBackoff` caps the idle interval at it, ADR 2232 FR-003) and `gateCooldowns` (so `fabrik_status`/`fabrik_situation` do not call the item `waiting-for-dispatch`); a unit test pins both.
4. **No consecutive-failure cap or fallback to the long interval (FR-004).** Admission consumes an expired cooldown (ADR 2096), so the entry is re-recorded only if the next read fails again: the re-read rate is at most one gate read per poll per held item, and the interval cap is skipped under GraphQL rate-limit backoff, which slows the poll itself. The merge gate already re-checks every poll under `merge-unsettled`, so this is the established cost envelope. A persistent non-transient failure (for example a 403 on a `base:<branch>` item's REST review read) costs that one item 1 read per poll instead of 1 per 10 and is visible through the repeated `[#N feedback-gate] holding … live feedback read failed` line. A counter would add state and a reset path, and would turn a recoverable hold into a long stall exactly when the forge recovers. **Revisit if** evidence shows a real spin (many items, or one item, driving a read rate that matters against the budget).
5. **No forge-unreachable-hold special case on the Phase 2 path.** The forge-unreachable hold (ADR 2393) gates dispatch, not `runCatchUpPhase2`'s advance, so during an outage that path still evaluates the gate once per poll. Lengthening the cooldown during the hold would delay recovery after it lifts, the opposite of this ADR's goal, and one extra read per poll is negligible. Accepted and recorded.
6. The gate stays fail-closed, and the log line, `noteGateHold` call and dispatcher-gap repair are unchanged.

## Consequences

- After one transient read failure at an advance, the item is re-evaluated at the next poll (sim twin `tests/sim/feedback_gate_read_failure_test.go`: 1 poll, against 10 with the change neutralised through `SetFeedbackReadFailShortCooldownDisabledForTest`).
- All `feedbackGateBlocks` callers (advance, landing decision, direct merge, no-work-needed, `fabrik_advance`) inherit the change.
- Docs: `CLAUDE.md` and `docs/state-machine.md` (feedback-gate Re-admission, idle backoff) updated.

## Related

ADR 2232, ADR 2096, ADR 1953-a, ADR 1831, ADR 2393, ADR 2121.
