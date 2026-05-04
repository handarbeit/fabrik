# ADR 032: Webhook-Driven Event Delivery via gh webhook forward

**Status**: Accepted  
**Date**: 2026-04-26  
**Supersedes**: ADR 003 (Polling Over Webhooks)  
**Supplemented by**: ADR 035 (Four-Layer Status Reconciliation) — replaces the 60-minute full-board reconcile described in §Board-column changes with a lightweight 10-minute status-only sweep

## Context

ADR 003 chose polling because Fabrik runs locally and webhooks require a publicly accessible endpoint. The `gh webhook forward` command (introduced in gh v2.32.0, October 2023) eliminates that constraint: it acts as an SSE client to GitHub and forwards events as HTTP POST requests to a localhost URL. No public endpoint, no ngrok, no infrastructure changes — just the `gh` CLI the user already has.

Two problems motivate adding webhooks alongside polling:

1. **GraphQL budget pressure.** The existing poll loop fetches the entire board every 30s–5min, consuming a significant fraction of the 5,000-points/hour GraphQL budget during active periods. Users running Fabrik alongside other `gh` workloads on the same token hit rate-limit caps.

2. **Event latency.** Comments, label changes, CI completions, and review submissions are visible to Fabrik only at the next poll tick — up to 30s of delay. Comment-driven steering feels sluggish; CI gate clearance is bottlenecked on poll cadence.

## Decision

Add an optional webhook-driven event ingestion path (`--webhooks` flag, disabled by default). When enabled:

- Spawn `gh webhook forward` as a managed subprocess, forwarding events to a Fabrik-internal HTTP listener on `127.0.0.1:<port>` (OS-assigned by default).
- Verify every incoming payload with HMAC-SHA256 before acting.
- Translate all events to `wakeCh` signals — the existing poll loop handles all subsequent work.
- Extend the idle-backoff cap from 5 minutes to 60 minutes when the webhook stream is healthy or starting up (the stream covers the events that would otherwise require frequent polling).
- Retain the existing poll loop as a safety net for missed/delayed webhook events.

## Transport: gh webhook forward

`gh webhook forward` was chosen over alternatives for these reasons:

- **No public endpoint needed.** The tool handles GitHub's SSE connection outbound; Fabrik only needs to listen on localhost.
- **No new accounts or third-party services.** Authentication piggybacks on the user's existing `gh auth login`.
- **Standard GitHub CLI.** Users already install `gh` for the existing `gh api` / `gh pr` usage in Fabrik stages. Floor version `gh ≥ 2.32.0` (October 2023) covers the vast majority of active installations.
- **Graceful fallback.** If `gh` is missing, too old, or fails to subscribe, Fabrik logs a warning and continues in polling-only mode. Webhook unavailability is always non-fatal.

Alternatives considered:
- **Self-hosted reverse tunnel (Cloudflare Tunnel, ngrok, SSH):** Functionally equivalent but adds infrastructure the user must provision and maintain. Deferred as a power-user option.
- **GitHub Apps webhook endpoint:** Requires GitHub App registration, complicates auth, and still needs a public URL or tunnel. Out of scope for a local CLI tool.

## Polling as Safety Net

Webhooks are not a replacement for polling — they are augmentation:

- **At-least-once delivery.** GitHub's webhook semantics are at-least-once. Events can be delayed, duplicated, or dropped (especially during GitHub incidents). The poll loop catches anything the webhook stream misses.
- **Board-column changes.** The `projects_v2_item` event class may not be supported by all `gh webhook forward` versions or GitHub configurations (see below). Polling at the extended 60-minute cap covers these changes with acceptable latency.
- **Startup.** On fresh start, the first board state is always fetched by polling; webhooks only deliver delta events.
- **Dedup.** Fabrik's existing idempotency mechanisms (rocket reactions on processed comments, label mutations, `processedSet`, `inFlight` map) already handle at-least-once delivery. A webhook wake followed immediately by a scheduled poll both converge to the same board state.

## Three-State Health Model

A two-state model (healthy/unhealthy) creates a false-unhealthy condition at startup before the first event arrives. Three states eliminate this:

- **`WebhookStreamStartingUp`**: the first 30 seconds (`WebhookStartupGrace`) after subprocess launch. Treated as healthy for idle-cap purposes (extended 60-min cap applies). TUI shows a blue/spinner indicator.
- **`WebhookStreamHealthy`**: at least one event received in the last 10 minutes (`WebhookHealthWindow`). TUI shows a green dot.
- **`WebhookStreamUnhealthy`**: grace expired with no first event, or health window elapsed. Falls back to the 5-minute idle cap. TUI shows a yellow dot.

Grace re-applies on every subprocess restart. This avoids permanently masking an unhealthy state from a crash-looping subprocess — after the grace period expires with no event, the engine transitions to unhealthy and the 5-minute cap resumes.

## Secret Rotation

`gh webhook forward` registers a webhook secret with GitHub at startup. If the secret becomes stale (e.g., `gh auth refresh` invalidated the token), HMAC verification will fail for every subsequent event. To handle this:

1. Track consecutive HMAC failures within a 2-minute window.
2. On 5 consecutive failures: log a warning, kill the subprocess, generate a fresh 32-byte random secret, restart.
3. If failures persist after 2 restart cycles: disable webhook mode for the session and fall back to polling. Log a clear error suggesting `gh auth status`.

This is a defense-in-depth mechanism; the primary cause of HMAC failures in practice is GitHub replaying events with the old secret after a token change.

## `projects_v2_item` Event Support

The spec requires attempting to subscribe to `projects_v2_item` for board-column changes. Empirical verification during Research was deferred to runtime detection:

- At subprocess start, Fabrik includes `projects_v2_item` in the `--events` list.
- If `gh webhook forward` emits an "invalid" or "unknown" error for this event on stderr, Fabrik removes it from the subscription, logs a clear warning, and restarts without it.
- When `projects_v2_item` is unavailable, board-column changes are caught by the safety-net poll within `WebhookIdleCap` (60 min). Correctness is preserved; only latency is affected.

This behavior is documented in USER_GUIDE.md.

## Multi-Repo Subscription

A single `gh webhook forward` subprocess covers all repos:

- When org-level webhook access is available (`gh webhook forward --org=<org>`), Fabrik uses it. One subscription covers all current and future repos in the org, with no restarts on new-repo discovery.
- When org-level access fails (detected by the subprocess exiting within 10 seconds of a startup), Fabrik falls back to per-repo `--repo` arguments.
- When a new repo appears on the board during a poll, the subprocess is killed and restarted with the updated repo list. The restart gap (seconds) is covered by the safety-net poll.

Per-repo subprocesses for finer-grained failure isolation were explicitly deferred. The single-subprocess restart approach is simpler; per-repo subprocesses are a follow-up if real users encounter problems.

## Security

The local HTTP listener binds to `127.0.0.1` only — never accessible off-box. HMAC-SHA256 verification is required on every payload before any action is taken. Both layers are necessary:

- `127.0.0.1` binding prevents off-box attackers.
- HMAC prevents malicious local processes from spoofing events.

The webhook secret is generated from `crypto/rand` (32 bytes, hex-encoded) at startup and regenerated on rotation. It is never logged or persisted to disk.

## Consequences

- **Steady-state GraphQL reduction:** Active boards that would otherwise poll every 30s now poll at most every 60 minutes when the webhook stream is healthy. For a board with events every few minutes, GraphQL load drops by ~100x.
- **Event latency:** Comments, label changes, and CI completions become visible within seconds of GitHub recording them, down from up to 30s.
- **Operational prerequisite:** Users need `gh ≥ 2.32.0` with `admin:repo_hook` scope (or org admin) to use webhook mode. Failure to meet prerequisites falls back gracefully to polling.
- **Non-fatal failures:** All webhook plumbing failures are non-fatal. The engine never blocks on webhook health.
- **Subprocess supervision:** A new long-running subprocess (`gh webhook forward`) is added to the engine's lifecycle. It follows the `runClaude` process-group patterns but runs asynchronously for the engine's lifetime.
- **ADR 003 is superseded** but its core insight (polling as the reliable foundation) remains: polling is now the safety net rather than the only mechanism.

---

## Addendum: Self-Feedback Loop Analysis and Partial Fix (Issue #490, 2026-05-03)

### Problem

The original design translated **all** verified webhook events to `wakeCh` signals with no filtering. Fabrik's own API actions — label mutations, comment posts, status field updates, PR opens — each generate one or more webhook events on the watched repos. Every arriving event triggered an immediate full board fetch, and the wake handler unconditionally reset idle backoff to 1× before polling. The net effect: Fabrik's own stage advances produced a self-reinforcing sequence of webhook wakes that defeated the idle-backoff savings webhooks were added to provide.

A single stage advance typically performs 3–6 API mutations, each generating a distinct webhook event (`issues.labeled`, `issues.unlabeled`, `projects_v2_item.edited`, `issue_comment.created`, etc.). The single-slot `wakeCh` buffer collapsed a burst of N events to at most 1–2 extra polls, but each self-event still wiped the backoff multiplier and triggered an unnecessary board fetch.

### Sender-Filter Approach — Considered and Rejected

An initial approach added a sender-login filter: parse `sender.login` from the webhook payload and suppress the `wakeCh` signal when the sender matches `cfg.User`. This was implemented and then reverted for a fundamental reason:

**Fabrik runs authenticated as the user's own GitHub account.** `cfg.User` is the human operator's login (e.g., `verveguy`), not a dedicated bot account. Filtering by `cfg.User` would suppress not only Fabrik's own API actions but also every event the human user generates — label changes, comments, PR reviews — silencing the webhook stream for the most important input class.

The sender-filter design is only viable when Fabrik runs as a dedicated bot account distinct from any human user. That is a future change; this PR does not implement it.

### Applied Fix: Backoff Preservation

The one correctness fix that does not require a bot account: the `case <-e.wakeCh:` branch in `poll.go`'s `Run()` loop unconditionally reset `e.idleStart` and `prevMultiplier` to 1× before calling `doPollCycle`. These two lines were removed. Backoff is now only reset inside `doPollCycle` when `result.Active == true` — the location that was already correct.

This means webhook wakes no longer unconditionally destroy the idle-backoff state. A wake during a quiet period still triggers an immediate poll (correct), but if the poll finds nothing active, the backoff multiplier is preserved rather than reset to 1×. The full backoff-reset-on-activity behavior is unchanged.

### Burst Coalescence (Existing Behavior, Confirmed)

The single-slot `wakeCh` channel (cap 1) naturally coalesces bursts: N simultaneous events queue at most 1 wake. This property was confirmed by a new test (`TestHandleWebhookBurstCoalescence`) and is unchanged by this fix.

### Remaining Gap

Fabrik-generated events still trigger webhook wakes (one per burst of actions). Each wake produces one unnecessary poll cycle, partially defeating the GraphQL budget savings that webhooks provide. The full fix requires either:
- Running Fabrik as a dedicated bot account (so sender-filter is safe), or
- A different event-attribution mechanism (e.g., X-request-ID correlation, or suppressing wakes during an active stage invocation window).

This is tracked as a known gap, not a regression. The burst-coalescence guarantee bounds the damage: a stage advance producing N API actions generates at most 1 extra poll, not N.

### Scope

- `engine/poll.go`: two pre-poll backoff reset lines removed from `case <-e.wakeCh:`.
- `engine/webhook_test.go`: `TestHandleWebhookBurstCoalescence` confirms burst coalescence is preserved.
