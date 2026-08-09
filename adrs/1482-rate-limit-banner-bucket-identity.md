# ADR 1482: Rate-Limit Alert Banner Bucket Identity

**Date**: 2026-08-09
**Status**: Accepted
**Issue**: #1482 — the rate-limit banner hardcoded "GraphQL" regardless of which bucket was
actually exhausted, and clearing was bucket-blind

## Context

GitHub tracks two independent API budgets — REST/core and GraphQL — and Fabrik gates on both
(`engine/backoff.go`), but `RateLimitAlertEvent` (`tui/events.go`) carried no identity of which
one an emit or clear event concerned. Four call sites fed the single event type: two REST
(`engine/poll.go` exhaust + clear) and two GraphQL (`engine/terminal.go` exhaust,
`engine/poll.go` clear). `AlertBannerComponent` (`tui/alert.go`) routed every one of them into a
single `bannerActive bool` + `graphqlStats RateLimitStats` pair and hardcoded the string
`"⚠ GraphQL rate limit exhausted..."` in `View()`.

Observed live: a REST exhaustion (`core: 0/5000`) fired the banner, which then quoted GraphQL's
reset time (`21:38`, actually REST's reset) and displayed the wrong resource name, while the
footer's live GraphQL readout (`2432/5000`, comfortably healthy) directly contradicted it. Two
further defects followed from the same single-field model:

- **Bucket-blind clearing**: any `Exhausted: false` cleared `bannerActive` regardless of which
  bucket sent it, so a GraphQL recovery could dismiss a banner a REST exhaustion had raised (and
  vice versa) while the real condition still held.
- **Bucket-blind visibility**: `isVisible()` gated on `graphqlStats`'s ratio
  (`Remaining/Limit <= 0.50` or `== 0`) regardless of which bucket set `bannerActive`. A
  REST-only exhaustion with GraphQL comfortably healthy (e.g. 90%) would set `bannerActive = true`
  via the REST emit site and then render **nothing**, because the ratio check short-circuited on
  GraphQL's healthy number.

## Decision

### `RateLimitBucket` typed enum on the event

`RateLimitAlertEvent` gains a `Bucket RateLimitBucket` field. `RateLimitBucket` is a typed
`int` enum (`RateLimitBucketGraphQL` = 0, `RateLimitBucketREST` = 1) with a `String()` method
used directly in rendered banner text, rather than a bare string field. A typed enum gives
compile-time exhaustiveness in the `Update`/`View` switches and a single naming authority,
instead of string literals that could typo at any of the four call sites. `RateLimitBucketGraphQL`
is deliberately the zero value, so any code path that forgets to set `Bucket` fails toward the
historical (pre-#1482) implicit meaning rather than silently becoming a new, unreviewed default —
though all four in-scope call sites (`engine/poll.go` ×3, `engine/terminal.go` ×1) now set it
explicitly.

### Drop the ratio-based visibility recheck; track REST and GraphQL as independent active/reset pairs

Rather than build a symmetric ratio-based visibility model for both buckets — which would require
plumbing live REST `Remaining`/`Limit` into `PollCompletedEvent` (REST and GraphQL exhaustion are
not symmetric conditions: REST is a hard ≤1% pause with no ratio-recheck concept at all, while
GraphQL's ratio check was redundant with its own explicit clear event, both already firing at the
same 50% threshold per ADR-028) — `AlertBannerComponent` is rebuilt around the simpler model
`ClaudeUsageLimitBannerComponent` (ADR-1120) already uses successfully for a structurally
identical banner: a `rateLimitAlertState{active bool; reset time.Time}` per bucket, with
`isVisible(now)` computed **only** from that bucket's own `active`/`reset` — no ratio, no
cross-bucket read. `PollCompletedEvent` is untouched (`AlertBannerComponent.Update` no longer has
a case for it at all); the banner's `active` flag is now set and cleared exclusively by
`RateLimitAlertEvent`, self-clearing on `TickEvent` past `reset` as a harmless backstop if an
event is ever missed, exactly like the usage-limit banner.

This trivially satisfies R3 (visibility depends only on the exhausted bucket's own state) and
closes the third defect: a REST-only exhaustion is visible regardless of what GraphQL's ratio is,
because GraphQL's ratio is no longer consulted by the REST path at all — the two are structurally
independent fields, `rest` and `graphql`, each with its own `isVisible`.

### Bucket-scoped clearing

`Update` routes each `RateLimitAlertEvent` by `ev.Bucket` into the matching `rest`/`graphql`
state; an `Exhausted: false` for one bucket only ever mutates that bucket's own state. This closes
R2's latent bug directly — a GraphQL clear cannot touch `rest`, and a REST clear cannot touch
`graphql`.

### Combined message on simultaneous dual-exhaustion, quoting the sooner reset

R2 permits "or names both." When both buckets are exhausted at once, `View()` renders a single
combined line (`"⚠ REST and GraphQL rate limits exhausted — polling suspended."`) rather than
stacking two lines or picking one arbitrarily — this preserves the `Height() == 1` invariant that
`updateLayout`'s budget math and several existing tests depend on. The quoted countdown uses
`soonerReset`, a small helper that treats a zero (unknown) reset as "no opinion" so it can never
win the comparison against a bucket with a known reset — this is the operationally useful choice:
it tells the operator the earliest moment anything could change.

### Footer stays as-is

Per the issue's Scope, `FooterComponent` is not extended with a REST readout. R4 ("banner and
footer must not disagree") is satisfied structurally rather than by adding new plumbing: the
footer only ever reads `PollCompletedEvent.GraphQLStats`, and the banner's `graphql` state is now
fully independent of `rest` — a REST-exhaustion banner can no longer make any footer number look
contradictory, because the footer was never showing REST numbers to begin with, and a
GraphQL-exhaustion banner now correctly names the same bucket the footer already reports on.

## Consequences

- `RateLimitAlertEvent{Exhausted: true, Reset: restStats.Reset}` (the old REST emit shape) is no
  longer valid without an explicit `Bucket` — all four call sites
  (`engine/poll.go:505,514,554`, `engine/terminal.go:35`) now set it. A future REST/GraphQL call
  site that omits `Bucket` silently defaults to `RateLimitBucketGraphQL` rather than failing to
  compile — reviewers should treat a missing `Bucket` on a new call site as a code-review finding,
  not assume the zero value is intentional.
- `AlertBannerComponent.graphqlStats`/`bannerActive` no longer exist; anything reading them
  (tests, or a future component) must be updated to `rest`/`graphql` `rateLimitAlertState` fields.
- The GraphQL banner's visibility ratio recheck (redundant with the explicit clear event per
  ADR-028) is gone. This is intentional — see "Drop the ratio-based visibility recheck" above —
  and not a regression: GraphQL's actual exhaustion/clear semantics (the 20%/50% hysteresis) are
  unchanged; only the banner's *second, always-agreeing* check on top of them was removed.
- A REST-only exhaustion now reliably renders (acceptance #6), closing a real, previously-silent
  gap in operator visibility.

## Related

- ADR-028 — GraphQL rate-limit backoff hysteresis (20%/50%); the source of the "clear at 50%"
  event this ADR's `graphql` state now tracks directly, with no independent recheck.
- ADR-1120 — `ClaudeUsageLimitBannerComponent`, the reference model this ADR's
  `rateLimitAlertState` is deliberately modeled on.
- #1482 — the incident and requirements this ADR implements.
