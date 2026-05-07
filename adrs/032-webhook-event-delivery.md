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

---

## Amendment: Per-Repo Failure Isolation (Issue #631, 2026-05-07)

### Problem

The "Multi-Repo Subscription" section of this ADR explicitly deferred per-repo failure isolation: *"Per-repo subprocesses for finer-grained failure isolation were explicitly deferred. The single-subprocess restart approach is simpler; per-repo subprocesses are a follow-up if real users encounter problems."* This amendment records the design that fills that deferred gap.

When `gh webhook forward` fails to subscribe to any repo in its argument list (e.g., the token lacks `admin:repo_hook` scope on one repo), the entire subprocess exits. Without isolation, the supervise loop retries indefinitely with the same bad repo in the argument list — a thrash loop that keeps the webhook stream down for all repos.

### Design: All-or-Nothing Attribution with Threshold Quarantine

Rather than per-repo subprocesses (high complexity), the fix attributes failures conservatively across the entire subscription set:

1. **Attribution rule**: When the subprocess exits within `orgModeProbeTimeout` (10s) with auth-shaped stderr, increment failure counts for **all** repos in the current subscription set. This is conservative — it may penalize repos that were working fine alongside the bad repo. The N=3 threshold and "consecutive" requirement make false quarantine unlikely in practice (a genuine transient would not replicate auth-shaped exits 3× consecutively).

2. **Quarantine threshold**: `webhookRepoFailureThreshold = 3`. A repo that accumulates 3 consecutive attributed failures is marked unsubscribable for the session and removed from `wm.repos`. This mirrors the `webhookRotationFailures = 5` constant for HMAC failures, but is more aggressive because auth failures are less likely to self-resolve.

3. **Counter reset on healthy start**: If the subprocess exits at or after `orgModeProbeTimeout`, failure counts are reset to zero. This means a long-running-then-crashing subprocess does not accumulate toward quarantine — only consecutive quick auth-shaped exits count.

4. **Zero-repo guard**: If quarantine reduces `wm.repos` to empty in per-repo mode, the supervise loop skips subprocess launch, logs a warning, and sleeps for `webhookMaxRestartBackoff` before retrying. This prevents infinite spinning when the entire subscription set is quarantined. Safety-net polling continues unaffected.

5. **In-memory only**: The quarantine set (`unsubscribableRepos`) lives only in the `webhookManager` instance. A fabrik restart clears it, allowing the operator to retry after fixing the permission issue.

6. **`UpdateRepos` interaction**: When `UpdateRepos` is called with a repo set that includes a quarantined repo, the quarantine persists — the repo is silently filtered out and a log note is emitted. The quarantined repo is not treated as "new" and does not trigger a subprocess restart.

### Auxiliary Fix: `projects_v2_item` Stderr Detector

The phrase list in the `projects_v2_item` rejection detector was extended to include `"not allowed"`, covering the actual GitHub error message: `"These events are not allowed for this hook: projects_v2_item"`. The phrase detection logic was also extracted into a standalone `isProjectsV2ItemRejection(line string) bool` helper to make it independently testable.

### Implementation Files

- `engine/webhook.go`: `webhookRepoFailureThreshold` constant; `repoFailureCounts` and `unsubscribableRepos` fields on `webhookManager`; `isProjectsV2ItemRejection` helper; `applyRepoAuthFailure` method; zero-repo guard and attribution call in `supervise`; quarantine filtering in `UpdateRepos`.
- `engine/webhook_test.go`: `TestIsProjectsV2ItemRejection`, `TestApplyRepoAuthFailure_*`, `TestUpdateRepos_Quarantine*`.

---

## Amendment: Orphan Hook Cleanup at Subprocess Launch (Issue #643, 2026-05-07)

### Problem

When `gh webhook forward` exits unexpectedly (WebSocket abnormal closure, 500 from GitHub's WS server, network blip), the hook it registered at GitHub remains active. The next restart attempt creates a new hook registration, which GitHub rejects with HTTP 422 ("Hook already exists") because only one forwarding hook per repo is allowed at a time. Three consecutive 422s trigger the existing circuit-breaker (see below), switching Fabrik to poll-only mode and requiring a manual restart.

Observed sequence from production (smoke #7, 2026-05-07):

```
[webhook] [gh] Error: error receiving json event: websocket: close 1006 (abnormal closure)
[webhook] gh webhook forward exited: exit status 1 — restarting in 1s
[webhook] [gh] Error: error creating webhook: HTTP 422: Validation Failed   ← orphan hook collision
[webhook] WARNING: webhook subscription permanently failed after 3 consecutive HTTP 422 errors
```

### Design: Cleanup Before Each Subprocess Launch

Before launching (or relaunching) `gh webhook forward`, Fabrik deletes any orphaned forwarding hooks it previously created at GitHub. The cleanup is a REST LIST + DELETE sequence inserted at the top of each `supervise()` loop iteration, after the lock-protected repos snapshot and the zero-repo guard, but before `buildGhArgs` / `startFn`.

**Discriminator**: `gh webhook forward` registers hooks with `config.url = "https://webhook-forwarder.github.com/hook"` (`webhookForwarderURL` constant). Only hooks matching this URL are eligible for deletion; all other hooks are left untouched.

**Error handling (non-fatal)**: If the LIST request fails (403, network error, 404) or any DELETE fails with a non-404 status, `cleanupFn` returns an error. The supervisor logs a warning and proceeds with subprocess launch regardless — the circuit-breaker remains as defense-in-depth. A 404 on DELETE is treated as success (hook already gone; idempotent).

**Counter reset (R9)**: After `cleanupFn` returns `nil` (orphans deleted or none found), `permanentFailureCount` is reset to zero under `wm.mu` before the subprocess starts. Prior 422s were caused by the orphan and are no longer evidence of a permanent failure. If cleanup itself fails, the counter is not reset.

**Scope**: Per-repo mode only (`GET /repos/{owner}/{repo}/hooks`, `DELETE /repos/{owner}/{repo}/hooks/<id>`). Org-mode cleanup (`/orgs/{org}/hooks`) is deferred; most fabrik tokens lack `admin:org` scope, as evidenced by 404 responses on every `/orgs/…` attempt. The PR for this issue documents this non-coverage explicitly.

### Injection Pattern

The cleanup function is injected as `cleanupFn func(repos []string) error` — a new field on `webhookManager` and a new final parameter to `newWebhookManager`. This matches the existing injection pattern for `killFn`, `startSubprocessFn`, `deltaFn`, and `healthChangeFn`. Tests that don't need cleanup pass `nil`; production wires a closure over `e.client.DeleteForwardingHooks`.

The `DeleteForwardingHooks(owner, repo string) error` method lives in a new `github/hooks.go` file. It uses the existing `restGetJSON` and `restDelete` helpers.

### Implementation Files

- `github/hooks.go`: `webhookForwarderURL` constant; `repoHook` unexported type; `DeleteForwardingHooks` method.
- `github/hooks_test.go`: `TestDeleteForwardingHooks_*` — five cases covering no-match, one-match, multi-match, GET failure, and 404-on-DELETE.
- `engine/webhook.go`: `cleanupFn` field on `webhookManager`; updated `newWebhookManager` signature; cleanup call in `supervise()` with logging and `permanentFailureCount` reset.
- `engine/webhook_test.go`: `TestSupervise_CleanupCalledBeforeEachSubprocess` (R8) — verifies ordering via a shared event sequence.
- `engine/interfaces.go`: `DeleteForwardingHooks` added to `GitHubClient` interface.
- `engine/poll.go`: `cleanupFn` closure wired at the `newWebhookManager` call site.
