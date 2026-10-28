# Feature Specification: Deprecate Hookdeck in the documentation (removed in 0.0.92)

**Feature Branch**: `fabrik/issue-2478`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: "## Problem

`event_source: hookdeck` is not in use. The dev daemon's session had been refused with 401 for at least a day before #2474 switched it off. The App receivers `tailscale` and `direct` replace it, and it is removed in 0.0.92 (#2477; Pruefer's replacement is #2476). Operators need notice in the release that ships the replacement.

## Requirements

- In USER_GUIDE, README and `cmd/pruefer/README.md`: mark Hookdeck deprecated in v0.0.90 and removed in v0.0.92, name the replacement (`tailscale`/`direct`; for Pruefer, poll until v0.0.92) and give the migration steps. Regenerate `llms-full.txt`.
- Point the README's App-auth note at the Tailscale receiver instead of Hookdeck.
- Docs only: no behaviour change and no startup notice."

## Background

Hookdeck (`event_source: hookdeck`) is the relay-based way to get near-real-time webhook delivery under GitHub App auth. It is not in use: the dev daemon's Hookdeck session was refused with 401 for at least a day before #2474 switched it off. Release v0.0.90 ships two relay-free replacements, the `direct` receiver and the embedded `tailscale` Funnel receiver. Hookdeck support is to be removed in v0.0.92 (#2477 for Fabrik; #2476 for Pruefer, which has no replacement transport and polls until then).

Operators who still configure Hookdeck must hear about this in the same release that ships the replacements, so they have a full release cycle to move before removal. This issue covers that notice, in documentation only. The removal itself, Pruefer's replacement, and any runtime warning are separate work.

Today the docs still present Hookdeck as a first-class option: a dedicated Fabrik User Guide section, the `--event-source` description and config-file comments, the README's App-auth note (which lists Hookdeck first), and a dedicated "Event-driven mode (Hookdeck)" section plus several option tables in the Pruefer README. The v0.0.90 highlights paragraph in the README says nothing changes "for poll-only or Hookdeck setups", which needs to carry the deprecation too.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A Fabrik operator on Hookdeck learns it is going away and how to move (Priority: P1)

An operator running Fabrik with `event_source: hookdeck` reads the release notes or docs for v0.0.90. They find a clear statement that Hookdeck is deprecated in v0.0.90 and removed in v0.0.92, which replacement to use, and the steps to migrate.

**Why this priority**: This is the point of the issue. Without it, operators discover the removal only when v0.0.92 breaks their setup.

**Independent Test**: Read the Fabrik User Guide's Hookdeck section and the `--event-source` description. Both state the deprecation and removal versions, name `tailscale` and `direct` as replacements, and link to migration steps that an operator could follow to end up on a receiver without Hookdeck.

**Acceptance Scenarios**:

1. **Given** an operator opens the Hookdeck section of the User Guide, **When** they read its opening, **Then** they see that Hookdeck is deprecated in v0.0.90 and removed in v0.0.92 and which `event_source` values replace it.
2. **Given** an operator wants to move off Hookdeck, **When** they follow the migration steps, **Then** the steps cover choosing `tailscale` or `direct`, pointing the GitHub App's webhook URL at the new receiver, setting the App's webhook secret for the new receiver, and removing the Hookdeck settings.
3. **Given** an operator reads the config-file reference or the flag table, **When** they reach `event_source`/`--event-source` and the `hookdeck_*` keys, **Then** those entries are marked deprecated with the removal version.

---

### User Story 2 - A new reader of the README is steered to the Tailscale receiver (Priority: P1)

A reader of the README sets up near-real-time delivery under App auth. The App-auth note recommends the Tailscale receiver (and `direct`), not Hookdeck.

**Why this priority**: The README is the first place new operators look. Pointing it at a deprecated transport would send new users onto a path with a one-release lifetime.

**Independent Test**: Read the README's App-auth webhook note and the v0.0.90 highlights paragraph. The note leads with `tailscale` (then `direct`) and mentions Hookdeck only as deprecated; the highlights paragraph tells Hookdeck users it is deprecated and removed in v0.0.92.

**Acceptance Scenarios**:

1. **Given** a reader views the README's App-auth note, **When** they look for how to get near-real-time delivery, **Then** the Tailscale receiver is the primary pointer and Hookdeck is not offered as a current option.
2. **Given** a Hookdeck user reads the v0.0.90 highlights, **When** they reach the upgrade guidance, **Then** it no longer implies Hookdeck is unaffected; it says deprecated in v0.0.90 and removed in v0.0.92.

---

### User Story 3 - A Pruefer operator learns Hookdeck goes away and polling is the path (Priority: P2)

A Pruefer operator using `event_source: hookdeck` reads the Pruefer README and learns Hookdeck is deprecated in v0.0.90 and removed in v0.0.92, and that until Pruefer's replacement lands they should poll.

**Why this priority**: Pruefer's replacement (#2476) is not available in v0.0.90, so the migration is different from Fabrik's, but operators need the notice in the same release.

**Independent Test**: Read the Pruefer README's Hookdeck section, `event_source` option rows and the webhook-related setup text. Each states the deprecation and removal versions and that the migration is to poll (`event_source: poll`).

**Acceptance Scenarios**:

1. **Given** a Pruefer operator opens "Event-driven mode (Hookdeck)", **When** they read its opening, **Then** they see the deprecation, the removal version, and that polling is the replacement until v0.0.92.
2. **Given** an operator wants to move off Hookdeck, **When** they follow the Pruefer migration steps, **Then** the steps cover switching `event_source` to `poll`, removing the `hookdeck.*` settings and unused credentials, and adjusting the GitHub App's webhook (inactive, per the poll-only guidance already in the README).
3. **Given** the Pruefer README's option and flag tables, **When** they list `event_source: hookdeck` and the `hookdeck.*` keys, **Then** those entries are marked deprecated.

---

### User Story 4 - Doc-consuming tooling and CI stay consistent (Priority: P2)

The generated documentation bundle matches the edited canonical pages, so the docs-drift check passes and LLM-oriented consumers read the same deprecation notice.

**Why this priority**: The bundle is a derived artifact checked in CI; a stale bundle fails the PR.

**Independent Test**: Regenerate the bundle and confirm there is no diff against the committed copy.

**Acceptance Scenarios**:

1. **Given** the User Guide has been edited, **When** the bundle is regenerated, **Then** the committed bundle contains the new deprecation text and the docs-drift check passes.

---

### Edge Cases

- The Pruefer README mentions Hookdeck in many places (provisioning, drift detection, option tables, setup steps for the GitHub App's webhook, an ADR reference). The deprecation must be visible at the top of the Hookdeck material and on the option entries; historical ADR links stay as they are.
- A reader who lands deep in the docs (a flag table row, a config comment) without seeing the section opening must still see a "deprecated, removed in v0.0.92" marker there.
- Wording must not imply Hookdeck is already removed or already warns at runtime: in v0.0.90 it works unchanged.
- Version wording must use "v0.0.90" and "v0.0.92" consistently with the surrounding highlights paragraphs.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The Fabrik User Guide's Hookdeck section states, at its opening, that `event_source: hookdeck` is deprecated in v0.0.90 and removed in v0.0.92, and names `tailscale` and `direct` as the replacements.
- **FR-002**: The Fabrik User Guide gives migration steps from Hookdeck to a replacement receiver: choose `tailscale` or `direct` (linking to their sections), point the GitHub App's webhook URL at the new receiver, make the App's webhook secret available to the new receiver, and remove the `hookdeck_*` settings and the Hookdeck account dependency.
- **FR-003**: Every other place in the User Guide that lists or describes Hookdeck as an option (the `event_source` configuration reference and comments, the `hookdeck_*` keys, flag/environment tables, and cross-references from the Direct and Tailscale sections) carries a short deprecation marker naming v0.0.92.
- **FR-004**: The README marks Hookdeck deprecated in v0.0.90 and removed in v0.0.92 wherever it describes it (the `--event-source` table row and the `--hookdeck-*` flag mention), and names the replacement.
- **FR-005**: The README's GitHub App auth note under Webhook Mode points at the Tailscale receiver (and the `direct` receiver) as the ways to get near-real-time delivery, and no longer lists Hookdeck as a current option; if Hookdeck is mentioned there at all, it is marked deprecated.
- **FR-006**: The README's v0.0.90 highlights paragraph no longer states that nothing changes for Hookdeck setups without qualification; it states that Hookdeck is deprecated in v0.0.90 and removed in v0.0.92 and points to the migration steps.
- **FR-007**: `cmd/pruefer/README.md` marks Pruefer's Hookdeck event-driven mode deprecated in v0.0.90 and removed in v0.0.92, at the opening of its Hookdeck section, and states that the replacement is polling until v0.0.92 (Pruefer's replacement transport is tracked separately).
- **FR-008**: `cmd/pruefer/README.md` gives migration steps for Pruefer: set `event_source: poll`, remove the `hookdeck.*` settings and the Hookdeck credentials, and handle the GitHub App's webhook settings consistently with the README's existing poll-only guidance.
- **FR-009**: `cmd/pruefer/README.md` marks the `event_source: hookdeck` and `hookdeck.*` entries in its option, flag and restart-behaviour tables, and the Hookdeck-related webhook setup notes, as deprecated, so the notice is visible without reading the main section.
- **FR-010**: The generated documentation bundle (`docs/llms-full.txt`) is regenerated from the edited canonical pages in the same change, and the docs-drift check passes.
- **FR-011**: The change is documentation only: no Go source, configuration default, flag, runtime behaviour, startup log line or warnings-panel entry is added or altered.

### Key Entities *(if applicable)*

- **Deprecation notice**: the statement "deprecated in v0.0.90, removed in v0.0.92" plus the named replacement; it appears consistently across the three documents.
- **Replacement path**: for Fabrik, the `tailscale` or `direct` receiver under App auth; for Pruefer, polling until its own replacement ships in v0.0.92.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In each of the User Guide, README and Pruefer README, every section, table row or note that presents Hookdeck as an option carries the "deprecated in v0.0.90, removed in v0.0.92" notice or sits directly beneath a section opening that does.
- **SC-002**: An operator can complete a migration off Hookdeck by following only the documented steps (Fabrik: to `tailscale` or `direct`; Pruefer: to `poll`), without needing to read source code or issues.
- **SC-003**: No document recommends Hookdeck for new setups; the README's App-auth note leads with the Tailscale receiver.
- **SC-004**: Regenerating `docs/llms-full.txt` produces no diff against the committed bundle.
- **SC-005**: The diff contains no Go source, YAML default, script, or workflow changes.

## Assumptions

- "Deprecated in v0.0.90" matches the release being cut now (v0.0.89 is the latest release; the README already carries v0.0.90 highlights), so the notice ships with the replacements.
- Hookdeck continues to work unchanged in v0.0.90 and v0.0.91; the notice is informational only (no startup notice, per the issue).
- Historical records (ADRs such as the Hookdeck ingestion ADRs, release notes of earlier versions, the intake description of Hookdeck as a transport, `docs/state-machine.md`) are not rewritten; only the three operator-facing documents named in the issue change. If an as-built description in the User Guide's delivery-intake section must keep naming Hookdeck, it stays accurate for v0.0.90.
- For Pruefer, the migration target is plain polling because its replacement (#2476) lands with v0.0.92; the notice mentions that replacement only as tracked separately, without promising its design.
- The migration steps describe configuration actions at the operator level and link to the existing `tailscale`/`direct` sections for detail rather than duplicating them.

## Out of Scope *(optional)*

- Removing Hookdeck code, flags or config keys (#2477) and Pruefer's replacement transport (#2476).
- Any runtime deprecation warning, startup notice or warnings-panel entry for Hookdeck users.
- Editing ADRs, `docs/state-machine.md`, `docs/stage-lifecycle.md` or earlier release highlights.
- Changing the behaviour or docs of the `direct` and `tailscale` receivers beyond cross-reference wording.
- Release-notes text for v0.0.90 (produced by the release process).

## Source References *(optional)*

- #2474 (dev daemon switched off Hookdeck), #2477 (Hookdeck removal in 0.0.92), #2476 (Pruefer replacement)
- `docs/USER_GUIDE.md`: "Event-Driven Ingestion via Hookdeck (App Auth)", "Direct App Webhook Receiver", "Embedded Tailscale Funnel Receiver"
- `README.md`: Webhook Mode "GitHub App auth" note, v0.0.90 highlights, `--event-source` flag table
- `cmd/pruefer/README.md`: "Event-driven mode (Hookdeck)" and the option/flag tables
