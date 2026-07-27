# ADR 1096: Explicit Issue Close on Non-Default-Base Merge

**Date**: 2026-07-26
**Status**: Accepted
**Issue**: #1096 — Engine must close the issue on a non-default-base merge (`base:<branch>`): cruise & non-train-yolo paths never call CloseIssue

## Context

GitHub only honours `Closes #N` auto-close when the merging PR's base branch is the repository's
*default* branch — long-standing, documented, by-design GitHub behavior, not something Fabrik can
work around on GitHub's side. `base:<branch>` (#1046) is a first-class, documented Fabrik feature for
repos running a two-branch trunk (e.g. `develop` → `main`). On such a repo, every Fabrik PR targets a
non-default base, so `Closes #N` is inert on every merge — nothing ever closes the issue on the
**cruise** or **non-train-yolo** paths. Because native GitHub issue-dependency edges (and Fabrik's own
`checkDependencies` gate) unblock dependents on issue *close*, not PR *merge*, an entire dependency
wave stalls behind work that has already shipped. Reported by the community in #1095.

Every existing engine-side `CloseIssue` call was confined to paths a cruise/non-train-yolo item never
reaches: the merge-train batching path (`engine/merge_train.go`), the merge-train member-close settle
(`engine/merge_train_member_close_settle.go`), and the `FABRIK_NO_WORK_NEEDED` short-circuit
(`engine/no_work_needed_settle.go`). The two paths that actually carry the overwhelming majority of
merges — `runValidatePRTerminalAdvance` (cruise) and `advanceConvergedPRToDone` (non-train yolo) — did
the board advance to Done but never closed the issue. Fabrik's own self-hosting pipeline never hit
this because it targets `main` = the default branch, where GitHub's auto-close already does the work.

## Decision

Add one shared helper, `closeIssueIfNonDefaultBase(item, prNumber)` (`engine/close_nondefault_base.go`),
called from both terminal-advance sites immediately after their respective `advanceToNextStage` calls:

- `runValidatePRTerminalAdvance` (`engine/pr_terminal_advance.go`), inside the `pr.Merged` branch —
  always a confirmed merge at that point.
- `advanceConvergedPRToDone` (`engine/merge_gate.go`), gated on a new `merged bool` parameter passed
  by its callers (see "Merged vs. closed-without-merging" below).

The helper skips PR board items and, for the common case (no `base:<branch>` label — guaranteed by
`baseBranchForItem`'s contract to resolve to exactly the repository default), returns without ever
touching the `WorktreeManager` or git. For a labeled item it resolves the base and the repo's actual
default and calls `CloseIssue` only when they differ, treating an already-closed issue,
`ErrNotFound`, and any `WorktreeManager`/`DefaultBaseBranch()` resolution failure as non-blocking
outcomes (logged, never propagated). The full as-built behavior — including the exact log strings and
error handling — is documented in `docs/state-machine.md` §2.8 ("Outbound case — engine-initiated
close") and in the helper's own doc comment (`engine/close_nondefault_base.go`); this ADR covers the
design decisions below, not the mechanics.

### Merged vs. closed-without-merging

`advanceConvergedPRToDone`'s terminal-first guard also advances a PR closed *without* merging to Done
(pre-existing behavior, out of scope here). Explicitly closing the *issue* in that case would be a
correctness bug distinct from what this issue asks for — the spec requires the close to fire "on a
confirmed merge." `advanceConvergedPRToDone` therefore takes a `merged bool` parameter, computed by its
callers from the already-authoritative merge-confirmation machinery described in
`docs/state-machine.md` (§5.5 auto-merge convergence table, row ①; `settlePRMergeState`/
`PRMergeTerminal`) — `closeIssueIfNonDefaultBase` is only called when `merged` is true.

## Rationale

### Why a single shared helper rather than inlining at each call site?

The spec's primary named risk is a copy-paste divergence between the two sites producing a
double-close on one side or a silent no-close on the other. Routing both through one helper makes the
base-vs-default comparison and the double-close guard structurally impossible to diverge.

### Why the `itemHasBaseLabel` fast path?

Without it, every single merged-PR advance — the overwhelming majority of which carry no `base:`
label at all — would need a `WorktreeManager` lookup and, for `runValidatePRTerminalAdvance`
specifically, would risk the panic described next. `baseBranchForItem`'s own contract (an unlabeled
item always resolves to the default) makes this fast path exact, not an approximation.

### Why a non-panicking WorktreeManager lookup instead of `e.worktreesFor`?

`advanceConvergedPRToDone` is always called after `checkAutoMergeConvergence`'s own
`ensureRepoReady(ctx, item)` call, so its `WorktreeManager` is always registered — using the panicking
`e.worktreesFor` there would be safe. `runValidatePRTerminalAdvance` has no such guarantee: it is
invoked unconditionally over `deepFetchCandidates` with no preceding `ensureRepoReady` call anywhere
in its call chain. On the first poll after an engine restart, a `base:`-labeled item whose PR merged
while the engine was down can reach this helper before any dispatch has registered a
`WorktreeManager` for that repo this process lifetime — a plausible production scenario (restart with
a backlog of already-merged PRs), not a contrived edge case. `TestCheckAutoMergeConvergence_UnregisteredRepo_NoPanic`
already exists to guard the sibling call site against exactly this class of panic; this helper reuses
`worktreeExistsForItem`'s established non-panicking pattern rather than introducing a new failure
mode. Threading a `context.Context` through `runValidatePRTerminalAdvance` to call `ensureRepoReady`
directly was rejected: ADR-057 scopes that function as synchronous and label-mutation-only, with no
new *unconditional* blocking dependency. This helper does not fully preserve that property — for the
minority of items that carry a `base:<branch>` label, `baseBranchForItem`/`DefaultBaseBranch()` run
synchronously and may shell out to git (`DefaultBaseBranch()`'s `git ls-remote` fallback has no
timeout, a pre-existing characteristic shared with its other callers, not introduced here). The
`itemHasBaseLabel` fast path keeps the common case (no label) exactly as blocking-free as ADR-057
requires; bounding the labeled-item git calls with a timeout is tracked as a separate cross-cutting
follow-up, not scoped to this issue.

### Why gate `advanceConvergedPRToDone` on an explicit `merged bool` rather than reusing "reached this function"?

`advanceConvergedPRToDone`'s terminal-first branch is, by existing design, reached for both a genuine
merge and a PR closed without merging — advancing either to Done. Explicitly closing the *issue* in
the closed-without-merging case would close an issue whose work was never actually shipped, which is
a correctness regression the spec's "confirmed merge" wording rules out. Since only the callers know
which case they are in (the function's own body already receives the ambiguity baked into
`prNumber`), the confirmed-merge determination is pushed to the call sites, each of which already has
(or can cheaply obtain) an authoritative answer.

### Why `pr-terminal` as the log tag from both call sites?

The spec's required log strings hardcode `pr-terminal` literally
(`[pr-terminal] issue #N base %q ≠ default %q — closing explicitly`). Using it from both sites, rather
than `auto-merge` in `advanceConvergedPRToDone` to match that file's own usual tag, treats the
explicit-close decision as one cross-cutting concern with a single grep-able tag — more useful for
debugging a close-related issue across both paths than caller-local tag consistency.

### Why `item.IsClosed` pre-check *and* `ErrNotFound` post-check?

The pre-check matches house style (`no_work_needed_settle.go`, `settleMergeTrainMemberClose`) and
saves an API call in the common already-closed case (e.g. GitHub's own auto-close raced ahead on a
mislabeled item). The post-check handles the rarer case where the board cache's `IsClosed` is stale.
Both are cheap; the spec explicitly requires `ErrNotFound`/already-closed to be treated as success.

## Consequences

**Positive:**
- A cruise or non-train-yolo item on a `base:<branch>` repo whose PR merges is now closed by the
  engine itself; native GitHub dependency edges and Fabrik's `checkDependencies` gate clear on close
  as they always expected to, unblocking downstream work without manual intervention.
- The base==default guard is structurally centralized, eliminating the double-close risk the spec
  named as its primary concern.
- `runValidatePRTerminalAdvance` gains no new panic surface despite gaining its first
  `WorktreeManager`/git dependency.

**Negative / Trade-offs:**
- The close is best-effort, not durable: if `CloseIssue` fails after the board has already advanced to
  Done, nothing currently retries it. This is deliberately out of scope here (see Issue B below) —
  the failure is loudly logged, never silently discarded.
- A `base:`-labeled item reaching either call site while its repo's `WorktreeManager` is unregistered
  (restart-window edge case) logs a warning and skips the explicit close for that one poll pass; since
  neither call site is retried specifically for this reason, and durability is explicitly out of
  scope, such an item could remain open on a race that resolves before the next opportunity to observe
  it. This mirrors the general best-effort/non-durable scoping decision above, not a new, separate
  risk.

## Sibling Audit

`engine/merge_train.go`'s existing member-issue close (unconditional, not gated on base != default —
see its own comment "the fallback for non-default bases and auto-close lag") and the
`FABRIK_NO_WORK_NEEDED` short-circuit's close (`no_work_needed_settle.go`) already close explicitly
and are unaffected by this change; both were explicitly out of scope per the issue's Scope section.

Durable retry/escalation for a failed explicit close — modeled on the `fabrik:awaiting-member-close`
settle pattern (ADR-061) — is chained as a follow-up issue ("Issue B") rather than folded in here, per
the issue's explicit scope split.

**References:** [ADR-061: Merge-Train Member-Issue Close Retry](061-merge-train-member-close-retry.md), [ADR-057: Single-Owner Validate PR Terminal Advance](057-validate-pr-terminal-advance.md)
