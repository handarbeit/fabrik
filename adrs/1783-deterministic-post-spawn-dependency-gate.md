# ADR 1783: Deterministic Post-Spawn Dependency Gate

**Date**: 2026-09-18
**Status**: Accepted
**Amends**: [ADR 048: Engine-Side Sub-Issue Spawning via blockedBy](048-spawn-child-engine-side.md)

## Context

ADR 048 specified that once `spawnChildren` links a parent's newly-created
children via `blockedBy` and applies `fabrik:children-spawned`, `checkDependencies`
"detects the new blockedBy edges on the next evaluation cycle" and blocks the
parent until they close. `docs/state-machine.md` §6.7 and `docs/USER_GUIDE.md`
repeated this as settled fact. It was true by design, but not true by
construction: nothing actually verified the edge was visible to a subsequent
dispatch decision, only that `spawnChildren` itself skipped the Claude
invocation for the dispatch that performed the spawn.

Reported in #1659 (v0.0.80, two epics affected): Plan decided decomposition,
children were filed, and within 1–2 minutes Implement built the whole parent
as a monolith anyway — competing with its own children and burning
Review/Validate cycles for days before `FABRIK_MAX_REVIEW_CYCLES` paused it.

Tracing the mechanism (`engine/spawn.go`, `engine/dependencies.go`,
`engine/poll.go`, `boardcache/boardcache.go`) showed the guarantee was a
**chain of independently-evolved subsystems**, not a single gate:

1. `spawnChildren` mutates GitHub (creates children, links `blockedBy`,
   applies `fabrik:children-spawned`) and returns; the calling dispatch skips
   Claude for itself.
2. The `fabrik:children-spawned` label-add produces a `LabelsChanged` Store
   event, which populates `cycleSet` for the *next* poll and wakes the poll
   loop early.
3. On the next poll, `cycleSet` membership lets the item bypass its
   `periodic-re-eval` cooldown and get a real `FetchItemDetails` call — the
   only thing that refreshes the deep field `BlockedBy` in the Store.
4. `checkDependencies`, called before any stage dispatch, reads that
   freshly-fetched `BlockedBy` and applies `fabrik:blocked` if anything is
   still open.

Nothing gated Implement dispatch directly on the shallow, always-current
`fabrik:children-spawned` label — the only thing that could stop a second
Claude invocation was `fabrik:blocked`, and `fabrik:blocked`'s applicability
depended entirely on step 3 succeeding **before** any other admission route
reached the parent. Two concrete ways that chain could fail to complete in
time:

- A transient `FetchItemDetails` failure right after the spawn's own
  deep-fetch sets `LastDeepFetchFailureAt`, which `itemMayNeedWork` turns into
  its own `PollSeconds*10` suppression window — during which `BlockedBy` stays
  stale-empty while `fabrik:children-spawned` (visible without any deep fetch)
  already reads as spawned. An item admitted to dispatch by any other route in
  that window (e.g. a comment) sees an empty `BlockedBy`, concludes "not
  blocked," and `preImplement`'s idempotency guard silently no-ops — Implement
  runs Claude on the full epic.
- GraphQL rate-limit pressure (a recurring condition elsewhere in this
  codebase, #971/#981) delays or drops deep-fetches for several poll cycles —
  long enough to plausibly explain the report's 1–2 minute gap on an instance
  processing multiple epics.

`checkDependencies` already had a live-re-read special case, but it only
fires when the item is `alreadyBlocked` — i.e., on the *second* time it's
found blocked, not the first. The very transition this issue is about was
never covered.

No existing test caught the absence of the fast guarantee:
`tests/sim`'s `TestCrossRepoSpawn` spawns children and waits for
`fabrik:blocked`, but `tests/sim`'s `Engine` is wired via `NewWithDeps` with a
bare `boardcache.GitHubAdapter`, never the `boardcache.CacheImpl` production
uses — so that test can only be passing because it tolerates the slow,
10-poll `periodic-re-eval` fallback, never because the fast `cycleSet` path
actually fired.

## Decision

Make `spawnChildren` the single point of truth for the Store's copy of
`BlockedBy`, instead of relying on a subsequent deep-fetch to discover what it
already knows.

The instant `spawnChildren` links a child via `AddBlockedByIssue` — whether
freshly linked or found already-linked on an ADR-1583 resume — it also
applies a new `itemstate.BlockedByEdgeAdded` mutation directly to the
engine's own `Store`, appending that one dependency edge (deduplicated by
`Repo`+`Number`) with no other field touched:

```go
e.store.Apply(itemstate.BlockedByEdgeAdded{
    Repo:   fmt.Sprintf("%s/%s", owner, repo), // parent's own owner/repo
    Number: item.Number,
    Dep:    gh.Dependency{Repo: block.Repo, Number: childNumber, State: "OPEN"},
})
```

Because `boardcache.CacheImpl.FetchProjectBoard` reconstructs every item's
fields — including `BlockedBy` — directly from the Store on every call
(`snapshotToProjectItem`), the very next board fetch already carries the
correct edges. This holds regardless of `cycleSet` wake timing, deep-fetch
admission, GraphQL rate-limit pressure, or whether a live re-fetch ever
succeeds — the write is synchronous, in-process, and costs zero additional
network calls. `checkDependencies` needs no changes: it already reads
whatever `BlockedBy` sits on the `gh.ProjectItem` it's handed, which is now
correct by the time it's handed anything.

`Dep.Repo` is always written as the full `"owner/repo"` string, matching
exactly what `github/project.go`'s `applyBlockedBy` produces from a genuine
live fetch (`dep.Repository.NameWithOwner`, always populated when the
dependency node resolves) — so a synthetic edge from this write and a
subsequent genuine deep-fetch's edge are byte-identical, never a
representational mismatch a future comparison could trip on.

### Sibling `DEPENDS_ON` edges get the identical treatment

`spawnChildren`'s second pass wires any declared `DEPENDS_ON` header as a
sibling `blockedBy` edge between two children (ADR-1337), via the same
`AddBlockedByIssue` primitive used for the parent edge. A PR review caught
that this pass originally applied `AddBlockedByIssue` on GitHub without a
matching `BlockedByEdgeAdded` write — leaving a dependent child exposed to
the identical stale-Store race this ADR closes for the parent, just one level
down: the child's first Store entry can arrive via a shallow reconcile
(which never populates `BlockedBy`) rather than a live deep-fetch, and
`checkDependencies` would see an empty `BlockedBy` and dispatch the child
before its sibling closes. The fix is symmetric — immediately after each
sibling `AddBlockedByIssue` call succeeds, `spawnChildren` also applies
`BlockedByEdgeAdded` keyed on the **dependent child's own** `(repo, number)`,
not the parent's:

```go
e.store.Apply(itemstate.BlockedByEdgeAdded{
    Repo:   block.Repo, // the dependent child's own owner/repo
    Number: childNumbers[i],
    Dep:    gh.Dependency{Repo: blocks[blockerIdx].Repo, Number: childNumbers[blockerIdx], State: "OPEN"},
})
```

This write can land before the child is otherwise known to the Store (a
freshly created child has no prior entry) — safe by the same `getOrCreate`
lazy-stub reasoning as the parent case: `BlockedBy` is a deep field no
shallow/probe apply ever touches, so a pre-seeded edge on an as-yet-unknown
item is never clobbered when that item is later discovered normally, and a
`gh.ProjectItem` with mostly-zero fields showing up transiently in a board
snapshot before that discovery is indistinguishable from any other item
sitting in a Status the configured stages don't recognize (`FindStage`
already returns nil for those without incident).

### Rejected alternative: extend `checkDependencies`'s live-re-read

The other candidate considered was widening `checkDependencies`'s existing
`alreadyBlocked`-gated live-re-read to also fire on the first evaluation
after a fresh `fabrik:children-spawned`. Rejected because label state alone
cannot distinguish "just spawned, not yet confirmed blocked" from "children
already closed, correctly unblocked" — both look identical
(`children-spawned=true`, `blocked=false`). Building that distinction safely
would mean either an extra field to track "has this parent had its first
post-spawn check yet" (a new piece of state to reason about, for what is
fundamentally the same fact the Store should already have), or accepting a
live `FetchItemDetails` call on every future dispatch of that parent for the
rest of its lifecycle — Review, Validate, forever — which is exactly the
"only spawn-adjacent items should pay this cost" risk the original research
into this issue flagged. Writing the real edge into the Store at the moment
it's created needs no live read at all in the common path: it is prevention,
not a narrowed race window.

### Not a new label, not a new admission-time guard

R2 of the originating issue explicitly preferred reusing existing state over
inventing new mechanism. Once `BlockedBy` is deterministically correct, the
pre-existing `fabrik:blocked` gate — already wired into both
`itemMayNeedWork`/`itemNeedsWork` and `checkDependencies` — is sufficient. No
new label, and no new admission-time check in `engine/poll.go`, was needed.

### `refreshForSpawnResume`'s own Store-blindness is subsumed, not separately fixed

`refreshForSpawnResume` (ADR-1583) live-refetches `item` in-place on a
resumed spawn attempt but never wrote that back into the Store. This is not
patched directly; the new per-child write in `spawnChildren`'s loop is
unconditional regardless of whether a child was freshly linked or found
already-linked on resume, so Store correctness at the end of `spawnChildren`
no longer depends on that function's own write-through at all.

### Partial-spawn Store state is a strict improvement, not a new behavior

The write happens per child, inside the loop, before any later block's
failure could pause the parent — so a partial (paused) spawn's
already-linked children are reflected in the Store even though the batch as
a whole did not complete. This does not change R5's existing pause/retry
semantics (`fabrik:children-spawned` is still applied only after the full
batch succeeds); it only means the Store's view of a partially-spawned
parent is accurate sooner, which cannot make anything worse.

## Rationale

### Why the Store and not the cache layer directly?

`e.store` is the single owner of all per-item state (`internal/itemstate`,
ADR-036) and is shared identically between production's `CacheImpl`-backed
engine and every test harness that wires `testEngineWithCache`. Writing to it
via the same `Mutation`/`Apply` interface every other engine code path uses
(rather than reaching into `boardcache.CacheImpl`'s internals directly) keeps
`spawn.go` ignorant of whether a cache is even wired — exactly how it already
treats every other Store write in this function (`CooldownRecorded`,
`applyLabelAdd`'s write-through).

### Why not a full `ItemDeepFetched`?

`ItemDeepFetched` overwrites every deep field (`Comments`, `Body`,
`Assignees`, `BlockedBy`, etc.) from a `gh.ProjectItem` snapshot — correct for
a genuine full re-fetch, wrong here: `spawnChildren` only knows one new fact
(a dependency edge), and feeding it a synthetic partial `gh.ProjectItem` to
satisfy `ItemDeepFetched`'s shape would silently wipe every other deep field
back to zero values the next time it's read. `BlockedByEdgeAdded` is scoped
to exactly the one field being asserted.

## Consequences

- The post-spawn dependency gate (ADR 048) is now structural: it holds
  deterministically under any deep-fetch delay, `cycleSet` timing, or
  GraphQL rate-limit condition, not merely under the common case.
- `docs/state-machine.md` §6.7 is revised to describe the actual mechanism
  (a synchronous Store write) rather than the prior "next poll cycle,
  checkDependencies sees the new blockedBy edges" phrasing, which described
  the intended emergent behavior rather than a guaranteed one.
  `docs/USER_GUIDE.md`'s decomposition section and
  `plugin/fabrik-workflows/LABELS.md`'s `fabrik:blocked` entry are updated to
  match.
- New coverage lives in `engine` package tests (`engine/spawn_test.go`) using
  `testEngineWithCache`, since `tests/sim`'s bare-`GitHubAdapter` wiring
  cannot exercise the `CacheImpl` board-reconstruction path this fix depends
  on. `tests/sim`'s `TestCrossRepoSpawn` is unaffected and continues to pass
  unmodified — it exercises the pre-existing slow fallback path, which this
  ADR does not remove.
- No change to `checkDependencies`, `itemMayNeedWork`, `itemNeedsWork`, or any
  label surface. An issue that spawns no children takes an identical code
  path to before this ADR — `BlockedByEdgeAdded` is only ever applied from
  inside `spawnChildren`'s per-child loop.
