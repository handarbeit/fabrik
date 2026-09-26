# ADR 1871: Merge-Train Live-Status Resume Guard

## Status

Accepted

## Context

On `verveguy/concept-maps` (2026-09-26), 3 of 25 merge-train landings ran twice: a second Done move, close and `Landed via …` comment 5–6 seconds after the first, on both the integration-PR path and the singleton fast path.

`poll()` reads the board once and reaches `handleMergeTrainBatch` seconds later with that same object. In between, the previous worker moves the member to Done and `finishTrain` releases the in-flight marker (ADR-067), so `mergeTrainInFlight.LoadOrStore` succeeds and a fresh worker forms a batch from the pre-landing snapshot. The two "resume an interrupted landing" branches — `completeDeferredLanding` (via `reconstructTrainState` Route 1) and the `pr.Merged` branch of `trySingletonFastPath` — treat "merged PR + member still Queued" as an interrupted landing, which is exactly what the stale snapshot looks like. Their own `Status == "Done"` and `fabrik:awaiting-landing-verification` guards also read snapshot fields, so they cannot catch it.

## Decision

A live Status read at the two resume decision points, via `liveLandingState`:

- Uses `e.client.FetchProjectItemStatus(item.ItemID)` (falling back to `LookupIssueProjectItem`), never `e.readClient`, `item.Status` or `item.Labels` — the live-read discipline of ADR-1773.
- **Skip only on positive evidence:** a non-empty Status that is not the holding stage. A holding Status resumes (crash between merge and Done; the `fabrik:awaiting-advance` retry). An empty Status resumes (no positive evidence).
- **Fail closed on a read error:** defer this poll. Both resume branches re-run every poll, so this delays but never strands a member; failing open would keep the duplicate exactly when reads are flaky.
- Per-member on the integration path (a merged batch PR can have members in mixed states); `return true` ("disposition decided", no trial) on the fast path.
- **Also at fresh batch formation** (added in Validate review): `dropLiveLandedMembers` in `prepareTrainWorker`, after `fetchTrainMembers` and before `admitTrainMembers`, drops `liveMoved` members and excludes `liveReadFailed` members from that batch (retried next poll). Without it, a stale snapshot listing several just-landed members reaches a fresh batch whenever no merged marker PR matches (e.g. consecutive singleton fast-path landings): `fetchTrainMembers` accepts a merged PR and `admitTrainMembers` sees green CI, the trial passes because the members' heads are already in base, and `landMergeTrainBatch` re-lands each one. The #882 duplicate only surfaced on the fast path's resume branch because that batch happened to be a singleton.

`Engine.SetMergeTrainLandingGuardDisabledForTest` restores the pre-#1871 behaviour so tests can show the duplicate with the guard off. `simgh.Sim.LagBoardStatus` reproduces the lagging read model in the sim.

## Alternatives considered

1. **In-memory "landed this process" record per train key.** Zero API cost and clears on restart, but every landing path must remember to write it (`finishSingletonFastPathLanding` does not call `markCreditedLanding`), so a missed write silently reintroduces the bug, and it cannot cover another instance sharing the board.
2. **Exclude just-landed members at batch formation *instead of* guarding the resume branches.** A live read per member in `prepareTrainWorker` does not cover restart recovery paths that never reach formation (Route 1, the fast path's `pr.Merged` branch), so it cannot replace them. It is adopted only *in addition* to the resume-branch guards (see Decision).

## Consequences

- Extra live reads: a few at the resume branches (only when a merged PR has already been found), plus one per member at every fresh batch formation — including polls where the batch is merely pending — since formation cannot know in advance whether the snapshot is stale.
- The restart-recovery contract (ADR-059 D5 / FR-2) is unchanged.
- The Done branches inside `landMergeTrainBatch` and `finishSingletonFastPathLanding` are reachable only if Status changes after the guard's read.
- A member a human moved to another column while its PR is merged is treated as landed and skipped.
- **Known gap:** `landSingleton`'s dedicated landing PR has no batch marker, so Route 1 never matches it; a stale-Queued member from that path is untraced. Bisection, `landOneAtATime` and `landGreenBatch` share the snapshot source but are not entered from the resume branches. To be filed as a follow-up.

See `docs/state-machine.md` §6.28.
