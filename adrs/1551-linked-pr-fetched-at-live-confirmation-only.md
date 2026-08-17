# ADR 1551: `linkedPRFetchedAt` Records Live Confirmation Only, By Design

**Date**: 2026-08-12
**Status**: Accepted
**Issue**: #1551 — boardcache: record that linkedPRFetchedAt tracks live confirmation
only — the poll-path coupling is a guard, not a defect

## Context

`CacheImpl.FetchLinkedPR`'s cache-hit path (`boardcache.go`) serves a Store-backed
`LinkedPR` record only when three conditions hold: the PR is linked, the record is
fully populated (`Title != "" && HeadSHA != ""`), and the record is **fresh**:

```go
fresh := hasFetchedAt && time.Since(fetchedAt) < linkedPRCacheTTL
```

`fetchedAt` comes from `CacheImpl.linkedPRFetchedAt`, a `map[string]time.Time` keyed
by item key (`"owner/repo#N"`). This field has exactly **one writer**: the line
immediately after `c.fallback.FetchLinkedPR` returns from a live GitHub call. No
webhook delta path writes it.

That asymmetry is easy to misread as a bug. Five sites in `delta.go` apply
`PRHeadSHAUpdated` — i.e. mutate the very `HeadSHA` field this TTL exists to
guard — from a webhook delta, and none of them touch `linkedPRFetchedAt`:

| site | function |
|---|---|
| normal path | `applyPullRequestDelta` (opened/closed/synchronize/reopened) |
| auto-heal path | `applyPullRequestDelta` |
| auto-heal path | `applyPullRequestReviewDelta` |
| auto-heal path | `applyPullRequestReviewCommentDelta` |
| auto-heal path | `applyCheckRunDelta` |

`synchronize` — GitHub's force-push/rebase event — is exactly the case
`linkedPRCacheTTL`'s own doc comment names as the failure mode: a webhook that
updates the Store's `HeadSHA` correctly and immediately, but whose delivery cannot
be trusted for correctness (only latency/cost), per ADR-003 and ADR-034 (see
Rationale below). A webhook-refreshed record is therefore still judged stale by the
TTL and gets refetched on the next `FetchLinkedPR` call. The cache can over-fetch;
it can never serve a record whose freshness rests on a channel — webhook
delivery — that may have silently dropped an event.

Nothing in the code said so before this ADR. A reader arriving at any of the five
write sites above, or at `linkedPRFetchedAt`'s own field comment, saw a delta path
mutating exactly the field a TTL depends on without ever refreshing that TTL's
trust — and no counter-signal explaining that this is intentional.

### Why this reads like an ADR-036 violation, and isn't

ADR-036 (`036-reactive-cache-single-owner.md`) establishes that all board-domain
state should flow through a single owner, `internal/itemstate.Store`, observable via
its mutation pipeline. Its own addenda record two precedents for exactly this kind
of migration, framed as **defects that were fixed**:

- **Phase 5 F2** (issue #562): `CacheImpl.linkedPRs` (PR detail fields — Title,
  State, Merged, Draft) and `CacheImpl.prNumToKey` (the PR→issue routing index)
  were both `CacheImpl`-local maps, invisible to the Store's observer pipeline.
  Both were migrated into the Store (`PRDetailsUpdated`, `Store.prToKey`).
- **Phase 5 F4** (issue #563): `CacheImpl.checkRuns`, a pre-linkage check-run
  buffer, was migrated to `Store.pendingCheckRuns`. F4's own closing line: "After
  F4, **every** per-item AND global pre-linkage state mutation flows through
  `Store.Apply`."

`linkedPRFetchedAt` is a bare `map[string]time.Time` on `CacheImpl`, guarded by
`CacheImpl.mu`, invisible to the observer pipeline — structurally identical to the
two maps F2 removed. A reader who arrives at `linkedPRFetchedAt` via those addenda,
especially via F4's "every… mutation" sentence, has an explicit, repo-sanctioned
template for "fixing" it the same way: migrate it into the Store, and stamp it from
`store.Apply` call sites including the delta handlers.

**That migration is exactly the change this ADR exists to reject.** Store membership
is what made `linkedPRs`/`prNumToKey`/`checkRuns` natural targets for delta-path
writes — that was the point of the migration for board-domain state that legitimately
needs multiple writers and observability. `linkedPRFetchedAt` is a different kind of
thing: it is not board-domain state describing what GitHub told us, it is
cache-trust bookkeeping describing *how we found out* — read only by its own
writer's sibling check (`FetchLinkedPR`'s freshness test), synchronously, under the
same `c.mu` lock that guards the write. Nothing anywhere needs to observe a change to
it; no `wakeChFlags` consumer, no dispatcher, no TUI. It fails every criterion F2/F4
used to justify migrating its siblings — it is precisely the kind of field the F2/F4
precedent does not apply to, and this ADR is the record that says so explicitly, so a
future reader (including an autonomous one) does not conclude otherwise from F4's
"every" alone.

### `#1378` proposes reversing this

Issue #1378 (open, `stage:Specify:complete` at the time of this ADR) proposes, as
its "Direction 1," making freshness "a first-class, source-agnostic property... have
*every* refresh path stamp it — poll, REST fallback, and webhook delta alike." Its
"Direction 2" proposes gating trust on webhook health instead. This ADR is the
argument against implementing either. #1378's other directions (cooldown-based
backoff for parked items, narrowing which callers need a live `HeadSHA`) are
cost-reduction work unaffected by this decision and remain #1378's to pursue.

## Decision

`linkedPRFetchedAt` continues to be stamped **only** by a live `FetchLinkedPR`
fallback fetch. No webhook/delta handler may write it, regardless of how directly
that handler's own mutation touches the field(s) the TTL guards (`HeadSHA` in
particular). This applies to all five `PRHeadSHAUpdated` write sites in `delta.go`
identified above, present and future — any new delta handler that writes
`PRHeadSHAUpdated` or otherwise mutates `LinkedPR` state must not also stamp
`linkedPRFetchedAt`.

`linkedPRCacheTTL`'s meaning is therefore: time since the last **live** GitHub
confirmation of this record, not time since the record's last mutation of any kind.

This changes no runtime behavior — the code already worked this way. The change is
that the reasoning is now recorded at the four sites in `boardcache.go`/`delta.go` a
reader is likely to arrive at, and here.

### Rejected alternatives

1. **Stamp `linkedPRFetchedAt` from `applyPullRequestDelta` (or any delta handler)
   on every SHA-tracking event** (`#1378` Direction 1). Rejected because it makes
   TTL freshness a function of webhook delivery. ADR-034's own Trade-offs section
   is explicit about the cost model this design already accepts: "Cache coherence
   depends on the webhook stream. Missed events produce stale reads until the next
   reconciliation (up to 60 min)." Stamping freshness from that same channel would
   let an unconfirmed signal satisfy a freshness bound whose entire purpose is to
   require confirmation. Per ADR-003 (`003-polling-over-webhooks.md`, "Superseded
   by" section): "The core principle of this ADR — that polling is the reliable
   foundation — is preserved: the cache reconciles against GitHub every 60 minutes
   and falls back to live API calls when the webhook stream is unhealthy." Webhooks
   exist to cut latency and cost between polls; per
   `docs/cache-refactor/05-webhook-correctness-audit.md`, "if webhooks are disabled
   or unhealthy, does the poll loop alone still converge on the correct GitHub
   state? … the answer must always be yes." A delta-path stamp would make that
   answer "no" for this field.

2. **Gate trust on webhook health instead of a live stamp** (`#1378` Direction 2).
   Rejected because health state itself lags and flaps — it doesn't eliminate the
   missed-event risk, it only makes the resulting failure intermittent and harder
   to reproduce. Measured on the dev instance over the same ~9h window cited below:
   76 `unhealthy` transitions. A gate keyed on a signal that flaps 76 times in 9
   hours is not a reliable substitute for a live confirmation.

3. **Migrate `linkedPRFetchedAt` into the Store as a first-class field**, following
   the ADR-036 Phase 5 F2/F4 template. Rejected because Store membership is exactly
   what would make the field a natural target for delta-path writes later — the
   whole point of migrating board-domain state into the Store is so more call sites
   (including delta handlers) can legitimately mutate it and have the mutation
   observed. `linkedPRFetchedAt` needs the opposite property: exactly one writer,
   forever. It stays a `CacheImpl`-local field for that structural reason, not
   because the F2/F4 migration was overlooked.

## Evidence

The trade-off (occasional redundant refetches, in exchange for never trusting an
unconfirmed channel for correctness) is favorable, measured on the dev instance over
a ~9h window (`.fabrik/fabrik.log`, `poll: 30`):

- **Cost is negligible.** 102 `stale: FetchLinkedPR` events total (~11/hour),
  against ~11,337 GraphQL units consumed (~1,260/hour, ~25% of the 5,000/hour
  ceiling). The redundant refetches this decision costs are order-1% of budget.
- **The cited items are already cooldown-gated, not refetched per poll.** The
  worst offender (#1254, `stage:Validate:complete`, parked on a human merge
  decision) refetches at ~327s intervals — `10 × PollSeconds` (300s) rounded to the
  next poll boundary, i.e. `CooldownAt` (`engine/catch_up_handlers.go:322`,
  `engine/poll.go:1126`). The 45s TTL is not the binding constraint for parked
  items; the cooldown is.
- **Webhook health on this instance cannot support gating freshness.** 76
  `unhealthy` transitions in the same ~9h window — see rejected alternative 2 above.

## `#1303` provenance

`linkedPRCacheTTL`'s own doc comment cites `#1303`, and this ADR inherits that
citation where it appears in code. That citation is **illustrative of the failure
class, not the literal originating incident** — worth stating plainly here so a
reader chasing `#1303` does not conclude the rationale is unfounded when the issue
they find has nothing to do with `FetchLinkedPR`.

`#1303`'s confirmed root cause was unrelated: `CacheImpl.FetchCheckRuns` used a
denylist cache-trust check (refetch only when the cached classification would read
as `FAILED`), so a cached `PENDING` classification was served forever on a
webhook-less deployment, letting a transient state silently latch permanently.

`linkedPRCacheTTL` and `linkedPRFetchedAt` were introduced in the same fix commit —
`c64ba7b0`, "Apply user feedback: fix confirmed root cause and bound
FetchLinkedPR" (2026-07-31) — but not as that incident's root-cause fix. The
commit message says so directly:

> boardcache: also bound `CacheImpl.FetchLinkedPR` with a TTL, so a cached
> HeadSHA/MergeableState/State/Merged record can't go stale forever either — **an
> earlier, retracted hypothesis in the same thread** but a real gap in its own right
> (every sibling mergeability call already bypasses caching for this exact reason).

So: the stale-`HeadSHA` concern was raised as a hypothesis for `#1303`, retracted
once the actual `FetchCheckRuns` mechanism was confirmed, and kept anyway as
independent hardening because it was a real gap on its own merits — every sibling
mergeability call (`FetchPRMergeableFields`, `FetchPRMergeable`,
`FetchPRMergeableState`, `FetchCombinedStatus`) already bypasses caching entirely
for the same reason. This is stronger provenance than a vague incident reference: the
gap was argued about, retracted for the incident it was proposed under, and retained
on independent merit. The originating incident for the `HeadSHA`/`FetchLinkedPR` gap
itself — as opposed to the `#1303` thread that surfaced and then set aside the
hypothesis — is not separately recorded; `c64ba7b0` is the concrete, verifiable
citation this ADR relies on instead of asserting one.

## Consequences

**Positive:**
- The intentional asymmetry between `linkedPRFetchedAt`'s single writer and the five
  webhook-delta sites that mutate PR head state is now documented at every site a
  reader is likely to arrive at (`boardcache.go`'s field comment, TTL comment, and
  single-writer site; `delta.go`'s `applyPullRequestDelta` SHA-tracking path and the
  three sibling `PRHeadSHAUpdated` sites), plus here.
- `#1378`'s Direction 1/2 are pre-empted with a citable rationale rather than left to
  be independently re-derived (or missed) by whichever agent picks up that issue.
- The `#1303` citation's oddity (an unrelated `awaiting-ci` incident, no mention of
  `FetchLinkedPR`) is explained rather than left to confuse a future reader who
  chases it.

**Negative / Trade-offs:**
- No code behavior changes; the cost/benefit numbers above (order-1% GraphQL budget)
  were already being paid before this ADR and continue to be paid after it. This ADR
  makes that cost legible, it does not change it.
- Extending this same "live-confirmation-only" reasoning to the other cached fields
  named in the issue (`FetchPRMergeableFields`, `FetchCheckRuns`'s own trust check,
  `blockedBy`) is explicitly out of scope here — those follow-ups, if warranted, get
  their own ADRs rather than being folded into this one by inference.

## References

- [ADR-036: Reactive Cache, Single State Owner](036-reactive-cache-single-owner.md) —
  the Phase 5 F2/F4 addenda whose precedent this decision explicitly declines to
  extend to `linkedPRFetchedAt`.
- [ADR-034: Boardcache Event-Sourced Delta](034-boardcache-event-sourced-delta.md) —
  Trade-offs section: "Cache coherence depends on the webhook stream. Missed events
  produce stale reads until the next reconciliation (up to 60 min)."
- [ADR-003: Polling Over Webhooks](003-polling-over-webhooks.md) — "Superseded by"
  section, line 45: "The core principle of this ADR — that polling is the reliable
  foundation — is preserved…"
- `docs/cache-refactor/05-webhook-correctness-audit.md` — prior audit of this class
  of freshness gap across other cached fields; source of the "the answer must
  always be yes" framing quoted above.
- #1378 — proposes the two directions this ADR argues against (Direction 1: stamp
  freshness from every refresh path; Direction 2: gate trust on webhook health).
  Its other directions are unaffected.
- #1303 — the incident whose thread raised, then retracted, the hypothesis that
  became `linkedPRCacheTTL`; see "`#1303` provenance" above for why it reads oddly
  when chased directly, and commit `c64ba7b0` for the actual origin of the TTL.
