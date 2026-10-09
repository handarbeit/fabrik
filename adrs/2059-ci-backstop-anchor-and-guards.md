# ADR 2059: CIBackstopTimeout backstop — resume-aware anchor and two guards

## Status

Accepted.

## Context

`settleAwaitingCIScan`'s unconditional `CIBackstopTimeout` backstop (ADR-1270, #1303, ADR-1410 R5) escalates an item once `time.Since(appliedAt)` reaches the timeout, where `appliedAt` is when `fabrik:awaiting-ci` was applied. That label is applied once and never reset (ADR-1314), so the clock keeps running through paused time and daemon downtime. On #2052 / PR #2053 a daemon clean stop paused an item that carried `fabrik:awaiting-ci`; a human's comment resumed it eleven hours later; three seconds after the resume the backstop re-paused it blind, in the same poll in which the resume had dispatched a comment worker. The worker then fixed the outstanding items under a pause that would not lift on its own. The ADR-1408 "existing pause comment → defer" rule did not apply: the earlier pause was a clean-stop pause, so no CI-timeout pause comment existed.

## Decision

The backstop keeps its timeout, its ADR-1408 deferral, its escalation message and its per-poll cost bound. It gains a corrected anchor and two guards, checked cheapest first:

1. **R3 — never with a worker in flight.** `snap.Worker() != nil` (the existing idiom, set synchronously at dispatch) defers the backstop for that poll and logs the stage. Only the backstop is deferred; the handler chain still runs. The next poll re-evaluates once the worker has exited, so the bound still holds.
2. **R2 — once per item per process, live first.** The backstop may not escalate an item the scan has not yet run the live-data chain for since the daemon started (`Engine.ciBackstopLiveEvaluated`, a `sync.Map`). The marker is set after the chain runs, so early `continue`s do not count. It is deliberately in memory: losing it on restart is the intent (downtime is not evidence that CI is stuck). Cost: a genuinely stuck item is escalated one poll later than before, and each awaiting-ci item costs one extra live evaluation per daemon start.
3. **R1 — do not count paused time.** The anchor is `max(appliedAt, latest fabrik:paused removal)`.

### The resume time

- A new accessor, `GitHubClient.FetchLabelRemovedAt` (newest `unlabeled` event for a label, zero when none), sharing a private paging helper with `FetchLabelAppliedAt` (same `restMaxPages` bound).
- Distinct from `FetchLabelAppliedAt`, not a reuse of it with the paused label: `mockGitHubClient.fetchLabelAppliedAtFn` returns one value for any label name, so reuse would have made every existing mock-based test read the awaiting-ci timestamp as the resume time.
- Read live from the event log, **not** cached in `ItemState.LabelAppliedAt`: a removal made in the UI would leave a stale entry (the reason `resumeAuthorised` bypasses the cache, ADR-1813).
- **Fail direction is the opposite of ADR-1813.** `resumeAuthorised` resumes on any indeterminate input, because a pause nobody can lift is the failure to avoid. The backstop falls back to `appliedAt` and still escalates, because failing open into "never escalate" would lose the per-poll cost bound that is the backstop's whole purpose.
- The read is only made after the cheap label-anchor, pause-comment, in-flight and first-evaluation checks pass, so it runs only for over-timeout items.
- A skip-only memo (`Engine.ciBackstopResumeSeen`) holds the newest removal time read. A resume still inside the timeout skips without another events page-through; the memo can only be older than the truth, so it can only produce a skip, and an escalation always follows a live read. Without it a recently resumed item would cost one events scan per poll for up to the timeout.

### Clock

The comparison stays on wall-clock `time.Since`, as every sibling timeout does. Moving the backstop to `e.now()` alone would change one line shared in spirit with others, and could flip existing sim tests that rely on the sim's default clock 24h in the past. Tests pin anchors explicitly instead (a past `StartTime`, advancing the injected clock).

### Sim

`simgh.LabelEvent` gains `At` (stamped from the injected clock in the same single read as applied-at), and `Sim.FetchLabelRemovedAt` answers from that log. A parallel `labelRemovedAt` map was rejected: GitHub's event log is the faithful model, and `RemoveLabelFromIssue` deletes the applied-at entry, so the log is where the history survives.

### Neutralisation

There is no repo convention or production test hook for proving a guard's test fails without the guard, and none is added. Each guard has a unit test that leaves the other two open, so that guard alone separates "escalated" from "not". A manual mutation pass (remove each guard in turn, restore it) confirmed that exactly the matching tests fail; the sim twin (`tests/sim/ci_backstop_resume_test.go`) fails with the R1 anchor removed.

## Sibling timeouts (fixed by #2064)

The same "paused time counts" defect existed wherever a timeout anchors on a never-reset label timestamp: `classifyCIFromMergeableState`'s R3 never-checked and mergeable-state-blocked dwells (`engine/ci.go`, `CIWaitTimeout`), the merge-queue stall dwell (anchored on `fabrik:auto-merge-enabled`) and `ConvergenceBudget` (`engine/merge_gate.go`). They are reached only after a live CI read, so they were not blind pauses, but a just-resumed over-timeout item whose live state is "no check runs and blocked" could still be re-paused through them with the smaller `CIWaitTimeout`.

#2064 fixed all four by moving the anchor logic into one shared helper, `effectiveAnchor` (`engine/resume_anchor.go`): the later of the label's applied time and the latest `fabrik:paused` removal, with the same fail direction (unreadable falls back to the label anchor and still escalates). The #2059 backstop now calls it too. Decisions:

- **Comparison stays at the call site.** The helper returns only a time. The dwells keep `>=` and `ConvergenceBudget` keeps its strict `>`; the `CIWaitTimeout > 0` and `ConvergenceBudget > 0` disable guards and the escalation messages are untouched. Each site tests the cheap label anchor first and calls the helper only once already over its timeout, so items under the timeout cost no event-log read.
- **One shared resume memo, per-call window.** The memo stores only the raw removal time; each caller tests it against its own timeout (4h backstop, 30m dwell, budget), so a resume that is recent for one site cannot wrongly skip at a shorter one.
- **`pauseForConvergenceFailed` reports elapsed from the effective anchor** — the time that actually counted toward the budget.

`classifyCIFromCheckRuns`'s liveness stall uses the in-memory `LastCIProgressAt`, which a restart resets to "never observed" (already safe, unchanged).

Neutralisation (same manual mutation pass as above): replacing the `effectiveAnchor` call with the raw label anchor at each of the five sites in turn makes exactly that site's "resumed recently is not escalated" test fail (`TestCIWaitDwells_ExcludePausedTime` for each dwell, `TestMergeQueueStall_ExcludesPausedTime`, `TestConvergenceBudget_ExcludesPausedTime`, and the #2059 `Backstop_R1` tests).

## Consequences

- An item resumed from a pause gets the full `CIBackstopTimeout` window again; paused time and downtime no longer count.
- A genuinely stuck item can be escalated up to one poll later than before after a daemon start, and a stuck item with a long-running worker is escalated only after the worker exits.
- `GitHubClient` gains one method; all five implementers are updated.
- As-built behaviour: `docs/state-machine.md` §6.14.6.
