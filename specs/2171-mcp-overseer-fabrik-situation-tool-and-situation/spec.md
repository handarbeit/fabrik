# Feature Specification: MCP overseer `fabrik_situation` tool and `situation` digest push (the rolled-up "what needs you" view)

**Feature Branch**: `fabrik/issue-2171`
**Created**: 2026-10-12
**Status**: Specified
**Input**: User description: "## Problem

The single most useful message in the 2026-10-11 in-situ MCP session was the five-entry `catch_up` snapshot on resubscribe: #1342 awaiting input, plus four items settled at Validate. That one snapshot was the whole \"what needs you\" picture.

An overseer has no way to get that picture on demand or on a cadence. It reconstructs it from `fabrik_board` filters, `fabrik_health` and its own memory of past pushes, and it still has to go to `gh` and the log for train state and anomalies.

## Requirements

- **R1 — `fabrik_situation` tool.** A read-only tool that returns one rolled-up view:
  - PRs awaiting a merge decision (settled under cruise), with the review verdict and the CI verdict;
  - items held at a gate, with the gate and how long it has held;
  - items waiting for human input, with the resume method;
  - pending decisions (#2120);
  - merge-train state per partition (#2124);
  - poll and worker health (in progress or overdue, slots in use, queued);
  - active anomalies (#2124's `pipeline-stalled`, and `retry-starved` / `item-wedged` / `cache-divergence` from #2170).

  Each entry carries the issue title, its link and the MCP tool that acts on it, where one exists.
- **R2 — `situation` digest push.** Pushed on a subscription-chosen cadence (off by default) and on request. It contains the same content as R1 and is deduplicated: no push when nothing changed since the last digest to that subscriber. The catch-up snapshot on attach/reconnect (#2169) is this digest.
- **R3 — Built from existing derivations.**
  - Reuse `internal/attention`, the settle predicates, the catch-up builder and `fabrik_health`'s sources. Build no second classifier.
  - The read path stays GitHub-free (`TestLocalAPISourceDoesNotReachGitHub`).
- **R4 — Bounded size.** Bound the view in size and order it by attention rank, with counts for anything truncated.
- **R5 — Docs.**
  - The tool table and event catalogue in `docs/USER_GUIDE.md`.
  - `docs/state-machine.md` §7.13/§7.14.
  - Regenerate `docs/llms-full.txt`.
  - Add an ADR.

## Scope

In scope:
- the tool, the digest event and the shared builder.

Out of scope:
- new underlying signals (they come from the issues they are built on).

## Acceptance

- **Tool, engine test:** an engine test with items in each category returns each in the right section, ordered by rank. The tool makes zero GitHub calls (nil-client test).
- **Digest, sim test:**
  - With a cadence set, two cadence ticks with no change publish one digest. A change between ticks publishes a second.
  - Shown non-vacuous against a builder that ignores the dedup.
- **Catch-up:** the attach/reconnect catch-up content equals the digest's content.
- `go test -race ./...` is clean.

Mentioned for context: #1966 (the human feature thread). Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966."

## Background

The single most useful message in the 2026-10-11 in-situ MCP session was the five-entry `catch_up` snapshot on resubscribe: #1342 awaiting input, plus four items settled at Validate. That one snapshot was the whole "what needs you" picture.

An overseer cannot get that picture on demand or on a cadence. It reconstructs it from `fabrik_board` filters, `fabrik_health` and its own memory of past pushes. It still has to go to `gh` and the daemon log for merge-train state and anomalies.

The pieces already exist separately:
- **Attention classification** (#1967, #2120): `needs-human`, `escalated`, `waiting`, `working`, `stalled`, `suspended` (pending decision) and `idle`, each with a rank, a reason, and the next action with the MCP tool that performs it.
- **Catch-up snapshot** (#1968): a store-only snapshot of items settled at Validate, suspended, needs-human or escalated, sent to a new subscription and to a session's first attach.
- **Health** (#2124): the poll block, worker slots, and per-partition merge-train phase and idle reason.
- **Anomalies and pipeline stalls** (#2124, #2170): `pipeline-stalled`, `retry-starved`, `item-wedged` and `cache-divergence`.

What is missing is one view that rolls these together, and a push that delivers the same view when it changes or at a chosen interval. #2169, which blocks this issue, makes the catch-up snapshot go to every reconnect and defines it as this digest once this issue lands. This issue provides the digest and the tool.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Ask "what needs me?" and get one answer (Priority: P1)

As an overseer session, I call `fabrik_situation` and receive one rolled-up view of everything that needs a decision or attention, with a way to act on each entry. I do not have to combine several board filters, health and my own memory.

**Why this priority**: This is the core value. The snapshot that proved most useful becomes available on demand.

**Independent Test**: Seed an engine with at least one item in each category, call the tool, and check each item lands in the right section, ordered by attention rank. Repeat with a client that fails on any GitHub call.

**Acceptance Scenarios**:

1. **Given** an item settled at Validate under cruise, **When** the tool is called, **Then** it appears under "awaiting a merge decision" with its review verdict and CI verdict as the daemon holds them.
2. **Given** items held at the feedback, review, CI or merge gate, **When** the tool is called, **Then** each appears under "held at a gate" naming the gate and how long it has held.
3. **Given** a paused item awaiting input, **When** the tool is called, **Then** it appears under "waiting for human input" with its resume method.
4. **Given** a suspended item (pending decision), **When** the tool is called, **Then** it appears under "pending decisions".
5. **Given** merge-train partitions, **When** the tool is called, **Then** each partition's state (phase, queued count, idle reason) is shown.
6. **Given** the daemon is running, **When** the tool is called, **Then** poll health (in progress or overdue) and worker health (slots in use, queued) are shown.
7. **Given** an active `pipeline-stalled`, `retry-starved`, `item-wedged` or `cache-divergence` anomaly, **When** the tool is called, **Then** it appears under "active anomalies".
8. **Given** any entry that concerns an issue, **When** it is returned, **Then** it carries the issue title, its link, and the MCP tool that acts on it where one exists.
9. **Given** a client that fails on any GitHub call, **When** the tool is called, **Then** it still answers and makes zero GitHub calls.

---

### User Story 2 - Receive the same view as a deduplicated push (Priority: P2)

As an overseer, I subscribe with a digest cadence and am pushed the situation view at that cadence, but only when it has changed since the last digest sent to me. I can also request one on demand, so a quiet system does not cost me context.

**Why this priority**: The tool needs me to ask. The push serves a session that reacts to what arrives, and deduplication keeps it cheap.

**Independent Test**: In the sim, subscribe with a cadence, advance two cadence ticks with no change, then change something and advance a third tick.

**Acceptance Scenarios**:

1. **Given** a subscription with a digest cadence and no change in the view, **When** two cadence ticks pass, **Then** exactly one digest was published.
2. **Given** a digest has been published, **When** the view changes and the next tick passes, **Then** a second digest is published.
3. **Given** a subscription with no cadence set, **When** time passes, **Then** no digest is pushed (off by default).
4. **Given** a subscriber requests a digest, **When** the request is handled, **Then** it receives the current view, subject to the same deduplication rule (an unchanged view since its last digest is not re-sent unless the request says otherwise; see Assumptions).
5. **Given** a builder that ignores deduplication, **When** the same scenario runs, **Then** the first acceptance test fails, so the test is not vacuous.

---

### User Story 3 - Attach and reconnect deliver the digest (Priority: P2)

As an overseer attaching or reconnecting, the catch-up snapshot I receive is the situation digest, so what I see on arrival matches what I would get from the tool.

**Why this priority**: It keeps a single builder behind all three paths (tool, push, catch-up). It removes the chance of the views drifting apart.

**Independent Test**: Attach a session to a populated sim and compare the catch-up content with the content of the digest and the tool result for the same state.

**Acceptance Scenarios**:

1. **Given** a populated engine, **When** a session attaches or reconnects and receives its catch-up, **Then** the catch-up content equals the digest content for the same state.
2. **Given** the same state, **When** the tool, a requested digest and the catch-up are produced, **Then** all three come from the same builder and their content is equal.

---

### User Story 4 - A large system stays readable (Priority: P3)

As an overseer on a busy board, I get a bounded view ordered by importance, with a count of what was cut, so a flood of items cannot drown the picture or blow up my context.

**Why this priority**: It protects the feature's usefulness at scale, but the small-board case works without it.

**Independent Test**: Seed more entries than the bound, call the tool, and check order, the bound and the truncation counts.

**Acceptance Scenarios**:

1. **Given** more entries in a section than the bound allows, **When** the view is produced, **Then** the most urgent entries by attention rank are kept, in rank order, and the section reports how many were omitted.
2. **Given** nothing is truncated, **When** the view is produced, **Then** no truncation count is reported (or it is zero).

---

### Edge Cases

- An item that qualifies for two categories (for example paused with a pending decision, or settled under cruise and also held at a gate) appears once, in the category its attention classification selects. It is not listed twice.
- A fact the daemon cannot vouch for after a restart (a gate hold start, a deadline, a milestone) is reported as `unknown`, never as "now" or a healthy default, as everywhere else in the local API.
- An empty system returns a valid, explicit "nothing needs you" view, not an error and not an empty response. A scheduled digest of this view is still deduplicated.
- With no daemon, the tool returns the same "daemon not running" tool error as the other overseer tools and does not hang.
- A digest cadence shorter than the daemon's own derivation tick is rounded up to a documented minimum (see Assumptions).
- A subscriber superseded by a newer attach under the same name stops receiving digests; the dedup memory belongs to the subscriber name, so a reattaching session is not sent a digest identical to the one it just received as catch-up.
- Stale items: an item that leaves a category (decision made, gate cleared) disappears from the next view and counts as a change for dedup purposes.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A read-only MCP tool `fabrik_situation` returns one rolled-up view with these sections: PRs awaiting a merge decision (settled under cruise) with review and CI verdicts; items held at a gate with the gate and hold duration; items waiting for human input with the resume method; pending decisions; merge-train state per (repo, base) partition; poll and worker health (poll in progress or overdue, worker slots in use, queued work); and active anomalies (`pipeline-stalled`, `retry-starved`, `item-wedged`, `cache-divergence`).
- **FR-002**: Every entry concerning an issue carries its title, its link and, where one exists, the MCP tool that acts on it.
- **FR-003**: A `situation` digest event carries the same content as FR-001's view.
- **FR-004**: A subscription can choose a digest cadence. The default is off; with no cadence set, no scheduled digest is pushed.
- **FR-005**: A subscriber can request a digest on demand.
- **FR-006**: Digests are deduplicated per subscriber: no digest is pushed when the view's content is unchanged since the last digest sent to that subscriber.
- **FR-007**: The catch-up delivered on attach and on reconnect (#2169) is this digest, with content equal to the digest built for the same state.
- **FR-008**: The tool, the requested digest, the scheduled digest and the catch-up all come from one shared builder. There is no second classifier: the builder reuses the attention classification, the settle predicates, the existing catch-up builder and the sources behind `fabrik_health`.
- **FR-009**: The read path makes no GitHub call. The existing static scans of the local-API and channel sources continue to hold and are not weakened, and a nil-client test proves zero GitHub calls.
- **FR-010**: The view is bounded in size. Entries are ordered by attention rank (most urgent first), and any section that is truncated reports how many entries were omitted.
- **FR-011**: A fact the daemon cannot vouch for is reported as `unknown`, consistent with the freshness rule of the existing overseer tools.
- **FR-012**: Each item appears in at most one category of the view.
- **FR-013**: The tool's response carries the daemon's `as_of` time like the other read tools.
- **FR-014**: Documentation: the tool table and event catalogue in `docs/USER_GUIDE.md`; `docs/state-machine.md` §7.13 and §7.14; the regenerated `docs/llms-full.txt`; and a new ADR.

### Key Entities

- **Situation view**: the rolled-up, bounded, rank-ordered snapshot of everything that needs attention, with its sections and truncation counts.
- **Digest**: the view delivered as a channel event to one subscriber, with a per-subscriber memory of the last content sent, for deduplication.
- **Digest cadence**: a per-subscription setting for how often to evaluate and push a digest (off by default).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An engine test with items in each category returns every item in the correct section, ordered by attention rank, and the same test run against a client that fails on any GitHub call still passes with zero GitHub calls.
- **SC-002**: In the sim, with a cadence set, two consecutive ticks with no change in between publish exactly one digest; a change between ticks publishes a second.
- **SC-003**: The same dedup scenario fails when run against a builder that ignores deduplication, demonstrating the test is not vacuous.
- **SC-004**: For the same state, the content of the attach/reconnect catch-up equals the content of the digest.
- **SC-005**: A view with more entries than the bound is truncated to the bound in rank order and reports exact omitted counts.
- **SC-006**: An overseer can answer "what needs me?" from one tool call or one push, with no `fabrik_board` filters, `fabrik_health` call, `gh` call or log reading.
- **SC-007**: `go test -race ./...` is clean, and `docs/llms-full.txt` is regenerated with no drift.

## Assumptions

- #2169 lands first (it blocks this issue) and routes the attach/reconnect catch-up through a single builder. This issue supplies that builder's final content (the digest) and does not itself change when the catch-up is delivered.
- The view is built only from the daemon's in-memory state (store, engine state and the health mirror). Anything not held there is reported as `unknown`. The one exception to this principle is none: no new GitHub read is added.
- "Held at a gate" covers the gates the #2170 `item-wedged` work already tracks (feedback, review, CI, merge); hold duration is measured from the daemon's own stamp and is `unknown` when the hold began before the current daemon run.
- "Awaiting a merge decision" means settled at Validate where the engine will not merge by itself (cruise); items under yolo that the engine lands itself are not listed there.
- The review verdict and CI verdict are the cached values the daemon already holds for the item, reported with the same freshness caveats as `fabrik_status`.
- The digest is a new event type in the event catalogue, an account-scoped summary rather than a per-item event; subscription scope filters (repo, milestone, labels) narrow the items in it as they narrow other events, while account-wide sections (poll health, anomalies, merge-train state) are always included.
- Digest dedup compares the content of the view excluding volatile fields (`as_of`, ever-growing durations), so an otherwise unchanged view does not re-send merely because time passed. The exact set of excluded fields is decided in the ADR.
- An on-demand digest request is subject to the same dedup rule by default; a request may force a send. The exact request shape is left to the ADR.
- The minimum cadence, the default per-section bound and the exact event name and field shapes are decided in the ADR, keeping the cadence default off.
- The situation tool and event are advisory and read-only; they add no action tool and change no engine behavior.

## Out of Scope *(optional)*

- New underlying signals: anything the view shows comes from the issues it is built on (#1967, #2120, #2124, #2169, #2170).
- Changing when the catch-up is delivered, restart and downtime deltas, and the semantic-by-default subscription family (all #2169).
- New action tools, and any tool that lifts a pause or posts a comment.
- Any GitHub read on the new path.
- A second classifier or any change to how items are classified by `internal/attention`.

## Source References *(optional)*

- #1966 (the human feature thread), mentioned for context only.
- #1967, #1968, #2120, #2123, #2124, #2169, #2170 (the work this view is built on).
- `docs/state-machine.md` §7.13 and §7.14; `docs/USER_GUIDE.md` "Overseer tools" and the event catalogue.
