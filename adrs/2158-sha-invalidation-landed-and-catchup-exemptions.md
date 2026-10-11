# ADR 2158: SHA-invalidation scan exempts landed items and the engine's pure catch-up head

## Status

Accepted

## Context

`settleSHAInvalidationScan` (§2.16 of `docs/state-machine.md`) removes `stage:Validate:complete` when the
linked PR's head differs from the SHA recorded at Validate completion (#1225: a head that Fabrik never
validated must not be auto-merged). It skipped an item only for an empty recorded SHA, an unchanged head or
an in-flight worker. It never asked whether the work had already landed, and it could not tell an
engine-authored head move from an external one.

The merge train's singleton catch-up (#2044, ADR-2044) merges the base into a queued member's own branch,
which legitimately moves the head, and then lands the member's own PR. About one poll after the landing the
scan stripped `stage:Validate:complete` from the closed, Done item and logged that Validate would
re-dispatch (4 of 4 catch-up landings on the dev daemon, 2026-10-11). Cosmetic for a landed item, but the
same mechanism would genuinely re-run Validate for an open item that is still settling.

## Decision

1. **Landed or terminal items are exempt, from cached facts only.** `validateItemLanded` skips an item that
   is closed, in a `cleanup_worktree` stage, has a merged linked PR, or carries
   `fabrik:awaiting-landing-verification` / `fabrik:credited-pr:<N>`. The shallow board item and the store
   snapshot are both consulted and ORed. Where they disagree the result is "landed", i.e. skip — the safe
   polarity for a closed/Done/merged item — so no live read is needed (satisfying "at most one live read,
   only on disagreement" with zero). The landing label alone is not relied on: the singleton fast path adds
   it only after the Done move, so closed/Done/merged cover that window.
2. **The engine's own pure catch-up head is not external.** `headIsPureCatchUp` reuses `catchUpMarkerCached`
   and so inherits `dropCatchUpFeedback`'s trust rules exactly: only the engine's own login counts, an empty
   login or head fails closed, the commit trailer is never trusted. The head is exempt only when the marker
   names the live head and is `pure=true`. On a match the scan re-records the live head with the existing
   `ValidateCompletedAtSHA` mutation (no new mutation, same in-memory durability as the original) so a later
   real push is still detected.
3. **A marker read error defers.** The scan skips the item for that poll and retries next poll, unlike
   `dropCatchUpFeedback`, which fails toward actionable. Here, acting on an unreadable fact is the
   defect being fixed; the cost of waiting one poll is a one-poll-late detection of a real external push.

## Alternatives rejected

- **A live `FetchPRMerged` on every invalidation.** Adds a read to the genuine force-push path for no gain.
- **A `prev=<sha>` field in the marker, a `FetchCommit` first-parent check, or tracking head history in the
  store**, to close the chain gap below. Each is a larger change (a wire-format change that Pruefer's copy of
  the regex and the contract test must follow — ADR-2066; a new `GitHubClient` method plus sim support; more
  state) for a narrow residual. Left as an additive follow-up.

## Consequences

- **Known gap.** The marker names the head but not its predecessor, so an external push P followed by an
  engine catch-up H2 on the same open item passes the marker check and P goes undetected. It needs an
  external push and an engine catch-up on the same settled item.
- Items with `pure=false` (conflict-resolved) catch-ups are not exempt, because that head holds content
  Validate has not seen.
- Other engine-authored head movements (auto-repair, rebase reinvokes) are not exempted.
- `channel_settled.go`'s `validate-settled` check needs no change: re-recording makes
  `ValidateCompletedSHA == HeadSHA`.
- Tests: `engine/sha_invalidation_test.go` (each signal, marker negatives, read error) and
  `tests/sim/validate_sha_catchup_test.go` (landed catch-up keeps the label; open pure catch-up is not
  re-validated, a following external push is). The seams `SeedValidateCompletedSHAForTest` and
  `SeedLinkedPRHeadForTest` exist because the sim never runs Validate and has no webhook feed. The landed and
  open-item sim scenarios were verified to fail with both exemptions disabled.
