# ADR 1583: Spawn-Children Resume-Safe Retry via Durable Per-Child Markers

**Date**: 2026-09-13
**Status**: Accepted
**Amends**: [ADR 048: Engine-Side Sub-Issue Spawning via blockedBy](048-spawn-child-engine-side.md)

## Context

`spawnChildren` (`engine/spawn.go`) creates each Plan-declared child issue in a
per-child sequence: `CreateIssue` → `AddProjectV2ItemById` →
`AddBlockedByIssue` → `AddLabelToIssue("fabrik:sub-issue")` (best-effort) →
`UpdateProjectItemStatus` (best-effort, already recoverable via
`fabrik:awaiting-placement`, ADR 062) → yolo/cruise label copy
(best-effort). A failure at `CreateIssue`, `AddProjectV2ItemById`, or
`AddBlockedByIssue` pauses the parent hard (`fabrik:paused`), with the
operator's documented recovery path being "remove `fabrik:paused`, then
re-advance to retry."

ADR 048 explicitly scoped this as "v1 does not skip already-created children
on retry" — `fabrik:children-spawned`, the only idempotency guard, is applied
once, only after the *entire* batch (including sibling `DEPENDS_ON` wiring)
succeeds. Following the documented recovery path therefore re-ran
`spawnChildren` from scratch: the child already created before the failure
was invisible to the retry, which created a **second** child issue with the
same title. Reproduced deterministically in the sim bed
(`tests/sim/restart_recovery_test.go`'s
`TestRestartRecovery_KillDuringSpawnSequence` and
`tests/sim/partial_mutation_test.go`'s
`TestPartialMutation_SpawnChildren_ProjectAddFails`), which faulted
`AddBlockedByIssue` and `AddProjectV2ItemById` respectively and confirmed the
duplicate.

`itemstate.Store` is in-memory only and does not survive a restart — and the
repro scenario restarts the engine between the fault and the operator's
un-pause — so any fix needs state that lives on GitHub itself.

## Decision

`spawnChildren` now writes a durable **per-child resume marker** on the
**parent** issue immediately after each child is created, and the three
risky per-child steps (`CreateIssue`, `AddProjectV2ItemById`,
`AddBlockedByIssue`) become skip-on-resume instead of always re-running.

### Marker shape: one label per child, encoding only `blockIndex:childNumber`

```
fabrik:spawned-child:<blockIndex>:<childNumber>
```

`blockIndex` is the 1-based position in the Plan-declared spawn block list
(matching the existing `DEPENDS_ON` convention); `childNumber` is the created
child issue's number. The child's `owner/repo` is deliberately **not**
encoded — it's always re-derivable from `blocks[blockIndex-1].Repo`, which is
itself deterministically reparsed from the same immutable Plan comment on
every retry. This keeps the label comfortably inside GitHub's 50-character
label-name limit regardless of repo name length (`fabrik:spawned-child:` is
21 characters; even a 4-digit block index and a 10-digit issue number stay at
36 — see `TestSpawnChildLabelLength`).

### Scope: gated to origins where `blocks` reparses deterministically (`resumable`)

`spawnChildren` takes a `resumable bool` parameter that gates the entire
mechanism above — the marker is read, written, and cleaned up only when
`resumable` is true. It is true for `preImplement` and
`recoverMissingPlanComment` (both re-derive `blocks` from the same immutable,
already-posted Plan comment on every call, so `blockIndex` is guaranteed
stable across a retry) and **false** for the Review/Validate mid-flight spawn
origin (`engine/item.go`, ADR-1419).

This distinction was missed in the original design of this ADR, which assumed
`blocks[blockIndex-1].Repo` was "deterministically reparsed from the same
immutable Plan comment (or, for a Review/Validate mid-flight spawn, the same
dispatch's own `output`) on every retry" — treating the two origins as
equivalent. They are not: a mid-flight spawn's `blocks` comes from
`ParseSpawnBlocks(output)`, where `output` is that one Claude dispatch's own
fresh generation. A retry of a *failed* mid-flight spawn (operator removes
`fabrik:paused`, re-advances) does not replay stored content — since the
stage never completed, it redispatches Claude, which is not guaranteed to
reproduce the same block count, order, repo, or title. A `blockIndex`-keyed
marker would then risk resuming an unrelated, already-created child under a
same-numbered but semantically different block on the new dispatch — silent
mis-wiring (the wrong child linked to the wrong block), which is strictly
worse than the duplicate-child bug this ADR set out to fix. Caught in review
on PR #1708 before merge.

With `resumable` false, the mid-flight origin's behavior is unchanged from
before this ADR: every block always takes the fresh-`CreateIssue` path, no
marker is ever read, written, or left behind, and this origin's own
(already-documented, lower-severity) duplicate-on-retry exposure is carried
forward rather than traded for the worse failure mode above. Closing that
exposure for the mid-flight origin — e.g. via a marker keyed on stable block
*content* (repo + title) rather than position, which tolerates reordering but
not a genuinely different title on retry — is a candidate follow-up, not
attempted here.

Rejected alternatives:
- **A machine-readable progress comment.** No existing convention in this
  codebase parses structured state back out of a comment body, and a shared
  comment invites read-modify-write races across the marker writes this fix
  makes per-child. A label is this codebase's own established "state" idiom
  (CLAUDE.md's "Labels are state"), with `fabrik:credited-pr:<N>` as a direct
  single-value precedent; a list of small integers — one label per child — is
  exactly what labels are good at.
- **Title-matching.** No `GitHubClient` primitive searches issues by title
  today, and `FindPRForIssue`'s own doc comment records a deliberate prior
  move away from the rate-limited `/search/issues` endpoint (30 req/min vs.
  core REST's 5000/hr) for exactly this kind of lookup. A durable marker
  avoids that cost entirely and doesn't depend on title uniqueness within a
  batch.

### Resume-cost model: the happy path is unchanged

A block with no prior marker takes the exact same call sequence as before
this ADR. The extra "check before mutate" calls
(`LookupIssueProjectItem`, a live `BlockedBy` freshness check) only fire for
a block recovered from a marker — i.e., only on an actual resume. This
sidesteps an unconfirmed assumption (whether `AddProjectV2ItemById` and
`AddBlockedByIssue` are safe to call twice against real GitHub) without
touching the happy path's behavior or cost at all: check first, only when
resuming; call directly, exactly as before, otherwise.

### Placement is skipped on resume only when already placed

`UpdateProjectItemStatus` (the Specify/first-processing-stage placement) is
**not** blindly re-run for a resumed child. `LookupIssueProjectItem`'s
returned Status is consulted: a non-empty Status means the child was already
placed — possibly it has since progressed under its own pipeline while the
parent sat paused — and placement is left alone to avoid clobbering that
progress. An empty Status (added to the board but never placed) still gets
the normal placement attempt.

### Marker write is best-effort, not blocking

The marker is written via `addLabelChecked` (mirroring
`markCreditedLanding`'s precedent for `fabrik:credited-pr:<N>`) so its
failure is logged, but does not abort the block. A dropped write regresses
only that one child to this ADR's pre-fix behavior if a *later* step in the
same attempt also fails — a narrow, logged residual risk, strictly smaller
than the unconditional bug being fixed.

### `BlockedBy` is refreshed live, once per call, only when resuming

Mirrors `recoverMissingPlanComment`'s existing live-read-with-cooldown
pattern: `BlockedBy` is a "deep field" the bulk board fetch never populates,
so a resume must not trust a possibly-stale (or entirely absent) snapshot
when deciding whether a child is already linked. `refreshForSpawnResume`
performs this read, gated by the same cooldown idiom
(`recoverMissingPlanComment`'s `spawn-recovery-deferred`, this function's own
`spawn-resume-deferred`); on a live-read failure or an active cooldown it
returns `errPreImplementDeferred` **without pausing** — a transient read
failure defers to the next poll exactly like the existing Plan-comment
recovery already does.

### Markers are cleaned up on success

Once the full batch (including the sibling `DEPENDS_ON` wiring pass)
succeeds, every marker this call knows about — whether inherited from a
prior attempt or newly written this attempt — is removed via a best-effort
`RemoveLabelFromIssue`, before `fabrik:children-spawned` is applied. Steady
state carries none of these markers; they exist only transiently during an
interrupted or in-progress spawn.

### A recorded-but-missing child pauses loud, naming the exact label to remove

If a marker names a child that can no longer be resolved (`FetchProjectItem`
errors, or returns no usable node ID — e.g. genuine deletion), the parent
pauses with an instruction naming the *specific*
`fabrik:spawned-child:<blockIndex>:<childNumber>` label to remove to force
re-creation of that one block, or to retry if the failure was transient. No
attempt is made to auto-distinguish "genuinely deleted" from "transient fetch
error" — kept simple and explainable, at the cost of one manual label removal
in the deletion case.

### Pause messages reworded

The four affected pause messages (`CreateIssue`, `AddProjectV2ItemById`,
`AddBlockedByIssue`, and the sibling `DEPENDS_ON` wiring failure) drop the
now-inaccurate "manually close any orphaned children" instruction — a
retried spawn no longer orphans anything it can resume — in favor of stating
that already-created children are tracked and will be reused automatically.

### Explicitly out of scope: sibling `DEPENDS_ON` wiring's own retry-idempotency

The wiring pass that links sibling `DEPENDS_ON` edges after every child in a
batch exists (`spawn.go`'s second loop) is left unchanged: a raw
`AddBlockedByIssue` re-call on every retry, exactly as before this ADR.
Neither reproduced defect exercised this pass, GitHub's `addBlockedBy`
mutation is an edge-add very likely (though unconfirmed against real GitHub)
safe to repeat, and building resume-tracking for it would require live-
fetching every child's own `BlockedBy` state — a materially larger change for
an untested failure mode. A follow-up issue is the right vehicle if this is
ever observed to duplicate an edge in practice.

### Also out of scope: an unconditional settle scan

This fix makes the existing *manual* recovery path (operator removes
`fabrik:paused`, re-advances) safe and non-duplicating. It does not add a new
ADR-1270-style settle scan that would resolve a stuck spawn without any
operator action at all — the originating issue's own suggested fix direction
and the Specify stage both scoped this to retry-safety, not full automation.
Nothing about this design would block adding one later.

## Rationale

### Why a marker on the parent rather than the child?

`fabrik:awaiting-placement` (ADR 062) is a marker on the *child* because it
records a fact about that child's own state (its board placement is
outstanding) that a settle scan can act on independent of the parent. This
fix's marker instead records a fact the *parent's own spawn loop* needs on
its next attempt (which blocks are done) — the parent is the actor that
consumes it, so it lives there. The two mechanisms are orthogonal by
construction: a resumed child can carry both `fabrik:awaiting-placement` (its
own placement is still outstanding) and be resolved via this ADR's marker
(it's already created) at the same time, with no coordination needed between
them.

### Why not verify `AddProjectV2ItemById`/`AddBlockedByIssue` idempotency instead of checking first?

Confirming real GitHub's behavior on a duplicate call was out of reach for
this fix (no live GitHub access in this environment, and the sim's own model
of these two calls, while idempotent, is not proof of real GitHub's
behavior). Checking first via `LookupIssueProjectItem` and a live `BlockedBy`
read avoids the assumption entirely, at the cost of one extra read per
resumed block — paid only on an actual resume, never on the happy path.

## Consequences

- Following `spawnChildren`'s own printed recovery instruction ("remove
  `fabrik:paused`, then re-advance to retry") no longer creates a duplicate
  child at any of the three previously-unsafe steps — for the `resumable`
  origins (Plan/`preImplement`). The Review/Validate mid-flight origin is
  unaffected by this fix (`resumable=false`): its pre-existing
  duplicate-on-retry exposure is unchanged, not worsened, not fixed.
- The parent temporarily carries one `fabrik:spawned-child:<i>:<n>` label per
  created child during an in-progress or interrupted spawn, for a `resumable`
  origin only; steady state (spawn complete) carries none. A mid-flight
  origin's spawn never writes this marker at all.
- `docs/state-machine.md` §6.7/§6.7.2's prior statement that there is "no
  change to `spawnChildren`'s own idempotency guard" for individual children
  is superseded by this ADR — updated in the same change set.
- `sim/simgh`'s `FetchProjectItem` previously required board membership to
  resolve an issue at all — a fidelity gap relative to real GitHub's
  `Client.FetchProjectItem` (a plain REST issue GET with no board
  precondition), surfaced by exercising this fix's `TestPartialMutation_SpawnChildren_ProjectAddFails`
  scenario (a child created but not yet added to the board) against the sim
  bed. Fixed in `simgh` alongside the engine fix, per this project's
  fidelity-drift policy.
