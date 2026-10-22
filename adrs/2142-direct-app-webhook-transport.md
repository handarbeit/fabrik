# ADR 2142: Direct App webhook transport (`event_source: direct`)

**Date**: 2026-10-22
**Status**: Accepted
**Issue**: #2142 — events: direct App webhook transport — receive App webhooks behind any tunnel or proxy
**Builds on**: ADR 2351 (transport-neutral intake), ADR 1142 / 1254 (Hookdeck), ADR 1563 (signature drift), ADR 1752 (`--webhooks` refused under App auth), ADR 043 (reconcile ticker), ADR 2027 (worker secret scrub)
**Chain**: first of #2081's chain — #2143 (embedded `tsnet`) reuses this transport with another listener; #2144 adds delivery replay.

## Context

Under GitHub App auth the only real-time event path was Hookdeck: a third-party account that sees every payload, plus one more API key and secret per daemon. An operator who can already expose a local port (Tailscale Funnel through the system `tailscaled`, Cloudflare Tunnel, a reverse proxy) had no way to point the App's webhook URL at the daemon. ADR 2351 made the intake transport-neutral, so a new transport only has to verify its own signature and hand a delivery to the sink.

## Decision

1. **A new `ingestTransport`, `appWebhookTransport` (`engine/app_webhook.go`)**, selected by `event_source: direct` (App auth only). It is a plain-HTTP receiver around an **injected listener factory** called in `Start`: a bind error is an ordinary `Start` error (poll-only via `attachTransport`), and #2143 reuses the whole transport by passing a different listener. The name is set by the constructor (`direct` here).
2. **Handler order is fixed**: path (404) → method (405) → read at most the 25 MiB cap + 1 byte (413, before verification, not counted as a signature failure) → `events.VerifySignature` (401, counted) → `json.Valid` (400 naming the content-type fix, not counted) → `ping` (a *verified* ping is acknowledged and logged, not submitted, and counts as a signature success) → `intake.Submit` and 200. A missing signature header is a signature failure. The transport adds no dedupe, ack, echo, reconnect or cap logic beyond the cap+1 read; the reconnect handler is stored and never called, and the reconcile ticker is unchanged.
3. **The drift tracker is extracted** from `hookdeck.Source` into `internal/events` as the mutex-guarded `SigDrift` (`RecordFailure`/`RecordSuccess` return the transition; the caller fires callbacks outside the lock), with `SignatureDriftThreshold` defined once (20). `hookdeck.SignatureDriftThreshold` aliases it, so Hookdeck's behaviour, its tests and Pruefer are unchanged. The lock is new: the HTTP server handles each request on its own goroutine, where Hookdeck's read loop was single-threaded.
4. **Health is derived, never overwritten**: the state is computed from *listening*, *cache drift* (the reconcile hint) and *signature drift*, so a reconcile "healthy" hint cannot mask drift or a dead listener. Cache drift affects the aggregate unhealthy mode only, not the row's name.
5. **`ingestTransport` gains a required `HealthRow()`**, and `fabrik_health.webhook` gains an additive `transports` list (`localapi.TransportHealth`: name, state, last delivery, rejected-signature/malformed/oversized counts, `"unknown"` where a transport cannot report). Required rather than an optional interface so a future transport cannot silently report nothing; the cost was the two existing transports and the test fake. The aggregate `mode`/`last_event_at` are unchanged. The direct states are `listening`, `not listening`, `signature drift`.
6. **Config** follows the `hookdeck_*` precedent (flat keys, flag > env > YAML): `direct_listen` (required), `app_webhook_path` (default `/webhook`), `app_webhook_secret_env` (a variable *name*; default `FABRIK_GITHUB_WEBHOOK_SECRET`, a separate constant from Hookdeck's although equal in value). The path and secret keys are shared with the later `event_source: tailscale`. All three env names are `workerenv.Scrubbed`; the configured secret-variable name is fed to `workerenv.Resolve` through `workerSecretEnvNames`, because a custom name is invisible to the literal-scan test.
7. **Misconfiguration fails startup, a runtime fault degrades.** `RefuseDirectMisconfigured` (in `New()`, beside the other App refusals) refuses PAT auth, a missing `direct_listen`, an empty secret and a non-absolute path, each naming the fix — unlike Hookdeck's missing-credential soft fallback, because these are configuration mistakes that would otherwise leave a silently deaf daemon. A bind failure (port in use, malformed address) leaves the daemon poll-only with a `direct_listen` warnings-panel entry naming the address, resolved by a later successful start.
8. **Panel entries**: `direct_signature_drift` (reported at the drift threshold, resolved on recovery, fingerprint pinned), `direct_listen`, `direct_coverage`. The Hookdeck installation-coverage check is generalised to `checkAppInstallationCoverage(transport, spec)`; each transport keeps its own persisted panel type (`hookdeck_coverage` unchanged) so two transports' `Reconcile` sweeps cannot clobber each other.
9. **No TLS in the daemon.** The transport speaks plain HTTP; docs and examples bind loopback and say TLS is the fronting proxy's job. Server timeouts bound a slow client (header 10 s, read 2 min — enough for a 25 MiB body —, write 30 s, idle 2 min); `Stop` shuts down with a 5 s grace.

## Consequences

- An App-mode operator with a public route to the daemon needs no Hookdeck account, key or relay.
- Deliveries sent while the daemon (or the route) is down are lost: there is no relay buffer. The reconcile ticker is the safety net and replay is #2144. A listener that is bound but never reached (a wrong webhook URL) looks healthy and widens the idle poll cap, the same weakness Hookdeck has when connected but silent; the reconcile ticker backstops it.
- The body is buffered up to the cap before HMAC verification (as in the gh-forward listener), so a non-loopback bind exposes a memory-pressure vector bounded only by the timeouts. The docs steer toward loopback plus a fronting proxy.
- A failed listen is only retried by a restart (`Start` runs once per process); a retry loop is out of scope.
- `fabrik_health` consumers see a new `webhook.transports` array (`[]` when poll-only).

## Verification

The sim bed has no transport layer (ADR 2351), so there is no sim twin. `engine/app_webhook_test.go` covers every status code, the cap boundary, `ping`, drift and recovery (including a concurrent `-race` hammer and the masking invariant), listen failure and its panel entry, and an end-to-end delivery over a real loopback listener through the real intake into the board cache, with the ledger dropping a duplicate ID. `internal/events/sigdrift_test.go` pins the tracker; Hookdeck's existing tests pass unmodified. The feature was not run against a real App behind a system `tailscale funnel` in this implementation; that live check (SC-010) is recorded in the PR.
