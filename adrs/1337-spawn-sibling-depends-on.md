# ADR 1337: Sibling Ordering for Spawned Sub-Issues via DEPENDS_ON

**Date**: 2026-08-02
**Status**: Accepted
**Extends**: [ADR 048: Engine-Side Sub-Issue Spawning via blockedBy](048-spawn-child-engine-side.md)

## Context

ADR 048 established engine-side spawning: Plan declares `FABRIK_SPAWN_CHILD_BEGIN/END` blocks, and `preImplement` (`engine/spawn.go`) creates each child and links it as a `blockedBy` dependency of the *parent* only. Every child of a decomposition is therefore admitted to Specify simultaneously, as an independent unit — a parallel star, gated solely on the parent's own `fabrik:blocked`.

This is correct for genuinely independent units of work, but breaks down for the common case of a single-repo feature decomposed into **sequentially dependent slices**, done purely for PR hygiene. `shadoworg/fantasy#1649` decomposed into four such slices; the spawn mechanism produced four unblocked children, and once `fabrik:yolo`/`fabrik:cruise` was applied, all four ran concurrently:

1. **Concurrent edits to the same surfaces** — three of the four slices modified the same shared files, and four independent worktrees each added a variant of the same union member, colliding at merge. Fabrik's existing "update from main" step rebases a *stale* worktree; it does not resolve four worktrees independently inventing incompatible variants of the same change.
2. **Implementing against an API that does not exist yet** — a later slice consumed a capability an earlier slice was supposed to build first. Run concurrently, the later slice's Implement stage had nothing to call.

Plan already worked around this in prose ("Depends on: Slice N" in the child's body), but prose is invisible to `checkDependencies` — nothing enforced it. The spec-kit v1 decomposition design (`specs/sub-issue-decomposition/spec.md`) explicitly deferred this: same-repo siblings were considered only as a *concurrency* question (parallel worktrees, handled by existing drift-update machinery), not an *ordering* question.

## Decision

Add an optional `DEPENDS_ON: <n>` header to the `FABRIK_SPAWN_CHILD` block grammar, parsed alongside `TITLE:`, letting Plan declare a forward-only dependency on an earlier sibling block in the same Plan output. The engine wires this as an *additional* sibling `blockedBy` edge, alongside the existing, unchanged `child → parent` edge — reusing `checkDependencies` and `PushUnblockObserver` entirely as-is, since both already operate generically over any `item.BlockedBy` entry regardless of where it originated.

### Grammar

```
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Retry-same-input: turn-attempt capture (slice 3/4)
DEPENDS_ON: 2

Full scoped spec body...
FABRIK_SPAWN_CHILD_END
```

`DEPENDS_ON:` is optional and, when present, must be the line immediately following `TITLE:` — no blank line between them. A `DEPENDS_ON:`-looking line separated from `TITLE:` by a blank line is parsed as ordinary body content, not a header; this avoids any ambiguity about how far the parser should scan for the optional field.

The value is a **1-based index into this Plan output's own block list** (`DEPENDS_ON: 2` on block 3 means "block 3 depends on block 2"), not an issue number — children don't have issue numbers yet when Plan runs, and indices scoped to a single Plan output compose cleanly with recursive decomposition (a grandchild's own `DEPENDS_ON` values operate on its own block set, never the parent's).

### Forward-only references — no cycle-detection walk

A block may only declare `DEPENDS_ON` on a **strictly lower** index (`1 <= DEPENDS_ON < ownIndex`). This is deliberate and load-bearing: it makes sibling dependency cycles **structurally impossible** without any graph walk, since no chain of forward-only edges can loop back on itself. `validateSpawnDependsOn` is therefore a single per-block index comparison, not a BFS. The existing `detectCycle` machinery in `engine/dependencies.go` is untouched and remains available as defense-in-depth against orthogonal cyclic misconfiguration (e.g. hand-edited dependencies after spawn) — it is simply never exercised by anything this feature adds.

A future extension to comma-separated multi-dependency (`DEPENDS_ON: 1, 2`, diamond-shaped graphs) is explicitly deferred; the single-index chain form covers the case this ADR was written for. Whoever builds that extension should re-examine whether the forward-only-index argument still holds (it does, for indices — the risk in a diamond shape is elsewhere), and should not casually add a general cycle-detection walk to "be safe" without first checking whether it's actually needed.

### Validation runs upfront, before any mutation

`validateSpawnDependsOn` runs against the parsed block list alone — no created-issue data is needed, since the check is purely structural (index bounds against `len(blocks)`). It therefore runs as the very first step of `spawnChildren`, before repo validation or any `CreateIssue` call. An invalid `DEPENDS_ON` — out-of-range, non-forward (self- or higher-index reference), or syntactically malformed (non-numeric, empty, zero, negative) — is a **hard failure**: the parent is paused with an explanatory comment (`Created so far: none`), and zero GitHub issues are created.

This ordering was a deliberate choice, not a requirement of the issue's acceptance criteria (which only constrain the externally observable "loud failure, no dropped edge"): validating first means a spawn that's guaranteed to fail never creates orphaned issues, and "Created so far: none" is always accurate for this failure class — cheaper than the existing invalid-repo/CreateIssue-error paths, which necessarily fail mid-loop after some siblings may already exist.

**Any invalid value is a hard failure, not just out-of-range/non-forward.** A syntactically malformed value (`DEPENDS_ON: abc`, or `DEPENDS_ON:` with no value) is treated identically to an out-of-range one, via a single sentinel: `ParseSpawnBlocks` leaves the parsed `DependsOn` field at `0` — never a legal 1-based index — for anything it cannot parse as a positive integer, so `validateSpawnDependsOn`'s one comparison (`DependsOn < 1 || DependsOn >= ownIndex`) catches all three cases (out-of-range, non-forward, malformed) uniformly. This departs from every *other* malformed-block case in `ParseSpawnBlocks` (missing `TITLE:`, missing `END`, bad repo), which are silently dropped rather than hard-failed. That asymmetry is deliberate: a silently-dropped block here would strand a sibling exactly as unordered as before this feature — reproducing the bug this ADR exists to fix. Every other malformed-block case drops the *entire block*, losing nothing enforceable; a dropped `DEPENDS_ON` would silently lose only the ordering guarantee while still spawning the child, which is the one outcome this feature must prevent.

### Two-phase spawn: create, then wire siblings

`spawnChildren` retains the existing per-block creation loop unchanged (parent `blockedBy` edge included), except it now also records a `childNodeIDs` mapping from block index to created child node ID. A second pass, run after all children exist, walks the blocks again and — for each declared `DEPENDS_ON` — calls the same `AddBlockedByIssue` primitive used for the parent edge, now linking two children: `AddBlockedByIssue(childNodeIDs[ownIndex], childNodeIDs[dependsOnIndex])`.

This two-phase shape (rather than wiring inline during creation) means a block may depend on any earlier sibling regardless of where in the creation loop that sibling's own creation happened to land, and keeps the creation loop's existing error-handling untouched.

`fabrik:children-spawned` — the idempotency guard — moves from immediately-after-the-creation-loop to after the sibling-wiring pass succeeds. This is a small behavioral extension: the guard must cover the full two-phase operation, or a sibling-wiring failure would be indistinguishable from success on the next `preImplement` invocation. A sibling-wire failure pauses the parent (same error-comment pattern, listing all children created so far) and retries the whole operation from scratch on the next attempt — consistent with the pre-existing, already-documented "v1 does not skip already-created children on retry" behavior from ADR 048. This is the same accepted partial-creation-on-failure shape the base mechanism already has; no new failure shape is introduced.

### Repo-agnostic by construction

`DEPENDS_ON` is not restricted to same-repo blocks. The underlying `AddBlockedByIssue` GraphQL mutation is already repo-agnostic — it's the exact same call ADR 048 uses for cross-repo parent edges — so no new capability was needed to support a sibling edge between two blocks targeting different repos.

## Rationale

### Why not a general dependency-graph field (list of indices)?

The issue this ADR implements explicitly scoped to the single-chain case (`DEPENDS_ON: <n>`, one dependency per block) and deferred comma-separated multi-dependency diamond graphs as a follow-up. The single-index form is sufficient for every case observed in practice — same-repo slice decomposition is naturally a chain — and its validation is a single comparison rather than a topological sort. Building the general case now would be speculative generality against a requirement that hasn't materialized.

### Why index-based rather than referencing a title or other identifier?

Blocks don't have stable identifiers until they're created as issues, and issue numbers don't exist yet when Plan emits the blocks. The 1-based position in the Plan output is the only ordering information available at declaration time, and it's already the numbering scheme existing per-block error messages use (`"invalid repo in spawn block #%d"`).

### Why not extend `checkDependencies`/`PushUnblockObserver`?

They needed no changes. Both already operate generically over `item.BlockedBy` regardless of where an edge originated — this was confirmed by Research before implementation began, not assumed. The value this ADR adds is entirely in *what edges get created*, not in how blocking is detected or cleared.

## Consequences

- Plan authors (the `fabrik-plan` skill) now have a first-class way to express slice ordering; the skill's Block Format section documents `DEPENDS_ON:` and instructs Plan to use it wherever it would otherwise write "Depends on: Slice N" in prose.
- A Plan output with no `DEPENDS_ON` headers is entirely unaffected — parses and spawns identically to before this ADR (the parallel-star graph), which is the required backward-compatible default.
- `fabrik:children-spawned` now marks the completion of a two-phase operation instead of a one-phase one. Any external tooling or documentation that assumed the label meant "creation loop finished" specifically (rather than "creation and sibling-wiring both finished") should be re-checked, though no such assumption is known to exist in this codebase today.
- The single-index constraint means diamond-shaped dependencies (a block depending on two or more earlier siblings) are not expressible yet. Plan must currently model such cases as a strict chain, or defer to the manual `gh api ... dependencies/blocked_by` workaround documented in the originating issue.
- No cycle-detection graph walk was added, and none should be added casually in a future extension of this mechanism — the forward-only-index constraint is what makes that unnecessary, and removing or weakening that constraint (e.g. to support back-references) would need to re-introduce one.
