# Feature Specification: Merge-train overlap-aware batch composition and post-landing invalidation of conflicting Queued members

**Feature Branch**: `fabrik/issue-2047`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "merge-train: overlap-aware batch composition, and reroute Queued members that actually conflict after a landing

## Problem

The train forms batches in entry order (deterministic since #1833, up to `effectiveMaxBatchSize`) and only learns about conflicts by running trials. More parallel work means Queued members increasingly edit the same files. That has two costs:
- A batch of overlapping members needs conflict resolution on the trial branch, or goes red and bisects. Each bisection step is a full trial CI run, run one after another.
- Once one member lands, every other Queued member that conflicts with it is stale, and the train only finds out by spending a trial (and often a pause) on each one.

Seen on shadoworg/liminis-models 2026-10-05: 6 of 9 open PRs edited the same small set of shared files. The fix was done by hand: read each PR's files, order the overlapping ones, and chain them with `blockedBy`.

Report: #2040 (on the public triage board). This is also proposal 2 of #1826 ("batch only disjoint file sets")."

(The full original requirements R1–R4, scope and acceptance text are restated below as FR/SC items without loss.)

## Background

The merge train batches ready PRs from the `Queued` column, validates the combined batch once, and lands them together. Batches are formed purely in deterministic entry order, up to the configured maximum batch size, with no knowledge of which files each member touches. Conflicts are only discovered by spending a trial: assembling a trial branch, resolving conflicts inline or going red and bisecting, with every bisection step costing a full CI run executed serially.

The cost grows with parallelism. When many Queued members edit the same small set of files, (a) batches of mutually overlapping members need conflict resolution or bisection, and (b) after any member lands, every other Queued member that now truly conflicts with the new base is stale, but the train only finds out by spending a trial (and often a pause) on each one.

Observed on shadoworg/liminis-models on 2026-10-05: 6 of 9 open PRs edited the same small set of shared files, and the operator had to read each PR's files by hand, order the overlapping ones and chain them with `blockedBy`. This was reported in #2040 (public triage board) and is also proposal 2 of #1826 ("batch only disjoint file sets"). This issue builds on the earlier work in the same chain (singleton catch-up, #2044; red-singleton auto-repair, #2045), which is already in place.

Two complementary mechanisms address this:
1. **Prevention** — when forming a fresh batch, avoid putting overlapping members in the same batch.
2. **Early invalidation** — after a landing, cheaply detect Queued members that now *actually* conflict with the new base and send them back to their own conflict-handling path before a trial is wasted on them. Detection is by real merge conflict, never by file overlap alone.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Fresh batches avoid overlapping members (Priority: P1)

As an operator running the merge train with many parallel PRs, I want a fresh batch to contain only members whose changed files are disjoint, so that trials rarely need conflict resolution or bisection.

**Why this priority**: This is the primary cost saver; it prevents the expensive trials rather than reacting to them.

**Independent Test**: Queue three PRs where #A and #B edit a common file and #C edits a different file. Form a fresh batch. The batch contains #A and #C; #B stays Queued for a later train.

**Acceptance Scenarios**:

1. **Given** two Queued candidates whose changed files intersect, **When** a fresh batch is formed, **Then** only the earlier one (in today's deterministic order) is admitted and the other stays Queued.
2. **Given** a disjoint third candidate, **When** the batch is formed, **Then** it joins the first candidate.
3. **Given** a candidate whose files cannot be read, **When** the batch is formed, **Then** it is admitted exactly as it is today.
4. **Given** a deferral, **When** the decision is made, **Then** one log line names the deferred member, the member it overlaps and an overlapping path.

---

### User Story 2 - A deferred member is never starved (Priority: P1)

As an operator, I want a member that keeps being deferred because of overlap to eventually be admitted, so that a hot-file PR is not postponed indefinitely.

**Why this priority**: Without it, overlap-aware ordering could starve a member forever behind a stream of newer disjoint PRs.

**Independent Test**: Defer a member for the starvation threshold number of consecutive formations; at the next formation it is admitted first regardless of overlap.

**Acceptance Scenarios**:

1. **Given** a member skipped the threshold number of times in a row (a named constant, e.g. 3), **When** the next fresh batch is formed, **Then** it is admitted first, regardless of overlap, and overlapping later candidates are deferred against it.
2. **Given** a member that was deferred and is then admitted (or leaves Queued), **When** it is next evaluated, **Then** its skip count has been reset.

---

### User Story 3 - Lockfiles and generated files do not count as overlap (Priority: P2)

As an operator, I want to name path globs (lockfiles, changelogs, generated indexes) that are left out of the overlap check, because conflict resolution handles them reliably and they would otherwise serialize every PR.

**Why this priority**: Without it, a changelog every PR touches would defeat batching entirely.

**Independent Test**: Two candidates whose only common file matches an ignored glob are admitted into the same batch; with the glob removed they are not.

**Acceptance Scenarios**:

1. **Given** `overlap_ignore` contains a glob matching the only shared path, **When** a batch is formed, **Then** the two candidates are not considered overlapping.
2. **Given** the default (empty) setting, **When** a batch is formed, **Then** every changed path counts toward overlap.

---

### User Story 4 - Members that truly conflict after a landing are rerouted early (Priority: P1)

As an operator, I want that, once a train lands, each still-Queued member of the same repository and base branch whose merge with the new base actually conflicts is sent back to its own conflict-handling path before the train spends a trial or a pause on it, while members that still merge cleanly stay Queued.

**Why this priority**: It removes the wasted trial/pause per stale member and is free (a local check, no CI, no API calls).

**Independent Test**: Land a batch; of two remaining Queued members, one conflicts with the new base and one overlaps on files but merges cleanly. Only the conflicting one is rerouted.

**Acceptance Scenarios**:

1. **Given** a landing completed and a Queued member in the same (repo, base) partition whose merge with the new base conflicts, **When** the post-landing scan runs, **Then** the member is moved off Queued to its conflict-handling path before any trial can pick it, using the existing reroute-before-side-effects mechanism.
2. **Given** a Queued member that overlaps the landed files but merges cleanly, **When** the scan runs, **Then** it stays Queued, untouched.
3. **Given** a rerouted member, **When** ejection accounting is evaluated, **Then** the reroute is not counted toward `MaxMergeTrainEjections` and does not pause the member.
4. **Given** a rerouted member, **When** the decision is made, **Then** one log line names the invalidated member and the landed member it conflicts with.

---

### Edge Cases

- A candidate's file list is unreadable (API error): admit it (fail-open); do not cache the failure as an empty file list.
- A candidate's head SHA changes between formations: its cached file list is not reused for the new head.
- Fully overlapping Queued set: the batch is a singleton (the first member in order); the singleton fast-path/catch-up logic applies as today.
- The first-ordered member is always admitted, so a batch is never empty because of overlap.
- A starved member is admitted first even if that makes later candidates overlap and be deferred.
- Ignored globs apply to both sides of the intersection; a member whose entire file set is ignored never overlaps anyone.
- Very large PRs with truncated or capped file listings: treated as unreadable if the list cannot be known complete (fail-open).
- The post-landing scan finds the local merge check itself failing (missing object, git error): the member is left alone (fail-safe: never reroute on ambiguity) and the failure is logged.
- A member that is mid-flight in a worker, paused, or no longer Queued when the scan reaches it is skipped.
- Restart between a landing and the scan: the scan is best-effort; a missed invalidation is no worse than today (the member is caught by a trial).
- Bisection, restart-reconstruction, and one-at-a-time landing paths do not apply overlap filtering (out of scope).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When forming a fresh merge-train batch, the system MUST read each candidate member's changed file list once per PR head SHA and MUST NOT re-read it on every poll while the head SHA is unchanged.
- **FR-002**: The system MUST admit candidates in today's deterministic order, skipping any candidate whose changed files intersect the files of a member already admitted to the batch, leaving the skipped candidate Queued for a later train.
- **FR-003**: A candidate skipped N times in a row (N a named constant, default 3) MUST be admitted first in the next fresh batch formation, regardless of overlap; its skip count resets once it is admitted or leaves Queued.
- **FR-004**: When a candidate's changed file list cannot be read (or cannot be known complete), the system MUST admit it as it would today (fail-open), consistent with the admission posture of the existing admission check (ADR-1821).
- **FR-005**: The system MUST support a configuration setting `overlap_ignore`, a list of path globs excluded from the overlap check on both sides of the comparison. Its default is empty (all paths count). It is configurable as a flat key `merge_train_overlap_ignore` in `.fabrik/config.yaml`, alongside the other merge-train tuning keys, with a matching CLI flag and `FABRIK_` environment variable following the existing convention (see Assumptions).
- **FR-006**: After a train lands, for each still-Queued member of the same (repo, base) partition, the system MUST test locally, in the repository's existing bare clone, whether merging the member's head into the new base produces a conflict (`git merge-tree --write-tree <new base> <member head>`), without CI and without GitHub API calls.
- **FR-007**: Only a member whose merge actually conflicts MUST be moved off Queued to its own conflict-handling path (Validate with `fabrik:rebase-needed` semantics, or the singleton catch-up path, whichever the Plan stage finds cleaner; the choice and its rationale are recorded in the ADR), using the existing reroute-before-side-effects helper, before the member can be picked for a trial.
- **FR-008**: A member that merges cleanly MUST be left alone. File overlap alone MUST NOT reroute any member, and a member that merges cleanly and passes must not be pushed out of Queued.
- **FR-009**: A reroute under FR-007 MUST NOT be charged to `MaxMergeTrainEjections` and MUST NOT pause the member.
- **FR-010**: The system MUST emit one log line per decision, e.g. `deferred #1555: overlaps #1549 on mcp/src/index.ts` for an overlap deferral and `invalidated #1560: conflicts with landed #1549 (merge-tree)` for a post-landing invalidation, in a form later TUI and board visibility work can consume.
- **FR-011**: If the local merge check cannot be completed for a member (git error, missing objects), the system MUST leave that member Queued and log the failure; it MUST NOT reroute on ambiguity.
- **FR-012**: Overlap filtering MUST apply only to fresh batch formation; bisection, restart-reconstruction, one-at-a-time landing and the green-batch paths are unchanged.
- **FR-013**: The simulation test bed MUST support per-PR changed-file lists (adding them to the simulated GitHub if missing) so composition and post-landing invalidation can be exercised deterministically.
- **FR-014**: Documentation MUST be updated: an ADR numbered after this issue (`adrs/2047-...md`), `docs/state-machine.md` (batch composition and post-landing invalidation), `docs/USER_GUIDE.md` (covering `overlap_ignore` and its flag/env/config forms), and `docs/llms-full.txt` regenerated in the same change.
- **FR-015**: Each new test MUST be demonstrated to fail when its behavior is neutralised (neutralisation check), per the repository's test conventions.
- **FR-016**: The PR body MUST mention #2040 and #1826 by bare number only, without any closing keyword (`Closes`/`Fixes`/`Resolves`) adjacent to either number, and MUST carry `Closes #2047` for this issue.

### Key Entities

- **Candidate member**: a Queued item with a linked PR, evaluated for admission to a fresh batch; carries a changed-file set (per head SHA) and a consecutive-skip count.
- **Changed-file set**: the set of paths a PR changes, minus paths matching `overlap_ignore`.
- **Landing**: a completed merge-train landing that advances the partition's base.
- **Partition**: a (repo, base branch) pair, the existing unit of independent train workers.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a fresh batch, no two admitted members share a changed (non-ignored) path unless one was admitted under the starvation guard or had unreadable files.
- **SC-002**: A disjoint candidate behind a deferred overlapping candidate is still admitted in the same batch.
- **SC-003**: No member is deferred more than N consecutive formations (N the named constant) while a formation occurs.
- **SC-004**: After a landing, 100% of Queued same-partition members whose merge with the new base conflicts are rerouted before their next trial, and 0% of cleanly merging members are rerouted.
- **SC-005**: Overlap checking performs at most one file-list read per candidate per head SHA, and the post-landing scan makes zero GitHub API calls and runs zero CI.
- **SC-006**: Every deferral and invalidation produces exactly one log line identifying the members (and the path or landed PR) involved.
- **SC-007**: The unit tests, sim twin, docs updates and neutralisation checks listed in FR-013 to FR-015 all exist and pass, including: two overlapping candidates never in one fresh batch; a disjoint third joins the first; starvation guard admits a member skipped 3 times; an ignored glob is not overlap; unreadable files fail open; after a landing a conflicting Queued member is rerouted and a cleanly merging overlapping member is not.

## Assumptions

- The issue's `merge_train.overlap_ignore` is realised as a flat key `merge_train_overlap_ignore` (with a `--merge-train-overlap-ignore` flag and `FABRIK_MERGE_TRAIN_OVERLAP_IGNORE` environment variable, comma-separated), because `merge_train` is already a scalar (`on`/`off`) and the other tuning knobs are flat keys (e.g. `max_train_auto_repair_attempts`). The user-facing concept is still called "overlap_ignore".
- Glob syntax follows the repository's existing path-glob conventions; `**` matching across directories is supported to the extent the existing matcher offers (the Plan stage confirms the exact matcher and documents it).
- "Today's deterministic order" is the order established by #1833; overlap filtering does not change it, only skips members.
- The skip count is held in memory and resets on restart (consistent with other train in-memory state); a restart can delay, never permanently defeat, the starvation guard.
- The starvation threshold is a code constant (default 3), not user-configurable.
- Issue #2046 (the blocking dependency) has completed, and the singleton catch-up (#2044) and auto-repair (#2045) paths referenced by this issue exist.
- The post-landing scan is best-effort and in-process; missing a scan only costs what the train costs today.
- Overlap is computed from the PR's changed paths only (not line ranges or content); actual conflict detection is delegated to the post-landing merge-tree check.

## Out of Scope *(optional)*

- Overlap hints at Plan/Implement time (separate, later work).
- Bisection and restart-reconstruction paths.
- Changing batch size, ordering rules, or the ejection/runaway-guard accounting.
- Rerouting members based on file overlap alone.
- TUI and board visibility of the new decisions (later in this chain; this issue only provides the log lines).
- Persisting skip counts or file-list caches across restarts.

## Source References *(optional)*

- Report #2040 (public triage board) and proposal 2 of #1826 ("batch only disjoint file sets")
- #1833 (deterministic batch ordering), ADR-1821 (admission fail-open posture), ADR-1208 (reroute-before-side-effects), #2044 (singleton catch-up), #2045 (red-singleton auto-repair)
