# ADR 2472: Tailscale 1.104.2 and Go 1.27.3

**Status:** Accepted (2026-10-25). Amends [ADR 2143](2143-embedded-tailscale-funnel.md) ("Dependency and module graph").

## Context

ADR 2143 pinned `tailscale.com` at v1.96.5, the newest release whose `go` directive fit the module's `go 1.26.1`. It deferred any bump until the repository's Go version moved. Dependabot PR #2436 then proposed v1.104.2. That PR did not compile, and v1.104.x requires `go 1.27.3`.

The embedded node is internet-facing through Funnel, and 0.0.90 is the first release that ships it. Starting that release on a Tailscale line already eight minors old would make the first upgrade an urgent one later, and raising a `go` directive after a release is no easier than before it.

## Decision

1. **Take v1.104.2 and raise the module's `go` directive to 1.27.3.** CI and release builds follow `go.mod` (`go-version-file`). A contributor or a `go install` user on an older toolchain gets 1.27.3 through `GOTOOLCHAIN=auto`, the default; with `GOTOOLCHAIN=local` an older toolchain refuses the module. The documented floor is now Go 1.27.3+. The same Dependabot group also brings `charmbracelet/x/ansi` 0.11.9, `vektah/gqlparser/v2` 2.5.64 and `golang.org/x/net` 0.61.0.
2. **Port the IPN bus watch** (`engine/tailnet_tsnet.go` `watchOnce`). Tailscale 1.104 removed `Notify.NetMap`, `NotifyInitialNetMap` and `NotifyRateLimit`, and tailscaled rejects the two flags (`ObsoleteNotifyInitialNetMap`, `ObsoleteNotifyRateLimit`). The watch now subscribes with `NotifyInitialState|NotifyInitialStatus` and re-reads the node status when a notification carries `State`, `InitialStatus` or `SelfChange`. `SelfChange` is sent to every watcher, with no subscription bit, whenever the self node changes, including its capabilities. The Funnel grant is one of those capabilities. A Funnel change that touches no self node, such as HTTPS certificates being disabled, was never reliably signalled by the netmap either; the 30 s status poll (`tsnetStatusPoll`) still covers it.
3. **Restart refusal unchanged.** `tailnetStateExists` reads tsnet's persisted state, which was checked against the v1.104.2 source: still `<state_dir>/tailscaled.state`, a JSON `map[StateKey][]byte`, with the profile prefs' `Persist` tagged `json:"Config"` and carrying `NodeID`.
4. **gorilla/websocket** stays at the untagged `v1.5.4-0.20250319132907-e064f32e3674`, which v1.104.2 still requires.

## Consequences

- Release-flag binary (`go build -ldflags "-s -w"`, darwin/arm64): 34.5 MB, down from 36.9 MB on v1.96.5.
- Every contributor and CI job builds with Go 1.27.3.
- No CI job runs the real tsnet path. The port is covered by the tracker unit tests and the next live run of the gate's `app/on/webhook` leg, which uses a real node.
