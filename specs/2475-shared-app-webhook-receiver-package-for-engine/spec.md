# Feature Specification: Shared App webhook receiver package for engine and Pruefer

**Feature Branch**: `fabrik/issue-2475`
**Created**: 2026-11-05
**Status**: Specified
**Input**: User description: "## Problem

The GitHub App webhook receiver lives in `engine/`: `app_webhook.go` (handler, signature drift, health row), `tailnet*.go` (the tsnet node adapter, tracker, Funnel reachability and the `/app/hook/config` check) and `delivery_replay.go`. Pruefer cannot import `engine` (ADR 1113), so it cannot use the receiver, and Pruefer's only event-driven mode is Hookdeck, which is deprecated and removed in 0.0.92.

## Requirements

- Move the receiver into a shared internal package (for example `internal/appwebhook`) that both `engine` and `pruefer` can import, as `internal/githubauth` and `internal/sessionreap` already are. The package imports neither `engine` nor `pruefer`.
- What moves: the HTTP handler (path 404, method 405, size cap 413, HMAC 401, JSON 400, `ping` acknowledged), the listener factories (`direct` TCP and the tsnet Funnel node), the tailnet/Funnel health tracker, route liveness (whatever #2452 lands as), the hook-config check and the delivery replayer.
- What stays in each consumer: the delivery sink, dedupe, ordering, echo attribution, warnings reporting and health publication. These are reached through small interfaces: a sink, a read-only \"seen\" check for replay, a panel reporter and a health sink. The engine's intake, ledger and echo registry do not move.
- Behaviour unchanged. Existing tests move with the code or keep passing where they are. `TestOnlyTheAdapterFileImportsTailscale` and `TestTailscale_SharesTheDirectTransportType` keep their guarantees in the new location.
- Documentation paths in CLAUDE.md, `docs/state-machine.md` §7.8b and the ADRs that name the moved files are updated; `llms-full.txt` regenerated.

## Scope

Out: any Pruefer wiring (the next issue in this chain); any behaviour change.

## Acceptance

- `engine` builds and behaves as before on `direct` and `tailscale`; `go test -race ./...` green.
- The new package compiles with no import of `engine` or `pruefer` (pinned by a test).

Mentioned for context: #2143, #2144, #2452."

## Background

The GitHub App webhook receiver is currently part of the engine. It covers the HTTP endpoint that verifies and accepts deliveries, the two ways of listening (a plain TCP listener, and an embedded Tailscale Funnel node), the health tracking for the tailnet and Funnel, the route-liveness check against GitHub's delivery log (added by #2452), the check that GitHub's configured hook URL matches the node, and the replayer that asks GitHub to redeliver failed deliveries (#2144).

Pruefer, the review bot, runs as a separate program that is architecturally barred from importing the engine (ADR 1113). It therefore cannot use any of this. Its only event-driven mode is Hookdeck, which is deprecated and removed in 0.0.92. Pruefer needs a first-party way to receive App webhooks, and re-implementing the receiver would duplicate security-sensitive code (signature verification, size caps, Funnel-only listening) in two places.

This issue is the first step in a chain: make the receiver a shared component that both programs can import, with no change in behaviour. Pruefer wiring is a separate, later issue.

Shared leaf packages with the same reason to exist already live in the internal tree (the GitHub App auth package and the process-reap package). The new package follows that precedent.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Engine behaves exactly as before after the move (Priority: P1)

An operator running the engine with `event_source: direct` or `event_source: tailscale` upgrades to a build containing the refactor. Deliveries are received, verified, deduplicated, applied and replayed exactly as before. Health rows, warnings-panel entries and log lines are unchanged in meaning.

**Why this priority**: The refactor has no value if it regresses the engine's existing receiver; this is the safety net for everything else.

**Independent Test**: Run the full test suite with the race detector, and run the existing receiver, replay, route-liveness and Funnel tests against the relocated code. They pass without weakening any assertion.

**Acceptance Scenarios**:

1. **Given** an engine configured for `direct`, **When** a signed delivery, an unsigned delivery, an oversized body, a wrong-method request, a wrong-path request, an invalid-JSON body and a `ping` are sent, **Then** the responses are 2xx-and-submitted, 401, 413, 405, 404, 400 and acknowledged-but-not-submitted respectively, as before.
2. **Given** an engine configured for `tailscale`, **When** it starts, **Then** it listens on the public Funnel only (never on its tailnet address), reports the same health row, and runs the same hook-config check.
3. **Given** an engine that was down while GitHub recorded failed deliveries, **When** it starts, **Then** the replayer requests redelivery of the same set as before and never submits a delivery to the sink itself.
4. **Given** a delivery redelivered by GitHub, **When** it arrives, **Then** the engine's own dedupe handles it and it is applied once.

---

### User Story 2 - A second program can import the receiver (Priority: P1)

A Pruefer developer (in the following issue) wants to start the receiver from Pruefer's own program. They import the shared package, supply their own delivery sink, panel reporter, health sink and "seen" check, and get a working receiver without touching engine code.

**Why this priority**: This is the reason for the change. Without a package that is genuinely importable from both programs, the follow-up cannot happen.

**Independent Test**: A test in the shared package fails if the package, or anything it imports within this module, depends on the engine or Pruefer packages. A small test double for each consumer interface exercises the receiver end to end without any engine code.

**Acceptance Scenarios**:

1. **Given** the shared package, **When** its dependency graph is inspected by the test, **Then** it contains neither the engine nor the Pruefer package.
2. **Given** test doubles for sink, seen-check, panel reporter and health sink, **When** a signed delivery is posted to the receiver, **Then** the double sink receives it and nothing else in the consumer is required.
3. **Given** both the engine and Pruefer packages, **When** each is built, **Then** both compile with the shared package imported (Pruefer needs only the ability to compile against it in this issue, not to use it).

---

### User Story 3 - Consumer-owned concerns stay with the consumer (Priority: P2)

The engine's intake queue, delivery ledger, echo registry, dedupe, ordering, warnings reporting and health publication stay in the engine. The shared receiver reaches them only through small interfaces, so a different consumer can plug in different implementations.

**Why this priority**: The boundary is what lets Pruefer reuse the receiver without inheriting engine state. It is secondary to the move itself because it can be verified by review and by the interface tests.

**Independent Test**: Review the shared package's exported interfaces: each of sink, read-only seen-check, panel reporter and health sink is small and mentions no engine type. The replay and liveness code has a test proving it never calls the sink.

**Acceptance Scenarios**:

1. **Given** the replayer, **When** it decides whether to request a redelivery, **Then** it consults only a read-only "seen" check and never one that records a delivery.
2. **Given** route liveness running a probe, **When** the probe's redelivered copy arrives, **Then** it is never applied; the consumer's dedupe alone guarantees this, as before.

---

### Edge Cases

- A test or helper that currently reaches into receiver internals from the engine's tests must keep working, either by moving with the code or via an exported test seam in the new location. No guarantee a test pins may be dropped.
- Test seams (for example the replay, liveness and error-log seams) keep their behaviour. If they change location or name, every caller is updated and the neutralisation tests (which prove a guard can fail) still fail when the guard is removed.
- Environment variables and flags that name the webhook secret, listen address, path and Tailscale settings keep their names and their classification as daemon-only (scrubbed from worker environments).
- The Tailscale dependency must remain imported by exactly one file, wherever that file ends up, and by no other package.
- The static source scans that currently look for forbidden identifiers or required markers in the moved files (for example, "never references the sink", "never calls Submit") must follow the code, not silently lose their target.
- The static scans that currently check the engine's registry of live tests, worker-environment literal classification and warnings-panel audit must still cover the moved code.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The App webhook receiver code MUST live in a shared internal package that both the engine and Pruefer can import. The package name and location are decided at planning; `internal/appwebhook` is the working example.
- **FR-002**: The shared package MUST NOT import the engine or Pruefer packages, directly or transitively within this module. A test MUST fail if it ever does.
- **FR-003**: The HTTP handler MUST move, with its existing check order and responses unchanged: wrong path 404, wrong method 405, body over the cap 413 (before signature verification), bad HMAC 401, invalid JSON 400, verified `ping` acknowledged without being submitted, otherwise submitted to the sink.
- **FR-004**: Both listener factories MUST move: the plain TCP listener used for `direct`, and the embedded Tailscale Funnel node used for `tailscale`. The Funnel node MUST still listen on the public Funnel only.
- **FR-005**: The tailnet/Funnel health tracker MUST move, including the network-condition signalling and reconnect callback behaviour.
- **FR-006**: Route liveness (as landed by #2452), including its passive and active probing, MUST move with its behaviour and bounds unchanged.
- **FR-007**: The hook-config check (comparing GitHub's configured hook URL and content type with the node's) MUST move with its behaviour unchanged.
- **FR-008**: The delivery replayer MUST move with its behaviour, caps and follow-up pass unchanged, and MUST still never submit a delivery to the sink.
- **FR-009**: The consumer-owned concerns MUST remain in the consumer and be reached only through interfaces defined by the shared package: a delivery sink, a read-only "seen" check used by replay, a panel reporter and a health sink. The engine's intake, delivery ledger, echo registry, dedupe, ordering and attribution MUST NOT move.
- **FR-010**: The engine MUST implement those interfaces and wire the shared package into its existing `direct` and `tailscale` transports without any change visible to an operator: same flags, environment variables, defaults, health rows, warnings-panel entries, log wording and local-API output.
- **FR-011**: Existing tests MUST move with the code or keep passing where they are. No assertion may be weakened or deleted to make the move pass.
- **FR-012**: The guarantees of `TestOnlyTheAdapterFileImportsTailscale` and `TestTailscale_SharesTheDirectTransportType` MUST hold in the new location: only the Tailscale adapter file imports the Tailscale library, and `tailscale` uses the same transport type as `direct` with no extra handler, dedupe or signature code.
- **FR-013**: The other static guarantees that name the moved files (replay never submits, liveness never references the sink or submit, the TLS-handshake log handling and similar) MUST be re-pointed so they still scan the moved code and still fail if the guarantee is broken.
- **FR-014**: Documentation that names the moved files MUST be updated: the project guide (`CLAUDE.md`), the as-built state-machine doc (§7.8b and any other section naming them), and the ADRs that name the moved files (path references only; decisions are not rewritten). The bundled documentation file MUST be regenerated in the same commit as the canonical-doc edits.
- **FR-015**: The change MUST NOT add any Pruefer wiring and MUST NOT change behaviour.

### Key Entities *(if applicable)*

- **Receiver**: The component that accepts App webhook requests, verifies them and passes accepted deliveries to a sink.
- **Delivery sink**: The consumer-supplied destination for an accepted, verified delivery.
- **Seen check**: A consumer-supplied, read-only question "do you already hold this delivery?", used by replay. It never records.
- **Panel reporter**: The consumer-supplied way to raise and clear operator-level warnings.
- **Health sink**: The consumer-supplied way to publish the receiver's health row.
- **Listener factory**: The piece that provides the network listener, either plain TCP or the Tailscale Funnel node.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: The full test suite, including with the race detector, passes after the change, with no test removed or assertion weakened.
- **SC-002**: A test that checks the shared package's dependencies fails if the engine or Pruefer package is ever imported (demonstrated by a deliberate-violation check, so the test is known to be able to fail).
- **SC-003**: With the same configuration and the same inputs, an engine on `direct` and an engine on `tailscale` produce the same responses, health rows, panel entries and operator-visible log lines before and after the change.
- **SC-004**: Exactly one source file in the whole module imports the Tailscale library, and it is in the shared package.
- **SC-005**: No documentation in the repository refers to a moved file by its old path, and the regeneration check for the bundled documentation passes.
- **SC-006**: The Pruefer program compiles with the shared package importable, and a consumer written only against the shared package's interfaces (a test double) can run the receiver end to end.

## Assumptions

- #2452 has landed (route liveness exists in the engine today), so "whatever #2452 lands as" means the current route-liveness code, which moves as it stands.
- The package name and exact file layout are a planning decision; the working name is only an example from the request.
- Splitting into several packages is allowed if planning finds it cleaner, provided none imports the engine or Pruefer and the Tailscale library stays confined to one file.
- Where the moved code currently reads engine configuration or engine types directly, the consumer-supplied interfaces and plain configuration values replace those reads without changing behaviour. The exact interface shapes are a planning decision.
- ADR edits are limited to updating file and symbol path references; the recorded decisions and rationale are left as written.
- Test seams used only by tests may be exported from the shared package where cross-package tests need them, as other internal packages already do.

## Out of Scope *(optional)*

- Any Pruefer wiring, configuration, flags or use of the receiver (the next issue in this chain).
- Any behaviour change, including defaults, thresholds, bounds, log wording or new health fields.
- Removing Hookdeck (handled separately; it is already deprecated).
- Moving the engine's intake, delivery ledger, echo registry, dedupe, ordering, echo attribution or health publication.

## Source References *(optional)*

- ADR 1113 (Pruefer cannot import the engine)
- ADR 2142 (direct App webhook transport), ADR 2143 (embedded Tailscale Funnel), ADR 2144 (delivery replay), ADR 2452 (route liveness from the delivery log)
- `docs/state-machine.md` §7.8b
- Precedent shared packages: `internal/githubauth`, `internal/sessionreap`
