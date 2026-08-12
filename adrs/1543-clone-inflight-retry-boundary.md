# ADR 1543: Identity-Gated Retry Boundary for `cloneInFlight` Failures

**Status:** Accepted

**Supersedes (in part):** ADR 022 (Per-Repo Singleflight Coordination for Bare Clones) — specifically its "Failure-path cleanup" and "Ordering invariant on failure" sections, which describe the buggy protocol this ADR replaces. ADR 022's election protocol, waiter-blocking design, and success-path handling are unchanged and remain in force.

## Context

ADR 022 introduced `cloneInFlight`, a `sync.Map`-based singleflight mechanism so
concurrent callers for the same repo elect one clone owner instead of racing
`git clone --bare` into the same destination directory. On failure, ADR 022's
protocol was:

```go
close(call.done)                       // release waiters
e.cloneInFlight.Delete(nameWithOwner)  // <-- entry gone here
msg := ...
e.pauseIssue(item, msg, ...)           // AddComment happens here
...
return ErrSkipItem
```

ADR 022 justified the `Delete` as enabling "future poll cycles (after the user
removes `fabrik:paused`)" to retry, and asserted the ordering invariant was safe
because "a re-entrant goroutine from the *next* poll cycle" is the only thing
that could see the cleared entry.

That assertion was wrong. `Delete` ran *before* the failure was fully handled
(`pauseIssue`'s comment + label writes, plus a TUI history append). Any
goroutine — including one from the *same* burst that simply hadn't reached
`LoadOrStore` yet — could observe the gap and become a second owner: re-running
`ensureBareClone` (wasteful, and for `ensureSpawnTargetReady` a second
concurrent `git clone --bare` into the same directory — the exact corruption
race ADR 022 exists to prevent) and posting a second "cannot clone repo"
comment.

This surfaced as an intermittent CI failure
(`TestEnsureRepoReady_ConcurrentSameRepo_OnlyOneCloneAttempt`) that reached
production: it failed a merge-train trial for an unrelated, clean PR (#1450) on
2026-08-11, pausing it with actionless advice ("fix the failing check(s) on
this PR"). The test had passed 5/5 on `main` and 40/40 under `-race` — it
caught the defect by luck of goroutine scheduling, not by design. See #1543.

### Why "just defer the `Delete`" is not enough

The obvious fix — move `Delete` to after `pauseIssue` completes — narrows the
race window but does not close it. `poll.go`'s dispatch loop is itself
throttled by a semaphore (`MaxConcurrent`, default 5): with more than
`MaxConcurrent` concurrent callers for one repo, the excess callers don't even
reach `ensureRepoReady` until an earlier worker (possibly the clone owner
itself) releases its semaphore slot — which can happen only *after* the
owner's entire failure-handling sequence has finished. A same-burst sibling
can therefore legitimately arrive after a deferred `Delete` too. Any complete
fix has to define the retry boundary in terms of something more precise than
"before" or "after" a fixed point in the owner's own control flow.

## Decision

Replace the unconditional `Delete` with an **identity-gated retry boundary**:
a failed `cloneInFlight` entry is only ever cleared by a caller whose own
identity matches the item that owned the failed attempt, *and* whose own
(already-fetched) `Labels` confirm `fabrik:paused` is no longer present.

### Protocol changes

`cloneCall` gains an `ownerKey` field (issueKey format, `"owner/repo#N"`),
set only on failure, before `close(call.done)`:

```go
call.ownerKey = callerKey   // the failing owner's own issueKey
close(call.done)
// no Delete here
e.pauseIssue(item, msg, ...)
...
return ErrSkipItem
```

The `Delete` is not moved — it is removed from the owner's failure path
entirely. The entry now persists until a *waiter* clears it:

```go
if existing.err != nil {
    if existing.ownerKey == callerKey && !hasLabel(callerLabels, "fabrik:paused") {
        e.cloneInFlight.CompareAndDelete(nameWithOwner, actual)
        continue // retry as a fresh owner
    }
    return ErrSkipItem // (or, for ensureSpawnTargetReady, post own comment + return error)
}
```

`ensureRepoReady` and `ensureSpawnTargetReady` are both restructured as a
`for` loop around the existing election/wait logic so a successful identity
match can retry in-place without recursion.

### What "a later poll may retry" now means, precisely

Only the *exact item* that owned the failed attempt can ever satisfy
`existing.ownerKey == callerKey`. That item carries `fabrik:paused` the moment
`pauseIssue` returns, and a paused item is never redispatched — dispatch
admission excludes paused items everywhere in this codebase. So the earliest
any caller can pass the identity+label check is a **strictly later poll
cycle**, specifically: the poll cycle after an operator removes
`fabrik:paused` from that item and the engine redispatches it. No sibling from
the original burst can ever match, regardless of how the semaphore staggers
its arrival, because it is provably a *different* item — this closes the gap
that a deferred-`Delete`-only fix leaves open for `N > MaxConcurrent` bursts.

For `ensureSpawnTargetReady`, the identity is the spawning `parentItem`
(`postSpawnCloneError`'s existing target), not the cloned repo's own item —
there is no "the repo's own item" for a spawn target, only the parent(s) that
discovered it.

### Test seam: `cloneAttemptHook`

A new `cloneAttemptHook func(nameWithOwner string)` field on `Engine`
(nil in production, mirroring the existing `trainRedBatchHook` precedent) is
invoked once per genuine owner attempt, immediately before `ensureBareClone`.
This gives tests a direct clone-attempt count independent of `AddComment`
counting — necessary for `ensureSpawnTargetReady`, where every distinct
parent legitimately posts its own comment by design, so comment-counting
cannot distinguish "one clone, N comments" from "two clones, N comments."

### Deterministic regression tests (R5)

The flaky goroutine-barrier test
(`TestEnsureRepoReady_ConcurrentSameRepo_OnlyOneCloneAttempt`) is kept — it is
still real concurrency/`-race` coverage — but fixed to give each goroutine a
distinct item number (matching real production bursts, where different board
items race on a never-before-cloned repo; a shared item number would trivially
satisfy the new identity gate for every goroutine, defeating the test).

New sequential (non-racing) tests prove each edge of the gate directly:

- **Different item after failure** → no second clone attempt, no second
  comment (proves the identity half of the gate).
- **Same item, still paused** → no retry (proves the label half of the gate;
  guards against the R2 wedge risk in the other direction — retrying too
  eagerly).
- **Same item, unpaused after fix** → retries and succeeds, demonstrating
  AC3's "fails, then succeeds on a later attempt." The bare-clone destination
  is pre-created as a real (empty) bare repo (`git init --bare`) so
  `ensureBareClone`'s existing "already exists → repair" path returns success
  deterministically, with no network dependency (per
  `.claude/rules/golang.md`).

The same three-shape coverage is added for `ensureSpawnTargetReady`, keyed on
`parentItem`.

**Non-vacuousness verified**: with the identity check in both functions
temporarily replaced with an unconditional `true` (reintroducing the pre-fix
"any waiter can retry" behavior), the new deterministic tests fail on 20/20
runs — not intermittently. This was verified locally and reverted before
committing the fix; see the PR description.

## Alternatives Considered

### Defer `Delete` until after `pauseIssue` completes

Simplest, smallest diff. Rejected as incomplete: as explained above, it
narrows but does not close the race for repos with more concurrent callers
than `MaxConcurrent` — a semaphore-delayed sibling can still legitimately
arrive after the owner's entire failure-handling sequence and become a second
owner. R1 requires "regardless of scheduling," which this does not achieve.

### Settle scan tied to the paused label

Clear the failed entry via a per-poll settle scan (following the established
pattern in `engine/ci_settle.go`, `engine/claude_limit_settle.go`, etc.) that
observes the owner item's `fabrik:paused` removal. This is the literal reading
of R2's wording and would close the same-burst gap identically to the chosen
design. Rejected in favor of the identity gate because it requires a new file,
new `poll.go` wiring, and an extra GraphQL round-trip per poll for something
answerable entirely from data already in hand (the map entry's recorded owner
+ the caller's own already-fetched `item.Labels`) — no live GitHub call is
needed, since `ensureRepoReady`/`ensureSpawnTargetReady` are themselves called
with a freshly-fetched `item` on every dispatch.

### Time/generation-based cooldown

Retain the failed entry with a timestamp or generation counter, clearing it
after a fixed cooldown independent of label state. Rejected: it would
silently retry (and re-fail, re-comment) on a schedule even if the operator
never fixed anything, contradicting the pause message's own instruction ("fix
the clone issue and remove `fabrik:paused` to retry"). It also has no
existing precedent in this codebase for this kind of state — `generation`
appears only in `merge_train.go` and `generated_files.go`, for unrelated
concepts — so it would introduce a novel pattern rather than reuse an
established one.

### Live per-waiter fetch of the owner item's current labels

Instead of trusting the caller's own already-fetched `item.Labels`, have a
waiter live-fetch the owner item's current state via `FetchItemDetails`
before deciding whether to retry. Rejected as unnecessary: the identity check
already restricts "who can retry" to the specific owning item, and that item
only ever calls `ensureRepoReady`/`ensureSpawnTargetReady` again via a fresh
dispatch (which always re-fetches its own labels first) — a live fetch inside
the singleflight path would be redundant GraphQL cost for no additional
correctness.

## Consequences

- **R1 (one attempt, one comment per failure, per burst)**: satisfied for
  arbitrary N, not just `N <= MaxConcurrent` — see "What 'a later poll may
  retry' now means, precisely" above.
- **R2 (retry across poll cycles)**: preserved, redefined precisely as "the
  owning item, redispatched after `fabrik:paused` is confirmed removed."
- **R4 (`ensureSpawnTargetReady`)**: fixed with the identical protocol, not
  exempted — its defect (concurrent-clone directory corruption) was more
  severe than `ensureRepoReady`'s (a cosmetic duplicate comment).
- **Merge-train's `batch[0]` anchor — detected and escalated, not silently
  wedged**: `prepareTrainWorker` (`engine/merge_train.go`) calls
  `ensureRepoReady(ctx, batch[0])` using an arbitrary Queued-column member as
  the repo anchor. Because `batch[0]` can be a different item on every poll
  whenever Queued membership churns, a later anchor's identity generally
  cannot match the `ownerKey` pinned by the original failure, so the retry
  gate above cannot reopen for it — left unaddressed, the repo's
  `cloneInFlight` entry would stay failed and every merge-train pass for that
  repo would keep silently skipping (safe: never duplicates the clone or
  comment) rather than retrying, until either the exact original anchor is
  reselected or the process restarts. See "Escalating the `batch[0]`
  wedge" below for how this is now surfaced instead of left as a silent,
  restart-only-recoverable condition.
- `cloneInFlight` remains per-process, unpersisted; an engine restart still
  clears all entries unconditionally, as before.
- No user-facing behavior change to the core `ensureRepoReady`/
  `ensureSpawnTargetReady` protocol: `ErrSkipItem` for all non-owners,
  `fabrik:paused`/`fabrik:awaiting-input` on the affected item(s), and one
  comment per genuinely distinct failure are all unchanged in shape — only
  the internal coordination correctness and the precise retry-boundary
  semantics improve. The merge-train anchor now additionally escalates on a
  bounded streak (see below) — a new, deliberate behavior, not a shape change
  to the core protocol.

## Escalating the `batch[0]` wedge (#1543 follow-up)

The original version of this ADR recorded the `batch[0]` anchor gap above as
an accepted, out-of-scope residual limitation — safe (no duplication) but
capable of leaving a repo's merge train silently skipping forever, recoverable
only by the exact original anchor being reselected or a process restart. Once
this PR was in front of a human reviewer, that framing was reconsidered: the
gap is *introduced by this PR* (the pre-fix code's unconditional `Delete`
never had this failure mode — it traded that liveness for the duplication bug
this ADR exists to fix), and it lands in merge-train machinery this release
actively depends on. A silent, restart-only-recoverable wedge in new
production code was judged not shippable as a footnote.

**Decision**: `prepareTrainWorker` now tracks a consecutive-skip streak per
repo (`Engine.mergeTrainCloneSkipCounts`, keyed `"owner/repo"` — repo-keyed
rather than item-keyed, since `batch[0]`'s identity is exactly what varies
across polls) each time `ensureRepoReady` returns `ErrSkipItem` for the
anchor. Once the streak reaches `e.cfg.MaxRetries`, `recordMergeTrainCloneSkip`
posts an explanatory **comment only** on the *current* anchor item, naming the
repo, the streak length, and — read from the live `cloneInFlight` entry — the
specific issue whose failed attempt still owns the retry gate, so an operator
knows exactly which issue's `fabrik:paused` to clear. It does **not** add
`fabrik:paused` (or any other label) to the anchor — see "Correction: the
anchor must not be paused" below for why. The streak resets to zero both on
escalation (so a fresh budget applies to any future streak) and whenever
`ensureRepoReady` succeeds for that repo (`resetMergeTrainCloneSkip`) —
mirroring `mergeTrainEjectionCounts`/`ejectMember`'s existing
counter-then-escalate-then-reset shape in the same file, and the
escalate-after-`MaxRetries` convention used throughout the ADR-1270 settle
scans elsewhere in the engine package. `MaxRetries <= 0` (unlimited) never
escalates, matching every other `MaxRetries`-gated escalation path.

**Alternatives considered**:

- *Promote the log line to a warning only* (no comment): makes the condition
  greppable in `fabrik.log` but leaves the operator-visible symptom (a repo's
  merge train quietly never landing anything) unexplained anywhere a human
  would normally look — GitHub, not the daemon's log file. Rejected as
  insufficient on its own for a condition this codebase otherwise always
  surfaces via a GitHub comment.
- *Pin the retry gate to the repo instead of the anchor's item identity*
  (removing the wedge at its source, for the merge-train call site only):
  considered, but the identity gate's correctness depends on checking the
  *caller's own already-fetched* `Labels` for `fabrik:paused` — a repo-level
  pin would make that check trivially pass for whichever new, never-paused
  item next becomes `batch[0]`, reopening a repeated-clone-attempt-and-comment
  pattern across polls for exactly this call site (a narrower echo of the bug
  this ADR fixes). Making it correct without that reopening would require a
  live label fetch inside the singleflight path — the same cost this ADR's
  main design explicitly avoids (see "Why not a live GitHub fetch" above) —
  or a structurally separate, merge-train-only clone-failure path bypassing
  the shared `cloneInFlight` coordination, which is exactly the
  batch-anchor-selection redesign Plan already judged out of this issue's
  scope. Rejected as more invasive than escalating the existing gap.
- *Resolve the pinned owner (`ownerKey`) to a real `gh.ProjectItem` and pause
  that instead of the anchor*: would target the item an operator actually
  needs to act on, but `ownerKey` is only a `"owner/repo#N"` string — turning
  it back into a full `gh.ProjectItem` (with the GraphQL node `ID` mutations
  need) requires an extra board lookup this path doesn't otherwise perform,
  and it would be redundant besides: that item was already paused with its
  own "cannot clone repo" comment by `ensureRepoReady` at the moment its
  clone attempt failed (see "What 'a later poll may retry' now means,
  precisely" above). Rejected as unnecessary complexity for a state that's
  already durable.

This closes the residual limitation as a *shippable* one: the repo is never
duplicated-cloned or duplicated-commented (R1 still holds), and it is now
never silently wedged past `MaxRetries` polls either — it escalates loudly
instead, following this codebase's established pattern rather than inventing
a new one.

### Correction: the anchor must not be paused

The first version of this escalation called `pauseIssue` on the current
anchor (adding `fabrik:paused` + `fabrik:awaiting-input` alongside the
comment), reasoning that the anchor was "the item a human reading the Queued
column right now can act on." PR review (both a bot review pass and a human
reviewer, independently) identified a real defect in that reasoning:
`batch[0]` is an **arbitrary, otherwise-healthy** Queued member — it did
nothing wrong, and pausing it removes it from dispatch eligibility exactly
like any other `fabrik:paused` item. Because fixing the *pinned owner's*
clone issue never un-pauses the *anchor* (they are different items, and
nothing links the two labels), every time the streak fired against a newly
rotated anchor, a **different**, previously-innocent Queued member would be
permanently exiled — with no effect on the actual wedge, since the anchor's
own clone was never even attempted. Left unaddressed, an unresolved clone
failure would progressively bench the repo's entire Queued population one
member at a time, purely as a side effect of an escalation mechanism meant
only to make the wedge *visible*.

The fix is the comment-only design described above: `recordMergeTrainCloneSkip`
now calls `postItemComment`, not `pauseIssue`. This preserves the goal
(the wedge is visible on GitHub, and the comment names exactly which issue's
`fabrik:paused` an operator needs to clear) without the collateral damage —
the anchor remains fully eligible for the next batch, since it never leaves
Queued in the first place.

### Correction: the anchor can *be* the pinned owner

A second review pass (again both a bot pass and independently) caught a
remaining defect in the escalation message itself: it unconditionally
asserted the anchor's "own clone was never attempted" and that it "has NOT
been paused." Both claims are false whenever the anchor *is* the pinned
owner — reachable on the very first `ErrSkipItem` for a repo (trivially with
`MaxRetries: 1`; also reachable at the default `MaxRetries: 3` if the same
anchor recurs across polls before its own `fabrik:paused` takes visible
effect). In that case the anchor's clone attempt is exactly what failed, and
`ensureRepoReady` already paused it with its own "cannot clone repo" comment
— telling that same issue to go look at a separately "pinned" issue that
doesn't exist would misdirect the one operator most able to act.

`recordMergeTrainCloneSkip` now compares `issueKey(anchor, e.defaultRepo())`
against the pinned `ownerKey` and branches the message: when they match, the
comment states plainly that this item's own clone attempt is the one pinning
the retry gate, and that it already carries `fabrik:paused`; only when they
differ does it use the original bystander framing ("its own clone was never
attempted... has NOT been paused"). Covered by
`TestRecordMergeTrainCloneSkip_MessageWhenAnchorIsOwner` (anchor-is-owner
case) alongside the existing `TestRecordMergeTrainCloneSkip_MessageNamesPinnedOwner`
(anchor-is-bystander case, now also asserting the bystander framing is
present).
