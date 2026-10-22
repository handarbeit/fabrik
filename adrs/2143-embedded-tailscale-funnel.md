# ADR 2143: Embedded Tailscale Funnel receiver (`event_source: tailscale`)

**Date**: 2026-10-22
**Status**: Accepted
**Issue**: #2143 — events: embedded Tailscale Funnel receiver — one tailnet node per daemon via tsnet, behind `-tags tailscale`
**Builds on**: ADR 2142 (direct App webhook transport), ADR 2351 (transport-neutral intake), ADR 1142 (Hookdeck: secret in a named variable, worker scrub), ADR 2027 (worker secret scrub), ADR 043 (reconcile)
**Chain**: second of #2081's chain; delivery replay is #2144.

## Context

`event_source: direct` (ADR 2142) lets a daemon receive its GitHub App's webhook behind any proxy, but the Tailscale route still needs `tailscale funnel` run by hand on the host, and every daemon on a machine shares one Funnel hostname, separated only by path. With one daemon per App, used to narrow each daemon's security scope, that boundary stops short of the network edge: nothing about a daemon's public exposure is visible or revocable per daemon.

## Decision

1. **Same transport, different listener.** `event_source: tailscale` builds the very same `*appWebhookTransport` as `direct`, through `newAppWebhookTransport`, with a listener factory that starts an embedded `tsnet.Server` (own hostname, own `Dir`), waits for `Up`, and calls `ListenFunnel("tcp", ":443")` (TLS terminated on the node with Tailscale's certificate). No handler, dedupe, ack or signature code is added; a test pins that the tailscale file constructs the shared transport and nothing else.
2. **A seam, and one tagged file.** The node is used only through the `tailnetNode` interface (`Up`, `ListenFunnel`, `Watch`, `Close`). The real adapter is `engine/tailnet_tsnet.go` (`//go:build tailscale`), the only file that imports `tailscale.com` (a scan test enforces it); `engine/tailnet_stub.go` (`!tailscale`) is the default build's factory. Tests drive a fake node, so none needs a tailnet. `tailscaleBuilt` is a variable only so both refusals are testable in one build.
3. **Startup is synchronous and bounded.** `Up` gets a 90 s deadline (a named constant). Any `Up` or `ListenFunnel` failure closes the half-built node and is an ordinary `Start` error: poll-only, a `not listening` health row, a `tailscale_listen` panel entry whose text names the usual causes (the `funnel` node attribute, HTTPS certificates, the key) — ADR 2142 decision 7. Funnel is not retried after startup; a restart retries it. The webhook URL is logged once, from the DNS name the node *reports* (tsnet suffixes a taken hostname).
4. **Auth key handling.** The variable named by `tailscale_auth_key_env` (default `FABRIK_TS_AUTHKEY`) is read only inside `Start`, and only when `<state_dir>/tailscaled.state` is absent or empty; it goes solely to `tsnet.Server.AuthKey`. Fabrik never logs or stores it; the node's own state in `state_dir` (created 0700) is the only persisted credential. Both the default and the configured name are scrubbed from worker environments (`workerenv.Scrubbed`, plus `workerSecretEnvNames` for a custom name). The node's user-facing log (`UserLogf`) goes through the engine log with any `login.tailscale.com` link replaced, because a one-time login URL is credential-like and Fabrik never logs in interactively; the backend `Logf` stays discarded.
5. **Health is the shared state plus a tailnet condition.** `appWebhookTransport` gains `SetNetworkCondition` (optional; `direct` never calls it) as one more independent input to the derived state, so a reconcile "healthy" hint cannot mask a tailnet drop. "Reachable" means connected and the Funnel not reported `unavailable` (an unknown Funnel status is not an outage). An unreachable→reachable transition after an earlier reachable state calls the reconnect handler (the intake's `requestReconcile`); the first one never does. `localapi.TransportHealth` gains `tailnet` and `funnel` (`"unknown"` for other transports). A drop is a `tailscale_connection` panel entry and an unavailable Funnel on a connected node is a `tailscale_funnel` entry, each cleared on recovery. `SetStopHook` lets `Stop` close the node after the listener.
6. **R7: the App's webhook config is checked.** `github.FetchAppHookConfig` reads `GET /app/hook/config` with the App JWT; its result type has no secret field, so the secret GitHub returns is never decoded. The URL (host case, default `:443`, trailing slash normalised) and content type (`json`/`application/json`) are compared with the node's; a mismatch reconciles `tailscale_webhook_config` entries, an unreadable config logs one line and changes nothing. It runs at startup and from the transport's coverage hook on every reconcile tick. The JWT is built in the engine from the configured key (`gh.BuildAppJWT`) rather than adding an accessor to `internal/githubauth`, which Pruefer shares.
7. **Restart and exec.** SIGHUP already calls `transports.StopAll()`, which now closes the node; the new process reuses `state_dir` with no key. The self-upgrade exec does **not** stop transports: a successful exec replaces the process and the new one reuses the state; a failed exec leaves exactly one running listener (stopping and restarting it would need a restart mechanism `ingestTransport` does not have and would add a window with no listener for no benefit).
8. **Self-upgrade is skipped for a tagged tailscale daemon.** The release asset is built without the tag and the dev rebuild runs a plain `go build`, so an upgrade would turn a working daemon into one that refuses `event_source: tailscale` after the exec. `checkAndUpgrade` returns early, announcing it once (never silent). An operator upgrades by rebuilding with `-tags tailscale`.

## Dependency and module graph

`tailscale.com` is pinned at **v1.96.5**, the newest release whose own `go` directive (1.26.0) is at or below the module's `go 1.26.1`. Newer lines require 1.26.2 (v1.98.x) and 1.27.x (v1.104.x), which would force a toolchain bump on every contributor and CI job and is hard to undo after release. `go mod tidy` leaves the `go` directive unchanged. The cost is an older Tailscale; a bump is a follow-up once the repository's Go version moves. A nested module was rejected: `engine` must import the adapter, so the root module would require `tailscale.com` anyway.

A build tag does not shield `go.mod`: the dependency and its closure are in `go.mod`/`go.sum` for every build (`go.sum` grew from 67 to ~290 lines; `go.mod` from 26 to 83 requirement lines; `gorilla/websocket` and `golang.org/x/text` were bumped by the graph). A **default build compiles none of `tailscale.com`** — `go build ./...`, `go vet` and `go test` without the tag never touch it. Two bumped modules are different, because the module graph (not the tag) selects their versions:

- `golang.org/x/text` (v0.3.8 → v0.32.0) is not in the default build's package graph (`go list -deps ./...` lists only the standard library's vendored copy), so it changes nothing there.
- `github.com/gorilla/websocket` (v1.5.3 → `v1.5.4-0.20250319132907-e064f32e3674`, an untagged upstream commit) **is** compiled by default builds: the Hookdeck source (`internal/events/hookdeck`) uses it. `tailscale.com v1.96.5` itself requires that pseudo-version, and gorilla has published no release newer than v1.5.3, so no released version satisfies the requirement; the only way to avoid the change is not to depend on `tailscale.com`. The Hookdeck and events tests pass unchanged on the new version. Default-build users therefore ship one untagged upstream websocket commit in the Hookdeck path; this is the accepted cost, and a gorilla release (or a Tailscale line that requires one) removes it.

The dependency is a deliberate exception to the "only `yaml.v3`" convention in `.claude/rules/golang.md`, accepted because the usage is isolated to one tagged file.

## Binary size and the release decision

Measured on darwin/arm64 with `CGO_ENABLED=0 go build -ldflags "-s -w" .` (the release flags):

| Build | `fabrik` size |
|---|---|
| default | 17,214,178 bytes (16.4 MiB) |
| `-tags tailscale` | 36,767,330 bytes (35.1 MiB) |

The tag adds ~19.5 MB (+114%). The tagged build links with `CGO_ENABLED=0`, so it is buildable by the release pipeline. **Decision: published release binaries do not carry the tag.** A doubling of every download and of every release self-upgrade, for a feature most operators will not use, is not worth it; the delta is well above the ~10 MB threshold this ADR set for considering a tagged release artifact. Operators build with `go install -tags tailscale` (documented in the user guide). A separate tagged goreleaser artifact is not added: it would need its own self-upgrade asset naming, which decision 8 shows is a design of its own. Revisit if demand appears.

## CI

Two steps belong in the Go test job: `go vet -tags tailscale ./engine/... ./cmd/... ./config/... ./internal/...` and `go test -race -tags tailscale -parallel "$(default_race_parallel)" ./engine/... ./cmd/... ./config/... ./internal/...`, each with the standard classifier `if: steps.classify.outputs.skip != 'true'` (and an entry for each in `tests/ciskip/workflow_test.go`'s table). The tag changes only the `engine` build (the adapter and its tagged tests) and no other package's tests are tag-dependent, so this is the FR-020 reading adopted here: the tagged run covers every package that can see the tag, not a second whole-module run (which links tsnet into every test binary). A whole-module tagged run remains a release check.

**Not applied in this change.** The implementation identity (a GitHub App without the `workflows` permission) cannot push edits to `.github/workflows/`, so the workflow edit and its `ciskip` table entries were left out rather than split from each other (the table test fails if they disagree). Both are a small patch recorded in the PR description; until it lands, the tagged build is covered by the manual check below and the local runs recorded in the PR.

## Consequences

- Each daemon is its own tailnet machine with its own Funnel endpoint, visible and revocable in Tailscale's admin console.
- Deleting `state_dir` while a valid key remains creates a second node and a new certificate request (Let's Encrypt rate limits can then block the listener for more than a day); the docs warn against it, and the reported-name URL plus the R7 check expose the resulting mismatch.
- A tagged daemon does not self-upgrade; the notice says why.
- Deliveries sent while the node is down are lost, as for `direct`; reconcile is the safety net and replay is #2144.
- `Up` can wait up to 90 s at startup with no network. The result is bounded and visible (panel entry, poll-only), not a hang.

## Verification

`engine/tailnet_test.go` (fake node), `engine/tailnet_refuse_test.go`, `engine/tailnet_hookcheck_test.go`, `github/app_test.go` (`FetchAppHookConfig`), the `!tailscale` test `engine/tailnet_default_build_test.go` (the build-hint refusal) and the `tailscale` test `engine/tailnet_tag_test.go` (adapter compiles; `Close` is safe at every point) run under `go test -race` in both builds. The adapter's real network behaviour (a node joining a real tailnet, Funnel, a real signed delivery from GitHub) cannot run in CI; that live check (AC1/SC-001) is a manual step on a test bed, recorded in the PR.
