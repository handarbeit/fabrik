# ADR 1347: Gate repo mutation and dispatch on write access

**Status:** Accepted
**Date:** 2026-08-02
**Issue:** [#1347](https://github.com/handarbeit/fabrik/issues/1347)

## Context

Fabrik derives its managed-repo set from board items, not configuration: any repo
that has ever had an issue tracked on the project board is treated as a repo Fabrik
administers. `poll.go`'s per-repo discovery block and `Run()`'s single-repo startup
block both unconditionally call `checkAllowAutoMerge` and `SeedLabels` (~25 label
`POST`s) for every repo seen, with no check that the configured token can actually
write to it.

Tracking an upstream issue on your own GitHub Projects board (a normal, legitimate
workflow — the `verveguy` project 5 case: `LadybugDB/ladybug`, `verveguy/liminis-graph`,
`verveguy/markdownload`, `verveguy/remarkable`) caused Fabrik to attempt ~25 label
creates against a repo the operator does not administer. The writes fail on
permissions and are logged non-fatal, so nothing surfaces it — but it is still an
unrequested write attempt against a third party's repository, visible in their audit
log. Worse, if the operator *does* happen to have write access to the upstream repo
(a contributor with triage/write role), the writes **succeed**, silently injecting
`fabrik:*`/`stage:*`/`model:*` labels into someone else's project. `checkAllowAutoMerge`
on the same path also produces a permanently unresolvable warning (its stated fix,
`gh api -X PATCH ... allow_auto_merge=true`, requires admin rights the operator by
definition doesn't have).

`GET /repos/{owner}/{repo}` — the same endpoint `FetchAllowAutoMerge` already calls
once per repo per run, cached via `checkedAutoMergeRepos` — returns a `permissions`
object (`push`, `admin`, `maintain`, `triage`, `pull`) for an authenticated request.
This is a write-access signal already arriving on the wire and being discarded.

## Decision

**Widen the existing probe, don't add a new one.** `FetchAllowAutoMerge(owner, repo)
(bool, error)` becomes `FetchRepoAccess(owner, repo) (RepoAccess, error)`, decoding
`permissions.push` alongside `allow_auto_merge` from the same response. `RepoAccess`
is a named struct (`{AllowAutoMerge, CanPush bool}`), not a wider tuple — the two
concerns are independent and a struct keeps them discoverable at call sites, at the
cost of a small, fully enumerable rename diff (4 implementations, ~6 test call sites).

**One shared cached resolver, three consumers.** `Engine.resolveRepoAccess(owner,
repo)` is the single source of truth, keyed `"owner/repo"` (matching the existing
`seededRepos`/`checkedAutoMergeRepos` convention), consulted identically by:

1. **Label seeding** (`poll.go`, both call sites) — skips `SeedLabels` when
   `!CanPush`.
2. **`checkAllowAutoMerge`** — skips its warning entirely when `!CanPush` (a repo
   Fabrik can't push to also can't have `allow_auto_merge` administered on it, so the
   unfixable-warning problem disappears as a side effect of the same gate).
3. **`itemMayNeedWork`'s dispatch gate** (`engine/item.go`) — a new early return,
   shaped exactly like the existing `stage.HoldingStage || stage.Unmanaged` check,
   denies dispatch for any item whose repo is cached as `!CanPush`.

This mirrors the codebase's existing "one shared resolver, multiple gates consult it
identically" convention (`effectiveReviewAuthority`/`effectiveExpectedReviewers`) and
guarantees the "reported once" notice (below) can live in exactly one place regardless
of which consumer resolves a given repo first.

**Board items in an unmanaged repo are skipped from dispatch entirely, not processed
read-only.** Nearly every stage past Specify/Research needs to push commits and
open/update PRs against the repo — exactly the access already found absent. Partial
processing would fail later, less clearly, closer to when a human is waiting on it.
The gate is placed in `itemMayNeedWork` (the shallow, no-deep-fetch prefilter that
already gates `HoldingStage`/`Unmanaged` stages the same way), not `itemNeedsWork` — a
rejected item never reaches `FetchItemDetails`, `selectDeepFetchCandidates`, or
`dispatchCandidates`, so it costs zero API calls beyond the one repo-access probe.

**The dispatch gate reads the cache; it never triggers a probe.** A repo not yet
resolved is admitted (fail-open on "unknown"), not gated. In production this default
never matters: `poll()`'s seeding block resolves every board-discovered repo before
`selectDeepFetchCandidates` runs, in the same poll cycle, for both the first time a
repo is seen and every poll after — an ordering the seeding block and dispatch gate
both rely on textually, not through a type-level guarantee.

**Fail open, cache the failure, for the rest of the process run.** If the probe
itself errors (network failure, transient 5xx — not a clean `200` with `push: false`),
`resolveRepoAccess` treats the repo as `{AllowAutoMerge: true, CanPush: true}` and
caches that result. This matches `checkAllowAutoMerge`'s prior error posture for the
same GET request and prioritizes not stranding a genuinely managed repo over closing
every edge case of a misprobed unmanaged one. A repo whose very first probe hits a
transient error is treated as managed for the rest of the run (self-heals on restart)
rather than losing dispatch for a blip — accepted as the safer default of the two.

**Gate on `permissions.push`, not `permissions.admin`.** This closes the issue's
concrete motivating case (zero-access upstream repos). A contributor with
push-but-not-admin access will still see the `allow_auto_merge` warning with its
admin-only remediation — a real, narrower residual gap, left out of scope: the
acceptance criteria for this issue don't exercise the push-vs-admin distinction, and
the spec frames the gate as "write access" throughout.

**No board-side mutation of any kind on an unmanaged repo, ever — not even to record
the determination.** Unlike `fabrik:blocked` (which persists its gate state as a label
on the blocked issue, in a repo Fabrik always has write access to by definition), the
repo being gated here IS the repo any label write would need to land in. The access
cache and its one-time log line are purely in-process (`Engine.repoAccess`) — never a
GitHub-side label, comment, or other mutation on the unmanaged repo or its issues.

**The "no write access" notice is a plain `logf` warn line, not a `warnings.Record`
TUI entry.** No existing `FixAction` value (`shell_command`, `fabrik_command`) fits
"remove the item from the board" — inventing a new informational `FixAction`
convention for this one notice was judged unnecessary scope.

## Consequences

- A repo the token cannot push to now produces zero label-mutation requests and no
  unfixable `allow_auto_merge` warning; its board items are skipped from dispatch with
  a single logged reason, reported once per repo per process run.
- A repo the token can write to is completely unaffected: same seeding, same
  auto-merge check, same call count as before this change — the write-access signal
  is decoded from a response already being fetched, not a new round-trip.
- The `FetchAllowAutoMerge` → `FetchRepoAccess` rename touches all four
  implementations (`github.Client`, `engine.mockGitHubClient`,
  `cmd.testGitHubUpgradeClient`, the `GitHubClient` interface) and their test call
  sites; `go build` catches any missed site immediately.
- A contributor with push-but-not-admin access on a repo still hits the
  `allow_auto_merge` unfixable-warning problem — not solved by this change. A future
  issue could split the `checkAllowAutoMerge`-specific suppression onto
  `permissions.admin` instead of `permissions.push` without touching the seeding or
  dispatch gates, which are correctly scoped to `push`.
- A repo whose first-ever probe hits a transient network error is treated as managed
  for the remainder of that process's lifetime, even if it is genuinely unwritable —
  self-heals on the next restart, and is strictly no worse than this codebase's
  unconditional pre-#1347 behavior for that one repo.
- **Cleanup stages are not exempted from the dispatch gate**, unlike the
  `fabrik:awaiting-done` gate a few lines above it in `itemMayNeedWork`. This was
  raised in PR review (#1356): doesn't a repo with no write access still need its
  local worktree cleaned up? No — `handleCleanupStage` is not a pure local
  filesystem operation, it calls `addLabel`/`removeLabel` (`stage:*:complete`,
  `fabrik:extend-turns`) and is reached via `acquireLockAndVerify`, which writes
  lock/`in_progress` labels first — all real GitHub mutations. Exempting cleanup
  would reopen exactly the write this ADR closes. Accepted consequence: a worktree
  already on disk for an issue whose repo is later resolved as `CanPush: false`
  (e.g., a pre-existing worktree from before this fix shipped, or a repo whose
  access was revoked after Fabrik had legitimately processed it) is never
  auto-cleaned — a disk-space leak, not a correctness or security issue. Manual
  removal of the worktree directory is the only path, consistent with this issue's
  own "retroactively removing labels already seeded" non-goal.
- **`checkAllowAutoMerge`'s `!CanPush` early return also clears any stale
  `allow_auto_merge` warning for the repo** (raised in PR review, #1356): a repo
  previously writable with `allow_auto_merge` disabled would have a warning
  recorded; if access is later revoked, the early return would otherwise skip past
  the function's own `Clear` branch forever (`checkedAutoMergeRepos` never lets it
  re-run for that repo in the same process), leaving an unfixable, now-moot warning
  immortal in `.fabrik/warnings.json`. This is a distinct case from #1348's
  stale-warning sweep, which only clears warnings for repos that have left the
  board entirely — a repo still on the board with newly revoked access is never
  "absent" from `seenRepos`, so #1348's sweep does not reach it.
