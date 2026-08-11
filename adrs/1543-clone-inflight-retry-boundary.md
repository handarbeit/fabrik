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
- **Documented residual limitation — merge-train's `batch[0]` anchor**:
  `prepareTrainWorker` (`engine/merge_train.go:496`) calls
  `ensureRepoReady(ctx, batch[0])` using an arbitrary Queued-column member as
  the repo anchor. If that specific (now-paused) item is never reselected as
  `batch[0]` on a later poll — e.g. another Queued item consistently sorts
  first — the repo's `cloneInFlight` entry stays failed and every merge-train
  pass for that repo keeps silently skipping (safe: never duplicates the
  clone or comment) rather than retrying, until either that item does become
  `batch[0]` again or the process restarts (an existing, uncontested reset
  boundary for `cloneInFlight` — restarts already clear all entries). This is
  a pre-existing characteristic of the batch-anchor design's arbitrary member
  selection, not introduced or worsened by this fix — the old code's
  unconditional `Delete` traded away race-safety for unconditional
  retry-liveness in exchange for the very duplication bug this ADR fixes.
  Redesigning batch-anchor selection to prefer a paused member (so it's
  reselected and can self-heal) is a reasonable follow-up but is out of this
  issue's scope.
- `cloneInFlight` remains per-process, unpersisted; an engine restart still
  clears all entries unconditionally, as before.
- No user-facing behavior change: `ErrSkipItem` for all non-owners,
  `fabrik:paused`/`fabrik:awaiting-input` on the affected item(s), and one
  comment per genuinely distinct failure are all unchanged in shape — only
  the internal coordination correctness and the precise retry-boundary
  semantics improve.
