# ADR 2180: Self-write anchoring may absorb only what the engine can account for

## Status

Accepted. Amends ADR-044 Addendum 4 (#1090). Retires the #1944 paused backstop (ADR 2096 item 5). Keeps the ADR 2160 repair, with a stated reason.

## Context

The board cache serves an item's deep copy while the board's `updatedAt` (E) is not after a per-item baseline (L, `LastSeenSourceUpdatedAt`). `SelfWriteObserved` (ADR-044 Addendum 4, #1090) moves L to the local clock at every engine self-write so that the write's own `updatedAt` bump is not read back as an external change.

One scalar compared with one scalar cannot tell "my write at T" from "an external change at T−ε plus my write at T". Three things followed:

1. **Same-layer pre-write window.** A human issue comment lands after the last probe and before the engine's next issue write. Its `updatedAt` is below the new L, so the cache keeps serving the copy without it. This is what #1944 patched for paused items (a reply within about a second of the pause comment).
2. **Cross-layer.** E is `max(issue, project item, linked PR)`. The engine writes the issue and the project item. The L they raise also hides a later-than-fetch change to the linked PR (review, thread comment, PR comment), which the engine almost never writes. #2160 / ADR 2160 patched this for review feedback the feedback gate holds on.
3. **CI.** Research on #2180 checked PR #2172: its checks completed after its last PR comment and the PR's `updatedAt` did not move. A check completion changes no `updatedAt` at all. So `SelfWriteObserved` cannot mask CI and no timestamp rule can reveal a CI change. The real hazard is a different mechanism: `boardcache.FetchCheckRuns` served a cached PENDING set indefinitely (only a would-be-FAILED set was confirmed live), and only `settleAwaitingCIScan` primed the store (#1303).

The ADR-044 text also claimed both baseline writers share a monotonic guard. Only `SelfWriteObserved` does: `ItemDeepFetched` assigns the fetched value unconditionally, which is why a real refetch cures both masking and clock skew.

## Decision

**A cached deep copy is fresh only if all three hold** (`boardcache.cacheIsStale`):

1. **Issue layer.** The issue and project-item `updatedAt` (`Evidence.IssueLayer`: the later of `content.updatedAt` and the project item's, display-field bumps discounted, linked PR excluded) is not after `LastSeenSourceUpdatedAt`. This is the baseline `SelfWriteObserved` advances, and the only layer the engine writes.
2. **PR layer.** The linked PR's own `updatedAt` is not after the new `ItemState.LastSeenPRUpdatedAt`. It is set only from GitHub values (`ItemDeepFetched`), zeroed by `DeepFetchInvalidated`, and never touched by `SelfWriteObserved`. A self-write on the issue can therefore not hide a PR-side change.
3. **Comment evidence.** GitHub's `comments { totalCount }` for the issue equals the number of issue comments in the cached copy (comments with `FromPR == 0` and no path), unless the cached copy already holds a first page (100) of them. The engine's own comments reach the cache through `LocalCommentAdded` (idempotent by `DatabaseID`), so a self-write keeps the two equal; a human comment the issue-layer baseline hid makes them differ.

A linked PR on the board that the cache has no record of (no PR baseline and no cached PR) means a PR appeared after the fetch, so the copy is stale; otherwise an issue-side self-write could mask the new PR's reviews through the overall compare. Where a producer did not capture the layers (zero `IssueLayer`, or no PR baseline for a cached PR), the predicate falls back to comparing the overall `updatedAt` with `LastSeenSourceUpdatedAt`, as before. A zero overall `updatedAt` is still "fresh" (no signal, no hot-loop refetch). Every refetch re-anchors all baselines to GitHub's values and rebuilds the cached comment list, so no mismatch survives a poll.

**Evidence plumbing.** `ProjectItem` and `BoardProbeItem` carry `IssueLayerUpdatedAt`, `LinkedPRUpdatedAt` and `IssueCommentTotal`/`IssueCommentTotalKnown`; the board and probe queries select `comments { totalCount }` on the Issue fragment; `ItemState` mirrors them (`applyLayerEvidence`); `boardcache.Evidence` (`EvidenceFromItem`, `EvidenceFromProbe`) is what `IsItemCacheFresh` and `fetchItemDetails` consume. A webhook stamp (`localDeltaAt`) raises the issue layer as well as the overall value.

**CI is fixed where it lives, in the cache-trust path.** `FetchCheckRuns` serves a cached set on trust only when it classifies READY. PENDING and FAILED are confirmed live, and the result is written back; a PENDING set confirmed within `checkRunsConfirmTTL` (15 s) is served, so several readers in one poll cost one REST call. This subsumes the #1303 per-poll prime in `settleAwaitingCIScan`, which is removed together with `RefreshCheckRunsLive`. It is a REST read, not GraphQL budget, and only while a head's checks are in flight.

**Display-field writes no longer anchor the baseline.** The board and probe parsers already discount a project-item bump explained by a display field's own `updatedAt` (#2048/#2049), so `writeDisplayField`'s `SelfWriteObserved` suppressed nothing and only raised L. It is removed.

## Candidates considered (R1)

| | Verdict | Why |
|---|---|---|
| (a) anchor L to GitHub's `updatedAt` from the write's response | Not chosen | Only the issue-body and comment REST responses carry a timestamp; label and board-move responses do not. It also leaves the pre-write window open (L = the write's own T, still above an earlier external change). |
| (b) always refetch once after a self-write | Not chosen | Cost scales with writes and is #1090 again. |
| (c) per-layer baselines | Adopted as rule 2 | Free (the parse already had the parts); removes the cross-layer case. |
| (d) content fingerprint in the probe | Adopted narrowly as rule 3, on issue comments only | The only way to close the same-layer window without an extra read. The PR layer is covered by rule 2, so the probe needs no PR-side counts. `totalCount` misses edits to existing comments; the engine does not act on those, and an edit still moves `updatedAt`. The first-page cap fails open to the `updatedAt` rule. |

## Existing backstops (R2)

- **#1944 paused backstop: removed.** `recentBaseline`, the pre-fetch `DeepFetchInvalidated`, `ItemState.PausedBackstopBaseline` and the `PausedBackstopServed` mutation are gone. Rule 3 makes the cache itself correct, so a paused item admitted by an expired `periodic-re-eval` simply reads through the cache: free while the cache is genuinely fresh, one refetch on real staleness. #1379's cost intent ("do not pay GraphQL for parked items") is preserved because a fresh cache costs nothing. Without a board cache every read is live, so the #1379 skip stays in that mode. ADR 2096 item 5 is amended accordingly.
- **ADR 2160 repair (`repairDispatcherBlindness`): kept.** The new rule removes the `updatedAt`-masking cause, but the gate's dispatcher-versus-live comparison also guards causes the baseline cannot: the `latestReviews(first: 10)` cap and per-author collapse, and a fetch that returns a stale PR. It is bounded (two repairs per item and gap) and fail-closed, and it is the only check that compares what the dispatcher sees with what the gate holds on. Removing it would trade a proven guard for an argument. No third backstop is added: the count of per-instance cache-masking backstops goes from two to one.

## Cost (R3)

Measured with `rateLimit { cost }` against the dev daemon's board (`handarbeit` project 1, 115 items, so 2 probe pages and 2 board pages per full read):

| Query (100 items per page) | Without `comments { totalCount }` | With it |
|---|---|---|
| Probe (`probeProjectBoardQueryTemplate` shape) | 1 point | 1 point |
| Board listing (`labels(first:30)`, `closedByPullRequestsReferences(first:5)`, `milestone`) | 2 points | 2 points |

The extra GraphQL spend per poll is **0 points** at this board size (GitHub's cost rounds the added scalar-count selection to nothing). Compared with the #1014/#1090 budget concern (the daemon's average spend is on the order of 56 points per interval, dominated by deep fetches and workers), the listing change is not measurable.

The behavioural cost is refetches: an engine write on the PR side (`pr.go`, `prcreate.go`, pushes) previously hid behind an issue-side write's anchor and now costs one deep fetch at the next probe, because the PR layer is never self-write anchored. That is about one per stage run, on an item that is deep-fetched after a stage anyway. Self-writes on the issue layer cost nothing (`TestCacheFreshness_SelfWritesAlone_NoRefetchLoop`: 20 rounds of label and comment writes, zero refetches). CI adds REST reads while a head's checks are PENDING, bounded to one per head per 15 s.

## Consequences

- An external issue comment, PR review, thread comment or PR comment whose `updatedAt` precedes an engine self-write is seen at the next probe, with no restart. Completed CI is seen by the next `readClient.FetchCheckRuns`.
- Rule 3 relies on the cached issue comment list matching GitHub's count. A path that posts an issue comment without writing it through the cache causes one refetch, which rebuilds the list, never a loop.
- `SelfWriteObserved` stays wall-clock (`time.Now()`) and issue-layer only. Future self-writes must not anchor the PR layer.
- Clock skew no longer matters for the PR layer; for the issue layer a skewed-ahead local clock can still hide an external change within the skew, which rule 3 now catches for comments.
- Tests: `boardcache/freshness_test.go` (rule table, no-loop, settle), `engine/cache_freshness_test.go` (human comment, PR thread comment and completed CI each fail on the single-baseline rule; self-writes alone cause no refetch), `internal/itemstate` (`LastSeenPRUpdatedAt` is never self-write anchored), `tests/sim/cache_freshness_test.go` (end-to-end plumbing).
