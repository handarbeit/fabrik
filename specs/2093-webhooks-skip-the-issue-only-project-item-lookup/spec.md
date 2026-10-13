# Feature Specification: Skip PR-comment project-item lookups and negative-cache off-board issues in webhook Status refresh

**Feature Branch**: `fabrik/issue-2093`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "## Problem

Report #1910: `applyLayer1StatusRefresh` (`engine/reconcile.go:124`), called from both webhook delta closures (`engine/poll.go:471` webhooks, `:581` Hookdeck), parses `issue.number` from `issues`/`issue_comment` payloads without checking `issue.pull_request`.

For a PR conversation comment, on an item-ID cache miss it calls `LookupIssueProjectItem` (`reconcile.go:158`), an `repository.issue(number:)` query that cannot resolve a PR number. That logs a warning (`reconcile.go:160`) on every PR comment, including Fabrik's own PR posts and bot reviews. The reporter measured about 546 calls in 20 hours.

The defect is slightly broader: there is no negative cache, so every event on an off-board issue in a watched repo repeats the lookup too. It returns "" silently (`reconcile.go:163`) but still costs a call.

## Requirements

**R1.** `applyLayer1StatusRefresh` returns early, with no GraphQL call and no warning, when the payload's `issue.pull_request` is present.

**R2.** A short-TTL in-memory negative cache (Plan picks the TTL, e.g. 10 minutes) for `(repo, number)` lookups that resolved to "not on the board", so repeated events on off-board issues don't repeat the call. Moving an issue onto the board must not be delayed past the TTL. Plan confirms that the board-add path (`projects_v2_item`) bypasses or clears the cache.

## Scope

- In scope: the early return, the negative cache, tests with payload fixtures.
- Out of scope: resolving PR comments to their linked issue in the cache delta (`boardcache/delta.go:333-368`); that is a separate latency improvement.
- Mention #1910 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1910. This issue's own number gets `Closes`.

## Acceptance

- Unit tests: an `issue_comment` payload with `issue.pull_request` makes no `LookupIssueProjectItem` call and logs no warning. A second event for an off-board issue within the TTL makes no call. A board-add clears the negative entry."

## Background

Fabrik's webhook path performs an opportunistic "Layer 1" Status refresh for every `issues` and `issue_comment` event: it reads the issue number from the payload and, when the board cache has no project-item ID for that issue, asks GitHub to look the issue up on the project board.

GitHub delivers conversation comments on pull requests as `issue_comment` events too, with the PR's number under `issue` and an `issue.pull_request` marker. The refresh does not look at that marker, so for every PR comment (including Fabrik's own PR posts and bot reviews) it issues an issue-by-number query that can never resolve a PR number. Each such call costs API budget and logs a warning. The community report #1910 measured roughly 546 of these calls in 20 hours.

The same code path has a second, broader cost: when a lookup resolves to "this issue is not on the board", nothing remembers that. Every later event on an off-board issue in a watched repository repeats the call, and only a silent early return hides it.

This issue removes both sources of wasted calls without changing behavior for issues that are on the board.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - PR comments no longer trigger an issue lookup (Priority: P1)

An operator running Fabrik with webhooks (direct or Hookdeck) sees PR conversation comments arrive, from humans, bot reviewers and Fabrik itself. These events should not cost a project-item lookup or produce a warning in the log.

**Why this priority**: This is the reported defect (#1910): a steady stream of guaranteed-useless API calls and warning noise, proportional to PR comment volume.

**Independent Test**: Feed the webhook Status refresh an `issue_comment` payload whose `issue` carries a `pull_request` object, with an empty item-ID cache and a ready project; verify the lookup client is never called and no warning is logged.

**Acceptance Scenarios**:

1. **Given** an `issue_comment` payload with `issue.pull_request` present and no cached item ID for that number, **When** the Status refresh runs, **Then** no project-item lookup or status fetch is made and no warning is logged.
2. **Given** an `issues` or `issue_comment` payload without `issue.pull_request` for an on-board issue, **When** the Status refresh runs, **Then** behavior is unchanged (fast path or fallback lookup as today).
3. **Given** the same PR-comment payload delivered through the Hookdeck path, **When** the refresh runs, **Then** the outcome is identical, because both delta paths share the one refresh function.

---

### User Story 2 - Repeated events on an off-board issue do not repeat the lookup (Priority: P2)

An issue in a watched repository that is not on Fabrik's project board generates several events (comments, edits, label changes). After the first lookup establishes it is off-board, further events within a short window should not repeat the lookup.

**Why this priority**: It removes the broader cost class the report identified, but its volume is lower than PR comments and the fix is a bounded cache with a freshness risk to manage, so it follows the primary fix.

**Independent Test**: Deliver two events for the same off-board `(repo, number)` within the cache lifetime; verify exactly one lookup occurs. Advance time past the lifetime and deliver a third; verify a second lookup occurs.

**Acceptance Scenarios**:

1. **Given** a lookup for `(repo, N)` resolved to "not on the board", **When** another `issues`/`issue_comment` event for the same `(repo, N)` arrives within the TTL, **Then** no lookup is made.
2. **Given** the TTL has elapsed since the off-board result, **When** another event for that issue arrives, **Then** a fresh lookup is made and the result re-evaluated.
3. **Given** a negative entry exists for `(repo, N)`, **When** the issue is added to the board (a `projects_v2_item` add event), **Then** the negative entry is cleared or bypassed, so the issue is recognized as on-board without waiting for the TTL.
4. **Given** a lookup fails with an error (not a clean "not on board" answer), **When** the next event arrives, **Then** a lookup is attempted again; errors are never cached as "off-board".
5. **Given** a negative entry for `(repo, A)`, **When** an event for a different issue `(repo, B)` or the same number in a different repo arrives, **Then** it is evaluated independently.

---

### Edge Cases

- A payload with `issue.pull_request` present but explicit `null`: treated as absent (a PR marker is a non-null object).
- An unparseable payload or one with no repository name / issue number: unchanged existing handling (log or return as today).
- Cache paused or project not yet bootstrapped: unchanged early returns; nothing is written to the negative cache in those states.
- An issue on the board whose item ID is already cached: the fast path is unchanged and never consults the negative cache.
- An issue added to the board while a negative entry is live, through a path that produces no `projects_v2_item` event the engine sees: bounded by the TTL.
- Engine restart: the negative cache is in memory only and starts empty.
- Concurrent webhook deliveries for the same issue: the cache must be safe for concurrent access.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When an `issues` or `issue_comment` payload carries a non-null `issue.pull_request`, the Layer 1 Status refresh MUST return without any GraphQL call (fast path or fallback lookup) and without logging a warning.
- **FR-002**: The PR-marker check MUST apply identically to both webhook delta paths (direct webhooks and Hookdeck), which both call the Layer 1 refresh.
- **FR-003**: When the fallback lookup for `(repo, number)` returns a clean "not on the board" result, the engine MUST record a negative entry in an in-memory cache keyed by `(repo, number)` with an expiry.
- **FR-004**: While a negative entry for `(repo, number)` is unexpired, the Layer 1 refresh MUST NOT make the fallback lookup for that issue.
- **FR-005**: After a negative entry expires, the next event for that issue MUST perform a fresh lookup.
- **FR-006**: A board-add for an issue (a `projects_v2_item` add or equivalent registration of the issue's item ID) MUST clear or bypass that issue's negative entry, so adding an issue to the board is never delayed by the TTL.
- **FR-007**: A lookup that fails with an error MUST NOT create a negative entry, and the existing warning for lookup errors is retained.
- **FR-008**: The negative cache MUST be bounded in lifetime (TTL chosen at Plan, on the order of minutes) and safe for concurrent use, and MUST NOT be persisted across restarts.
- **FR-009**: Behavior for on-board issues (cache hit fast path, successful fallback registration of item ID and Status) MUST be unchanged.
- **FR-010**: Tests with payload fixtures MUST cover: PR-comment payload makes no lookup and logs no warning; a second off-board event within the TTL makes no lookup; a board-add clears the negative entry.

### Key Entities *(if applicable)*

- **Negative entry**: A record that `(repo, number)` was found not to be on the project board, with an expiry time. In memory only.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With webhooks enabled, PR conversation comments produce zero project-item lookup calls and zero "layer1 fallback lookup failed" warnings attributable to PR numbers.
- **SC-002**: For an off-board issue receiving N events within the TTL, at most one lookup call is made (versus N today).
- **SC-003**: An issue added to the board is recognized on the board without waiting for the negative-cache TTL to elapse.
- **SC-004**: The unit tests named in FR-010 exist and pass under the standard `go test -race ./...` run, and no existing test for on-board behavior changes outcome.

## Assumptions

- The Layer 1 refresh is the only call site that issues this issue-by-number fallback lookup from webhook events; other lookup callers are unaffected.
- A PR payload is identified by the presence of a non-null `issue.pull_request` field, which is how GitHub marks PR conversation events in `issues`/`issue_comment` deliveries.
- The TTL value is a Plan decision; a default around 10 minutes is the working assumption, trading a small bound on staleness for a large reduction in calls.
- Cache hits on an item ID (the board-add path registering the ID) already bypass the fallback lookup, so clearing the negative entry on board-add is hygiene and a guarantee rather than the sole mechanism; Plan confirms the exact mechanism.
- No new user-facing configuration is needed; the TTL is an internal constant.
- No operator-visible documentation change beyond the as-built state-machine/webhook notes, if those describe the Layer 1 refresh.

## Out of Scope *(optional)*

- Resolving a PR comment to its linked issue in the cache delta (`boardcache/delta.go`); that is a separate latency improvement.
- Changing how `projects_v2_item` events or the periodic reconcile populate the cache.
- Caching positive lookups or errors.
- Persisting the negative cache across restarts.
- Any change to GitHub-side behavior or to community report #1910's status (it stays open until a release containing the fix ships; the PR mentions #1910 by bare number only and uses no closing keyword for it).

## Source References *(optional)*

- Community report #1910 (measured ~546 calls in 20 hours)
- `engine/reconcile.go` `applyLayer1StatusRefresh`; call sites in `engine/poll.go` (webhook and Hookdeck delta closures)
