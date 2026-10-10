# ADR 2047: Merge-Train Overlap-Aware Batch Composition and Post-Landing Invalidation

**Date**: 2026-10-10
**Status**: Accepted
**Issue**: #2047 — merge-train: overlap-aware batch composition, and reroute Queued members that actually conflict after a landing
**Builds on**: [ADR 1821](1821-merge-train-red-member-admission-gate.md), [ADR 1208](1208-queued-review-finding-ejection.md), [ADR 1833](1833-deterministic-queued-batch-ordering.md), [ADR 1648](1648-merge-train-per-base-partitioning.md), [ADR 2044](2044-singleton-catch-up.md), [ADR 2045](2045-merge-train-red-singleton-auto-repair.md)

Mentioned for context: #2040, #1826 (proposal 2, "batch only disjoint file sets").

## Context

The train formed batches in entry order and learned about conflicts only by running trials. With
many parallel PRs, Queued members increasingly edit the same files. A batch of overlapping members
needs conflict resolution on the trial branch or goes red and bisects, and every bisection step is a
full trial CI run, one after another. Once one member lands, every other Queued member that now
conflicts with the new base is stale, and the train only finds out by spending a trial (and often a
pause) on each one.

## Decision

Two complementary mechanisms, sharing one in-memory file-list cache.

### 1. Prevention: `admitByOverlap`

A third fresh-formation filter in `prepareTrainWorker`, after the live-landed guard (#1871) and the
CI admission gate (#1821). It keeps members that change the same non-ignored path out of one batch.

- **Placement is fresh formation only.** Both restart routes return from `reconstructTrainState`
  first, and bisection, `landOneAtATime` and `landGreenBatch` never call it, so FR-012 holds by
  construction.
- **Filter after the cap.** `routeQueuedGroup` caps the batch at `max_batch_size` before the worker
  runs, so file reads are bounded by the cap, but an overlap deferral can leave a batch smaller than
  the cap and a disjoint member beyond the cap is not pulled in. Filtering the whole partition before
  the cap would fill batches better but would read the files of every Queued PR. We chose the bound.
- **Order is not changed.** Members are walked in today's deterministic order (ADR-1833); a member is
  kept if its files are disjoint from the union of those already kept. Survivors keep their
  relative order. The first member considered is always admitted, so overlap never empties a batch.
- **Fail-open.** A member whose changed files cannot be read, or cannot be known complete, is
  admitted as before (ADR-1821's posture) and contributes nothing to the union. A read error is
  never cached as an empty list. GitHub silently caps `/pulls/{n}/files` at 3000 entries with no
  truncation signal, so a list of 3000 or more is treated as unknown.
- **One read per head SHA.** The cache holds one entry per `(repo, PR)` keyed on the head SHA the
  list was read at; a new head overwrites it. In memory only.
- **`merge_train_overlap_ignore`** (flat key; `merge_train` is a scalar) lists globs excluded from
  both sides of the comparison. Matching uses the new leaf package `internal/pathglob`, extracted
  from Pruefer's `excluded_paths` matcher so the repository has exactly one documented path-glob
  semantics: `*` never crosses `/`, `**` matches zero or more segments, so `*.lock` matches only a
  top-level file and a lockfile at any depth needs `**/*.lock`.
- **Starvation.** A member deferred for overlap in `overlapStarvationThreshold` (3) consecutive
  fresh formations of its partition is considered first in the next one, so later overlapping members
  defer against it. The count resets when the member is admitted or is absent from a formation's
  candidate list, and is kept per partition (`trainKey`) so another partition's formation cannot
  clear it. It counts once per formation, literally as specified: a worker that exits on a CI-pending
  timeout and re-forms with the same blockers also ticks it. That can only make a starved member go
  first sooner, which is harmless; de-duplicating by blocker set would complicate the "no member is
  deferred more than N formations" guarantee and could fail to fire against a stuck blocker.
- A deferred member stays Queued with **no side effects**: no reroute, comment, counter or pause.

### 2. Early invalidation: post-landing `git merge-tree` scan

Each landing path records the landed member (`noteTrainLanded`, beside `resetEjectionCount`). On
the next poll that finds the partition without a worker in flight, `routeQueuedGroup` runs
`invalidateConflictingQueued` before the batch is capped or dispatched.

- **Where it runs.** On the poll goroutine, only while no merge-train worker is in flight for the
  partition's `trainKey`. With no worker, nobody owns the Queued members, so the poll goroutine may
  reroute them directly (the ADR-1208 ownership rule), and it sees the whole uncapped partition,
  including members the worker deferred for overlap. A signal recorded mid-flight waits for the
  worker to exit. This needs no change to `mergeTrainWorkerState` and no stashing of uncapped lists.
- **The check.** `git fetch origin` (a git call, not an API call), then for each non-landed,
  non-paused, non-editing candidate `git merge-tree --write-tree --name-only --no-messages
  refs/remotes/origin/<base> refs/remotes/origin/fabrik/issue-N` in the repo's bare clone. The head
  is the fetched remote-tracking ref, not the board snapshot's SHA, so a stale snapshot cannot cause a
  reroute on an old head. No CI, no GitHub API reads.
- **Exit 1 alone is not a conflict.** `git merge-tree` also exits 1 for an unresolvable ref
  ("not something we can merge", empty stdout). A conflict is believed only when both refs resolve to
  commits and stdout begins with a result-tree object ID. Everything else is an error.
- **Fail-SAFE, opposite to the filter.** Any error, missing ref or object, a git too old for
  `--write-tree` (< 2.38) or a failed fetch leaves the member Queued and logs. File overlap alone
  never reroutes. The two polarities are opposite on purpose: in the filter the costly mistake is
  stranding a member, in the scan it is moving one that was fine.
- **FR-007 reroute target.** The member is rerouted to the stage before the holding stage (Validate)
  with `rerouteQueuedMemberOffHolding` (reroute-before-side-effects), and `fabrik:rebase-needed` is
  applied directly (best-effort), with one `🏭 **Fabrik merge-train — rerouted (conflicts with new
  base)**` comment deduplicated per (member, head SHA). Not the singleton catch-up: catch-up only
  handles a length-1 batch and needs a Claude conflict-resolution slot inside the train, while the
  reroute is free and is the established, uncharged path. Validate's `wait_for_ci` mergeability gate
  re-derives `fabrik:rebase-needed` from live mergeability every pass, so a directly applied label
  is safe and self-heals if the member turns out to be clean. Applying it immediately closes the
  yolo/cruise bounce window: a member that kept `stage:Validate:complete` would otherwise re-queue
  before GitHub's mergeable state caught up. The rebase cycle is charged normally by
  `dispatchRebaseReinvoke` (bounded by `MaxRebaseCycles`); the reroute itself charges nothing.
- **Not an ejection.** No `ejectMember`, no `mergeTrainEjectionCounts`, no pause (FR-009).
- **Precondition.** As in ADR-1821 (R9), the scan is skipped unless the reroute target has
  `wait_for_ci`; otherwise nothing would claim the rerouted member.
- **Attribution.** merge-tree cannot say which landed member caused a conflict. The log line
  `invalidated #N: conflicts with landed #M (merge-tree)` names the landed members whose cached file
  lists intersect the conflicted paths, falling back to every member landed in that pass.
- **Best effort, in memory.** A restart, a failed fetch or a fetch that has not yet seen the landing
  drops or misses the scan; the cost is one trial, no worse than before. The signal is consumed once,
  so there is no retry loop. `git merge-tree --write-tree` writes unreferenced tree objects into the
  shared bare clone; `git gc` collects them and they are not refs, so ADR-1835's ref sweeping never
  sees them.

### Interface and sim

`FetchPRFiles` is added to `engine.GitHubClient` (the production client already had it). The sim
derives each PR's changed files from real git (`git diff --name-only <base>...<head>`), so overlap and
conflict in sim scenarios are whatever the commits really produce.

## Consequences

- An unignored hot file serialises its members: the batch degrades to a singleton and each landing is
  one trial. That is the intent; operators should list generated files and lockfiles in
  `merge_train_overlap_ignore`. The starvation guard bounds the delay for any single member.
- A rename's source path is not in `FetchPRFiles`' output, so a rename colliding with another member's
  edit is not seen as overlap; the post-landing scan catches the resulting conflict.
- Skip counts, the file cache and the landed record are in memory; a restart delays the starvation guard
  and drops a pending scan.
- Neutralisation seams (`SetMergeTrainOverlapDisabledForTest`, `SetMergeTrainInvalidationDisabledForTest`)
  let each new test be shown to fail with its behaviour off.
