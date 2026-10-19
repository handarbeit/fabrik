# Feature Specification: Self-write staleness baseline must never mask a concurrent external write

**Feature Branch**: `fabrik/issue-1945`
**Created**: 2026-10-19
**Status**: Specified
**Input**: User description: "## Problem

`SelfWriteObserved` (#1090) advances an item's staleness baseline (`LastSeenSourceUpdatedAt`) to the engine's **local** `time.Now()` after each of its own writes. Both the probe (`cycleSet`) and the board cache's `IsItemCacheFresh`/`FetchItemDetails` then treat the item as unchanged while GitHub's second-granular `updatedAt` is not after that value.

Any external write landing within about a second of an engine write, or within the clock skew between the local host and GitHub, is therefore absorbed as \"already seen\". That covers a human comment, a label change, or a review. The most common case is a human replying straight after a pause comment. Nothing flags the item as changed, and the cache keeps serving the pre-write copy.

#1944 bounds the damage for **paused** items only: at the periodic re-evaluation it deep-fetches a paused item with a recent baseline, invalidating the cache first. Non-paused items rely on other admission paths, but the cache can still serve them stale comments until something else bumps `updatedAt`.

## Requirements

- **R1** — An external write made after the engine's own write must never be masked. Anchor the baseline to GitHub's own timestamp for the write where the API returns one: `created_at`/`updated_at` on comment creation, `updated_at` on issue mutations. Where it doesn't, the baseline must be conservative: never ahead of the latest GitHub timestamp the engine has actually observed.
- **R2** — #1090's purpose is preserved: the engine's own write does not trigger a redundant deep fetch.
- **R3** — Once R1 holds, re-evaluate whether #1944's paused backstop is still needed, and keep it as defence in depth if it's cheap.

## Acceptance

- A test puts an external comment one second after an engine comment, with local clock skew in both directions. The comment is seen on the next poll, and the cache does not serve a copy without it.
- The #1090 tests still pass."

## Background

When Fabrik writes to an issue (a comment, a label, a body edit, a board Status move), the write bumps the issue's `updatedAt` on GitHub. If the next probe treated that bump as an external change, every self-write would force a redundant deep fetch (the #1083 ping-pong incident). `SelfWriteObserved` (#1090) prevents this by moving the item's staleness baseline forward to cover the write.

The baseline is set to the **engine host's wall clock** (`time.Now()`), not to a time GitHub reported. Two clocks are therefore compared: the baseline from the local host, and `updatedAt` from GitHub, which has one-second granularity. An external write that GitHub stamps at or before the local baseline is read as "already seen". This happens when:

- the external write lands within about a second of the engine's own write (a human replying straight after a pause comment is the most common case), or
- the local clock runs ahead of GitHub's, in which case every external change inside the skew window is absorbed.

Nothing marks the item as changed afterwards, so the cache keeps serving the pre-write copy. The 2026-10-15 engine state audit confirmed the problem is still real and found it persistent, not bounded by one poll, for any fact no webhook carries: blockedBy edges, sub-issues, comment edits and, on a poll-only daemon, body edits and non-gate labels.

Related work already shipped on `main`:

- **#1944** added a backstop for paused items. It was later replaced by the cache freshness rule of **#2180 (ADR 2180)**, which serves a cached copy only if the issue-layer `updatedAt`, the linked-PR `updatedAt` and GitHub's issue comment total all agree with the cache. That rule catches a masked human *comment* (the comment total differs). It does not catch other masked external writes (label change, body edit, comment edit, blockedBy or sub-issue change), because for those the only signal is `updatedAt` compared with the local-clock baseline. The #1944 backstop itself is already removed.
- **#2348 (ADR 2348)**, the blocker named in the issue history, introduced the general observed-at ordering rule for writes into the item store: each GitHub-derived fact carries an authority time and a stale write is dropped. This issue should build the baseline on that rule's notion of authority time.

The remaining defect is the clock source and the masking window of the baseline itself, which is what this issue fixes.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A human reply right after an engine comment is always seen (Priority: P1)

An operator replies to a Fabrik pause comment within a second of it being posted. Fabrik must notice the reply on the next poll and resume the item, whether the host clock is ahead of or behind GitHub's.

**Why this priority**: This is the common real-world trigger, and it silently stalls a paused item.

**Independent Test**: Post an engine comment, then an external comment stamped one second later. Run the next poll. Repeat with the local clock offset ahead and behind GitHub's.

**Acceptance Scenarios**:

1. **Given** an engine comment was just posted and the item is cached, **When** an external comment is created one second later and the next poll runs, **Then** the item is re-fetched, the external comment is present in the engine's view, and the cache does not serve a copy without it.
2. **Given** the local clock is ahead of GitHub's by several seconds, **When** the same sequence runs, **Then** the outcome is the same.
3. **Given** the local clock is behind GitHub's by several seconds, **When** the same sequence runs, **Then** the outcome is the same.

---

### User Story 2 - External writes other than comments are not masked (Priority: P1)

A human changes a label, edits the issue body or a comment, or changes a blockedBy edge or sub-issue shortly after an engine write. Fabrik must treat the item as changed, even though no comment-count signal exists for these.

**Why this priority**: The comment-count evidence from #2180 covers only new comments. These other writes are masked for as long as nothing else bumps `updatedAt`, and on a poll-only daemon nothing else does.

**Independent Test**: Perform an engine write, then an external write of each kind within the masking window (same second, clock skew in both directions). Run the next poll.

**Acceptance Scenarios**:

1. **Given** an engine label write, **When** an external label change is made in the same second and the next poll runs, **Then** the item is re-fetched and the label change is visible.
2. **Given** an engine comment, **When** an external edit to the issue body or to a comment follows within the window, **Then** the item is re-fetched and the edit is visible.

---

### User Story 3 - Engine writes still cost no redundant deep fetch (Priority: P2)

An engine write with no concurrent external activity must not cause the next probe to deep-fetch the item.

**Why this priority**: This is the purpose of #1090. Fixing the masking must not bring back the #1083 ping-pong loop.

**Independent Test**: Run the existing #1090 tests, plus a scenario with an engine write and no external activity, and count deep fetches.

**Acceptance Scenarios**:

1. **Given** a cached item, **When** the engine posts a comment and nothing else changes, **Then** the next probe does not deep-fetch the item.
2. **Given** the same item, **When** the engine performs each other supported write (label add or remove, body edit, board Status move), **Then** the next probe does not deep-fetch the item.

---

### Edge Cases

- A write whose API response carries no GitHub timestamp (for example, label add or remove). The baseline must stay conservative: it must not move ahead of the latest GitHub timestamp the engine has observed.
- A failed engine write must never advance the baseline.
- An engine write racing a deep fetch in flight. The baseline must not move backward, and a deep fetch that observed a later GitHub value must win.
- Two engine writes in quick succession, with an external write between them.
- GitHub's one-second timestamp granularity: an external write in the same second as the engine write.
- Local clock far ahead of or behind GitHub's: a baseline taken from the local clock must not decide freshness in either direction.
- A write made while a webhook delivery for the same change arrives, so both an echo and the poll path see it.
- Display-field writes (status line, Last activity, Last run) remain excluded from the baseline, as today.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: After an engine write, an external write made later than it (as GitHub orders them) MUST cause the item to be treated as changed at the next probe and MUST NOT be absorbed into the staleness baseline.
- **FR-002**: Where GitHub's response to an engine write returns its own timestamp (`created_at` on comment creation, `updated_at` on issue mutations), the staleness baseline MUST be anchored to that GitHub timestamp, not to the engine host's wall clock.
- **FR-003**: Where GitHub's response to an engine write returns no timestamp, the baseline MUST NOT be advanced beyond the latest GitHub-reported timestamp the engine has actually observed for that item. The local wall clock MUST NOT be used as the baseline value in any case.
- **FR-004**: An engine write with no concurrent external activity MUST NOT cause the next probe to deep-fetch the item (the #1090 guarantee), for every write type that advances the baseline today.
- **FR-005**: Freshness MUST NOT depend on the offset between the engine host's clock and GitHub's, in either direction.
- **FR-006**: The baseline anchored to GitHub time MUST follow the observed-at ordering rule of #2348: it never moves backward, a later GitHub-observed value wins over an earlier self-write anchor, and a failed write does not advance it.
- **FR-007**: Masking protection MUST cover external writes that no webhook carries and that leave the comment total unchanged (label changes, issue-body edits, comment edits, blockedBy edges, sub-issues), not only new comments.
- **FR-008**: The mechanism MUST behave the same on a poll-only daemon (no webhooks) as on a webhook-fed daemon.
- **FR-009**: After the change, the repository MUST state whether any paused-item or cache-freshness backstop remains necessary. The #1944 backstop is already removed; any defence-in-depth retained, or the reasons none is, MUST be recorded in the as-built documentation, and any retained mechanism MUST be cheap (no extra GitHub calls on a fresh, long-parked item).
- **FR-010**: The as-built documentation of the baseline (`SelfWriteObserved`, the freshness rule and its known scope boundaries) MUST be updated to describe the new anchoring and to remove statements that no longer hold.
- **FR-011**: Automated tests MUST cover an external comment one second after an engine comment with the local clock skewed ahead and behind, asserting that the comment is seen on the next poll and that the cache does not serve a copy without it. The existing #1090 tests MUST still pass.

### Key Entities

- **Staleness baseline (`LastSeenSourceUpdatedAt`)**: The per-item value against which GitHub's issue-layer `updatedAt` is compared to decide whether the cached copy is fresh. Advanced by a deep fetch and by an engine self-write.
- **Self-write**: An engine mutation of an item (comment, label, body edit, board Status move) that GitHub records as an `updatedAt` bump on that item.
- **GitHub-reported timestamp**: A time returned by GitHub for a write or a read (`created_at`, `updated_at`), as opposed to the engine host's wall clock.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a test where an external comment is created one second after an engine comment, the next poll sees the comment in 100% of runs, with the local clock skewed ahead, behind and unskewed.
- **SC-002**: The same test for a label change, a body edit and a comment edit shows each seen by the next poll in 100% of runs.
- **SC-003**: An engine write with no external activity causes zero additional deep fetches at the next probe, for each write type that advances the baseline today.
- **SC-004**: The #1090 test suite and the existing cache-freshness tests pass without weakening any assertion.
- **SC-005**: No staleness decision depends on a comparison between the engine host's wall clock and a GitHub timestamp.

## Assumptions

- A baseline that is conservative where GitHub supplies no timestamp may cost an occasional redundant deep fetch after such a write (for example, a label add). Correctness (never masking an external write) is preferred over the zero-cost guarantee in that specific case, so long as engine writes with GitHub-returned timestamps stay free (FR-004 limits the zero-fetch guarantee to what can be known; the planning stage will decide how to keep label and status writes cheap).
- The comment-total evidence added by #2180 stays as an independent check and is not removed by this issue.
- #2348's observed-at ordering rule is the base the new baseline is built on, as agreed in the issue discussion; this issue does not redefine that rule.
- The write types in scope are those wired to `SelfWriteObserved` today (label add and remove, comment post, issue body edit, board Status move and the other callers). PR-body self-writes remain a documented separate boundary unless the research stage finds they are required to satisfy FR-001.
- The #1944 paused backstop's removal in #2180 stands. R3 of the issue is satisfied by recording the re-evaluation (FR-009), not by reintroducing a mechanism, unless the research stage finds a gap that justifies a cheap defence in depth.

## Out of Scope *(optional)*

- Changing the observed-at ordering rule itself (#2348).
- Review-feedback masking caused by the `latestReviews` cap or per-author collapse (#2160), which the gate-side repair already guards.
- CI check-run staleness, which `updatedAt` does not carry and which is handled separately.
- Making display-field writes (status line, Last activity, Last run) advance the baseline.
- Switching the daemon from polling to webhooks.

## Source References *(optional)*

- #1090 (self-write baseline), #1083 (ping-pong incident), #1944 (paused backstop, now removed), #2180 / ADR 2180 (cache freshness rule), #2348 / ADR 2348 (observed-at ordering rule), #2160 (review feedback masking)
- `docs/state-machine.md`, "Self-write staleness baseline" and "Paused deep-fetch skip"
