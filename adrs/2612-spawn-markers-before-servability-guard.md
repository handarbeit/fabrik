# ADR 2612: Spawned-Child Markers Are Read Before the Servability Guard

**Date**: 2026-11-24
**Status**: Accepted
**Amends**: [ADR 1419](1419-cross-repo-spawn-servability-and-midflight-recognition.md) (the all-or-nothing servability guard), [ADR 1583](1583-spawn-children-resume-safe-retry.md) (markers for unserved targets)

## Context

A `repo:`-scoped instance asked to spawn a child into a repo it does not serve
pauses the parent ("Spawn target … is not served by this Fabrik instance"). The
supported recovery is manual: the operator creates the child in the target repo
and links it as a blocker. That recovery never worked: once the child closed and
the parent returned to Implement, `spawnChildren` re-parsed the immutable Plan
comment and ran the servability guard over **every** block's repo *before* it
read the `fabrik:spawned-child:<i>:<n>` markers (ADR 1583), so nothing could
tell the engine that a block's child already existed, and the parent re-paused
for ever (community report #2601: WebBrain#548 and SemanticHistory#283).

## Decisions

1. **Markers are read first.** For a resumable origin the markers (and the live
   refresh ADR 1583 already performs when one exists or the label list hits the
   shallow cap) are read before the guard. A marker whose index is outside
   `1..len(blocks)` is not a resume record; it is ignored, as before.
2. **The guard applies per block that still needs creating.** A block with a
   marker needs no creation, so its repo need not be served. A block without one
   still pauses the whole batch before any write, so "Created so far: none" stays
   accurate. The guard remains a scope-check, not a route-finder. A non-resumable
   origin (Review/Validate mid-flight) reads no markers, so every block there
   still needs servability: its block index is not stable across a re-dispatch.
3. **A marked child in an unserved repo is resolved read-only.** `FetchProjectItem`
   in the block's own repo; open or closed counts as created. Nothing is written
   to that repo or to this instance's board for it: no `CreateIssue`, no board
   lookup or add, no `fabrik:sub-issue`, no inherited labels, no Status
   placement. An unresolvable marker keeps ADR 1583's "recorded child can no
   longer be resolved" pause (it is not "needs creating", so the servability
   pause never masks it). No bare clone is made for such a repo.
4. **The parent-side edge is still ensured.** The live `blockedBy` check runs as
   before and `AddBlockedByIssue` is called only when the edge is absent. That is
   a write on the *parent*, not the unserved repo; if it fails the parent pauses
   as for any failed link. Where the edge already exists no write happens, so
   nothing is reported as a problem.
5. **Sibling `DEPENDS_ON` is skipped for a remote-marked dependent.** The edge
   would be written on a child in a repo this instance does not serve. It is
   logged, not paused; the operator who created the child owns that edge.
6. **Write-through edge state follows the resolved child.** A resumed child's
   `BlockedByEdgeAdded` carries `CLOSED` when the child is closed (`OPEN`
   otherwise; fresh children stay `OPEN`), so a closed hand-made child does not
   re-block the parent through a stale `OPEN` edge in the Store.
7. **R4: the pause comment names the manual path.** Create the child in the
   target repo, add `fabrik:spawned-child:<i>:<n>` to the parent, then remove
   `fabrik:paused`; the comment lists each unserved block that needs a marker. The
   existing "not served by this Fabrik instance" wording is kept.
8. **R5: a title-matching `blockedBy` edge is NOT accepted.** Only the marker is
   evidence that a child exists. `gh.Dependency` carries no title, so matching
   would need one extra `FetchProjectItem` per cross-repo edge; it is ambiguous
   with duplicate or edited titles, and it re-opens the path ADR 1583 rejected
   (no title-search primitive). The manual path in the pause comment is the
   supported route.

## Residual

- The marker has no repo or title check: an operator can point it at an unrelated
  issue (or a PR number, which `FetchProjectItem` also resolves). This is the same
  trust as the engine-written markers.
- A closed child whose edge is written as `CLOSED` relies on the Store's own
  blocker state once the child is known to it; `checkDependencies` still reads
  `blockedBy` live at decision points.

## Verification

`engine/spawn_marker_first_test.go` (including the neutralised run via
`SetSpawnMarkerFirstDisabledForTest`) and the sim twin in
`tests/sim/cross_repo_spawn_test.go`.
