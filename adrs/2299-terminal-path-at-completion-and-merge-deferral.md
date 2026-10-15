# ADR 2299: Terminal Path at Stage Completion and Merge Deferral While a Worker Runs

## Status

Accepted

## Context

Report #1810: a Validate worker that returns after its own PR merged armed `fabrik:awaiting-ci` against the merged PR. Two defects combined. `handleStageComplete` never looked at the PR, and `settleAwaitingCIScan` → `runCatchUpPhase2` could merge a Validate item while a worker for that item was running (only the CI backstop checked for an in-flight worker). A post-stage push to the branch GitHub had deleted on merge was only a warning, and could re-create the branch.

## Decision

1. **Live terminal check at completion, Validate only.** `handleStageComplete` classifies the linked PR live (`classifyLinkedPRTerminal`: `e.client`, `FetchPRMerged` confirmation for a closed PR). Merged → `advanceMergedValidateItem`, the merged branch of `advanceValidateTerminalItem` extracted verbatim and shared; closed unmerged → `pauseForPRClosedNotMerged`; read error → arm as before. The terminal helper is Validate-specific and only Validate ships `wait_for_ci: true`, so other `wait_for_ci` stages keep today's behaviour; the settle scan's `PRMergeTerminal` handling covers them.
2. **Merge deferral in `runCatchUpPhase2`.** While a worker is registered for the item the merge decision waits and a `merge-unsettled` admission cooldown (`mergeGateRecheckInterval`) re-admits the item (ADR 2096). Placed in `runCatchUpPhase2` rather than `attemptMergeOnValidate`, which the worker itself calls. An already-merged PR takes the terminal path before the guard (advancing merges nothing).
3. **Pre-push merged check.** `pushBranchUnlessQueued` and `pushBranchOnCancel` skip the push for a merged PR (`ErrPushSkippedPRMerged`), failing open on a read error. Parsing git's error text cannot stop the case where the push succeeds and re-creates the branch. Cost: one REST read per post-stage push.
4. **Orphan escalation never fires for a landed item.** `clearMergedOrphanAwaitingCI` back-fills a withheld `stage:*:complete` label, then clears the leftover label instead of retrying or pausing with the "move back to Validate" advice. It does not repeat the Done advance or landing-verification marker. Every `ErrPushSkippedPRMerged` caller (post-stage, spec-commit, cancellation, marker-failure and `markPRReady`) treats it as informational.

## Consequences

- **ADR-1216 same-poll handoff.** The CI-clear → landing handoff is delayed by at most one `mergeGateRecheckInterval` (half a poll interval), and only while a worker for the item is running. Gate evaluation and label clearing are unchanged. A stuck worker holds the landing until it exits or its wall-time limit kills it; cancelling the worker when its PR merges is out of scope and a possible follow-up. The guard also holds a merge while a comment-review worker runs on a `hasComplete` item, which is intended.
- No existing guarantee breaks, so the requirement stands.
- A wrong "merged" read would close the issue and mark landing verification; closed-but-unflagged PRs are therefore always confirmed with `FetchPRMerged`, a read error falls back to arming, and `settleLandingVerification` remains the backstop.
- The terminal path now also runs on the worker goroutine; its steps are idempotent (add-if-absent, remove-if-present, `ErrNotFound` tolerated) and `awaitingAdvanceStuckOrReset` prevents an unpause/re-escalate loop.
- A considered extra guard — not re-arming `fabrik:awaiting-ci` when `stage:<X>:complete` is already live — was dropped: the comment-review path legitimately re-arms the CI gate after pushing new commits to a completed stage.
