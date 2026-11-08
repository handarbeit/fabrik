# Feature Specification: Update docs for v0.0.92

**Feature Branch**: `fabrik/issue-2570`
**Created**: 2026-11-08
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.92.

See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.92

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)"

## Background

v0.0.92 was released with these user-visible changes:

- **Breaking:** the Hookdeck event transport is removed from Fabrik and Pruefer. `event_source: hookdeck` is refused at startup; leftover `hookdeck_*` settings are ignored with one notice. The replacements are `event_source: tailscale` and `event_source: direct`, both of which need GitHub App auth.
- **Pruefer** supports `event_source: direct` and `tailscale` on the shared App receiver, with an installation ownership check, dedupe, replay and route liveness, plus a footer health segment.
- The stage key `feedback_bots` was added. A bot's PR conversation comments count as feedback only when the bot is opted in, either through `feedback_bots` or as an expected reviewer.
- `fabrik init` without `--create-board` lists the board columns the stages need and no longer claims Fabrik is ready.
- Label and lock reliability: ambiguous lock-label writes are reconciled against live labels, orphaned locks are reclaimed at runtime (`lock-orphaned`), and a lost write to an engine state label is confirmed and re-applied. A merge-train trial's CI verdict is acted on within one poll.
- The Tailscale node listens on the public Funnel only, App webhook server errors go to the engine log, the HTTPS-vs-SSH git decision is made once at startup under App auth, and first-run empty-board handling was polished.
- Known issue: a Funnel outage that does not drop the tailnet connection triggers no delivery replay.

The code PRs for v0.0.92 already updated much of `docs/USER_GUIDE.md` and `docs/state-machine.md`. A survey of the current tree found:

- **Already covered** in the User Guide: the "Migrating from Hookdeck" section, `feedback_bots`, runtime lock reclaim and `lock-orphaned`, the Hookdeck refusal in the `--event-source` and `FABRIK_EVENT_SOURCE` references, and the scrub notes.
- **Stale or missing**:
  - `README.md` has release highlights up to v0.0.91 but none for v0.0.92.
  - `docs/index.md` says "New in v0.0.91" and does not mention v0.0.92.
  - `docs/pruefer.md` still says Pruefer "polls, with no webhook setup" and does not mention the new event sources.
  - `docs/USER_GUIDE.md` has a plugin-pinning example showing `"ref": "v0.0.91"`, while `marketplace.json` now pins `v0.0.92`.
  - Whether the User Guide mentions the `fabrik init` column listing, the Funnel-outage known issue, and the Pruefer direct/tailscale receiver needs a per-item check.

The task is therefore to close the remaining gaps and verify the existing coverage, not to rewrite documentation that is already accurate.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Hookdeck users can find and follow the migration path (Priority: P1)

An operator running `event_source: hookdeck` reads the README, the User Guide or the release notes before upgrading, and needs to learn that Hookdeck is gone and how to move to `tailscale` or `direct`.

**Why this priority**: it is the only breaking change in the release, and an operator who misses it starts a daemon that refuses its own config.

**Independent Test**: starting from the README highlights and the User Guide alone, an operator can identify the replacement transports, the App-auth requirement, the new webhook URL step, and the `--auto-upgrade` caveat.

**Acceptance Scenarios**:

1. **Given** the README release highlights, **When** an operator reads the v0.0.92 entry, **Then** it states that Hookdeck was removed and links to "Migrating from Hookdeck".
2. **Given** the User Guide, **When** the operator follows "Migrating from Hookdeck", **Then** the steps match the release notes: choose a receiver, repoint the App's webhook URL, keep the secret variable, and remove the leftover settings.
3. **Given** the User Guide config example and reference tables, **When** read, **Then** none of them present Hookdeck as a valid option.

---

### User Story 2 - Pruefer operators learn Pruefer can receive webhooks (Priority: P2)

A Pruefer operator reads the short Pruefer overview page and needs to know that webhook-driven operation is available, not only polling.

**Why this priority**: the overview currently says the opposite, which is now wrong.

**Independent Test**: the overview page describes `direct` and `tailscale` event sources, keeps polling as the default and fallback, and defers detail to the Pruefer README.

**Acceptance Scenarios**:

1. **Given** `docs/pruefer.md`, **When** read, **Then** it no longer claims Pruefer has no webhook support and points to the Pruefer README for the settings.
2. **Given** the Pruefer README and the overview, **When** compared, **Then** they do not disagree (the README stays authoritative).

---

### User Story 3 - Newcomers see current release highlights (Priority: P3)

A visitor to the README or the marketing page sees what is new in the latest release.

**Why this priority**: the highlights are the public-facing summary, but they carry no operational risk.

**Independent Test**: the README and `docs/index.md` both name v0.0.92 as the latest release and summarise its themes in the same style as earlier entries.

**Acceptance Scenarios**:

1. **Given** the README, **When** read, **Then** a v0.0.92 highlights entry follows the v0.0.91 entry.
2. **Given** `docs/index.md`, **When** read, **Then** the "New in" line describes v0.0.92 and keeps a short pointer to the previous release.

---

### User Story 4 - Operators find the remaining v0.0.92 behaviour in the User Guide (Priority: P2)

An operator looks in the User Guide for the label-reliability changes, the `fabrik init` output, the Funnel-outage caveat, and plugin pinning.

**Why this priority**: the guide is the reference, and gaps there cause support questions.

**Independent Test**: each item in the v0.0.92 release notes maps to a User Guide passage, or is recorded as intentionally not user-facing.

**Acceptance Scenarios**:

1. **Given** the release notes' Features, Fixes and Known issues, **When** each is searched in the User Guide, **Then** each user-visible item is found and accurate, or is added.
2. **Given** the plugin-pinning example, **When** read, **Then** its version example is consistent with the current release.

---

### Edge Cases

- Release-note items that are purely internal (shared receiver package, e2e gate budget guard, harness fixes) need no user documentation.
- Historical mentions of Hookdeck (the deprecation in v0.0.90, past highlights, the scrub of leftover secret variables) must stay accurate rather than be deleted.
- `docs/USER_GUIDE.md` and `docs/state-machine.md` are canonical pages, so any edit to them or to `docs/stage-lifecycle.md` or `docs/positioning.md` requires regenerating `docs/llms-full.txt` in the same change.
- `cmd/pruefer/README.md` is the authority on Pruefer settings and already covers the new sources. It should be changed only if the survey finds an inaccuracy.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The README MUST include a v0.0.92 highlights entry, in the style of the earlier entries, covering Hookdeck removal (with a link to the migration section), Pruefer webhook sources, and the label/lock reliability changes.
- **FR-002**: `docs/index.md` MUST update its "New in" line to v0.0.92 and keep a short pointer to the previous release's themes.
- **FR-003**: `docs/pruefer.md` MUST replace the claim that Pruefer polls with no webhook setup with an accurate description of polling plus the optional `direct` and `tailscale` event sources, deferring detail to the Pruefer README.
- **FR-004**: The User Guide MUST be checked against each user-visible item in the v0.0.92 release notes, and any item that is missing or inaccurate MUST be added or corrected. Specific items to verify are the `fabrik init` board-column listing, the Funnel-outage known issue, Pruefer direct/tailscale coverage, the `fabrik_health` transport fields, and `feedback_bots`.
- **FR-005**: The User Guide's plugin-pinning example MUST use a version consistent with the current release.
- **FR-006**: No documentation outside the migration/historical context may present `event_source: hookdeck` or `hookdeck_*` settings as supported.
- **FR-007**: Configuration examples and reference tables touched by this work MUST match the current flags, environment variables and YAML keys.
- **FR-008**: `docs/llms-full.txt` MUST be regenerated with `bash scripts/generate-llms-full.sh` and committed in the same change as any canonical-doc edit.
- **FR-009**: The change MUST be documentation only, with no code, schema or behaviour changes.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Every user-visible item in the v0.0.92 release notes maps to at least one accurate passage in the README, User Guide or marketing pages, and the mapping is recorded in the PR description.
- **SC-002**: A search for "hookdeck" across the README, User Guide, marketing pages and Pruefer docs returns only migration, deprecation-history or secret-scrub references.
- **SC-003**: The README and the marketing page both identify v0.0.92 as the latest release.
- **SC-004**: The `docs-drift` workflow passes on the first push, because the regenerated `docs/llms-full.txt` matches the committed copy.

## Assumptions

- "v0.0.92" means the published release notes, and `release-notes/v0.0.92.md` matches them.
- The marketing site is `docs/index.md` and its layouts. No other marketing pages need changes unless the survey shows stale v0.0.91-or-older claims about webhooks.
- The `v0.0.91` example in the plugin-pinning text is a stale literal and may be updated, or reworded so it need not be updated every release.
- Release-note items marked internal need no user-facing documentation.
- The README highlight entry is concise, like its neighbours, and does not duplicate the migration steps.

## Out of Scope

- Changes to code, tests, stage configs or ADRs.
- Rewriting User Guide sections already accurate for v0.0.92.
- New tutorials, diagrams or blog posts.
- Changing the `cmd/pruefer/README.md` structure.

## Source References

- Release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.92 (also `release-notes/v0.0.92.md`)
- `CLAUDE.md` rule on regenerating `docs/llms-full.txt`
