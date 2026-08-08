# ADR 1364: Conditional Pre-Completion Rebase in fabrik-validate

**Date**: 2026-08-08
**Status**: Accepted
**Issue**: #1364 — fix(validate-skill): make the mandated Pre-Completion rebase conditional — it livelocks CI and ejects queued PRs

## Context

`fabrik-validate/SKILL.md`'s Pre-Completion Gate Step 1 rebased onto the newest base branch on **every** Validate invocation, unconditionally, inside a block headed MANDATORY. There was no up-to-date check, no merge-queue check, and no branch-protection check — just `gh pr view --json baseRefName` → `git fetch` → `git rebase`. The rebase pushes, and that push restarts CI.

**The livelock**: any repo whose required-check duration exceeds its merge interarrival time cannot converge under this rule — each Validate attempt rebases onto a newer base, restarting a check that cannot finish before the base moves again. Observed on `verveguy/liminis-context-graph#297` (2026-08-02): three Validate attempts, each rebasing onto a newer `main`, each restarting an 18-minute required check, each dying before it finished — eight CI runs on one branch. This was instructed behavior, not model error.

**The rebase was often not even required**: that repo's branch protection has `strict: false`, so "up to date with base" is not a merge precondition. The mandated rebase bought nothing there and cost eight CI runs.

**The rebase was not merge-queue-safe**: `docs/state-machine.md` §5.8 documents that any push, rebase, or base-branch change to a PR currently in a merge queue ejects it, and that two engine-side guards (`engine/catch_up_handlers.go`, `engine/merge_gate.go`) make every **engine-initiated** git/PR mutation queue-aware (ADR-058 D3). Step 1's rebase is **skill-initiated**, inside the worker process, so neither guard covered it. It is reachable via CI-fix re-invocation or `fabrik:revalidate` against a PR already enqueued on the ADR-058 path. An ejection there burns a cycle against `MaxEnqueueCycles` (default 5), after which the issue pauses.

This was reported and verified in #1345 (a `ScheduleWakeup`/background-tool report that, mid-investigation, surfaced this rebase defect as a separate, higher-priority root cause), then split out as this issue — the root-cause fix — from two independent siblings (#1365, tool suppression; #1366, imperative CI-wait framing, blocked by this issue since both edit the same file).

### Why the fix lives in the skill, not the engine

The engine already has a working pattern for queue-aware mutation (ADR-058 D3: `LinkedPRIsInMergeQueue`/`LinkedPRIsMergeQueueEnabled`, sourced from a GraphQL-populated `ProjectItem`). But Step 1's rebase runs inside a Claude Code subprocess in a git worktree, invoking `gh`/`git` via Bash — it has no access to Fabrik's internal Go structs or the poll-time `ProjectItem` snapshot, only what `gh`/`git` commands run from inside the worktree can observe, using the same GitHub token the engine itself uses (`engine/claude.go`). Making the engine refuse to dispatch Validate for an in-queue PR was considered (see Alternatives) but rejected as a larger design question that shouldn't block the skill-side fix, and out of this issue's scope by the issue's own framing.

## Decision

Replace the unconditional fetch/rebase with three ordered, short-circuiting checks. Each either skips the rebase (recording why, for the stage summary) or falls through to the next; if none apply, the rebase runs exactly as before.

1. **Check A — merge-queue safety.** Query the PR's live `isInMergeQueue` via `gh api graphql` (not available through `gh pr view --json` — verified against the installed `gh` CLI, whose field whitelist has no merge-queue fields). If `true`, skip. **If the query fails or returns anything other than exactly `true`/`false`, also skip** — treat "unknown" as "queued."
2. **Check B — already up to date.** `git rev-list --count HEAD..origin/$base_branch` after the fetch. Zero means rebasing would push nothing — skip.
3. **Check C — branch protection doesn't require up to date.** `gh api repos/{o}/{r}/branches/{b}/protection --jq .required_status_checks.strict`. If exactly `false`, skip. On read failure (commonly a 403 for tokens without admin-level repo access — see below) or any other value (including `true` or `null`), **do not skip** — fall through to the rebase.
4. **Otherwise, rebase** — same command as before.

The internal merge-train's `Queued` column gets no equivalent live check: it is a `holding_stage`, and the engine never dispatches Validate for an issue sitting in that column, so the two states are mutually exclusive by construction. There is no live-Validate-session case for the skill to observe, so this is documented as a reachability argument rather than built as dead code.

The stage summary (`FABRIK_SUMMARY_BEGIN`/`END`) now always states which of five outcomes applied — `rebased`, `skipped-in-queue`, `skipped-detection-failed`, `skipped-up-to-date`, `skipped-non-strict` — so an operator reading the stage comment can distinguish a deliberate skip from an omission.

### Check order: harm, not requirement number

The checks run queue-safety → up-to-date → strict-protection, ordered by the cost of getting each one wrong, not by the order the requirements happen to be numbered in the issue:

- A queued-PR push is **actively destructive** — it ejects the PR, and nothing downstream can undo that.
- A redundant rebase is **merely wasteful** — CI time spent restarting a check that would have passed anyway.
- A wrongly-skipped genuinely-required rebase is **the least-bad failure mode**, because the engine's own `fabrik:rebase-needed` path (`attemptMergeOnValidate` → `ErrNotMergeable`) catches a stale branch after Validate completes and dispatches a rebase re-invoke.

Checking up-to-date first (cheapest, no API call) was considered and rejected: it doesn't change correctness — all three still have to be evaluated before any push can happen — and it buries the highest-consequence check second, which reads worse and invites a future editor to reorder them without noticing the harm ordering was load-bearing.

### Asymmetric fail-safe defaults on detection failure

Check A defaults to **skip** on failure or ambiguity; Check C defaults to **rebase** (i.e., does not skip) on failure. This is deliberate, not an oversight, and the two checks must not be made symmetric:

- Check A guards against an **unrecoverable** action. A queued PR pushed to accidentally is ejected; nothing downstream repairs that. When the queue-membership signal is unavailable, the only safe default is to assume the worse case and skip.
- Check C guards against a **recoverable** one. A repo whose protection read fails or reports non-`false` still just gets today's rebase — the same cost the skill has always paid, caught and corrected by the engine's existing `fabrik:rebase-needed` path if it turns out to have been unnecessary. There is no plausible harm in defaulting to "still rebase" here, only redundant CI time in the false-positive case.

Applying the same default in both directions would either reopen the exact ejection risk this issue exists to close (if both defaulted to "proceed") or make Check C spuriously skip a genuinely required rebase on every transient `gh api` hiccup (if both defaulted to "skip"). The asymmetry is the point.

### Why branch-protection reads are a SHOULD, not a MUST (FR-3)

`gh api repos/{o}/{r}/branches/{b}/protection` is the exact API call ADR-933 already documented as unreliable: it 403s in practice for classic PATs and tokens without admin-level repo access, which Fabrik's documented minimal scopes (`repo`, `project`, `workflow`) don't reliably cover. Since the worker's `gh` calls run under the same token the engine uses, ADR-933's finding transfers directly — this is not new risk, it is a known, already-hit limitation of the same credential. That is why the issue frames FR-3 as a SHOULD and why Check C's failure mode is "fall through to the safe, already-existing behavior" rather than block the stage.

A `jq` read of `.required_status_checks.strict` on a repo with no required-status-checks configuration returns `null`, not `false`. This correctly falls through to "still rebase" (the fail-safe direction) under the "any value other than exactly `false`" rule above — not a bug, but worth calling out explicitly so a future reader doesn't mistake the `null` case for something that needs special-casing.

## Alternatives Considered

### Making the engine's queue guards cover worker-initiated mutations

The engine could refuse to dispatch Validate at all for a PR it knows (from its poll-time `ProjectItem` snapshot) to be in a merge queue, extending ADR-058 D3's engine-side pattern instead of adding a parallel skill-side one. Rejected for this issue: it's a larger design question (what does the engine do instead — skip the stage entirely? defer it?), it doesn't address the up-to-date/strict-protection waste (FR-1/FR-3), and the issue's own scope explicitly carves it out as a plausible follow-up, not a blocker for the skill-side fix. It's also strictly narrower than the skill-side fix in one respect: the engine's snapshot is taken once per poll cycle before dispatch, so a PR that enters the queue mid-invocation (a live edge case for CI-fix re-invocation or `fabrik:revalidate`) would still slip past a dispatch-time check. A live, in-session `gh api graphql` read closes that staleness gap regardless of whether a dispatch-time guard is ever added later.

### Symmetric fail-safe defaults (both checks fall back to "still rebase")

This was Research's initial blanket suggestion — treat every detection failure the same way, falling back to today's unconditional behavior. Rejected specifically for Check A: the two checks guard against opposite-cost mistakes (see above), and applying the same default to both would silently reintroduce the exact merge-queue ejection risk this issue exists to close, in the narrow but real case of a flaky or malformed GraphQL response.

### Live detection for the internal merge-train `Queued` column

Considered giving FR-2's internal-queue clause an active runtime check, e.g. by exposing project-board Status to the worker via a new context file or environment variable. Rejected: `Queued` is a `holding_stage`, and current dispatch logic never runs Validate for an issue sitting in that column — the two states are mutually exclusive by construction, so there is no live case to detect. Building the check anyway would be dead code; plumbing new board-status visibility into the worker would itself be an engine change, which is explicitly out of this issue's scope.

## Consequences

**Positive:**
- Breaks the common-case livelock (FR-1 alone: an up-to-date branch never gets rebased, so a converged PR stops restarting its own CI).
- Closes the merge-queue ejection gap for the skill-initiated rebase path, matching the engine-side ADR-058 D3 guarantee in spirit, on the one mutation site D3 didn't reach.
- Reduces wasted CI runs on non-strict repos where up-to-date was never a merge precondition.
- The stage summary's explicit outcome string gives operators a fast, unambiguous signal — no more inferring "did it skip the rebase or just forget" from a full transcript read.

**Negative / accepted costs:**
- Two additional `gh` calls per Validate invocation (Check A's GraphQL query, Check C's protection read) on top of the existing `gh pr view`/`git fetch`. Both are cheap, read-only, and bounded by the same rate limits the worker already operates under.
- Check C's protection read will 403 on a meaningful fraction of repos (per ADR-933); those repos always fall through to "still rebase" — no worse than the unconditional behavior this issue replaces, just without the (rare, since FR-3 is a SHOULD) benefit of the skip.
- The internal-`Queued`-column reachability argument is unverified by a runtime check — if a future dispatch-logic change ever makes Validate reachable from that column, this ADR's reasoning would need revisiting alongside it.
