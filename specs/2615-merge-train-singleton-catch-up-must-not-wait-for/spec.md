# Feature Specification: Merge-train singleton catch-up must not wait for ever on a worktree busy only with Fabrik scratch files or a stale REBASE_HEAD

**Feature Branch**: `fabrik/issue-2615`
**Created**: 2026-11-24
**Status**: Specified
**Input**: User description (verbatim, from the issue as filed):

> ## Problem
>
> The merge train can stall for ever on a Queued member that nothing is working on. Singleton catch-up refuses a member when `worktreeBusyReason` (`engine/worktree_catchup.go`) calls its worktree busy, and `ErrCatchUpBusy` is retried every formation, uncharged, with no bound. The check fires on leftovers nothing will ever clear:
>
> - untracked worker scratch files: a Validate worker wrote logs to `.fabrik/scratch/*.log` inside the issue worktree, so `git status --porcelain` reports "worktree has uncommitted changes";
> - a stale `REBASE_HEAD` in the worktree gitdir hours after a rebase finished, with no `rebase-merge/` or `rebase-apply/`, so "REBASE_HEAD present".
>
> Seen on 0.0.93 (liminisapp/liminis-context-graph): #859 deferred on every formation from 15:44 UTC, first for 14 untracked scratch logs, then (after they were moved by hand) for a `REBASE_HEAD` dated 01:43. #885 was deferred behind it on an overlapping file until the starvation guard admitted it. The only signal was `pipeline-stalled … (merge-train-idle)` with `idle_reason: unknown`.
>
> ## Requirements
>
> - **R1.** Fabrik's own leftovers are not busy. Ignore worker and engine scratch paths (`.fabrik/scratch/`, `.fabrik-context/`, and any other path Fabrik itself writes into a worktree) in the porcelain check. Decide in Plan whether to also keep worker scratch out of the worktree (e.g. a git exclude written at worktree setup); record it in the ADR.
> - **R2.** `MERGE_HEAD`, `REBASE_HEAD` and `CHERRY_PICK_HEAD` mean busy only when an operation is actually in progress (`rebase-merge/` or `rebase-apply/` present, or unmerged index entries). A stale ref with none of those is removed (logged) and is not busy.
> - **R3.** Bounded deferral: when the item has no worker in flight and catch-up has been deferred as busy for N consecutive formations (N small, e.g. 2, decided in Plan), stop waiting. Either reroute the member off Queued to Validate (as for a real failure, via `rerouteQueuedMemberOffHolding`, uncharged) or pause it naming the reason. Never leave it Queued and uncharged with no limit.
> - **R4.** Surface the cause: when members are queued, no run is active and a member keeps being deferred, the partition's `idle_reason` in `fabrik_health` and the `pipeline-stalled` event name the member and the reason (e.g. `#859 catch-up deferred: REBASE_HEAD present`), not `unknown`.
> - **R5.** A worktree that is really busy (a worker in flight, `fabrik:editing`, a real rebase in progress, a real uncommitted tracked change) keeps today's deferral.
>
> ## Scope
>
> In: `engine/worktree_catchup.go`, `engine/merge_train_catchup.go`, partition liveness (`engine/pipeline_liveness.go`), `docs/state-machine.md` §6.30 (regenerate `docs/llms-full.txt`), an ADR numbered after this issue.
> Out: the CI classification of trials (separate work issue).
>
> ## Acceptance
>
> - Tests: a member worktree with only untracked `.fabrik/scratch/*.log` is caught up; one with a stale `REBASE_HEAD` and no rebase directory is caught up and the ref is removed; a real in-progress rebase still defers.
> - Sim twin: a member deferred as busy with no worker is no longer Queued after N formations, and `idle_reason` names it while it waits.
> - Each requirement's test fails with its fix neutralised.
>
> **Report linkage:** this restates community report #2607. Mention #2607 in the PR body by bare number only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it, not even inside a negation (safe: "mentioned for context: #2607"). The report is closed by hand once a release containing the fix ships.

## Background

The merge train's singleton catch-up merges the pinned base into a lone Queued member's own branch before a trial is built. Before it touches the member's worktree it asks whether that worktree is busy, and if so it defers the member. A deferral is uncharged (no ejection count, no pause, no retry budget) and is retried on every formation, with no upper bound.

The busy check cannot tell a real in-flight operation from a leftover that nothing will ever clear. Two such leftovers were seen in production on 0.0.93 (liminisapp/liminis-context-graph):

- **Fabrik scratch files.** A Validate worker wrote logs under `.fabrik/scratch/` inside the issue worktree. They are untracked, so the porcelain status check reported "worktree has uncommitted changes" and the member was deferred.
- **A stale `REBASE_HEAD`.** Hours after a rebase had finished, `REBASE_HEAD` remained in the worktree's git directory with no `rebase-merge/` or `rebase-apply/`. The check reported "REBASE_HEAD present" and the member was deferred.

Member #859 was deferred on every formation from 15:44 UTC: first for 14 untracked scratch logs, then (after an operator moved them by hand) for a `REBASE_HEAD` dated 01:43. Member #885 was held up behind it on an overlapping file until the starvation guard admitted it. The only operator-visible signal was `pipeline-stalled … (merge-train-idle)` with `idle_reason: unknown`, which names neither the member nor the cause.

Because the deferral is uncharged and unbounded, the existing safety nets (retry budget, ejection cap, runaway guard) never fire, so the train stalls until a human notices and cleans the worktree by hand.

This restates community report #2607 as a work issue. #2607 stays the human-owned thread and is closed by hand once a release containing the fix has shipped.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Fabrik's own leftovers do not block catch-up (Priority: P1)

An operator queues a member whose worktree contains only files Fabrik or its workers wrote there (for example `.fabrik/scratch/*.log`, `.fabrik-context/*`). The train catches the member up as usual instead of deferring it.

**Why this priority**: This is the first of the two observed stall causes and the most common one: any worker that logs into the worktree triggers it.

**Independent Test**: Build a member worktree whose only untracked content is under `.fabrik/scratch/` and `.fabrik-context/`; run the busy check and a catch-up. The worktree is not busy and the catch-up proceeds.

**Acceptance Scenarios**:

1. **Given** a Queued member whose worktree has only untracked `.fabrik/scratch/*.log` files, **When** singleton catch-up runs, **Then** the worktree is not reported busy and the member is caught up.
2. **Given** a worktree with untracked `.fabrik-context/` files and nothing else changed, **When** the busy check runs, **Then** it is not busy.
3. **Given** a worktree with scratch files and also a modified tracked file, **When** the busy check runs, **Then** it is busy because of the tracked change (the scratch files do not mask it).

---

### User Story 2 - A stale in-progress marker does not block catch-up (Priority: P1)

A member's worktree git directory still holds `REBASE_HEAD`, `MERGE_HEAD` or `CHERRY_PICK_HEAD` from an operation that finished long ago. Fabrik recognises that no operation is actually in progress, removes the stale ref (logging it), and proceeds with catch-up.

**Why this priority**: This is the second observed stall cause, and it appeared immediately after the first was cleared by hand, so fixing only scratch files would not have unblocked the member.

**Independent Test**: Create a worktree with `REBASE_HEAD` present and no `rebase-merge/`, `rebase-apply/` or unmerged index entries; run the busy check and a catch-up. The ref is removed, a log line says so, and the worktree is not busy.

**Acceptance Scenarios**:

1. **Given** a worktree with a stale `REBASE_HEAD` and no rebase directory and no unmerged index entries, **When** the busy check runs, **Then** the ref is removed, the removal is logged, and the worktree is not busy.
2. **Given** a worktree with a stale `MERGE_HEAD` or `CHERRY_PICK_HEAD` under the same conditions, **When** the busy check runs, **Then** the same happens.
3. **Given** a worktree with a real rebase in progress (`rebase-merge/` or `rebase-apply/` present), **When** the busy check runs, **Then** it is busy and nothing is removed.
4. **Given** a worktree with unmerged index entries from a conflicted merge, **When** the busy check runs, **Then** it is busy and nothing is removed.

---

### User Story 3 - A member that stays busy with no worker is not left Queued for ever (Priority: P1)

If catch-up has been deferred as busy for N consecutive formations (N small) and the member has no worker in flight, the train stops waiting and moves the member off Queued, either by rerouting it to Validate or by pausing it with the reason named. It is never left Queued, uncharged, with no limit.

**Why this priority**: Stories 1 and 2 remove the two known causes, but any other leftover would recreate the same unbounded stall. This is the general safety net.

**Independent Test**: In the sim, defer a member as busy with no worker for N formations; after the Nth, the member is no longer Queued and the move names the busy reason.

**Acceptance Scenarios**:

1. **Given** a Queued member with no worker in flight whose catch-up is deferred as busy on N consecutive formations, **When** the next deferral would occur, **Then** the member is moved off Queued (rerouted to Validate without being charged, or paused naming the reason) and is no longer retried as Queued.
2. **Given** a member with a worker in flight, `fabrik:editing`, or a real in-progress operation, **When** catch-up is deferred, **Then** the deferral counter does not trigger the bound and today's deferral continues.
3. **Given** a member deferred as busy on fewer than N formations whose worktree then becomes clean, **When** the next formation runs, **Then** catch-up proceeds and the count starts over.

---

### User Story 4 - The operator can see why a partition is idle (Priority: P2)

When members are queued, no run is active, and a member keeps being deferred, the partition's `idle_reason` in `fabrik_health` and the `pipeline-stalled` event name the member and the reason (for example `#859 catch-up deferred: REBASE_HEAD present`) instead of `unknown`.

**Why this priority**: Even with the bound in place, an operator watching a stall should see the cause during the up-to-N formations of waiting; it is what made the original incident slow to diagnose.

**Independent Test**: In the sim, with a member deferred as busy and no active run, read the partition's health row and the stall event; both name the member and the reason.

**Acceptance Scenarios**:

1. **Given** queued members, no active run and one member currently deferred as busy, **When** health is read, **Then** the partition's `idle_reason` names the member number and the busy reason.
2. **Given** the same state when a `pipeline-stalled` event for the partition is raised, **When** the event is published, **Then** it carries the same named reason.
3. **Given** the member is later moved off Queued or its worktree becomes clean, **When** health is read again, **Then** the named reason is gone.

---

### User Story 5 - Genuinely busy worktrees are still protected (Priority: P1)

A worktree that is really in use keeps today's deferral: a worker in flight, a live `fabrik:editing` label, a real rebase in progress, or a real uncommitted change to a tracked file.

**Why this priority**: The fix must narrow the busy check without letting catch-up run over someone's real work.

**Independent Test**: For each genuine-busy condition, run the busy check and confirm it still reports busy.

**Acceptance Scenarios**:

1. **Given** each of: a worker in flight, `fabrik:editing`, a real rebase in progress, a modified tracked file, **When** the busy check runs, **Then** each reports busy with a reason naming the condition.
2. **Given** an untracked file that is not a Fabrik-written path (for example a human's stray source file), **When** the busy check runs, **Then** it is still treated as an uncommitted change.

---

### Edge Cases

- A worktree that has both scratch files and a stale `REBASE_HEAD`: both are handled in one pass and the worktree is not busy.
- A `REBASE_HEAD` that cannot be removed (permissions, read-only filesystem): the check must not report the worktree as clean on the strength of a ref it failed to remove; it falls back to busy with a reason that says the stale ref could not be removed, and that still counts toward the bounded deferral.
- Unmerged index entries with no `MERGE_HEAD` (a conflict left behind): this is a real in-progress state and stays busy.
- Scratch-like paths that are tracked in the repository: a tracked change under an ignored-for-busy path is still a real change. The exemption applies to untracked Fabrik-written files only.
- A member that is deferred, moved off Queued, and later re-queued: its consecutive-deferral count starts from zero.
- A restart in the middle of a run of deferrals: the count may reset, which only lengthens the wait by at most N formations; it must never cause an early reroute.
- The bound firing on a transient condition (for example a worker that exited a moment ago): the bound requires no worker in flight at the time it fires, and a rerouted member is uncharged, so a wrongly early reroute costs one extra Validate pass, not a failure count.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The singleton catch-up busy check MUST NOT treat untracked files at paths Fabrik or its workers write into a worktree (at minimum `.fabrik/scratch/` and `.fabrik-context/`, and any other path Fabrik itself writes there) as uncommitted changes.
- **FR-002**: The busy check MUST still treat any other untracked file, and any modified, staged or deleted tracked file (including tracked files under the exempted paths), as an uncommitted change.
- **FR-003**: `MERGE_HEAD`, `REBASE_HEAD` and `CHERRY_PICK_HEAD` MUST make the worktree busy only when an operation is actually in progress, meaning `rebase-merge/` or `rebase-apply/` is present or the index has unmerged entries.
- **FR-004**: When one of those refs is present and no operation is in progress, the engine MUST remove the stale ref, log the removal, and treat the worktree as not busy on that account. If the removal fails, the worktree MUST be reported busy with a reason that names the failed removal.
- **FR-005**: When a Queued member has no worker in flight and singleton catch-up has been deferred as busy for N consecutive formations, the engine MUST stop deferring and move the member off Queued: either reroute it to Validate through the existing uncharged reroute, or pause it naming the reason. N is a small number fixed in Plan.
- **FR-006**: The bounded-deferral move in FR-005 MUST NOT count toward the member's ejection count, retry budget, `max_retries` or any failure label, and MUST leave a comment or log line naming the busy reason that caused it.
- **FR-007**: The consecutive-deferral count MUST reset when catch-up proceeds, when the member leaves Queued, or when a worker, `fabrik:editing` or a real in-progress operation is observed. A member in any of those states MUST NOT be moved by FR-005.
- **FR-008**: While queued members exist, no run is active, and a member is being deferred as busy, the partition's `idle_reason` in `fabrik_health` MUST name that member and the reason (for example `#859 catch-up deferred: REBASE_HEAD present`) instead of `unknown`.
- **FR-009**: The `pipeline-stalled` event for such a partition MUST carry the same named reason, and the reason MUST clear once the member is moved, caught up, or no longer queued.
- **FR-010**: A worktree with a worker in flight, a live `fabrik:editing` label, a real rebase in progress, or a real uncommitted tracked change MUST keep today's deferral behaviour (uncharged, retried next formation, subject to FR-005 only when none of the "really busy" worker or editing conditions holds).
- **FR-011**: The as-built behaviour MUST be recorded in `docs/state-machine.md` §6.30 (with `docs/llms-full.txt` regenerated in the same change) and in an ADR numbered after this issue, which also records the Plan decision on whether worker scratch is additionally kept out of the worktree at setup time.
- **FR-012**: Each of FR-001, FR-003/FR-004, FR-005/FR-006, FR-008/FR-009 MUST have a test that fails when that fix alone is neutralised (a per-requirement neutralisation seam), and FR-005 and FR-008 MUST have a sim twin.

### Key Entities

- **Member worktree**: the per-issue git worktree a Queued member's branch is checked out in; the thing the busy check inspects.
- **Busy reason**: the short text the busy check returns naming why catch-up was deferred; carried into logs, the bounded-deferral comment and the partition's `idle_reason`.
- **Consecutive-deferral count**: the per-member count of formations in which catch-up was deferred as busy with no worker in flight.
- **Partition idle reason**: the `idle_reason` field of the merge-train partition row in `fabrik_health`, also carried on `pipeline-stalled`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A Queued member whose worktree holds only Fabrik-written untracked files is caught up on the first formation that reaches it, with zero deferrals attributable to those files.
- **SC-002**: A Queued member whose worktree holds only a stale `MERGE_HEAD`, `REBASE_HEAD` or `CHERRY_PICK_HEAD` is caught up on the first formation that reaches it, and the stale ref is gone afterwards.
- **SC-003**: No member with no worker in flight stays Queued through more than N consecutive busy deferrals; after the Nth it is off Queued.
- **SC-004**: During any busy deferral with no active run, an operator reading `fabrik_health` or the `pipeline-stalled` event can identify the member number and the cause without reading logs (`idle_reason` is never `unknown` for this case).
- **SC-005**: Every "really busy" condition in FR-010 still produces a deferral, with no catch-up run over it.
- **SC-006**: For each of FR-001, FR-003/FR-004, FR-005/FR-006 and FR-008/FR-009, the dedicated test passes with the fix and fails with that fix neutralised.

## Assumptions

- N (the number of consecutive busy deferrals before the member is moved) is small, around 2; the exact value, and whether the member is rerouted to Validate or paused, is decided in Plan. The issue leans towards reroute as for a real failure, because that path is uncharged and already exists.
- "Any other path Fabrik itself writes into a worktree" is the set of paths the engine and its workers write today, determined in Research; the list lives in one place so a new path is added once.
- Whether worker scratch is also kept out of the worktree at setup (for example through a git exclude) is a Plan decision recorded in the ADR; this spec requires only that the busy check ignore the paths.
- A stale-ref removal is safe because with no `rebase-merge/`, `rebase-apply/` or unmerged entries nothing can resume from that ref.
- The consecutive-deferral count is in memory; a restart may lengthen the wait by up to N formations but never shorten it.
- Pausing, if chosen over rerouting, follows the engine's existing pause conventions (cause label, one deduplicated comment) rather than introducing a new mechanism.

## Out of Scope *(optional)*

- The CI classification of trials (separate work issue).
- Changing the merge train's overlap, admission or starvation-guard behaviour.
- Cleaning up scratch files or other leftovers from worktrees (the fix is to ignore them, not delete them), apart from removing a stale operation ref.
- Any change to how a really busy worktree is detected beyond what FR-001 to FR-004 narrow.
- Closing community report #2607, which is closed by hand once a release containing the fix has shipped.

## Source References *(optional)*

- Community report #2607 (mentioned for context: #2607), restated here as a work issue.
- `docs/state-machine.md` §6.30 (singleton catch-up) and the pipeline liveness section (partition `idle_reason`).
- ADR 2044 (singleton catch-up), ADR 2150 (unpushed engine rebase, `PrepareCatchUp`), ADR 2124 (pipeline liveness and stall events).
