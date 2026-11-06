# ADR 2542: Act on a merge-train trial's CI verdict within one poll of completion

## Status

Accepted. Builds on ADR 2051 (persisted run, per-poll evaluation) and ADR 2528 (cooldown caps on the
idle interval); leaves ADR 2483 (train PRs never attached to a member) unchanged.

## Context

On the 0.0.91 gate (engine `000885c6`) trial CI took 8–20 s, but each verdict waited 2–10.6 minutes
for a poll, and a bisection pays that on every trial. The trial is evaluated only by `settleTrainRuns`,
which runs when a poll runs, and nothing shortened the wait: an open trial is neither activity nor a
cooldown, so the idle backoff climbed (2× → 4× → ~5m24s); where an admission cooldown was pending, the
#2528 cap (`due + one interval`) set the next poll at 10m51s. The #2483 guard also drops every
`pull_request`, review and `check_run` delivery for a `fabrik/merge-train/` head, so even a healthy
webhook transport woke nothing when the trial's checks completed.

## Decision

1. **Open-trial cap (R1, the guarantee).** `PollWithBackoff` caps the idle interval at one configured
   interval while any live, unfinished run's persisted record holds a trial (`openTrialPresent`). It is
   applied after the short- and long-cooldown caps and the forge-hold cap, only ever shortens, is
   skipped under GraphQL rate-limit backoff, and does not touch `pollResult.Active`. It works on a
   poll-only engine. Making an open trial count as activity was rejected: it would reset the idle
   backoff for unrelated work and change activity accounting.
2. **Trial PR wake (R2, an acceleration).** `applyDelivery` calls `wakeForOpenTrial` before the board
   cache's delta. It matches a `completed` `check_run`/`check_suite`, or any `pull_request` event,
   against the persisted trial of each live run by repository and head SHA (and PR number when the
   payload carries one) and sends a non-blocking wake. The match lives in the engine, not in a callback
   from `boardcache`'s `skipTrainPR` sites: that makes "the wake never reaches `resolvePRLinkage`"
   structural and leaves the #2483 guard byte-identical. This deviates from the issue's scope line
   listing the boardcache handlers; the requirement (the wake must not reach the heal) is met more
   strongly. Head SHA is the key because a fork's `check_run` has an empty `pull_requests` list and a
   `check_suite` has no number.
3. **R3: no engine change.** The survivors were paused by the failing live test's cleanup
   (`repauseOnFailure`); the engine logged no pause. `refuseIfMembersPaused` (ADR 2341) refused the
   landing as specified and the run finished, releasing the partition. The extra ~11 minutes were the
   admission-cooldown cap, which decision 1 bounds. Releasing a run with paused members earlier would
   not be a fix; refusing is the specified behaviour. A sim test pins the shape.
4. **R4a: the stale `train CI red` line.** The dispatch guard prints the claim's `CIResult`, set only
   at a verdict. It is now reset to pending where a trial opens or is adopted. Log only; no decision
   reads it.
5. **R4b: `MERGE_RR.lock`.** A refusal naming a leftover lock in the forget-reconstruction worktree
   removes the lock, aborts the half-applied merge (the refused merge leaves a conflicted index that
   blocks a second merge) and retries once; a replay that still fails keeps the skip-forget path with
   the lock lines trimmed from the warning. The removal is justified by exclusivity, not age: the
   worktree is created for the reconstruction and each git command has exited before the next starts,
   so a lock found there is not held by a process of ours. No decision reads the result.

## Consequences

- Verdict latency on a poll-only engine is at most one configured interval (plus the poll), and
  shorter where deliveries arrive. While a trial is open the engine polls once per interval; the
  trial's own deadline bounds that.
- A wake can arrive before `FetchCheckRuns` shows the completed run; the poll then sees pending and the
  cap guarantees the next one. The wake is not relied on.
- The origin of the leftover lock is not established. The tolerance may hide a second git process in the
  same worktree if one exists; none is known, and the worktree is single-owner by construction.
- A neutralisation seam exists for each of decisions 1 and 2 (`SetOpenTrialCapDisabledForTest`,
  `SetTrialWakeDisabledForTest`), plus `SetIdleStartForTest` because `idleStart` is wall time.

## Tests

`engine/train_wake_test.go` (decode, match matrix, cap ordering against the long-cooldown cap, stale
claim result), `engine/merge_train_rerere_lock_test.go` (real git, planted lock),
`tests/sim/mergetrain_trial_latency_test.go` (poll-only cap, intake wake, heal-guard pin, paused-members
release), each with a non-vacuity variant.
