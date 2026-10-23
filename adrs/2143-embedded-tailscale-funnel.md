# ADR 2143: Embedded Tailscale Funnel receiver (`event_source: tailscale`)

**Date**: 2026-10-22 (revised 2026-10-23: the build tag is dropped)
**Status**: Accepted
**Issue**: #2143 — events: embedded Tailscale Funnel receiver — one tailnet node per daemon via tsnet
**Builds on**: ADR 2142 (direct App webhook transport), ADR 2351 (transport-neutral intake), ADR 1142 (Hookdeck: secret in a named variable, worker scrub), ADR 2027 (worker secret scrub), ADR 043 (reconcile)
**Chain**: second of #2081's chain; delivery replay is #2144.

## Context

`event_source: direct` (ADR 2142) lets a daemon receive its GitHub App's webhook behind any proxy, but the Tailscale route still needs `tailscale funnel` run by hand on the host, and every daemon on a machine shares one Funnel hostname, separated only by path. With one daemon per App, used to narrow each daemon's security scope, that boundary stops short of the network edge: nothing about a daemon's public exposure is visible or revocable per daemon.

## Decision

1. **Same transport, different listener.** `event_source: tailscale` builds the very same `*appWebhookTransport` as `direct`, through `newAppWebhookTransport`, with a listener factory that starts an embedded `tsnet.Server` (own hostname, own `Dir`), waits for `Up`, and calls `ListenFunnel("tcp", ":443")` (TLS terminated on the node with Tailscale's certificate). No handler, dedupe, ack or signature code is added; a test pins that the tailscale file constructs the shared transport and nothing else.
2. **A seam, and one adapter file.** The node is used only through the `tailnetNode` interface (`Up`, `ListenFunnel`, `Watch`, `Close`). The real adapter is `engine/tailnet_tsnet.go`, the only file that imports `tailscale.com` (a scan test enforces it). It compiles in every build: there is no build tag and no stub (see "Build tag dropped" below). Tests drive a fake node, so none needs a tailnet.
3. **Startup is synchronous and bounded.** `Up` gets a 90 s deadline (a named constant). Any `Up` or `ListenFunnel` failure closes the half-built node and is an ordinary `Start` error: poll-only, a `not listening` health row, a `tailscale_listen` panel entry whose text names the usual causes (the `funnel` node attribute, HTTPS certificates, the key) — ADR 2142 decision 7. Funnel is not retried after startup; a restart retries it. The webhook URL is logged once, from the DNS name the node *reports* (tsnet suffixes a taken hostname).
4. **Auth key handling.** The variable named by `tailscale_auth_key_env` (default `FABRIK_TS_AUTHKEY`) is read only inside `Start`, whenever it is set; it is *required* only when `<state_dir>/tailscaled.state` is absent or empty. It goes solely to `tsnet.Server.AuthKey`, which tsnet ignores for a node logged in from stored state but uses when the node is in `NeedsLogin` (expired or revoked registration), so a restart with a fresh key re-authenticates the existing node without deleting `state_dir` (which would create a second node). The first version read the key only when no state existed, which made that recovery impossible. Fabrik never logs or stores it; the node's own state in `state_dir` (created 0700) is the only persisted credential. Both the default and the configured name are scrubbed from worker environments (`workerenv.Scrubbed`, plus `workerSecretEnvNames` for a custom name). The node's user-facing log (`UserLogf`) goes through the engine log with any `login.tailscale.com` link replaced and a line ending added when the line lacks one (tsnet supplies none, so lines otherwise run together), because a one-time login URL is credential-like and Fabrik never logs in interactively; the backend `Logf` stays discarded.
5. **Health is the shared state plus a tailnet condition.** `appWebhookTransport` gains `SetNetworkCondition` (optional; `direct` never calls it) as one more independent input to the derived state, so a reconcile "healthy" hint cannot mask a tailnet drop. "Reachable" means connected and the Funnel not reported `unavailable` (an unknown Funnel status is not an outage). An unreachable→reachable transition after an earlier reachable state calls the reconnect handler (the intake's `requestReconcile`); the first one never does. `localapi.TransportHealth` gains `tailnet` and `funnel` (`"unknown"` for other transports). A drop is a `tailscale_connection` panel entry and an unavailable Funnel on a connected node is a `tailscale_funnel` entry, each cleared on recovery. `SetStopHook` lets `Stop` close the node after the listener. The adapter's Funnel status is `ipn.CheckFunnelAccess` on the node's self status (the check `ListenFunnel` makes: HTTPS certificates, the `funnel` attribute, port 443). It re-reads the status on backend-state and netmap notifications and on a 30 s timer, because revoking the attribute changes no backend state, and a failed read keeps the last known Funnel status instead of reporting `unknown`.
6. **R7: the App's webhook config is checked.** `github.FetchAppHookConfig` reads `GET /app/hook/config` with the App JWT; its result type has no secret field, so the secret GitHub returns is never decoded. The URL (host case, default `:443`, trailing slash normalised) and content type (`json`/`application/json`) are compared with the node's; a mismatch reconciles `tailscale_webhook_config` entries, an unreadable config logs one line and changes nothing. It runs at startup and from the transport's coverage hook on every reconcile tick. The JWT is built in the engine from the configured key (`gh.BuildAppJWT`) rather than adding an accessor to `internal/githubauth`, which Pruefer shares.
7. **Restart and exec.** SIGHUP already calls `transports.StopAll()`, which now closes the node; the new process reuses `state_dir` with no key. The self-upgrade exec does **not** stop transports: a successful exec replaces the process and the new one reuses the state; a failed exec leaves exactly one running listener (stopping and restarting it would need a restart mechanism `ingestTransport` does not have and would add a window with no listener for no benefit).
8. **Self-upgrade is not special-cased.** Because every build carries the node, a release download or a dev rebuild produces a binary that can still run `event_source: tailscale`, so a tailscale daemon self-upgrades like any other.

## Dependency and module graph

`tailscale.com` is pinned at **v1.96.5**, the newest release whose own `go` directive (1.26.0) is at or below the module's `go 1.26.1`. Newer lines require 1.26.2 (v1.98.x) and 1.27.x (v1.104.x), which would force a toolchain bump on every contributor and CI job and is hard to undo after release. `go mod tidy` leaves the `go` directive unchanged. The cost is an older Tailscale; a bump is a follow-up once the repository's Go version moves. A nested module was rejected: `engine` must import the adapter, so the root module would require `tailscale.com` anyway.

`go.mod` and `go.sum` carry the dependency and its closure (`go.sum` grew from 67 to ~290 lines; `go.mod` from 26 to 83 requirement lines; `gorilla/websocket` and `golang.org/x/text` were bumped by the graph). Because the node is compiled into every build, all of it is in the shipped binary too.

`github.com/gorilla/websocket` moved from v1.5.3 to `v1.5.4-0.20250319132907-e064f32e3674`, an untagged upstream commit. `tailscale.com v1.96.5` itself requires that pseudo-version, and gorilla has published no release newer than v1.5.3, so no released version satisfies the requirement. The Hookdeck source (`internal/events/hookdeck`) uses the package; its tests pass unchanged on the new version. A gorilla release (or a Tailscale line that requires one) removes the untagged commit.

The dependency is a deliberate exception to the "only `yaml.v3`" convention in `.claude/rules/golang.md`, accepted because the usage is isolated to one adapter file behind the `tailnetNode` seam.

## Binary size and the build tag

Measured on darwin/arm64 with `CGO_ENABLED=0 go build -ldflags "-s -w" .` (the release flags), before the tag was dropped:

| Build | `fabrik` size |
|---|---|
| without the node (the former default) | 17,214,178 bytes (16.4 MiB) |
| with the node (the former `-tags tailscale`) | 36,767,330 bytes (35.1 MiB) |

The node adds ~19.5 MB (+114%). The first version of this ADR kept it behind `-tags tailscale` and left it out of release binaries. **Build tag dropped (decision of 2026-10-23): every build, release binaries included, carries the node.** About 19.5 MB is not worth a split build, which also meant a tailscale daemon could not self-upgrade from the release assets and needed its own self-upgrade guard. With one build there is one set of release assets, no guard, no "built without the node" refusal, and `go test -race ./...` covers all of the code. The cost is that every download and every release self-upgrade is about 19.5 MB larger.

## CI

No extra job or step is needed: the single `go vet ./...` and `go test -race ./...` of the Go test job compile and test the adapter and its tests (including the status-tracker tests in `engine/tailnet_tsnet_test.go`). The tagged steps the first version proposed were never added.

## Consequences

- Each daemon is its own tailnet machine with its own Funnel endpoint, visible and revocable in Tailscale's admin console.
- Deleting `state_dir` while a valid key remains creates a second node and a new certificate request (Let's Encrypt rate limits can then block the listener for more than a day); the docs warn against it, and the reported-name URL plus the R7 check expose the resulting mismatch.
- Deliveries sent while the node is down are lost, as for `direct`; reconcile is the safety net and replay is #2144.
- `Up` can wait up to 90 s at startup with no network. The result is bounded and visible (panel entry, poll-only), not a hang.

## Verification

`engine/tailnet_test.go` (fake node), `engine/tailnet_refuse_test.go`, `engine/tailnet_hookcheck_test.go`, `engine/tailnet_tsnet_test.go` (the adapter's status tracker and Funnel mapping) and `github/app_test.go` (`FetchAppHookConfig`) run under `go test -race ./...`. The adapter's real network behaviour cannot run in CI. It was verified live once on a test bed (AC1 and AC5, recorded on the PR): the daemon joined the tailnet as its own machine, logged its Funnel URL, applied a real signed GitHub redelivery, raised and then cleared the R7 mismatch entry after the App URL was corrected, and a restart with state and no auth key reused the same node. The live run also showed that a new node's public Funnel name can take several minutes to resolve (the first redelivery got a 502 meanwhile), which the user guide now notes.
