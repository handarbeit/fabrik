# Feature Specification: Update docs for v0.0.90

**Feature Branch**: `fabrik/issue-2493`
**Created**: 2026-10-26
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.90.

See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.90

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)"

## Background

Every release is followed by a documentation pass so that operators reading the user guide, the README and the marketing page see the behaviour of the version they are running. v0.0.90 has one theme, **webhooks without a relay**:

1. **Receive App webhooks in the daemon.** Under GitHub App auth, `event_source: direct` is a plain HTTP receiver placed behind the operator's own tunnel or proxy (#2142). `event_source: tailscale` embeds a Tailscale node (via `tsnet`) so each daemon joins the tailnet as its own machine and serves its own Funnel endpoint, with per-daemon exposure visible and revocable (#2143). Both share the delivery intake's persisted dedupe and ordering.
2. **Nothing lost while the daemon is down.** On start and on every reconnect, the direct and Tailscale receivers find deliveries GitHub failed to deliver in the App's delivery log and ask GitHub to redeliver them; duplicates are dropped by delivery ID. Controlled by `replay_failed_deliveries` (default on) (#2144).
3. **Health you can trust.** `fabrik_health.webhook.transports[].route` reports `reachable`, `unreachable` (with GitHub's reason) or `unknown`, judged from GitHub's own delivery log and, when nothing recent decides it, a test redelivery that the dedupe never applies. An unreachable route held for 10 minutes raises the `webhook_route_unreachable` warning (#2452, superseding #2425). `fabrik_health.webhook.intake` counts accepted, duplicate, dropped, echo-matched and applied deliveries (#2427).

Smaller items: a review-thread resolution delivered by webhook now also drops thread comments that arrived by webhook before a deep fetch gave them a thread ID (#2444); channel label announcements around a SIGHUP restart are sent exactly once and keep their attribution (#2450); tailscale.com 1.104.2 means building from source needs Go 1.27.3 (#2472); every build, release binaries included, now carries the Tailscale node, so the binary is about 35 MB. The live e2e gate gained an events axis (poll and webhook cells) and eleven live webhook coverage tests; that is internal and not user documentation.

Two known issues are listed in the release notes: with webhooks and the merge train both on, a delivery for the train's integration PR can briefly overwrite one member's cached linked PR, with landings unaffected (#2483); and a label delivery accepted just before a SIGHUP restart may not be applied until the next reconcile and is then announced with `actor=unknown` (#2484).

**Current state of the docs** (checked against the repository before writing this spec). The feature PRs already put the bulk of the new behaviour in `docs/USER_GUIDE.md`: the "Direct App Webhook Receiver" and "Embedded Tailscale Funnel Receiver" sections (setup, config forms, failure visibility, route liveness, `webhook_route_unreachable`, delivery replay and `replay_failed_deliveries`), the command-line flag table rows for `--event-source` (`poll`/`hookdeck`/`direct`/`tailscale`), `--direct-listen`, `--app-webhook-path`, `--tailscale-hostname` and `--replay-failed-deliveries`, the environment-variable table, and the Go 1.27.3 requirement. The remaining gaps are:

- **README.md** ends its "highlights" series at v0.0.89. Its `--event-source` flag row still lists only `poll` or `hookdeck`, its Webhook Mode text says App-auth setups can get near-real-time delivery only through Hookdeck, and the new `direct`/`tailscale` flags are absent.
- **docs/index.md** carries a "New in v0.0.89" note, and its Webhook Mode feature card describes only the `gh webhook forward` stream.
- **docs/USER_GUIDE.md** has no description of `fabrik_health.webhook.intake` (the delivery counters), and the `fabrik_health` description of `webhook.transports` omits the `route` and `replay` objects it now carries (they are explained only inside the transport sections). Section 10 "Webhook Mode" and the Hookdeck section do not point readers to the two new App-auth receivers. The #2483 and #2484 known issues are not mentioned anywhere in the guide.
- **docs/llms-full.txt** must be regenerated after any canonical-doc edit.

Precedent: the previous docs pass (#2419, "Documentation updated for v0.0.89") added a README "vX highlights" paragraph, updated the `docs/index.md` "New in" note, extended the user guide, and regenerated the llms bundle. This issue follows the same shape and is mostly gap-fill and release-surfacing, not a rewrite.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An App-auth operator discovers the relay-free webhook options (Priority: P1)

An operator running Fabrik under GitHub App auth reads the README to decide how to get near-real-time events. Today it tells them Hookdeck is the only route. They need to learn that the daemon can receive App webhooks itself, which of the two receivers fits their network, and what to set.

**Why this priority**: This is the headline of the release and the README is the first surface operators read; it currently points them at a more complex option.

**Independent Test**: Read only the README. Confirm that the v0.0.90 highlights, the Webhook Mode text and the `--event-source` flag row name `direct` and `tailscale`, and link to the user-guide sections for setup.

**Acceptance Scenarios**:

1. **Given** the README, **When** an operator looks for the latest release, **Then** a v0.0.90 highlights entry follows the v0.0.89 entry in the same style and links to the user-guide sections for the two receivers, delivery replay and route health.
2. **Given** an App-auth operator reading the README's Webhook Mode text, **When** they look for a way to get event delivery without `gh webhook forward`, **Then** they are told about `event_source: direct` and `event_source: tailscale` as well as Hookdeck, with a one-line difference between them.
3. **Given** the README's command-line flag table, **When** an operator reads the `--event-source` row, **Then** it lists `poll`, `hookdeck`, `direct` and `tailscale`, notes that all but `poll` need App auth, and the receiver flags (`--direct-listen`, `--tailscale-hostname`, `--replay-failed-deliveries`) are findable from it or listed beside it.

---

### User Story 2 - An operator upgrading to v0.0.90 knows what changes for them (Priority: P1)

An operator running v0.0.89 reads the highlights before upgrading. They need to know what is opt-in, what is automatic, and what changes unconditionally.

**Why this priority**: Nothing in v0.0.90 changes behaviour for an existing poll-only or Hookdeck setup, and the docs should say that plainly. The one unconditional change (binary size and the Go version needed to build from source) should not surprise anyone.

**Independent Test**: Read the README v0.0.90 highlights and the linked sections. Confirm that they state the new sources are opt-in and need App auth, that replay is on by default for the two new receivers only, that the binary is about 35 MB because every build carries the Tailscale node, and that building from source needs Go 1.27.3.

**Acceptance Scenarios**:

1. **Given** the v0.0.90 highlights, **When** an operator reads the "Upgrading" notes, **Then** they see the settings needed to turn a receiver on (event source, webhook secret variable, and for Tailscale the hostname and first-start auth key), pointing at the user guide for the rest.
2. **Given** an operator who does not enable either receiver, **When** they read the highlights, **Then** it is clear nothing changes for them apart from the larger binary.

---

### User Story 3 - An operator can read webhook health and understand it (Priority: P2)

An operator with a receiver running asks "can GitHub actually reach my daemon, and how many events is it taking?". They look at `fabrik_health`.

**Why this priority**: The route and intake figures are the diagnostic surface for the new feature, but the transport sections of the guide already describe the route. The remaining gap is the central `fabrik_health` description and the intake counters.

**Independent Test**: Read the user guide's `fabrik_health` description and confirm it lists, under `webhook`, the `transports` rows with `route` and `replay`, and the `intake` counters with what each means.

**Acceptance Scenarios**:

1. **Given** the `fabrik_health` description in the user guide, **When** an operator looks for the `webhook` block, **Then** it documents `webhook.intake` (accepted, duplicates, dropped, echo-matched, and applied per event type), states that the counters are in-memory and reset on restart, and states that `route` and `replay` appear on the receiver rows.
2. **Given** an operator who sees `route: unknown`, **When** they follow the description, **Then** it says that unknown raises no warning and that only a sustained `unreachable` raises `webhook_route_unreachable`, linking to the transport section.

---

### User Story 4 - A reader of the marketing page and a model reading `llms-full.txt` see the same release (Priority: P3)

A visitor to the project site, and a tool that ingests `docs/llms-full.txt`, should see v0.0.90 as current.

**Why this priority**: Lower impact than operator-facing docs, but the llms bundle is checked by CI and the site note otherwise still describes v0.0.89 as new.

**Independent Test**: Open `docs/index.md` and confirm the "New in" note names v0.0.90 and the Webhook Mode card mentions the relay-free App receivers. Regenerate the bundle and confirm no diff remains.

**Acceptance Scenarios**:

1. **Given** `docs/index.md`, **When** a visitor reads the hero note, **Then** it says "New in v0.0.90", summarises webhooks without a relay, and links to the user-guide receiver section.
2. **Given** the Webhook Mode feature card, **When** a visitor reads it, **Then** it mentions that App-auth setups can receive events directly or through an embedded Tailscale Funnel node, while keeping the existing "polling stays on as a safety net" message.
3. **Given** any edit to a canonical doc, **When** the change is committed, **Then** `docs/llms-full.txt` has been regenerated in the same commit and the `docs-drift` check passes.

---

### Edge Cases

- A claim that is already correct in the user guide (for example the 19.5 MB node size or the Go 1.27.3 requirement) is left alone, not reworded; the pass edits only what is missing or stale.
- The two known issues (#2483, #2484) describe transient or cosmetic effects. They are stated as limitations of the current release, with no promise of a fix version.
- The README is a summary: it links to the user guide for setup and does not duplicate the full receiver configuration.
- Statements about `fabrik_health` must match what the shipped code reports (field names and values, `"unknown"` where a figure cannot be read), and the guide's existing wording for `route` is reused rather than restated differently.
- Pruefer and the other components are unchanged in v0.0.90 and need no documentation changes.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The README MUST carry a v0.0.90 highlights entry after the v0.0.89 entry, in the same style, covering: relay-free App webhook receivers (`direct`, `tailscale`), delivery replay, and route health.
- **FR-002**: The README's v0.0.90 entry MUST state that the receivers need GitHub App auth, are opt-in, and that replay is on by default for those two receivers and can be turned off with `replay_failed_deliveries: false`.
- **FR-003**: The README MUST state the unconditional changes: every build now includes the Tailscale node (binary about 35 MB), and building from source needs Go 1.27.3 (already the stated requirement, verified).
- **FR-004**: The README's Webhook Mode text and its App-auth note MUST name `direct` and `tailscale` alongside Hookdeck as event sources for App-auth setups, and MUST link to the matching user-guide sections.
- **FR-005**: The README's `--event-source` flag row MUST list `poll`, `hookdeck`, `direct` and `tailscale`, and the receiver flags (`--direct-listen`, `--app-webhook-path`, `--tailscale-hostname`, `--replay-failed-deliveries`) MUST be listed or linked consistently with the user guide's flag table.
- **FR-006**: The user guide's `fabrik_health` description MUST document `webhook.intake` (its counters and that they reset on restart) and MUST mention that receiver rows under `webhook.transports` carry `route` and `replay`.
- **FR-007**: The user guide's Webhook Mode section (section 10) and the Hookdeck section MUST point App-auth readers to the direct and Tailscale receivers, so the three App-auth options are discoverable from either place.
- **FR-008**: The user guide MUST record the two v0.0.90 known issues (#2483, #2484) in the section where each behaviour is described, worded as limitations of the current release, using issue numbers only without any closing keyword.
- **FR-009**: `docs/index.md` MUST replace the "New in v0.0.89" note with a "New in v0.0.90" note and update the Webhook Mode card to mention the relay-free App receivers.
- **FR-010**: Every configuration key, flag, environment variable, default and field name added or changed in the docs MUST match the shipped code and the existing user-guide tables; discrepancies found during the pass are corrected in the docs.
- **FR-011**: `docs/llms-full.txt` MUST be regenerated with `scripts/generate-llms-full.sh` and committed in the same commit as any edit to `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md` or `docs/positioning.md`.
- **FR-012**: Any pull request that mentions the community report or any other issue MUST do so by bare number only, with no closing keyword near it; the work issue's own number is the only one given `Closes`.

### Key Entities *(if applicable)*

- **Event source**: the way the daemon learns about GitHub events; `poll` (default), `hookdeck`, `direct`, `tailscale`.
- **App webhook receiver**: the daemon-side listener that `direct` and `tailscale` share, with its delivery intake, replay and route liveness.
- **Delivery replay**: the background request to GitHub to redeliver failed App webhook deliveries at start and on reconnect.
- **Route liveness**: the `reachable`/`unreachable`/`unknown` verdict for a receiver, taken from GitHub's delivery log.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A search of the README for `direct` and `tailscale` finds them in the highlights, the Webhook Mode text and the `--event-source` flag row.
- **SC-002**: The README, `docs/index.md` and the user guide no longer describe Hookdeck as the only App-auth event source, and `docs/index.md` no longer says "New in v0.0.89".
- **SC-003**: The user guide documents every field of `fabrik_health.webhook` that v0.0.90 added (`intake` and the `route`/`replay` objects) in the central description, not only in the transport sections.
- **SC-004**: Regenerating `docs/llms-full.txt` with `bash scripts/generate-llms-full.sh` after the edits produces no diff, so the `docs-drift` workflow passes on the first push.
- **SC-005**: Each new statement can be traced to the v0.0.90 release notes or to a shipped section of the user guide, with no new behaviour claimed beyond the release notes.

## Assumptions

- The docs-only change touches `README.md`, `docs/USER_GUIDE.md`, `docs/index.md` and the regenerated `docs/llms-full.txt`; `docs/state-machine.md` and `docs/stage-lifecycle.md` were updated by the feature PRs and are edited only if the pass finds a stale statement.
- The new User Guide sections for the two receivers are already accurate; they are checked for consistency but not rewritten.
- The marketing page change is limited to the "New in" note and the Webhook Mode card; no new page or card is warranted for one release.
- The `docs/v0.0.NN.md` release pages are not produced for every release (there is none for v0.0.89), so none is added here.
- The existing wording for route liveness in the guide is the source of truth and is reused in any summary.
- The ADR and `specs/` files for v0.0.90 features are internal records and are not edited.

## Out of Scope *(optional)*

- Changing any code, configuration default or behaviour; this is a documentation-only pass.
- Fixing the known issues #2483 and #2484, or making a promise about when they will be fixed.
- Rewriting the existing receiver sections of the user guide beyond correcting statements that are wrong.
- Documentation of the live e2e gate's events axis and webhook coverage tests (internal, covered in `tests/e2e/README.md` and `CLAUDE.md`).
- New blog posts, a comparison page, or other marketing material.

## Source References *(optional)*

- Release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.90
- Previous docs pass: #2419 (`specs/2419-update-docs-for-v0-0-89/spec.md`)
- Feature issues: #2142, #2143, #2144, #2427, #2444, #2450, #2452, #2472; known issues #2483, #2484
