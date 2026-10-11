# ADR 2125: Channel label-event actor attribution

## Status

Accepted. Supersedes, in part, ADR-1966-c's accepted limitation that an overseer action's label write appears as actor `fabrik`, and narrows ADR-1966-b's "the actor is `unknown`" known limit.

## Context

Channel `label-applied` / `label-removed` events (ADR-1966-b) carry an `actor`: `fabrik`, `human`, `bot` or `unknown`. Two cases made it misleading once several overseers and operators share a board:

1. An overseer action (`fabrik_set_autonomy` and the others, ADR-1966-c) writes through the engine's own label idioms, so its write-through is a `LocalLabelAdded`/`LocalLabelRemoved` and reads `actor=fabrik`. A second overseer saw "by fabrik" for a change a peer requested.
2. With webhooks unavailable, a label change first seen by a poll or reconcile has no sender and reads `unknown`, even when a human applied it.

The channel and local-API layers must stay free of GitHub calls (R9) and observation-only (R10), so any attribution must reach them as data already on the state change.

## Decision

### 1. The requester travels on the mutation

`itemstate.LocalLabelAdded` / `LocalLabelRemoved` gain `Requester`; `annotateLabelChange` copies it onto `Change.Requester`. `CacheImpl.ApplyLabelAddedBy` / `ApplyLabelRemovedBy` (the existing methods delegate with `""`) and the engine's `addLabelCheckedBy` / `removeLabelCheckedBy` / `syncLabelAddBy` / `syncLabelRemovalBy` carry it. Only the label writes in `engine/overseer_actions.go` (`set_autonomy`, `revalidate`, `clear_claude_limit`) pass a requester; every autonomous call site is unchanged and stays `fabrik`. `promote` writes a board column and the audit is a comment; neither produces a channel event, so neither needs attribution.

A keyed side registry consulted by the channel layer was rejected: it has a misattribution window against a concurrent engine write of the same label, and a stale entry can linger when the cache no-ops the write. With the requester on the mutation, a failed write applies no mutation and leaves nothing pending, a concurrent engine write is a different mutation, and the engine's later removal of `fabrik:revalidate` / `fabrik:clear-claude-limit` is its own write and reads `fabrik`.

### 2. The webhook echo carries the requester for the late case

When the store did not yet hold the item at write-through, the webhook that follows is the first observer-visible change. The echo registry therefore records the requester: `webhookManager.RegisterEchoVia` stores it in a parallel `echoVia` map (always a subset of `pendingEchoes`; every delete of a `pendingEchoes` entry deletes its `echoVia` entry), `MatchEchoVia` returns `(matched, requester)` and consumes both, and `CacheImpl.SetMatchEchoViaFn` makes the webhook delta set `IssueLabeled.Requester` **only when the echo matched**. An unmatched webhook can never claim a requester. A webhook delivered before `RegisterEcho` keeps today's attribution (echo missed): never a wrong specific claim.

### 3. Vocabulary

`labelAttribute` (replacing `labelActor`) resolves, in order: a requester on an engine write or matched echo → `overseer` (with `via`), taking precedence over the App-sender-equals-self check; the engine's own write or an echo → `fabrik`; a webhook with no sender, and anything seen first by a reconcile, deep fetch or probe → `unknown`; under App auth a sender equal to the App login → `fabrik`; a `[bot]` sender → `bot`; any other sender → `human`.

Meta keys (identifier keys, string values): `actor` (now also `overseer`), `via` (the sanitised subscriber, the same value as the audit comment, overseer only) and `actor_login` (the webhook sender's login, `human`/`bot` only). The content text reads `(by overseer x)` / `(by human alice)`. There is no new event type and no actor filter.

### 4. R2 audit: which `unknown` actors can be resolved

The rule is: resolve only where the engine already reads the data, with no new call, and never from the channel layer.

| Path | Cost | Result |
|---|---|---|
| Webhook `sender` | none, already on the delta | Resolved (`human` / `bot`); now also carries `actor_login`. |
| Events API behind `FetchLabelAppliedAt` / `FetchLabelRemovedAt` (`fetchLatestLabelEvent`) | a paginated read the engine already makes | The decoder discards `actor{login,type}`. The callers read `awaiting-ci`, `bot-reprompted`, `awaiting-review`, `auto-merge-enabled`, the pause label, `stage:*:complete` and legacy locks: almost all engine-applied, none of them `fabrik:cruise` or `fabrik:yolo`. The read also happens after the event was built and pushed, and already-published events are not amended. Changing the return shape touches about 16 files. **Not adopted; `unknown` stays.** |
| Board / deep-fetch GraphQL | adding `timelineItems` is a new, wider hot-path read and a wire-contract change | **Not adopted.** |
| Reconcile / probe (`OriginOther` / `OriginProbe`) | no actor is on hand | **`unknown` stays.** |

So no GitHub read is added, and the only R2 gain is that a webhook-attributed `human` / `bot` event now names the login. A human operator's change seen first by polling, with webhooks off, remains `unknown`. The tests pin that outcome, including that attribution causes no label-event read.

### 5. Observation-only is preserved

Attribution fields are plain data on `Change`; the hooks stay void; `channel_*.go` gains no forbidden identifier. `TestChannelEventsSourceDoesNotReachGitHub`, `TestLocalAPISourceDoesNotReachGitHub` and `TestChannelEventsAreObservationOnly` pass unchanged.

## Consequences

- An overseer sees a peer's action as `actor=overseer via=<subscriber>`; the engine's autonomous writes keep `actor=fabrik`. This reverses ADR-1966-c's accepted limitation.
- The requester remains a self-asserted, advisory name on an unauthenticated socket; surfacing it as `via` makes a spoofed name more visible but grants nothing.
- Under PAT auth the operator's own login is still indistinguishable from Fabrik's (`human`); an overseer action is attributed by requester, not login.
- Events already pushed as `unknown` are not retracted.
- Known residual: a webhook that beats `RegisterEcho` is attributed by the webhook (`human`/`bot`), as before.
