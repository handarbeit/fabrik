# Feature Specification: Remove Hookdeck from the engine and Pruefer

**Feature Branch**: `fabrik/issue-2477`
**Created**: 2026-11-07
**Status**: Specified
**Input**: User description: "## Problem

Hookdeck (`event_source: hookdeck`, ADR 1142 / ADR 1254, both engine and Pruefer) is deprecated in 0.0.90 and to be removed in 0.0.92. Nothing in use depends on it. The dev daemon's session had been refused with 401 for at least a day before it was switched off (#2474). The replacement is `event_source: tailscale` / `direct`, which reaches Pruefer through the previous issue in this chain.

## Requirements

- Engine:
  - `event_source: hookdeck` is refused at startup. The message names `tailscale` and `direct` and the USER_GUIDE section.
  - A configured `hookdeck_*` key or `FABRIK_HOOKDECK_*` variable is logged once as ignored. It never fails startup.
  - Remove `hookdeckManager`, its health row and its coverage check; `checkAppInstallationCoverage` stays.
  - `events.SigDrift` stays in `internal/events`; it is shared with the App receiver.
- Pruefer: the same refusal for `event_source: hookdeck` and the same once-only notice for leftover `hookdeck.*` keys.
- Delete `internal/events/hookdeck` once neither consumer imports it.
- **Secret scrub kept:** an operator's leftover `HOOKDECK_API_KEY` (or the variable a `hookdeck_api_key_env` names) in `.env` must still never reach a worker environment. Keep those names in `workerenv.Scrubbed` for at least this release, with a reason, rather than dropping them with the code.
- Tests: remove Hookdeck-only tests. Update the intake sim twin (`tests/sim/webhook_intake_test.go`), `tests/e2e/bedevents` and `tests/gate` config tests where they name Hookdeck. The registry stays complete.
- Docs:
  - remove the Hookdeck sections from USER_GUIDE, README, `cmd/pruefer/README.md`, CLAUDE.md, `docs/state-machine.md` and `docs/stage-lifecycle.md`;
  - regenerate `llms-full.txt`;
  - mark the Hookdeck ADRs superseded (a status line naming this issue's ADR), without deleting them;
  - write an ADR `adrs/<this issue>-remove-hookdeck.md`.
- Release notes call it out as a breaking change, with the migration path.

## Before merge

- The other local Fabrik directories still configured for Hookdeck (`fabrik-ideas`, `fabrik-release`) are switched off or confirmed not running. A daemon on `--auto-upgrade` would otherwise re-exec into the startup refusal.

## Acceptance

- `event_source: hookdeck` refuses with the migration message, in both engine and Pruefer (tested).
- A worker environment built with `HOOKDECK_API_KEY` set does not contain it (tested).
- No `internal/events/hookdeck` import remains; `go test -race ./...` green; docs and regen; ADR."

## Background

Hookdeck is the relay-based way to get near-real-time webhook delivery under GitHub App auth, in both the Fabrik engine (ADR 1142, ADR 1254, ADR 1563) and Pruefer. It was deprecated in v0.0.90 (documentation notice, #2478) and is scheduled for removal in v0.0.92. Nothing in use depends on it: the dev daemon's Hookdeck session had been refused with 401 for at least a day before it was switched off (#2474).

Two relay-free replacements exist: `event_source: direct` and `event_source: tailscale`, the latter an embedded Tailscale Funnel receiver. Pruefer gets them through the previous issue in this chain (#2476), which this issue depends on. Keeping Hookdeck costs a second ingestion transport, its health row and coverage check, a client library and a large test surface, for a path no one uses.

The removal is a breaking change for any operator still configured for Hookdeck, so the failure mode matters: a stale setting must fail loudly with a pointer to the migration, while merely leftover keys must not break startup. A leftover Hookdeck API key in a `.env` file must also never leak into a worker's environment once the code that knew about it is gone. Shared pieces stay: `events.SigDrift` (signature-drift tracking, also used by the App receiver) and `checkAppInstallationCoverage` (the generalised App coverage check).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An engine operator still on Hookdeck gets a clear refusal (Priority: P1)

An operator upgrades Fabrik to v0.0.92 with `event_source: hookdeck` still configured (flag, environment variable or `config.yaml`).

**Why this priority**: This is the breaking change itself; the migration message is the only help an affected operator gets at the moment of failure.

**Independent Test**: Start the engine with `event_source: hookdeck` through each configuration route and confirm startup is refused with a message naming `tailscale`, `direct` and the User Guide section.

**Acceptance Scenarios**:

1. **Given** `event_source: hookdeck` is set, **When** the engine starts, **Then** startup is refused before any polling begins, and the message names `tailscale` and `direct` and points to the User Guide section on event sources.
2. **Given** `event_source` is `direct`, `tailscale` or unset (poll), **When** the engine starts, **Then** behaviour is unchanged.

---

### User Story 2 - Leftover Hookdeck settings never break startup (Priority: P1)

An operator has already moved to `tailscale`, `direct` or poll, but their config still holds `hookdeck_*` keys or `FABRIK_HOOKDECK_*` variables.

**Why this priority**: Failing startup over dead settings would turn a clean migration into an outage.

**Independent Test**: Start the engine and Pruefer with leftover Hookdeck keys and variables and a non-Hookdeck `event_source`; confirm they start and the leftovers are reported once.

**Acceptance Scenarios**:

1. **Given** a `hookdeck_*` key or `FABRIK_HOOKDECK_*` variable is present and `event_source` is not `hookdeck`, **When** the engine starts, **Then** it starts normally and logs once that the setting is ignored.
2. **Given** leftover `hookdeck.*` keys in Pruefer's configuration and a non-Hookdeck `event_source`, **When** Pruefer starts, **Then** it starts normally and logs the same once-only notice.

---

### User Story 3 - Pruefer operator on Hookdeck gets the same refusal (Priority: P1)

An operator running Pruefer with `event_source: hookdeck` upgrades to v0.0.92.

**Why this priority**: Pruefer carries its own Hookdeck path and must refuse it the same way as the engine.

**Independent Test**: Start Pruefer with `event_source: hookdeck` via config file, flag and environment variable; confirm it refuses with the migration message.

**Acceptance Scenarios**:

1. **Given** Pruefer is configured with `event_source: hookdeck`, **When** it starts, **Then** startup is refused with a message naming the supported replacements and the relevant documentation section.
2. **Given** Pruefer is configured for poll or for a replacement receiver delivered by #2476, **When** it starts, **Then** behaviour is unchanged.

---

### User Story 4 - A leftover Hookdeck secret never reaches a worker (Priority: P1)

An operator's `.env` still contains `HOOKDECK_API_KEY`, or a variable whose name was set by `hookdeck_api_key_env`. The `.env` is loaded into the daemon's process environment.

**Why this priority**: Removing the Hookdeck code must not silently remove the protection that keeps a credential out of the Claude subprocess environment.

**Independent Test**: Build a worker environment with `HOOKDECK_API_KEY` set (and, separately, with a custom-named key variable declared by a leftover `hookdeck_api_key_env`/`FABRIK_HOOKDECK_API_KEY_ENV`) and confirm neither value is present.

**Acceptance Scenarios**:

1. **Given** `HOOKDECK_API_KEY` is set in the daemon's environment, **When** a worker environment is built, **Then** the variable is absent.
2. **Given** a leftover setting names a custom API-key variable, **When** a worker environment is built, **Then** that variable is absent too.
3. **Given** the scrub list, **When** an operator or maintainer reads it, **Then** each retained Hookdeck name has a recorded reason (leftover-secret protection, kept for at least this release).

---

### User Story 5 - Maintainers: no Hookdeck code, tests or documentation remain, history preserved (Priority: P2)

A contributor searches the repository after the change.

**Why this priority**: The cleanup is the purpose of the issue, but it carries no user-visible behaviour beyond Stories 1-4.

**Independent Test**: Search the module for imports of the Hookdeck package and for Hookdeck sections in the listed documents; run the full test suite with the race detector.

**Acceptance Scenarios**:

1. **Given** the change is merged, **When** the module is searched, **Then** nothing imports the Hookdeck events package and the package is gone, while `events.SigDrift` and the generalised App coverage check remain and still serve the App receivers.
2. **Given** the Hookdeck ADRs, **When** they are opened, **Then** each is still present and carries a status line naming this issue's ADR as superseding it.
3. **Given** the documentation bundle is regenerated, **When** the docs-drift check runs, **Then** it passes.

---

### Edge Cases

- `event_source: hookdeck` together with `direct_listen`/Tailscale settings: the refusal still applies (Hookdeck is not a valid value regardless of other settings).
- A leftover Hookdeck key appears in more than one place (YAML and environment): it is reported once per startup, not once per source.
- A leftover custom key-variable name (`hookdeck_api_key_env`) points at a variable that is unset: nothing to scrub, no error.
- `FABRIK_GITHUB_WEBHOOK_SECRET`, the default webhook-secret variable, is shared with the `direct` receiver: it stays scrubbed and its handling is unchanged.
- A daemon on `--auto-upgrade` that is still configured for Hookdeck re-execs into the startup refusal after upgrading (see the pre-merge condition below).
- Historical release notes (v0.0.83 and later), existing `specs/` entries and design audit documents that mention Hookdeck are history and are not rewritten.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The engine refuses to start when `event_source` is `hookdeck`, whether set by flag, environment variable or configuration file. The message names `tailscale` and `direct` as replacements and the User Guide section describing them.
- **FR-002**: A configured `hookdeck_*` key or `FABRIK_HOOKDECK_*` variable is reported once at engine startup as ignored, and never causes startup to fail.
- **FR-003**: The engine's Hookdeck transport, its health row and its Hookdeck-specific coverage check are removed. The generalised App-installation coverage check and the shared signature-drift tracker (`events.SigDrift`) remain and keep serving the `direct` and `tailscale` receivers unchanged.
- **FR-004**: Pruefer refuses to start when `event_source` is `hookdeck`, by any configuration route, with a message naming the supported replacements and the relevant documentation section.
- **FR-005**: Pruefer reports leftover `hookdeck.*` configuration once at startup as ignored, and never fails startup because of it.
- **FR-006**: The Hookdeck events package is deleted, and no code in the module imports it.
- **FR-007**: A worker environment never contains `HOOKDECK_API_KEY`, the variables `FABRIK_HOOKDECK_API_KEY_ENV` and `FABRIK_HOOKDECK_WEBHOOK_SECRET_ENV` themselves, or the variable that a leftover `hookdeck_api_key_env`/`hookdeck_webhook_secret_env` (or the corresponding environment variable) names. These names stay in the worker-environment scrub classification, each with a recorded reason, for at least this release.
- **FR-008**: Tests that exist only for Hookdeck are removed. The intake simulation twin, the e2e bed-events helpers and the gate configuration tests no longer name Hookdeck as a transport, and the live-test registry remains complete and consistent.
- **FR-009**: The Hookdeck sections are removed from the User Guide, README, Pruefer README, CLAUDE.md, `docs/state-machine.md` and `docs/stage-lifecycle.md`; where those documents describe event sources, the migration path to `tailscale`/`direct` is stated. The documentation bundle (`docs/llms-full.txt`) is regenerated in the same change.
- **FR-010**: Every Hookdeck ADR is kept and marked superseded by a status line naming this issue's ADR; none is deleted.
- **FR-011**: An ADR `adrs/2477-remove-hookdeck.md` records the decision, including the refusal and ignore-with-notice behaviour, the retained scrub names and their sunset condition, and the pieces deliberately kept.
- **FR-012**: The v0.0.92 release notes list the removal as a breaking change with the migration path (choose `tailscale` or `direct`, repoint the GitHub App webhook URL and secret, remove Hookdeck settings; for Pruefer, the replacement from #2476 or poll).
- **FR-013**: Before this change merges, the other local Fabrik directories still configured for Hookdeck (`fabrik-ideas`, `fabrik-release`) are switched off or confirmed not running, so no `--auto-upgrade` daemon re-execs into the refusal. (An operational precondition, not a code change.)

### Key Entities

- **Hookdeck configuration**: `event_source: hookdeck`, `hookdeck_*` keys, `FABRIK_HOOKDECK_*` variables (engine), and `hookdeck.*` keys (Pruefer). After this change the value is refused and the keys are ignored with a notice.
- **Worker-environment scrub list**: the audited classification of daemon-only variables that must never reach a Claude worker; keeps the Hookdeck names as leftover-secret protection.
- **Hookdeck ADRs**: historical decision records, retained and marked superseded.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Starting the engine or Pruefer with `event_source: hookdeck` always ends in a refusal whose message contains the names `tailscale` and `direct` and a documentation pointer (covered by tests for both programs).
- **SC-002**: Starting either program with leftover Hookdeck keys and a non-Hookdeck `event_source` succeeds and emits exactly one ignored-settings notice.
- **SC-003**: A worker environment built with `HOOKDECK_API_KEY` set contains no such variable (covered by a test), and the same holds for a custom-named key variable declared by a leftover setting.
- **SC-004**: The module contains no import of the Hookdeck events package, and the full test suite passes with the race detector.
- **SC-005**: The documentation-drift check passes, and none of the listed documents describes Hookdeck as a usable event source.
- **SC-006**: No Hookdeck ADR is deleted, and each carries a superseded status line pointing to the new ADR.

## Assumptions

- This issue depends on #2476 (Pruefer's replacement receivers); it is blocked until that issue closes, and Pruefer's refusal message may name those replacements only because #2476 has landed.
- "Logged once" means a single startup notice line listing the ignored settings, not a recurring warning or a warnings-panel entry that needs operator action; a stale unused key neither blocks nor degrades work.
- The "ignore leftover keys" behaviour applies for as long as the keys are recognised at all; deciding when the notice itself is retired is out of scope and recorded in the ADR.
- The retained scrub names stay for at least this release (v0.0.92); removal in a later release is a separate decision recorded in the ADR.
- `FABRIK_GITHUB_WEBHOOK_SECRET` and the other variables shared with `direct`/`tailscale` are not Hookdeck-specific and are unaffected.
- Historical documents (old release notes, existing `specs/`, the design audit) are not edited.
- The release-notes text is produced at release-cutting time; this issue supplies the content (migration path) in the ADR and PR so it can be carried over.

## Out of Scope *(optional)*

- Building Pruefer's replacement receivers (#2476).
- Changing the `direct` or `tailscale` receivers, or `events.SigDrift` / `checkAppInstallationCoverage` behaviour.
- A runtime deprecation warning in earlier versions (v0.0.90 shipped a documentation-only notice, #2478).
- Rewriting historical release notes, specs or audit documents.
- Retiring the leftover-key notice or the retained scrub names (a later release).

## Source References *(optional)*

- ADR 1142, ADR 1254, ADR 1563 (Hookdeck ingestion decisions, to be marked superseded)
- #2474 (dev daemon Hookdeck session refused with 401), #2476 (Pruefer replacement transports), #2478 (documentation deprecation notice)
