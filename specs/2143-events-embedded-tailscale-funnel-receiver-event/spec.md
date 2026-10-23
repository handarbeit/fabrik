# Feature Specification: Embedded Tailscale Funnel receiver (`event_source: tailscale`)

**Feature Branch**: `fabrik/issue-2143`
**Created**: 2026-10-22
**Status**: Specified (revised 2026-10-23: R4 changed; the original request below is kept as filed)
**Input**: User description: "events: embedded Tailscale Funnel receiver (event_source: tailscale) — one tailnet node per daemon via tsnet, behind -tags tailscale

## Problem

`event_source: direct` (#2142) lets a daemon receive App webhooks behind any proxy, but the Tailscale route still needs the operator to run `tailscale funnel` by hand on the host, and every daemon on a machine shares one Funnel hostname, separated only by path. With one daemon per GitHub App, used to narrow each daemon's security scope, that boundary stops short of the network edge: the hostname, listener and Funnel config are shared, and nothing about a daemon's exposure is visible or revocable per daemon.

Mentioned for context: #2081 (the proposal this chain implements).

## What already exists (do not rebuild)

- **#2351 (ADR 2351)** — the transport-neutral intake: dedupe ledger, fast ack, per-item order, engine echo registry, reconnect catch-up via `requestReconcile`, 25 MiB body cap.
- **#2142** — the App webhook transport: an `ingestTransport` built around an injected `net.Listener`, with the handler, signature drift, per-transport health, refusals, coverage check and the shared `app_webhook_path` / `app_webhook_secret_env` keys.

This issue supplies a different listener to that same transport. It does not add a handler, a dedupe, or a second copy of the transport.

## Requirements

- **R1 — `event_source: tailscale`.** Starts a `tsnet.Server` with its own hostname and `Dir`, calls `ListenFunnel("tcp", ":443")` (TLS terminated on the node with Tailscale's certificate), and passes that listener to #2142's transport. All request handling is #2142's code; a test or static check pins that the two event sources construct the same transport type.
- **R2 — one node per daemon.** The daemon joins the tailnet as its own machine (e.g. `fabrik-models`). Its App webhook URL is `https://<hostname>.<tailnet>.ts.net<app_webhook_path>`, resolved from the node once it is up and logged once at startup, so the operator can paste it into the App settings.
- **R3 — config.** Flat keys, flag > env > YAML:
  ```yaml
  event_source: tailscale
  tailscale_hostname: fabrik-models           # required
  tailscale_auth_key_env: FABRIK_TS_AUTHKEY   # name of the env var; tagged, pre-approved key, needed only until state exists
  tailscale_state_dir: \"\"                     # default .fabrik/state/tailscale, created 0700
  app_webhook_path: /webhook                  # shared with direct (#2142)
  app_webhook_secret_env: FABRIK_GITHUB_WEBHOOK_SECRET   # shared with direct (#2142)
  ```
  Every new `FABRIK_*` name is classified in `internal/workerenv`; the auth key is `Scrubbed`, and so is the variable `tailscale_auth_key_env` names (resolved once, as Hookdeck's are). The auth key is never logged or written to disk by Fabrik (tsnet's own node state in `state_dir` is the only persisted credential).
- **R4 — no build tag (decision changed 2026-10-23).** `tsnet` (wireguard-go, gVisor netstack) adds about 19.5 MB to the binary, which is not worth a split build (a split build also stops a tailscale daemon from self-upgrading from release assets). The embedded node compiles into every build, release binaries included; there is no `-tags tailscale`, no default-build stub and no "built without the tag" refusal. The `tsnet` interface seam stays so tests need no tailnet. The ADR records the measured sizes and the decision.
- **R5 — refusals:** #2142's (no App auth, missing secret) plus missing `tailscale_hostname`, and a missing auth key when `state_dir` holds no node state.
- **R6 — health.** #2142's per-transport row, with this transport's state extended by tailnet connection state and Funnel status (from `tsnet`'s local client). A lost tailnet connection makes the transport unhealthy and is a warnings-panel entry, not silent. Re-establishing it calls the reconnect handler (never on first connect), so the intake's catch-up reconcile runs. Reconcile (ADR-043) stays the safety net.
- **R7 — App webhook URL check.** `CheckCoverage` additionally compares the App's configured webhook URL (`GET /app/hook/config`, App JWT) with this node's Funnel URL and its content type with `json`, warning through the panel on a mismatch. Unreadable config → skipped with one log line, never fatal.
- **R8 — clean shutdown and restart.** `Stop` closes the listener and `tsnet.Server`. A SIGHUP restart and a self-upgrade re-exec must not leave a stale node or listener and must reuse `state_dir` (no new auth key, no duplicate node). Self-upgrade works as for every other daemon. The SIGHUP restart stops the node before the exec; the self-upgrade exec does not stop it (a successful exec replaces the process and the new one reuses `state_dir`; a failed exec leaves exactly one running listener; see ADR 2143 decision 7). tsnet's own logging is routed through `logf` and quietened by default, and each routed line ends with a newline.

## Scope

In: the tsnet listener, config, refusals, health, the webhook-URL check, tests (`tsnet` behind an interface seam so tests need no tailnet), USER_GUIDE operator setup (access-policy `nodeAttrs` granting `funnel` to the tag, HTTPS certificates enabled, tagged pre-approved auth key, App webhook URL/secret/content type, and a note that a new node's public name can take minutes to resolve), `docs/llms-full.txt` regen, ADR `adrs/2143-embedded-tailscale-funnel.md`.

Out: delivery replay itself (#2144; superseded in part by ADR 2143 decision 9, which wires #2144's replayer for `event_source: tailscale`, the same as for `direct`); any change to #2142's transport beyond what the listener needs; tailnet-only (non-Funnel) exposure.

## Acceptance

- AC1: Configured, the daemon appears as its own tailnet node and logs its Funnel webhook URL; a signed delivery to it is applied (verified live once on a test bed, recorded in the PR; passed on the earlier tagged build at `ca9713df`, not yet re-run on the final build).
- AC2: `direct` and `tailscale` construct the same transport type (pinned by a test).
- AC3: Withdrawn. There is no build tag, so no build refuses `event_source: tailscale` for lack of the node.
- AC4: R5 refusals tested.
- AC5: Restart with existing `state_dir` and no auth key succeeds and reuses the node.
- AC6: `fabrik_health` shows tailnet and Funnel state; a simulated tailnet drop marks it unhealthy and its recovery triggers the reconnect handler.
- AC7: The R7 mismatch warning fires for a wrong URL or content type (stubbed App API).
- AC8: Binary size with and without the node and the decision that every build carries it are recorded in the ADR.
- AC9: Docs, regen, ADR; `go test -race ./...` green."

## Background

`event_source: direct` (#2142) lets a daemon receive GitHub App webhooks behind any reverse proxy. The Tailscale route still depends on the operator running `tailscale funnel` by hand on the host. When several daemons share a machine, they all share one Funnel hostname and are separated only by URL path.

Each daemon is meant to run with its own GitHub App, so that each daemon's security scope is narrow. That separation currently stops short of the network edge. The hostname, listener and Funnel configuration are shared. Nothing about one daemon's public exposure can be seen or revoked on its own.

This issue gives each daemon its own tailnet node and Funnel endpoint, embedded in the daemon. Per-daemon exposure becomes visible and revocable in Tailscale's own admin tooling. It is the Tailscale-specific half of the chain proposed in #2081.

It deliberately adds no new request handling. The transport-neutral intake (#2351, ADR 2351) already provides dedupe, fast acknowledgement, per-item ordering, the echo registry, reconnect catch-up and the 25 MiB body cap. #2142 provides the App webhook transport built around an injected listener, with the handler, signature-drift handling, per-transport health, refusals, coverage check and the shared `app_webhook_path` / `app_webhook_secret_env` keys. This work supplies a different listener to that same transport.

Existing conventions it must fit:
- Hookdeck is the precedent for a secret held in a named environment variable that the worker scrub must also cover (`FABRIK_HOOKDECK_*` and the variables they name).
- Operator-level faults go to the warnings panel, not a bare log line.
- The `event_source` value is validated at startup, and an unrecognised value is refused rather than silently downgraded to polling.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A daemon receives App webhooks on its own Funnel node (Priority: P1)

An operator running one daemon per GitHub App sets `event_source: tailscale` with a `tailscale_hostname` and a tagged, pre-approved auth key. The daemon joins the tailnet as its own machine, exposes a Funnel endpoint on that node, and logs the full webhook URL. The operator pastes the URL into the App settings. Signed deliveries from GitHub reach the daemon and are applied by the same transport `direct` uses.

**Why this priority**: This is the whole feature. Without it, the other stories have nothing to apply to.

**Independent Test**: With the tailscale seam stubbed, start the engine with `event_source: tailscale`. Verify the startup log carries the Funnel URL built from the node's name and the `app_webhook_path`. Verify a signed delivery sent to the stub listener is applied through the intake. Separately, verify live once on a test bed with a real tailnet and a real App delivery (recorded in the PR).

**Acceptance Scenarios**:

1. **Given** valid App auth, a webhook secret, `tailscale_hostname`, and a valid auth key, **When** the daemon starts, **Then** a node with that hostname appears in the tailnet and one log line states the Funnel webhook URL.
2. **Given** the daemon is running, **When** GitHub sends a correctly signed delivery to that URL, **Then** it is acknowledged and applied exactly as it would be under `event_source: direct`.
3. **Given** a delivery with a bad signature, **When** it reaches the endpoint, **Then** it is handled by the shared transport's existing signature handling, with no tailscale-specific behaviour.

---

### User Story 2 - Restart and upgrade reuse the node (Priority: P1)

A daemon that has joined once restarts through SIGHUP, a self-upgrade re-exec, or an ordinary stop and start. It rejoins as the same node without a fresh auth key, leaves no stale node or listener behind, and does not create a duplicate machine in the tailnet.

**Why this priority**: Daemons restart often, especially on upgrade. A restart that needs a new key, or that leaks nodes, would make the feature unusable in practice.

**Independent Test**: Start with an empty state directory and a key, stop, then start again with the state directory populated and the key variable unset. Verify startup succeeds, the same node identity is used, and the previous listener has been closed.

**Acceptance Scenarios**:

1. **Given** a state directory holding node state from an earlier run, **When** the daemon starts with no auth key, **Then** it starts successfully and reuses that node.
2. **Given** a running daemon, **When** it performs a SIGHUP restart, **Then** the listener and tailnet server are closed before the exec, and the new process reuses the state directory. **When** it performs a self-upgrade re-exec, **Then** the node is not stopped first: the exec replaces the process, the new process reuses the state directory, and a failed exec leaves exactly one running listener (ADR 2143 decision 7).
3. **Given** `Stop` is called, **When** it returns, **Then** the listener and the tailnet server are closed.

---

### User Story 3 - Misconfiguration is refused with an actionable message (Priority: P1)

An operator who configures `event_source: tailscale` incorrectly gets a clear refusal at startup instead of a daemon that silently receives nothing. This covers a missing hostname, a missing auth key when no node state exists yet, no GitHub App auth, and a missing webhook secret.

**Why this priority**: An operator who configures the new event source needs to be told exactly which setting is missing.

**Independent Test**: Drive each refusal and verify each names the missing setting.

**Acceptance Scenarios**:

1. **Given** `event_source: tailscale` with no `tailscale_hostname`, **When** the daemon starts, **Then** it is refused naming `tailscale_hostname`.
2. **Given** an empty state directory and no auth key, **When** the daemon starts, **Then** it is refused naming the auth key variable.
3. **Given** no GitHub App auth, or no webhook secret, **When** the daemon starts, **Then** it is refused with the same refusal `event_source: direct` gives.

---

### User Story 4 - Tailnet loss is visible and self-healing (Priority: P2)

If the daemon loses its tailnet connection, the operator sees it: the transport is unhealthy in `fabrik_health`, and the warnings panel shows an entry. When the connection returns, the intake's reconnect handler runs so deliveries missed during the gap are caught up by a reconcile.

**Why this priority**: The listener adds a new failure mode (tailnet or Funnel loss) that polling alone would hide. Visibility and catch-up make the transport trustworthy, but the feature delivers value without them.

**Independent Test**: With the seam stubbed, simulate a tailnet drop and verify the health row is unhealthy and a panel entry appears. Simulate recovery and verify the reconnect handler is called, and that it is not called on the first connect.

**Acceptance Scenarios**:

1. **Given** a healthy tailscale transport, **When** `fabrik_health` is read, **Then** it shows the tailnet connection state and the Funnel status alongside the per-transport row `direct` provides.
2. **Given** the tailnet connection is lost, **When** health is next evaluated, **Then** the transport is unhealthy and a warnings-panel entry exists.
3. **Given** the connection is re-established, **When** the transition is observed, **Then** the reconnect handler runs, the panel entry is cleared, and the first connect at startup did not trigger the handler.

---

### User Story 5 - A wrong App webhook URL is flagged (Priority: P2)

The operator may have pasted the wrong URL, or left the content type as form-encoded. At the coverage check, the daemon compares the App's configured webhook URL and content type with its own Funnel URL and `json`, and warns on a mismatch.

**Why this priority**: A wrong URL fails silently, because deliveries simply never arrive. Detecting it saves a long debugging session. It is advisory, so the daemon works without it.

**Independent Test**: With a stubbed App API, return a wrong URL, then a wrong content type, and verify a panel warning for each. Return a matching config and verify no warning. Make the read fail and verify one log line, no warning and no startup failure.

**Acceptance Scenarios**:

1. **Given** the App's configured URL differs from this node's Funnel URL, **When** the coverage check runs, **Then** a panel warning names both URLs.
2. **Given** the App's content type is not `json`, **When** the coverage check runs, **Then** a panel warning names the content type.
3. **Given** the App's hook config cannot be read, **When** the coverage check runs, **Then** it is skipped with one log line and the daemon continues.

---

### User Story 6 - Operators can follow the setup end to end (Priority: P2)

An operator unfamiliar with Funnel can set up a tailscale-sourced daemon from the user guide alone. The guide covers the tailnet access-policy and certificate prerequisites, creating a tagged pre-approved auth key, the daemon config, and the GitHub App webhook settings. An ADR records the binary-size trade-off and the decision that every build carries the node.

**Why this priority**: Most of the setup happens outside Fabrik. Without documentation, the feature cannot be adopted.

**Independent Test**: Review the user guide section against the live test-bed setup performed for AC1 and confirm every manual step used there is documented.

**Acceptance Scenarios**:

1. **Given** the user guide, **When** an operator follows the tailscale section, **Then** every external prerequisite is listed: the `nodeAttrs` entry granting `funnel` to the tag, HTTPS certificates enabled for the tailnet, the tagged pre-approved auth key, and the App's webhook URL, secret and `json` content type.
2. **Given** the repository after the change, **When** the doc bundle is regenerated, **Then** it matches the committed bundle.

---

### Edge Cases

- The auth key is supplied but the state directory already holds node state: the existing state is reused and the key is not required. If the variable is set it is still handed to the node, which ignores it while the node is logged in; a node whose registration expired or was revoked (`NeedsLogin`) re-authenticates with it on restart, with no need to delete the state directory.
- The state directory exists but is corrupt or belongs to a different node identity: the failure surfaces as a startup refusal or transport-unhealthy state with the underlying reason, not a silent empty listener.
- The state directory does not exist: it is created with mode 0700.
- The configured hostname is already taken in the tailnet: the node's actual name may differ from the configured one, so the logged URL must be built from the name the node reports, not the configured string.
- Funnel is not permitted for the node (missing `funnel` node attribute or HTTPS certificates disabled): the failure is reported actionably (unhealthy transport and panel entry), not as a generic listen error.
- Tailnet connection flaps repeatedly: each recovery triggers the reconnect handler, which is already coalesced by the intake.
- Two daemons on one machine: they must use distinct hostnames and distinct state directories. Sharing a state directory is not supported.
- `app_webhook_path` is customised: both the served path and the logged URL use it.
- The auth key environment variable is configured but empty: treated as missing.
- Self-upgrade exec fails and returns: the listener is still usable, or is re-established, without leaving two listeners.
- `event_source` is `direct` or `poll`: no tailnet node is started, although the node's code is in the binary.
- A new node's public Funnel name can take several minutes to resolve publicly: early deliveries can fail until it does, and the operator redelivers from the App's Advanced tab.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `event_source: tailscale` is accepted as a value alongside the existing ones. It starts an embedded tailnet node with the configured hostname and its own state directory. It obtains a public Funnel listener on port 443 with TLS terminated on the node, and hands that listener to the same App webhook transport that `event_source: direct` (#2142) uses.
- **FR-002**: All request handling for this event source (signature verification, dedupe, acknowledgement, ordering, body cap, health row) is the code of the shared transport and intake. A test or static check pins that `direct` and `tailscale` construct the same transport type.
- **FR-003**: Once the node is up, the daemon resolves its tailnet DNS name and logs, once at startup, the full App webhook URL of the form `https://<hostname>.<tailnet>.ts.net<app_webhook_path>`. The name used is the one the node reports, not the configured string.
- **FR-004**: Configuration uses flat keys resolved flag > environment > YAML: `tailscale_hostname` (required), `tailscale_auth_key_env` (the name of the environment variable holding the key; default `FABRIK_TS_AUTHKEY`), `tailscale_state_dir` (default `.fabrik/state/tailscale`, created with mode 0700). `app_webhook_path` and `app_webhook_secret_env` are the shared #2142 keys with the same defaults and meaning.
- **FR-005**: Every new `FABRIK_*` name is classified in `internal/workerenv`. The auth key variable and the variable named by `tailscale_auth_key_env` are scrubbed from worker environments, the latter resolved once at construction as Hookdeck's are. The classification test must pass with no unclassified literal.
- **FR-006**: Fabrik never logs the auth key and never writes it to disk. The embedded node's own state in `tailscale_state_dir` is the only persisted credential.
- **FR-007**: The embedded node compiles into every build; there is no build tag. Behaviour for every other `event_source` value is unchanged.
- **FR-008**: Startup refuses `event_source: tailscale` when: GitHub App auth is not configured, or the webhook secret is missing (the same refusals as `direct`); `tailscale_hostname` is empty; or the auth key is missing or empty while `tailscale_state_dir` holds no node state. Each refusal names the missing setting.
- **FR-009**: With existing node state in `tailscale_state_dir`, the daemon starts without an auth key and reuses the existing node. It does not create a second node.
- **FR-010**: The transport's health row extends the shared per-transport row with the tailnet connection state and the Funnel status, read from the embedded node's local client, and `fabrik_health` exposes both.
- **FR-011**: A lost tailnet connection makes the transport unhealthy and records a warnings-panel entry, so it is never silent. The entry is cleared on recovery.
- **FR-012**: Re-establishing a lost tailnet connection invokes the intake's reconnect handler so a catch-up reconcile runs. It is never invoked on the first connect after startup.
- **FR-013**: The coverage check additionally reads the App's webhook configuration using App credentials and compares its URL with this node's Funnel URL, and its content type with `json`. A mismatch of either produces a warnings-panel warning naming the expected and actual values.
- **FR-014**: When the App's webhook configuration cannot be read, the URL check is skipped with a single log line. It is never fatal and produces no panel warning.
- **FR-015**: `Stop` closes the listener and the embedded node. The SIGHUP restart stops the transports (so closes them) before the exec. The self-upgrade exec does not stop them: a successful exec replaces the process, and a failed exec leaves exactly one running listener (ADR 2143 decision 7). Either way no stale node or listener is left behind, and the new process reuses the state directory with no new auth key and no duplicate node.
- **FR-016**: The embedded node's own logging is routed through the engine's logging path and is quiet by default. Each routed line ends with a newline.
- **FR-017**: The embedded-node dependency is accessed behind an interface seam so unit and sim tests exercise listener acquisition, health, drop and recovery, URL resolution and shutdown without a real tailnet.
- **FR-018**: `docs/USER_GUIDE.md` gains an operator section for this event source covering: the access-policy `nodeAttrs` entry granting `funnel` to the node's tag, HTTPS certificates enabled for the tailnet, creating a tagged pre-approved auth key, the Fabrik configuration, and the App's webhook URL, secret and `json` content type. A setup step notes that a new node's public Funnel name can take several minutes to resolve, so the operator redelivers from the App's Advanced tab once it does. Flag, environment and YAML references list the new settings. `docs/llms-full.txt` is regenerated in the same change.
- **FR-019**: `adrs/2143-embedded-tailscale-funnel.md` records the decision, the measured binary size with and without the node, and the decision that every build, release binaries included, carries the node.
- **FR-020**: Tests pass under `go test -race ./...`.

### Key Entities *(if applicable)*

- **Tailnet node**: the embedded machine identity of one daemon, with its own hostname and persisted state directory. It is the unit of per-daemon exposure and revocation.
- **Funnel listener**: the public, TLS-terminated listener on the node, handed to the shared App webhook transport.
- **Node state directory**: where the embedded node persists its identity. Its presence is what makes the auth key optional on later starts.
- **Webhook URL**: `https://<node name>.<tailnet>.ts.net<app_webhook_path>`, the value the operator configures on the GitHub App.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a live test bed, a daemon joins the tailnet as its own machine, logs its webhook URL, and applies a real signed GitHub delivery sent to it. This is recorded in the PR.
- **SC-002**: A restart with a populated state directory and no auth key succeeds, and the tailnet shows exactly one node for that daemon afterwards.
- **SC-003**: Every misconfiguration listed in FR-008 is refused at startup, and each refusal message names the specific setting. Zero such cases start a daemon that receives nothing silently.
- **SC-004**: A simulated tailnet drop produces an unhealthy transport and a panel entry. Recovery clears the entry and triggers exactly one reconnect handler call. The first connect triggers none.
- **SC-005**: A wrong App webhook URL or a non-`json` content type produces a panel warning in 100% of stubbed cases. An unreadable config produces none and does not fail startup.
- **SC-006**: `direct` and `tailscale` are shown by a test to construct the same transport type, so no second copy of request handling exists.
- **SC-007**: Behaviour for every other `event_source` value is unchanged. `go test -race ./...` passes.
- **SC-008**: The ADR states the binary size with and without the node and the decision that every build carries it, and the doc bundle regeneration produces no diff.

## Assumptions

- #2142 lands first and its transport accepts an injected listener, exports the shared keys and refusals, and exposes the per-transport health row and a coverage-check hook this work can extend. The issue is blocked on it, and this spec uses its vocabulary as given.
- Funnel exposure on port 443 is the only exposure mode. Tailnet-only access is out of scope.
- The operator has already enabled HTTPS certificates for the tailnet and granted the `funnel` node attribute to the node's tag. Fabrik does not configure the tailnet policy itself and only documents it.
- The auth key is a tagged, pre-approved key. Fabrik does not mint keys or call the Tailscale control API.
- Adding the embedded-node library to the module's dependencies is acceptable, even though the project minimises dependencies, because its use is isolated to one adapter file behind the interface seam. The ADR records the module-graph effect.
- Distinct daemons on one machine use distinct hostnames and distinct state directories.
- The reconnect handler and the reconcile it triggers are the intake's existing ones (#2351). No change to them is needed.
- The default state directory is relative to the daemon's `.fabrik/` directory, consistent with other persisted state.
- Every build, release binaries included, carries the node (decision of 2026-10-23, about +19.5 MB), so a tailscale daemon self-upgrades like any other.

## Out of Scope *(optional)*

- Delivery replay and redelivery handling (#2144) as a feature of its own. The tailscale transport only wires #2144's existing replayer (ADR 2143 decision 9); it adds no replay logic.
- Any change to #2142's transport beyond what accepting this listener requires.
- Tailnet-only (non-Funnel) exposure.
- Creating or rotating the tailnet access policy, auth keys or the GitHub App's webhook settings from Fabrik.
- Support for PAT-mode webhook sources (`--webhooks`). This event source requires App auth.
- Running more than one Fabrik daemon against one node or one state directory.

## Source References *(optional)*

- #2142 — the `event_source: direct` App webhook transport and its shared `app_webhook_*` keys.
- #2351 / `adrs/2351-transport-neutral-intake.md` — the transport-neutral webhook intake.
- #2081 — the proposal this chain implements.
- #2144 — delivery replay (follow-up).
- `adrs/1142-hookdeck-ingestion-for-app-auth.md` — precedent for an App-auth ingestion transport, secret handling and worker scrub.
- Tailscale documentation for `tsnet` and Funnel (node attributes, HTTPS certificates and the tag-based auth key model).