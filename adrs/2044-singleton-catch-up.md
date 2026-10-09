# ADR 2044: Merge-Train Singleton Catch-Up

**Date**: 2026-10-08
**Status**: Accepted
**Issue**: #2044 — a singleton that is behind the base catches up its own branch and lands via the fast path, not a trial

## Context

ADR-1644's singleton fast path lands a one-member batch directly from the member's own PR, but
only when the pinned base SHA is already an ancestor of the member's head. A member that is
*behind* the base falls through to a trial: a throwaway branch, a draft integration PR and a full
CI run (10–25 minutes), after which the trial is discarded and the member's own branch is exactly
as stale as before. On a busy board most landings are singletons and nearly every member is behind
by the time it reaches the front of the queue (observed on shadoworg/liminis-models, 14 merges
behind, and on every fast-path decline in verveguy/concept-maps's log; reported in #2041, and
proposal 1 of #1826).

## Decision

For a length-1 batch the fast path declines, `runMergeTrainWorker` calls `trySingletonCatchUp`
(`engine/merge_train_catchup.go`) before building a trial. If the member is behind the pinned base
it:

1. **Merges `p.baseSHA` into the member's own branch** with a merge commit — never a rebase, never
   a force-push, so history and review threads stay intact — in the member's own issue worktree
   (`WorktreeManager.PrepareCatchUp`/`PushCatchUp`). Conflicts go through the trial's path
   (`resolveTrainConflict`: rerere → regeneration → Claude, `NoResume`, the same turn budget); only
   the prompt is re-worded (`InvokeOptions.CatchUpBaseSHA`).
2. **Verifies the result is exactly the merge asked for** (two parents: the member's head and the
   pinned base), stamps a `Fabrik-Train-Catch-Up: <base sha>` trailer, and pushes with
   `--force-with-lease=<ref>:<expected head>` — a compare-and-swap that rejects a concurrent push
   instead of overwriting it.
3. **Posts a marker comment** on the member's PR (`<!-- fabrik:train-catch-up head=… base=…
   pure=… -->`) — the authenticated record the review gates read.
4. **Waits in the worker for the member PR's own CI** on the new head (`waitMemberCI`:
   `classifyLandingCI`/`ciSuiteHold` unchanged), then re-enters the existing `trySingletonFastPath`,
   whose eligibility checks (live head == the caught-up commit — R5's TOCTOU check — pinned base an
   ancestor, mergeable, non-zero green and complete CI) are the landing gate, unmodified, and lands
   with `MergePRAtHeadSHA`.

A new config switch `singleton_catch_up: merge | off` (default `merge`; flag
`--singleton-catch-up`, env `FABRIK_SINGLETON_CATCH_UP`, invalid value → warn and use `merge`)
turns it off, restoring today's trial path byte for byte, for repos that forbid merge commits on PR
branches. No rebase variant is built.

### Worktree ownership (R4)

The catch-up pushes to a branch that a stage session or a human may be using. It defers — the
member stays Queued, nothing is charged, no trial is built that poll — when a stage session is in
flight on the member (`itemstate` worker), `fabrik:editing` is set (read **live**, not from the
batch snapshot; an unreadable label set also defers), the issue worktree is dirty or mid-merge, or
the remote branch is no longer the head the batch was snapshotted at. The checks run before the
merge and again before the push, since a conflict resolution can take minutes. A local branch that
is ahead of or diverged from the remote carries unpushed work: it is never touched and the member
falls back to the trial.

Running in the member's own worktree (rather than a disposable detached one) keeps the local
`fabrik/issue-N` equal to the remote after the push. A detached push would leave the local branch
stale, and a later stage's `PushBranch` (`--force-with-lease` against the remote-tracking ref the
same bare repo just advanced) could overwrite the catch-up commit. `PushCatchUp` also advances the
remote-tracking ref. On any failure after the merge the worktree is reset to the member's head.

### Uncharged fallbacks

A rejected push (branch protection, signed commits, the member moved), a refused
`PrepareCatchUp`, an unexpected merge shape, or CI that never starts on the new head (no check run
within a dwell; the fast path rejects zero check runs anyway) fall back to the trial for that poll.
None of them calls `recordTrial`, `ejectMember`'s counting or a pause; the runaway guard and
`MaxMergeTrainEjections` are untouched. Only an unresolvable conflict ejects (via `ejectMember`,
exactly as the trial would, with the reason naming the catch-up) and only a confirmed red pauses.

A per-member attempt counter (in memory, keyed by train and issue, cap
`effectiveMaxTrainRebaseCycles()`, reset on landing and on eject) hands a member that keeps getting
caught up without landing — a very busy base — to the trial. No new knob.

### Review churn (R3)

Pruefer reviews every new head, and since #1953 an unaddressed review body holds landing while
`settleQueuedReviewFindings` ejects a Queued member with review findings — which would eject the
member the catch-up meant to land. A bot review or review-thread comment is **non-actionable for
this landing** only when all of the following hold (`engine/catchup_feedback.go`):

- the member's PR carries a marker comment **authored by the engine's own login** whose head is the
  PR's live head;
- the marker says `pure=true` — the merge brought in only base commits, with no conflict-resolution
  edits (a clean `git merge`; a rerere-replayed, regenerated or Claude-resolved merge is not pure
  and its reviews are actionable as usual);
- the finding was made against exactly that head — an exact SHA match on the review's `commit` /
  the thread comment's `originalCommit`, never a timestamp (the board query now selects both);
- the author is a bot (`gh.IsBotLogin`, or an identity declared in a stage's `expected_reviewers`).

Everything else — a human, an older head, no attribution, no marker, a marker from another author,
an unreadable marker — stays actionable (fail closed). Human comments are never filtered. The rule
is applied in the two shared places, `queuedReviewFindings` (the settle scan and the worker's eject
checkpoints) and `unprocessedFeedback` (the landing/advance gate), so they cannot disagree.

The marker lookup is cached per (PR, head): a found marker for good, "no marker" for two minutes
(a stale negative only keeps findings actionable, the fail-closed direction), a read error never. The
worker drops the entry when it posts that head's marker. Without the cache the Queued settle scan
would read the PR's comments every poll for every bot-reviewed member, caught up or not.

**The commit trailer is not trusted.** The member branch is writable by anyone with push access, so
a forged trailer must not be able to mute review feedback. It is for audit and for a possible future
Pruefer skip only; the engine's decision rests on the self-authored marker, the live head and the
per-finding commit SHA. A restart between the push and the marker comment merely leaves reviews
actionable for one cycle.

The trade-off is explicit: a genuine finding on a merge-only head is not acted on; the member's own
CI on that head (which must be green and complete before landing) is the safety net. Whether Pruefer
should itself skip a catch-up-only push is a separate Pruefer change; the engine is safe either way.

### The red disposition

A confirmed red on the caught-up head first re-runs the failed jobs once (#2052's flaky-job
handling, reusing `rerunFailedWorkflowRuns`; the member's PR is never closed and reopened, which is
a trial-PR technique) and only a second failure is red. It then takes the red-singleton disposition
the spec names — reroute off Queued, comment, pause (`ejectRedCatchUpSingleton`, `ejectRedSingleton`
with catch-up wording) — because the failure is on the member's own PR. The comment names the
catch-up commit, since a failure caused by an interaction with newer base commits is otherwise
indistinguishable from the member's own defect. `deferRedMember` (ADR-1821: reroute without a pause)
was considered, but it only works when the stage before Queued has `wait_for_ci: true`, so it cannot
be the default.

### `landOneAtATime` (R6)

Unchanged: a batch of two or more keeps trial branches, and the one-at-a-time fallback does **not**
use the catch-up. That fallback exists because members interacted in a combined trial, which a
member-only CI cannot see; a catch-up per member would also serialize N CI waits. ADR-1644 likewise
kept it out of the fast path. The hook sits in the re-form loop, after the fast path and before the
trial, for the same loop-level reason ADR-1644 gave — never inside `assembleAndValidate`, so
bisection sub-trials are structurally out of reach.

## Consequences

- A behind singleton lands in one member CI run instead of a trial CI run, with no throwaway draft
  PR; total CI is the same or lower, but the member's own PR now shows an extra merge commit and CI
  run, and a Fabrik-authored comment explaining it.
- On upgrade with the default `merge`, repos that previously only got trials start receiving
  Fabrik merge commits on member branches. Repos that forbid them hit a rejected push and fall back
  per poll (a per-repo memo is a possible follow-up), or set `off`.
- The worker holds its slot and the engine semaphore while it waits for the member's CI, bounded by
  `CIBackstopTimeout`, as a trial does; on timeout the member stays Queued.
- The catch-up gives up the main-moved recovery the trial path has (`landGreenBatch`): base movement
  during the wait is tolerated because the fast path only checks ancestry of the *pinned* base.
- If CI is not triggered by the catch-up push (e.g. an installation-token push to a workflow that
  does not run on it) the engine waits one dwell (default 10 minutes) and falls back to the trial.
- A restart loses the attempt counter; the next worker may catch up again within the cap.

## See also

- ADR-1644 (the fast path this routes behind-singletons into), ADR-1208/1863 (the Queued eject scan
  recognition must not defeat), ADR-1953-a (the feedback gate), ADR-1821/1545 (red-member
  dispositions), ADR-2052 (CI infrastructure failures), ADR-1648 (per-(repo, base) partitions).
- `docs/state-machine.md` §6.30; §6.20 and §6.16 updated.
