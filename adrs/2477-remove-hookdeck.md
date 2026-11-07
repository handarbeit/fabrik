# ADR 2477: Remove Hookdeck

**Date**: 2026-11-07
**Status**: Accepted
**Issue**: #2477 — Remove Hookdeck from the engine and Pruefer
**Supersedes**: ADR 1142, ADR 1254, ADR 1563
**Builds on**: ADR 2027, ADR 2142, ADR 2143, ADR 2351, ADR 2475, ADR 2476

## Context

`event_source: hookdeck` was the relay-based way to get near-real-time webhook delivery under GitHub App auth, in the engine (ADR 1142) and in Pruefer (ADR 1254, ADR 1563). It was deprecated in v0.0.90 with a documentation notice (#2478). Two relay-free replacements now exist for both programs: `event_source: direct` and `event_source: tailscale` (ADR 2142, ADR 2143 for the engine; ADR 2476 for Pruefer). Nothing in use depends on Hookdeck: the dev daemon's Hookdeck session had been refused with 401 for at least a day before anyone noticed (#2474). Keeping it costs a second transport, its health row and coverage check, a client library (`internal/events/hookdeck`, about 2,400 lines) and a large test surface.

Removal is a breaking change for any operator still configured for it. The failure mode therefore matters in two directions: a stale `event_source: hookdeck` must fail loudly with a pointer to the migration, while merely leftover keys must not turn a clean migration into an outage. And the code that knew a Hookdeck API key was a secret goes away, so the protection that keeps that key out of a Claude worker's environment must not go with it.

## Decision

1. **`event_source: hookdeck` is refused at startup**, in both programs, by every configuration route (flag, environment, YAML). One message names `tailscale` and `direct` and the User Guide section "Migrating from Hookdeck".
   - Engine: `RefuseHookdeckEventSource` (`engine/github_app_auth.go`) runs first in `engine.New()`, before `validateGitHubAppConfig`, so a partial-App error can never mask the migration message; `cmd/root.go` also calls it right after `event_source` is resolved, before any token or App work. `RefuseUnknownEventSource` no longer lists `hookdeck` as valid and delegates the exact value to the dedicated refusal; a typo such as `Hookdeck` is an ordinary unknown value.
   - Pruefer: refused at config load in `LoadConfig` (`pruefer/config.go`). Pruefer cannot import the engine (ADR 1113), so the message text is duplicated in `pruefer/legacy_hookdeck.go`; both name `tailscale`, `direct` and the migration section.
2. **Leftover Hookdeck settings never fail startup.** `hookdeck_api_key_env`/`hookdeck_webhook_secret_env` (engine YAML), `FABRIK_HOOKDECK_*` variables, the `--hookdeck-api-key-env`/`--hookdeck-webhook-secret-env` flags, and Pruefer's `hookdeck.*` keys, `-hookdeck-*` flags and `PRUEFER_HOOKDECK_*` variables are ignored and reported **once per process start** in a single `[startup] notice: ignoring removed Hookdeck setting(s): …` line (Pruefer: the same text under its `startup` log tag). The line lists setting names only, never values. The flags stay registered as no-ops, because a flag-parse failure would break startup over a dead setting. It is deliberately a log line and not a warnings-panel entry: a stale unused key is not an operator action item. The engine keys are excluded from `UnrecognizedTopLevelKeys` so the generic `config_key` panel entry does not also fire. A SIGHUP or self-upgrade re-exec is a fresh process start and prints the line again.
3. **The worker-environment secret scrub is kept.** `FABRIK_HOOKDECK_API_KEY_ENV`, `FABRIK_HOOKDECK_WEBHOOK_SECRET_ENV` and `HOOKDECK_API_KEY` stay in `workerenv.Scrubbed`, moved to a new `GroupLegacy` with a recorded reason ("leftover Hookdeck secret protection; retained through v0.0.92, sunset per ADR 2477"). The variables a leftover setting *names* (a custom `hookdeck_api_key_env: MY_HD_KEY`) reach `workerenv.Resolve` through the new `engine.Config.LegacyScrubEnvNames`, which `cmd` fills from the flag, the environment and YAML, taking the value from every route and not only the winning one. `workerSecretEnvNames` appends them. `TestBuildClaudeEnv_LeftoverHookdeckSecretsNeverReachAWorker` covers the config-to-`Resolve` path end to end. The `HOOKDECK_` prefix stays in Pruefer's `reviewEnvNeverPassthrough` as defence in depth; Pruefer's review environment is an allowlist, so a leftover key never reaches the review process either way.
4. **The leftover-settings reader is one deletable surface.** `config/legacy_hookdeck.go` (`LoadLegacyHookdeck`), `cmd/legacy_hookdeck.go` (`collectLegacyHookdeck`, `legacyHookdeckNotice`) and `pruefer/legacy_hookdeck.go` hold the legacy handling; `ProjectConfig` and `engine.Config` carry no Hookdeck fields.
5. **What is deliberately kept:**
   - `events.SigDrift` and `events.SignatureDriftThreshold` (shared with the direct and tailscale receivers, ADR 2142);
   - `checkAppInstallationCoverage` and its helpers, moved verbatim to `engine/app_coverage.go` (the direct and tailscale transports call it; only the Hookdeck wrapper and its `hookdeck_coverage` panel type are gone; `clearRetiredHookdeckWarnings` sweeps any `hookdeck_coverage` entry an earlier version left in `.fabrik/warnings.json` once at each startup, so a migrated operator is not left with a stale warning nothing re-checks, and is sunset with the other legacy handling);
   - `FABRIK_GITHUB_WEBHOOK_SECRET`, still scrubbed and still the default `app_webhook_secret_env`;
   - the `worker_env:hookdeck_dropped` warnings-panel key, kept so existing dismissals stay valid; its text now reads "leftover secret-variable names not scrubbed";
   - the generic drop accounting and health handlers in `pruefer/daemon.go` (`recordDrop`, `HealthHandler`, `SignatureDriftHandler`), which the direct and tailscale sources use.
6. **Deleted:** `internal/events/hookdeck/`, `engine/hookdeck.go` and its tests, `RefuseHookdeckWithoutGitHubApp`, `startHookdeckTransport`, the Hookdeck source wiring in `pruefer/execute.go`, the `hookdeckManager` health row.
7. **History is preserved.** ADR 1142, ADR 1254 and ADR 1563 are kept with a status line naming this ADR (ADR 1142's notes that its coverage check and its separation from `--webhooks` live on in ADR 2142 and ADR 2351). Older release notes, `specs/` and the design audit are not rewritten.

## Sunset

The ignore-with-notice behaviour, the deprecated no-op flags and the retained scrub names stay for at least v0.0.92. Retiring them is a separate later decision: the notice and flags can go once no supported release upgrades from a Hookdeck-era config; the scrub names can go when an operator can no longer plausibly have a Hookdeck key in a `.env` the daemon loads. Removing them is a three-file deletion (the three `legacy_hookdeck.go` files), the `GroupLegacy` entries, and the `LegacyScrubEnvNames` plumbing.

## Operational precondition (FR-013)

The other local Fabrik directories still configured for Hookdeck (`fabrik-ideas`, `fabrik-release`) must be switched off or confirmed not running before this merges. A daemon on `--auto-upgrade` would otherwise re-exec into the startup refusal after upgrading. This cannot be checked from code.

## Consequences

- An operator upgrading to v0.0.92 with `event_source: hookdeck` gets a refusal and a pointer, not a silently deaf daemon.
- An operator who already moved to `tailscale`, `direct` or poll sees at most one notice line per start.
- A leftover Hookdeck key in `.env` keeps being kept out of workers.
- One fewer transport, health row, client library and test surface.

## Release notes (carry into `release-notes/v0.0.92.md` at cut time)

**Breaking: Hookdeck has been removed (Fabrik engine and Pruefer).**
`event_source: hookdeck` is now refused at startup. To migrate:

1. Choose `event_source: tailscale` (an embedded Tailscale Funnel node; needs `tailscale_hostname` and a Tailscale auth key on first start) or `event_source: direct` (a plain HTTP listener behind your own tunnel or proxy; needs `direct_listen`), or `event_source: poll`. Both receivers need GitHub App auth.
2. Repoint the GitHub App's webhook URL at the new receiver (content type `application/json`) and keep the webhook secret in the variable named by `app_webhook_secret_env` (default `FABRIK_GITHUB_WEBHOOK_SECRET`). For Pruefer, use the replacement from the Pruefer README, or poll.
3. Delete `event_source: hookdeck`, `hookdeck_api_key_env`, `hookdeck_webhook_secret_env` (Pruefer: the `hookdeck:` block), the `--hookdeck-*` flags, the `FABRIK_HOOKDECK_*`/`PRUEFER_HOOKDECK_*` variables and the Hookdeck API key.

Leftover Hookdeck settings alone do not stop startup; they are reported once as ignored. See "Migrating from Hookdeck" in the User Guide.
