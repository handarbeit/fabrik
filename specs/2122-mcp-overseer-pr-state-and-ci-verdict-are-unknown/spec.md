# Feature Specification: Overseer PR facts and CI verdict for every linked open PR

**Feature Branch**: `fabrik/issue-2122`
**Created**: 2026-10-12
**Status**: Specified
**Input**: User description: "## Problem

`fabrik_status` hides a failing PR check unless the item is in a CI-gated stage.

This was observed in an in-situ test against the live dev daemon on 2026-10-11:

- **#2112 at 13:20Z:**
  - **Status said:** `pr.number: 2118` and `head_sha: b1da0861…`, but `pr_state`, `draft`, `mergeable` and `ci` were all `\"unknown\"`. Attention said `waiting / cooldown:feedback-pending`.
  - **In reality:** `gh pr checks 2118` showed **\"Test and vet\" failing**.
  - An overseer reading this would never learn the item's CI was red.
- **#2110 / PR #2117 at 13:08Z:** the same unknown fields.
- **#2096**, which carried `fabrik:awaiting-ci`, had them populated (red, then green). So the fields appear to be filled only when the engine is already tracking CI for a gate.

The local API builds these fields from the store's linked-PR record (`engine/localapi.go`, `ciVerdict(lpr.CheckRuns)` and friends). Saying `unknown` is honest, but for an overseer an unknown CI on an open PR is a blind spot exactly where it matters.

## Requirements

- **R1 — PR facts for every linked open PR.** For every managed item with a linked open PR, the store carries the facts below. They are kept fresh by the engine's existing poll, deep-fetch or webhook paths, never by a GitHub call from the local API:
  - state;
  - draft;
  - head SHA;
  - mergeable state;
  - the green-and-complete CI verdict for the head (the same classifier the gates use).

  Prefer data the engine already fetches, such as the board query's linked-PR selection, deep fetch, or `check_run`/`pull_request` deltas. If a new fetch is needed:
  - it is bounded and in the engine's poll path;
  - its GraphQL cost is measured and stated in the PR;
  - it reuses the existing `RefreshCheckRunsLive` semantics where they fit.
- **R2 — Freshness is reported.** Each PR fact carries its as-of time, or the response's `cache` block covers it. A value older than one poll interval is marked stale rather than shown as current.
- **R3 — Attention uses it.**
  - A red CI verdict on the linked PR of an item that is not in a CI-gated wait, and has no worker addressing it, surfaces in attention with a summary naming the failing check.
  - When a worker is addressing it, the summary says so.
- **R4 — Docs.**
  - The `docs/USER_GUIDE.md` MCP section.
  - `docs/state-machine.md` §7.13 if the classifier changes.
  - Regenerate `docs/llms-full.txt`.

## Scope

In scope:
- PR fact freshness in the store;
- local-API rendering;
- the attention rule for red CI.

Out of scope:
- changing any gate's CI semantics;
- worker kinds (separate issue).

## Acceptance

All of the following are shown non-vacuous:
- **Engine/sim test:** an item in Implement or Review with a linked PR whose head check run is failing (no CI gate active) → `fabrik_status` reports `ci: red` with the failing check name, and attention surfaces it.
- **Freshness:** an as-of older than the poll interval is rendered as stale.
- **Zero GitHub calls:** the local-API no-GitHub scan stays green and is not weakened.
- **Cost:** the GraphQL cost per poll before and after is stated in the PR.
- `go test -race ./...` is clean.

Mentioned for context: #1966. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966."

## Background

The overseer read tools (`fabrik_status`, `fabrik_board`, `fabrik_health`, part of the #1966 overseer work) answer from the daemon's in-memory state and never call GitHub. They report a linked PR's state, draft flag, mergeable state and CI verdict, and report `"unknown"` when the daemon does not hold the value. That honesty is a deliberate design rule (docs/state-machine.md §7.13: anything the daemon cannot vouch for is `unknown`, never a healthy default).

The problem is how often the daemon does not hold these values. They are populated only while the engine is already tracking CI for a gate, for example an item carrying `fabrik:awaiting-ci`. For an item in any other stage or wait, the PR number and head SHA are known but `pr_state`, `draft`, `mergeable` and `ci` are all `unknown`.

Observed on the live dev daemon on 2026-10-11:

- #2112 (PR #2118) reported all four as `unknown` and attention `waiting / cooldown:feedback-pending`, while the PR's "Test and vet" check was failing.
- #2110 (PR #2117) showed the same unknown fields.
- #2096, which carried `fabrik:awaiting-ci`, had them populated (red, then green).

An overseer therefore cannot tell that CI is red on exactly the items where it most needs to know, namely items that are not in a CI-gated wait and where nothing in the engine will react to the failure on its own.

This feature makes the daemon hold, and the read tools report, the PR facts and CI verdict for every linked open PR, together with how fresh they are, and makes attention surface a red CI that nothing is addressing.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - See the PR facts and CI verdict of any item with an open PR (Priority: P1)

An overseer (a human or a Claude session using `fabrik mcp`) asks for the status of an item that has a linked open PR but is not in a CI-gated wait, for example during Implement or Review. It sees the PR's state, draft flag, head SHA, mergeable state and the green-and-complete CI verdict for the head, with the name of any failing check, instead of `unknown`.

**Why this priority**: This is the reported defect. Without it the overseer is blind to red CI on most items.

**Independent Test**: In the sim bed, put an item in Implement or Review with a linked open PR whose head check run is failing and no CI gate active. Ask the status tool. It reports CI `red`, names the failing check, and reports the PR state, draft, head SHA and mergeable state as known values.

**Acceptance Scenarios**:

1. **Given** an item in Implement with a linked open PR and a failing head check run and no `fabrik:awaiting-ci`, **When** the overseer asks for its status, **Then** CI is `red`, the failing check is named, and state, draft, head SHA and mergeable state are populated.
2. **Given** the same item after the check run turns green on the same head, **When** the engine's normal refresh paths have run, **Then** the status reports CI `green` without any change to the item's labels or stage.
3. **Given** a linked PR that is merged or closed, **When** the overseer asks for status, **Then** no open-PR CI verdict is presented as current.
4. **Given** an item with no linked PR, **When** the overseer asks for status, **Then** the PR fields report that there is no PR, not `unknown`.

---

### User Story 2 - Know how old each PR fact is (Priority: P1)

An overseer reading the PR facts can tell whether they are current or stale. A fact older than one poll interval is marked stale rather than shown as current, so a failing check that was fixed (or a green that has since gone red) is not mistaken for the present.

**Why this priority**: Surfacing more facts without reporting their age would replace a blind spot with a possibly misleading value, which breaks the "never a healthy default" rule.

**Independent Test**: Seed a PR fact with an as-of time older than the poll interval. The status response marks it stale; with a fresh as-of it is not marked stale.

**Acceptance Scenarios**:

1. **Given** PR facts last refreshed less than one poll interval ago, **When** the overseer asks for status, **Then** they are reported as current with their as-of time (or covered by the response's `cache` block).
2. **Given** PR facts last refreshed more than one poll interval ago, **When** the overseer asks for status, **Then** they are marked stale.
3. **Given** a fact the daemon has never captured (for example right after a restart, before the item is refreshed), **When** the overseer asks for status, **Then** it is `unknown`, not stale and not a guessed value.

---

### User Story 3 - Attention flags a red CI that nobody is addressing (Priority: P1)

An overseer scanning the attention view sees an item whose linked PR has a red CI verdict, is not in a CI-gated wait, and has no worker addressing it. The attention summary names the failing check. When a worker is already addressing the item, the summary says so instead of presenting it as unattended.

**Why this priority**: The attention view is what an overseer triages from. A red CI that the classifier calls `waiting / cooldown:feedback-pending` is the exact miss in the observed incident.

**Independent Test**: In the sim bed, create the User Story 1 situation with no worker in flight. The item's attention entry surfaces the red CI and names the failing check. Start a worker on the item. The summary now says a worker is addressing it.

**Acceptance Scenarios**:

1. **Given** an item with a red CI verdict on its linked PR, not in a CI-gated wait, no worker in flight, **When** attention is computed, **Then** the summary names the failing check and the item is surfaced rather than reported as an ordinary wait.
2. **Given** the same item with a worker in flight, **When** attention is computed, **Then** the summary says a worker is addressing it.
3. **Given** an item in a CI-gated wait (`fabrik:awaiting-ci`), **When** attention is computed, **Then** its existing classification is unchanged.
4. **Given** a green, pending or unknown CI verdict, **When** attention is computed, **Then** no red-CI statement is added.

---

### User Story 4 - The overseer tools stay free of GitHub calls (Priority: P2)

The operator relies on the read tools costing no GitHub quota and being safe to call at any rate. Getting the new facts must not change that: they arrive through the refresh the engine already performs, and any extra engine-side cost is measured and disclosed.

**Why this priority**: The zero-GitHub-cost guarantee is a documented, tested property of the read API; weakening it is a regression even if the data improves.

**Independent Test**: The existing check that the local-API source cannot reach GitHub passes unmodified, and a status call against an engine with no GitHub client still succeeds.

**Acceptance Scenarios**:

1. **Given** the change is complete, **When** the local-API no-GitHub scan runs, **Then** it passes without any exemption or loosening.
2. **Given** a status request, **When** it is served, **Then** no GitHub call is made on its behalf.
3. **Given** the change introduces any new engine-side fetch, **When** the PR is opened, **Then** its description states the GraphQL cost per poll before and after.

---

### Edge Cases

- A new commit is pushed and the head SHA changes: the CI verdict must not carry the previous head's result as current; until the new head's checks are known the verdict is pending or unknown, never the old green or red.
- Checks exist but are still running: the verdict is pending, not red or green.
- A PR with no check runs at all: reported the way the gates classify "none", not as red.
- A draft PR: facts are reported like any other open PR, with the draft flag set.
- A PR whose mergeable state GitHub has not computed yet: reported as unknown for that field only; the other facts are still shown.
- Daemon restart: facts the daemon has not yet refreshed report `unknown` until the first refresh, then become known without any change to the item.
- Webhooks unavailable (polling only): freshness is bounded by the poll cycle, and staleness is reported accordingly.
- An item with more than one linked PR: the PR the engine itself treats as the item's linked PR is the one reported.
- Red CI on an item that is paused or awaiting input: the existing pause classification takes precedence; red CI is additional information, not a reclassification.
- Rate-limit back-off of the engine: facts go stale and are reported as stale, not frozen as current.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: For every managed item with a linked open PR, the daemon MUST hold the PR's state, draft flag, head SHA, mergeable state and the green-and-complete CI verdict for that head, regardless of whether the item is in a CI-gated stage or wait.
- **FR-002**: The CI verdict reported for a PR head MUST be the one produced by the same classification the engine's gates use, so the overseer view and the gates cannot disagree about the same head.
- **FR-003**: The facts in FR-001 MUST be kept current by the engine's existing poll, deep-fetch and webhook paths; the local API MUST NOT make any GitHub call to obtain them.
- **FR-004**: Any new fetch needed to satisfy FR-001 MUST be bounded, run in the engine's poll path, and reuse the existing live check-run refresh semantics where they fit; its GraphQL cost per poll, before and after, MUST be measured and stated in the PR description.
- **FR-005**: Each PR fact MUST carry the time it was last refreshed, or be covered by the status response's cache-age information, and the status response MUST mark any such value older than one poll interval as stale instead of presenting it as current.
- **FR-006**: A fact the daemon has not captured MUST be reported as `unknown`; it MUST NOT be defaulted to a healthy value, and MUST NOT be confused with stale.
- **FR-007**: A CI verdict MUST NOT be carried across a head SHA change as if it applied to the new head.
- **FR-008**: When the CI verdict is red, the status response MUST name the failing check(s).
- **FR-009**: Attention MUST surface an item whose linked PR has a red CI verdict, which is not in a CI-gated wait and has no worker addressing it, with a summary that names the failing check.
- **FR-010**: When a worker is addressing such an item, the attention summary MUST say so rather than presenting the item as unattended.
- **FR-011**: Attention classification of items in a CI-gated wait, paused items and items with a known escalation MUST remain unchanged by FR-009 and FR-010.
- **FR-012**: The change MUST NOT alter the semantics of any gate that depends on CI (when it waits, clears, retries or escalates).
- **FR-013**: The existing static and runtime checks that the local API and channel code cannot reach GitHub MUST continue to pass without being weakened or exempted.
- **FR-014**: The user guide's overseer (MCP) section MUST describe the PR fields, their freshness reporting and the red-CI attention behaviour; the state-machine specification's attention classification section MUST be updated for the classifier change; the bundled documentation file MUST be regenerated in the same change.

### Key Entities

- **PR facts**: for an item's linked open PR, its state, draft flag, head SHA, mergeable state and CI verdict (with failing check names when red), each with a last-refreshed time.
- **CI verdict**: the green-and-complete classification of the checks on a PR head (`green`, `red`, `pending`, `none`, `unknown`), as used by the engine's gates.
- **Attention entry**: the per-item classification the overseer triages from, including its summary text.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a sim scenario with an item in Implement or Review, a linked open PR, a failing head check run and no CI gate active, the status tool reports CI `red` with the failing check named, and the test fails if the facts are removed (non-vacuous).
- **SC-002**: In the same scenario with no worker, the attention entry surfaces the red CI naming the failing check; with a worker in flight the summary states a worker is addressing it; the test fails against the current classifier.
- **SC-003**: A PR fact whose last refresh is older than one poll interval is rendered as stale, and one refreshed within the interval is not.
- **SC-004**: The local-API no-GitHub scan and the nil-client status test pass with no changes that loosen them.
- **SC-005**: The PR description states the GraphQL cost per poll before and after the change (including "no change" if no new fetch was added).
- **SC-006**: The full test suite passes with the race detector.
- **SC-007**: For items observed in the incident class (open PR, no CI gate), no PR fact is reported as `unknown` once the engine has refreshed the item at least once since daemon start.

## Assumptions

- "Linked open PR" means the PR the engine already treats as the item's linked PR; merged and closed PRs are out of scope for the CI verdict.
- "Stale" is defined as older than one poll interval, as the issue states; the threshold is not operator-configurable in this change.
- A red verdict on an item that is paused, escalated or awaiting input keeps its existing classification; the red-CI statement is additional information for those items rather than a new state.
- Surfacing red CI in attention does not by itself cause the engine to dispatch work; it changes what is reported, not what the engine does (FR-012).
- Whether the facts arrive through existing data the engine already fetches or through a bounded new fetch is a design choice for later stages, constrained by FR-003 and FR-004.
- This work lands after #2121, which the issue is blocked on.

## Out of Scope *(optional)*

- Changing any CI, review or merge gate's semantics.
- Worker kinds and how a worker's purpose is reported (a separate issue).
- Making the stale threshold configurable.
- Having the engine automatically react to a red CI on items outside CI-gated waits.
- New channel (push) events for red CI.
- Reporting CI for merged or closed PRs.

## Source References *(optional)*

- #1966 (overseer MCP work); mention by bare number only in the PR body, with no closing keyword near it.
- docs/state-machine.md §7.13 (attention classification and the local read API, "anything the daemon cannot vouch for is `unknown`") and the user guide's overseer tools section.
- In-situ observations of #2112 / PR #2118, #2110 / PR #2117 and #2096 on 2026-10-11.
