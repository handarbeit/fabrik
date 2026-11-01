# Feature Specification: Update documentation for v0.0.91

**Feature Branch**: `fabrik/issue-2547`
**Created**: 2026-11-01
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.91.

See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.91

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)

## Labels

documentation, fabrik:yolo"

## Background

Every release is followed by a documentation pass so that the User Guide, the README and the marketing site describe the shipped behavior, not the previous release's. v0.0.91 shipped on 2026-11-01 with two themes and a set of smaller fixes:

- **Review gates that read the reviewer, not the clock.** The authoritative review gate judges each reviewer at the PR's current head; a declared bot's clean review at head withdraws its own earlier change request, and a verdict on an older commit reads as "awaiting re-review". Every route out of a `wait_for_reviews` stage, including the catch-up advance, asks that same verdict. A clean review from an expected reviewer (Pruefer now adds a hidden findings-count marker to each review body) starts no worker and holds no gate, including for Queued merge-train members. A bot finding that Review declined at the current head pauses for a decision instead of waiting out a timeout. The review-wait timeout restarts when a new head arrives.
- **Pauses that say why, and clean stops that resume themselves.** Every engine pause carries exactly one `fabrik:pause:<cause>` label, surfaced as `pause_cause` in `fabrik_status` and channel events. Items paused by a daemon clean stop are resumed on the next start (`resume_after_clean_stop`, default on).
- **Smaller changes.** Optional reserved worker slots for review, CI-fix and rebase rounds (`fix_round_slots`, default 0 = off; `fabrik_health.workers` reports `fix_in_use`/`fix_max`); `history.json` records token counts and the effective model per invocation; a closed paused item at Done is cleaned up; `fabrik upgrade` refuses to migrate the plugin while a daemon holds the directory; engine rebases that only shift diff context are no longer treated as diverged; a failed live read in the feedback gate retries within one poll; stage start failures record a retry cooldown and warn when persistent; webhook deliveries for merge-train integration PRs are no longer attached to a member; label changes around a SIGHUP restart are applied before the exec and announced once, with `fabrik_health.channel.ready` reporting channel readiness. Hookdeck remains deprecated and is removed in v0.0.92. A companion-tools page (Pruefer and Werkbank) was added.
- **Known issue.** A merge-train trial's CI verdict can be read up to one idle poll interval after CI finishes, slowing bisection on a quiet daemon; landings are unaffected (#2542, planned for v0.0.92).

A first survey shows much of this was documented alongside the features themselves (`fabrik:pause:`, `resume_after_clean_stop`, `fix_round_slots`, `fix_in_use`, the findings marker and "awaiting re-review" all already appear in `docs/USER_GUIDE.md`). The remaining work is therefore a reconciliation: find what is missing, stale or inconsistent, and fix only that. Notably, the "highlights" blocks in `README.md` and `docs/index.md` stop at v0.0.90, and the `history.json` token/model fields are not described anywhere.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A reader of the release highlights sees v0.0.91 (Priority: P1)

An operator or evaluator opens the README or the marketing page and reads the "what's new" text. It should describe v0.0.91, not stop at v0.0.90.

**Why this priority**: The highlights are the first thing readers see and the part most likely to be stale right after a release.

**Independent Test**: Read the highlights in `README.md` and `docs/index.md` and compare them to the v0.0.91 release notes; each theme is present, accurate and links to the User Guide section that explains it.

**Acceptance Scenarios**:

1. **Given** the v0.0.91 release notes, **When** a reader opens the README highlights, **Then** a v0.0.91 entry summarizes the review-gate theme and the pause-cause/clean-stop-resume theme, alongside the existing earlier entries.
2. **Given** the marketing page, **When** a reader looks at its "New in" text, **Then** it describes v0.0.91 (or deliberately keeps v0.0.90 with a recorded reason) and does not contradict the README.

---

### User Story 2 - An operator finds every v0.0.91 setting and field in the User Guide (Priority: P1)

An operator wants to configure or interpret a v0.0.91 behavior. Each new or changed setting, label, status field and health field is documented in the User Guide and, where the User Guide has a configuration/flag/environment reference, listed there consistently.

**Why this priority**: The User Guide is the authoritative user-facing reference, and the issue's core scope item is making sure its configuration references are current.

**Independent Test**: For each v0.0.91 item in the release notes, search the User Guide for its name and check that it is described accurately and, for settings, appears in the flag, environment-variable and YAML tables.

**Acceptance Scenarios**:

1. **Given** the `fix_round_slots` and `resume_after_clean_stop` settings, **When** the operator checks the YAML example, flag table and environment-variable table, **Then** each is present with its default and its off/on meaning.
2. **Given** `history.json`, **When** the operator reads the job-history section, **Then** it states that each invocation records token counts and the effective model.
3. **Given** a paused item, **When** the operator reads the pause documentation, **Then** the `fabrik:pause:<cause>` vocabulary, the `pause_cause` surfaces and the fact that `fabrik:awaiting-input` is still present on engine pauses are described consistently across sections.
4. **Given** the review gate sections, **When** the operator reads them, **Then** they describe head-aware per-reviewer verdicts, the declined-at-head decision pause, the clean-review-starts-no-worker rule (including Queued members), and the review-wait restart on a new head, with no remaining text that contradicts them.

---

### User Story 3 - Deprecation, upgrade and known-issue notes are accurate for v0.0.91 (Priority: P2)

Operators planning an upgrade need to know Hookdeck is removed in v0.0.92, that `fabrik upgrade` refuses while a daemon holds the directory, and about the one known issue.

**Why this priority**: These are actionable upgrade-planning facts, but each is a small, localized check.

**Independent Test**: Search for Hookdeck deprecation text, the `fabrik upgrade` description and the known-issues list and compare with the release notes.

**Acceptance Scenarios**:

1. **Given** Hookdeck, **When** a reader reads any mention, **Then** wording is consistent: deprecated since v0.0.90, removed in v0.0.92, with the replacement transports named and no text implying removal has already happened.
2. **Given** `fabrik upgrade`, **When** a reader reads its description, **Then** it mentions the refusal while a running daemon holds the directory and the remedy.
3. **Given** the documented known issues, **When** a reader looks for v0.0.91 issues, **Then** the bisection-latency issue (#2542) is listed as affecting trial verdict timing only, not landings, if the docs maintain a known-issues list.

---

### User Story 4 - The LLM bundle matches the canonical pages (Priority: P1)

**Why this priority**: CI's `docs-drift` workflow fails the PR otherwise.

**Independent Test**: Run the bundle generator and confirm no diff remains.

**Acceptance Scenarios**:

1. **Given** edits to `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md` or `docs/positioning.md`, **When** the change is committed, **Then** `docs/llms-full.txt` is regenerated in the same commit and a re-run of the generator produces no difference.

---

### Edge Cases

- A v0.0.91 item is already documented correctly: leave it untouched rather than rewording it.
- Version-pinned phrases ("in v0.0.90 and v0.0.91", "v0.0.89 highlights") must stay true: do not rewrite historical statements to say v0.0.91, and update phrases that are now wrong (for example text saying Hookdeck "keeps working in v0.0.90 and v0.0.91" remains accurate and needs no change).
- `README.md` mirrors much of the User Guide; where both describe the same setting, the two must not disagree.
- Internal-only items in the release notes (gate harness fixes, plugin pinning, marketplace refs, e2e coverage) are not user-facing and are not documented.
- Docs describing behavior the release notes do not mention must not be changed in this issue.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `README.md` MUST include a v0.0.91 highlights entry in the same style and place as the existing per-release highlights, covering the review-gate theme, the pause-cause label and clean-stop resume, and the smaller changes worth a reader's attention.
- **FR-002**: `docs/index.md` MUST be reviewed against the v0.0.91 release notes and its "New in" text updated, or left with a recorded reason, so it does not describe an older release as the latest without cause.
- **FR-003**: `docs/USER_GUIDE.md` MUST describe each user-facing v0.0.91 feature and fix listed in the release notes (Features, Fixes, Improvements), accurately and in the section where a reader would look for it.
- **FR-004**: Every new or changed v0.0.91 setting (`fix_round_slots`, `resume_after_clean_stop`) MUST appear with its default in the YAML configuration example and the flag and environment-variable reference tables of the User Guide, and in the README's equivalent tables where they exist.
- **FR-005**: The job-history documentation MUST state that `.fabrik/history.json` records token counts and the effective model for each invocation.
- **FR-006**: The `fabrik:pause:<cause>` label, its eight causes, the `pause_cause` field in `fabrik_status` and channel events, and the retained role of `fabrik:awaiting-input` MUST be described consistently wherever pauses, labels and the status/channel surfaces are documented; the label must also appear in any labels reference in `README.md` or `docs/index.md`.
- **FR-007**: The documentation of the review gate MUST be consistent with the v0.0.91 verdict rules: per-reviewer judgement at the current head, "awaiting re-review" for a verdict on an older commit, a declared bot's clean review at head superseding its earlier change request, one shared verdict for every route out of a `wait_for_reviews` stage, a clean expected-reviewer review starting no worker and holding no gate (including for Queued members), the declined-at-head decision pause, and the review-wait timeout restarting on a new head.
- **FR-008**: The `fabrik_health` documentation MUST list `workers.fix_in_use`/`fix_max` and `channel.ready`.
- **FR-009**: All mentions of Hookdeck deprecation MUST consistently say deprecated since v0.0.90 and removed in v0.0.92, with the replacement transports named, and the Hookdeck migration guidance MUST be present in the User Guide.
- **FR-010**: The description of `fabrik upgrade` MUST mention that it refuses while a running daemon holds the directory, and how to proceed.
- **FR-011**: If the documentation keeps a list of known issues, it MUST include the #2542 bisection-latency issue, stating that landings are unaffected and that a fix is planned for v0.0.92.
- **FR-012**: The new companion-tools page and Pruefer reference MUST be reachable from the README and the site navigation, and Pruefer's documentation MUST mention the hidden findings-count marker in review bodies.
- **FR-013**: Documentation changes MUST be limited to what the v0.0.91 release notes require plus the consistency fixes needed to remove contradictions they expose; no unrelated rewording.
- **FR-014**: If any of `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md` or `docs/positioning.md` is changed, `docs/llms-full.txt` MUST be regenerated with `bash scripts/generate-llms-full.sh` and committed in the same commit.
- **FR-015**: `docs/llms.txt` MUST be checked and updated if it lists pages or capabilities affected by this release.
- **FR-016**: Configuration names, defaults and examples written into the docs MUST match the shipped behavior (checked against the release notes and, where ambiguous, the as-built `docs/state-machine.md`).

### Key Entities *(if applicable)*

- **Canonical doc pages**: `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, `docs/positioning.md`; changes require a regenerated `docs/llms-full.txt`.
- **Release highlights**: the per-release summary paragraphs in `README.md` and the "New in" text in `docs/index.md`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Every user-facing item in the v0.0.91 release notes (Features, Fixes, Improvements, Known issues, Upgrading) is findable in the docs, or is recorded as intentionally not documented with a reason.
- **SC-002**: `README.md` and `docs/index.md` no longer present v0.0.90 as the latest release.
- **SC-003**: Every v0.0.91 setting appears with the same default in all places the docs list settings (YAML example, flag table, environment-variable table, README table).
- **SC-004**: Running `bash scripts/generate-llms-full.sh` after the change produces no diff against the committed `docs/llms-full.txt`, so the `docs-drift` check passes on the first push.
- **SC-005**: No documented statement contradicts a v0.0.91 behavior, in particular around review-gate verdicts, pause labels and Hookdeck's removal date.

## Assumptions

- The v0.0.91 release notes are the source of truth for scope; items marked Internal are out of scope.
- The README keeps its current structure of per-release highlights blocks; a new v0.0.91 block is added in the same style rather than replacing earlier ones.
- The marketing page's "New in" text is replaced by a v0.0.91 summary if one fits the page's tone; if not, keeping v0.0.90 is acceptable only with a note in the PR.
- Features that earlier doc commits already describe correctly are left as they are.
- Detail in `docs/state-machine.md` and `docs/stage-lifecycle.md` was written with the features and is verified, not rewritten, unless a contradiction is found.

## Out of Scope *(optional)*

- Changes to engine code, tests, stage configuration or release tooling.
- Documenting internal items (e2e gate harness, marketplace pinning, plugin version bumps).
- Blog posts or new marketing pages beyond the existing highlights text.
- Documenting v0.0.92 changes, including the actual removal of Hookdeck.
- Restructuring or rewording documentation unrelated to v0.0.91.

## Source References *(optional)*

- Release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.91
- `CLAUDE.md` — "Documentation bundle (docs/llms-full.txt)"
- `scripts/generate-llms-full.sh`, `.github/workflows/docs-drift.yml`
