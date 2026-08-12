# ADR 1254: Event-Driven Webhook Ingestion via Hookdeck, with Pruefer as the Fabrik Test Bed

**Date**: 2026-07-30
**Status**: Accepted
**Issue**: #1254 — webhook event ingestion via Hookdeck (`EventSource`; test bed for fabrik)

## Context

Pruefer is poll-only (ADR-1113 §7): `pruefer/daemon.go` polls every `poll_interval_seconds` (default 120s) across every watched repo, listing open PRs and running SHA-based eligibility on each. This has two costs that grow with the watched-repo count: REST call volume scales with repo count regardless of activity, and review latency is bounded below by the poll interval — a PR can sit up to two minutes before Pruefer even notices it needs review.

Pruefer is also, deliberately, the lowest-risk place to prove out an event-driven alternative before touching Fabrik's own webhook infrastructure. It is small, each review is a single independent unit of work (no multi-stage state machine), SHA-idempotent by construction (ADR-1113 §2 — review eligibility is always re-derived from GitHub, never trusted from local state), and not yet publicly relied upon. Fabrik's own webhook path (`engine/engine.go`'s `webhookMgr`, ADR-032/034/044) is higher-stakes and has a known limitation (#1142: `gh webhook forward --repo` isn't repeatable, so it can't cover more than one repo at a time) that this issue does not attempt to fix — it only de-risks the pattern that a later, separate issue will use to fix it.

Two design questions had to be settled before implementation: how to structure the abstraction so a transport-specific concept (Hookdeck) never leaks into Pruefer's review/domain code, and whether to embed Hookdeck's own open-source Go CLI transport or reimplement the minimal protocol subset needed.

## Decisions

### 1. A narrow `EventSource`/`EventSink` boundary, with `GitHubEvent` as the only thing that crosses it

`pruefer/events/` defines exactly three things: `EventSource.Run(ctx, sink) error`, `EventSink.Handle(ctx, event)`, and the normalized `GitHubEvent` struct (`DeliveryID`, `EventType`, `Owner`, `Repo`, `ResourceID`, `Action`, `Installation`, `ReceivedAt`, `Payload`). Nothing about *how* an event arrived — session protocol, connection health, retry state — crosses this boundary. `pruefer/events/hookdeck/` is the sole implementation of `EventSource`; it owns Hookdeck auth, session lifecycle, reconnect/backoff, and per-webhook signature verification/dedupe/normalization entirely internally.

The sink side (`pruefer/eventsink.go`, `daemonEventSink`) resolves `event.Owner`/`Repo`/`ResourceID` to a `GitHubLister` client, fetches **authoritative** current PR state via `FetchPRDetails` — never trusting the webhook payload as PR state — and dispatches into the exact same `reviewOne` path `poll()` already uses. `ReviewPR` and `pruefer/select.go` are untouched by this issue; they receive a `gh.PRDetails` exactly as before, regardless of whether a poll cycle or an event triggered the fetch. This is what makes "no Hookdeck concept in domain code" a structural guarantee rather than a discipline every future contributor has to maintain by hand.

GitHub HMAC signature verification (`events.VerifySignature`) lives in `pruefer/events/`, not `pruefer/events/hookdeck/`, despite currently having exactly one caller — it's transport-agnostic per-webhook mechanics, reusable by any future GitHub-webhook-forwarding `EventSource` (a hypothetical `gh webhook forward`-based one, for instance), and scoping it to the one adapter that happens to exist first would be an arbitrary boundary.

### 2. Reimplement Hookdeck's CLI-session protocol directly; do not embed `hookdeck-cli`'s Go transport

Hookdeck's CLI (`github.com/hookdeck/hookdeck-cli`) is open source Go, and its `pkg/listen/proxy` package is publicly importable, tagged, and Apache-2.0 licensed — embedding it was the issue's stated preference if viable. Reading its current source directly (not just tagged-release docs) surfaced two disqualifying problems:

- `Proxy.Run` calls `signal.Notify(interruptCh, os.Interrupt, syscall.SIGTERM)` internally. `pruefer/execute.go` already owns process shutdown via `signal.NotifyContext`; embedding `Proxy` would install a second, competing SIGTERM handler with unpredictable interaction on shutdown.
- It pulls in `logrus` plus its own hand-rolled `pkg/hookdeck` REST client and `pkg/websocket` wrapper, adding dependency weight `.claude/rules/golang.md`'s "minimize external dependencies" convention asks Pruefer to avoid — for a package whose public shape had already changed once (a `v2.0.0` release, ~4 months before this issue, removed a `hookdeck-go-sdk` dependency from its internals and changed the `Proxy` constructor's signature in the process).

Instead, `pruefer/events/hookdeck/` speaks the actual wire protocol directly, traced from `hookdeck-cli`'s current source: `POST /2025-07-01/cli-sessions` (HTTP Basic auth, API key as username) creates a session; a single WebSocket connection to `wss://ws.hookdeck.com` (dialed with a `Websocket-Id` header carrying that session ID) then carries JSON `attempt` frames (one per forwarded webhook, containing the raw body and original GitHub headers in `request.data_string`/`request.headers`) and outgoing `attempt_response` frames as acks. This is small enough to own directly (`protocol.go`, `client.go`, `source.go`) and needs exactly one new dependency, `gorilla/websocket`, for correct RFC 6455 framing — versus the `logrus` + hand-rolled-client weight the embed path would have brought in, for a comparable amount of actually-load-bearing code.

A further simplification falls out of this: `hookdeck-cli`'s `proxy.go` converts each `attempt` into a local HTTP POST because it's forwarding to an arbitrary local server (the CLI's general-purpose use case). Pruefer has no such server — `Source` reacts to the `attempt` frame's contents directly (verify → dedupe → normalize → dispatch → ack, all in-process), binding no local port at all.

This is a **reimplementation of an undocumented internal protocol**, not a versioned public API — a real, accepted risk (see Consequences), mitigated by keeping the wire-shape structs isolated in `protocol.go` behind the `EventSource` boundary, so a future break is a contained fix to one adapter, not a domain-code change.

### 3. Webhooks are triggers; poll remains the source-of-truth fallback, never removed

`pull_request` events with action `opened`, `reopened`, `synchronize`, or `ready_for_review` trigger `Daemon.ReviewFromEvent`, which — critically — re-fetches the PR from GitHub rather than trusting the webhook payload, then dispatches through `reviewOne`. An installation/repo-selection-change event triggers a full `poll()` reconciliation pass rather than dynamic re-bootstrap: `Daemon.Clients` is owner-keyed and fixed at startup (#1233), and re-deriving "what does GitHub say is true right now" via a poll sweep satisfies the requirement without inventing new auth machinery mid-run.

`event_source: hookdeck` does not disable polling — it demotes it (`Daemon.runEventDriven`, as opposed to the unchanged `runPollOnly`): an optional pass at startup (`reconciliation.startup`, default `true`), then one every `reconciliation.fallback_interval` (default `2m`, deliberately a **separate** config field from `poll_interval_seconds` — poll-only mode is untouched by this issue and keeps using its own interval), for the entire lifetime of the run. A reconnect (a transition into `HealthConnected` following any prior connection — tracked via `Daemon.HealthHandler`, wired as the source's `OnHealth` callback) also triggers an immediate reconciliation poll, since Hookdeck's own replay/history is not trusted alone to guarantee no event was missed while disconnected.

This is what makes at-least-once, possibly-duplicate, possibly-out-of-order delivery safe to build on without any new idempotency mechanism at the event layer: `ReviewPR`'s SHA-based eligibility (`alreadyReviewedAtHead`, ADR-1113 §2) was already the authoritative "has this been reviewed" check before this issue, and remains so — an event is only ever a *hint* to look now instead of at the next poll tick, never a replacement for that check. Duplicate deliveries at the same head SHA are absorbed with only one `SubmitPRReview` call regardless of whether the transport's own delivery-ID dedupe (`events.Dedupe`, a bounded in-memory cache) already caught them.

### 4. Delivery-ID dedupe is in-memory, bounded, and not user-configurable

`events.Dedupe` is a fixed-capacity (4096 IDs), insertion-order-eviction cache, cleared on restart. This is consistent with ADR-1113's zero-persistence philosophy — Pruefer has never had on-disk state, and this issue doesn't introduce the first piece of it. A full cache loss (restart, or eviction past capacity under sustained high delivery volume) only costs a redundant network round-trip through `ReviewPR`'s own SHA-idempotency check, never a duplicate review or any correctness gap; the issue itself frames the exact window size as a Plan-stage judgment call, not a fixed requirement, so it is not exposed as config.

### 5. A new config field beyond the issue's literal example: `hookdeck.webhook_secret_env`

The issue's Requirements mandate GitHub signature verification "on every received event," but its own config example only names `hookdeck.api_key_env`. There was no existing Pruefer config surface for a GitHub App webhook secret — the App's Webhook setting has been unchecked/inactive until this issue. `hookdeck.webhook_secret_env` (default env var `PRUEFER_GITHUB_WEBHOOK_SECRET`) is the minimal addition that makes the mandatory signature-verification requirement satisfiable at all, not scope creep beyond the issue.

### 6. Concurrency: one shared semaphore, not two independent budgets

Before this issue, `poll()` created a fresh per-cycle semaphore local to itself. An event-triggered review calling into that shape would have drawn from a *different* concurrency budget than concurrently-running poll dispatch, silently doubling Pruefer's effective `claude` concurrency in event-driven mode. `Daemon.sem` is now a lazily-built, `Daemon`-level field (`semaphore()`), and the per-PR dispatch body was extracted from `poll()`'s inline goroutine into `reviewOne`, called identically by both `poll()`'s fan-out and `ReviewFromEvent`. This makes "hand off to the existing concurrency-capped review dispatch" (the issue's literal requirement) true in the sense that actually matters — one budget, not two paths that happen to look alike.

## Consequences

**Positive:**
- Review latency in event-driven mode is bounded by Hookdeck's forwarding latency (typically sub-second) rather than up to a full poll interval, without giving up the safety of GitHub-derived, SHA-based eligibility.
- `event_source: poll` (the default) is byte-for-byte unchanged — `runPollOnly` is `Run`'s original body, untouched; every new config field and code path is inert unless explicitly opted into.
- The `EventSource`/`EventSink` boundary and its normalized `GitHubEvent` are the concrete artifact this issue set out to produce: a proven, tested pattern for the later, higher-stakes fabrik port (replacing `gh webhook forward`, resolving #1142's single-repo limitation) to build on, without that port needing to re-derive the abstraction from scratch.
- No Hookdeck-specific concept appears anywhere in `ReviewPR`/`select.go`/the rest of Pruefer's domain code — verified structurally by the fact that `daemonEventSink` only ever constructs a `gh.PRDetails` via the same `FetchPRDetails` call poll-mode already uses.

**Negative / Trade-offs:**
- `gorilla/websocket` is a new external dependency — small and justified relative to the embed alternative's `logrus` + hand-rolled-client weight, but still a real departure from "minimize external dependencies," worth naming explicitly rather than treating as free.
- The Hookdeck CLI-session wire protocol reimplemented here (`protocol.go`) is an internal implementation detail of `hookdeck-cli`, not a versioned public API. A future upstream change could break it; the blast radius is contained to `pruefer/events/hookdeck/` by the `EventSource` boundary, but it is not a zero-maintenance integration the way a stable public API would be.
- `daemon.go` gained real complexity: a shared semaphore, an extracted `reviewOne`, a `runPollOnly`/`runEventDriven` split, and a health-transition callback (`HealthHandler`) — all net-new surface area even though `runPollOnly`'s own behavior is unchanged.
- No live Hookdeck account is exercised in CI; all coverage is `httptest`/`gorilla/websocket`-mocked at the transport boundary (signature validation, delivery-ID dedupe, normalized-event mapping, reconnect-after-drop, session-creation-failure retry). A manual smoke test against a real Hookdeck source remains a pre-production step outside this repo's test suite.

## Amendment (2026-08-12, from #1563): what the reimplementation actually rests on

Decision 2 and the trade-off above record that the CLI-session protocol is
undocumented and could break upstream. Two specifics were left implicit and are
load-bearing enough to name.

**Signature verification depends on byte-exact body preservation.**
`processAttempt` computes GitHub's HMAC over `attempt.Request.DataString`:

```go
if !events.VerifySignature([]byte(attempt.Request.DataString), sig, s.cfg.WebhookSecret) {
```

This is correct only if Hookdeck forwards the request body byte-for-byte as
GitHub sent and signed it. Nothing enforces that; it is a property of a wire
format traced from `hookdeck-cli`'s source, not a contract. Any re-serialization
in transit — re-encoding, whitespace normalization, JSON key reordering — makes
**every** signature check fail. Of everything the reimplementation assumes, this
is the single most consequential detail, and unlike a struct-shape mismatch
(which fails loudly at unmarshal) it fails as a uniform, silent rejection.

**The resulting failure is unobservable.** Four individually defensible choices
compose into an invisible outage:

1. A failed verification drops the event.
2. The attempt is still acked `Status: 200`, so nothing queues Hookdeck-side.
3. The signature-failure warning is rate-limited to one line per
   `sigFailureLogInterval` (30s) and terminates in the log.
4. The reconciliation fallback ticker keeps polling, so reviews still happen.

Degrading to poll rather than dying is Decision 3 working as designed and is
right. Degrading *undetectably* is not: with all four in play, a total protocol
break is distinguishable from "a quiet hour on the board" only by reading a
rate-limited log line. #1563 tracks making that state visible.

**Claim strength.** `client.go`'s reference to "the verified upstream protocol"
overstates it. The protocol was traced from upstream source; it has not been
verified against a published spec, and — per the CI trade-off already recorded
above — not against a live Hookdeck session in any automated test. "Traced from
upstream source" is the accurate phrasing.

None of this changes Decision 2. Embedding `hookdeck-cli` was weighed and
rejected on dependency weight, and the containment argument (wire structs
isolated in `protocol.go` behind `EventSource`) still holds. The amendment
records what the accepted risk concretely consists of, so a future reader
diagnosing "Pruefer stopped reviewing promptly" finds the failure mode written
down rather than having to rediscover it.

## Related Work

- ADR-1113 (`adrs/1113-pruefer-v1-architecture.md`) §2 (GitHub-derived, not on-disk, review state) and §7 (poll interval defaults) — this issue is additive to both: §2's philosophy is exactly what makes at-least-once event delivery safe to build on, and §7's 120s default becomes the value `reconciliation.fallback_interval` also defaults to, kept as a genuinely separate field.
- ADR-032 (`adrs/032-webhook-event-delivery.md`) — Fabrik's own webhook design (`gh webhook forward`, HMAC-verified, poll-as-safety-net, three-state health model). The closest existing precedent in this repo and the direct model for this issue's shape; a sibling design for a different transport, not superseded by it.
- #1233 (`adrs/1233-pruefer-multi-installation-auth.md`) — the owner-keyed `Daemon.Clients` routing this issue's `GitHubEvent.Installation` field is designed to eventually hook into; today's install/repo-selection-change handling (a full poll reconciliation, Decision 3 above) deliberately does not attempt dynamic re-bootstrap of that routing.
- #1142 (open) — `gh webhook forward --repo`'s single-repo coverage limitation. Motivates this issue but is explicitly not resolved by it; resolving it is the later, separate fabrik-side port this issue de-risks.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
