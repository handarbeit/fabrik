# ADR 2475: Shared App webhook receiver package

## Status

Accepted (#2475). Builds on ADR 2142 (direct receiver), ADR 2143 (embedded Tailscale Funnel), ADR 2144 (delivery replay), ADR 2452 (route liveness) and ADR 2549 (server error log). Follows ADR 1113 (Pruefer cannot import the engine).

## Context

The GitHub App webhook receiver lived in `engine/`: the HTTP handler, the two listener sources, the tailnet/Funnel health tracker, route liveness, the hook-config check and the delivery replayer. Pruefer cannot import `engine` (ADR 1113), and its only event-driven mode, Hookdeck, is deprecated. Reimplementing the receiver there would duplicate security-sensitive code (signature verification, the size cap, Funnel-only listening).

## Decisions

1. **A leaf package tree, `internal/appwebhook`, with the Tailscale library in a sub-package.** `internal/appwebhook` holds everything except `tailscale.com`; `internal/appwebhook/tsnetnode` holds only the adapter (`node.go`), the single file in the module importing it. The core package reaches the adapter through the `Node` seam and an injected `NodeFactory` (the engine passes `tsnetnode.New`, tests pass a fake), never by import. A consumer that wants only the `direct` receiver therefore links no tsnet (about 34 MB). Rejected: one package, which forces tsnet into every importer.
2. **The boundary is a test.** `deps_test.go` runs `go list -deps` for both packages and fails on `engine` or `pruefer`; it fails, rather than skips, when `go list` cannot run. The check is a pure `forbiddenDeps([]string)` with a synthetic-violation test, so it is known able to fail. The package also does not import `tui`.
3. **Consumer-owned concerns stay with the consumer, behind small interfaces.** `Sink` (with the package's own `Delivery`; the engine converts to its `delivery`, which carries engine-only `ReceivedAt`), the read-only `ReplayLedger`/`LivenessLedger` (`Seen`, `Newest`, `ReceivedAt` — no method records a delivery, so replay and liveness cannot make a redelivery look like a duplicate), `Panel` (`Report`, `Resolve`, `Reconcile`), `HealthSink` and `LogFunc`. The intake, ledger, echo registry, dedupe, ordering and attribution do not move. Fault flags and the hook-config reader stay engine-side and are applied in the engine's factories.
4. **`internal/localapi` is imported as is** for `TransportHealth`, `RouteLiveness` and `ReplayHealth`, so `fabrik_health` is byte-identical with no mapping layer to drift. The spec forbids only `engine` and `pruefer`; `localapi` imports neither. Rejected: neutral types plus a mapping, for a leaner Pruefer graph nobody required.
5. **Operator-visible text moves with the code.** Panel keys, fingerprints (persisted in `.fabrik/warnings.json`), log wording (`tests/gate/delivery_probe.go` parses one line) and health strings are copied verbatim; the tracker takes the hostname and auth-key variable name as configuration. The mechanical move is the oracle: the moved tests assert the same things.
6. **Static guards follow the code.** The replay and liveness "never submits" scans point at `replay.go`/`liveness.go` and first assert the target holds the replayer/controller; `TestOnlyTheAdapterFileImportsTailscale` is a module-wide walk requiring exactly `internal/appwebhook/tsnetnode/node.go`; `warnings/scan_test.go` scans `internal/appwebhook`.
7. **Engine wrapper.** `engine/app_webhook.go`'s `appWebhookTransport` embeds `*appwebhook.Receiver` and adds only `SetSink(func(delivery))` and the Hookdeck-shared `setInstallationCoverageNote`, so `ingestTransport` and `appCoverageTransport` are unchanged.

## Consequences

- Pruefer can import the receiver in the next issue; this change wires nothing in Pruefer and changes no behaviour, flag, variable, default, health row, warning or log line.
- A larger exported surface (the types above plus a few `...ForTest` seams) replaces package-private access; the engine's wiring tests keep driving the real intake and cache.
- The sim bed has no transport layer, so regressions here are caught by the unit and integration tests and the release-gate live webhook tests, as before.
