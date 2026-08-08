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
2. **Check B — already up to date.** `git rev-list --count HEAD..origin/$base_branch` after the fetch. Zero means rebasing would push nothing — skip. (This does not by itself address the reported livelock — see "Check B does not break the reported livelock" below.)
3. **Check C — CI has already run against the current base.** Compares the base branch's own commit time (`git log -1 --format=%ct origin/$base_branch`) against the *start* time of the PR's most recent successful *required*-check run — queried via `gh api graphql`'s `isRequired(pullRequestNumber:)` field on the PR's last commit's `statusCheckRollup` contexts (not `gh pr view --json statusCheckRollup`, which has no required/non-required distinction — see "Check C's required-check filter" below), filtered to `isRequired==true` and a successful conclusion, latest `startedAt` (not `completedAt` — see "Check C: completion time vs. start time" below), converted to epoch seconds. If the check run started after the base commit, skip. On read failure, an empty result, or a check run that started at or before the current base, **do not skip** — fall through to the rebase.
4. **Otherwise, rebase** — same command as before.

The internal merge-train's `Queued` column gets no equivalent live check: it is a `holding_stage`, and the engine never dispatches Validate for an issue sitting in that column, so the two states are mutually exclusive by construction. There is no live-Validate-session case for the skill to observe, so this is documented as a reachability argument rather than built as dead code.

The stage summary (`FABRIK_SUMMARY_BEGIN`/`END`) now always states which of five outcomes applied — `rebased`, `skipped-in-queue`, `skipped-detection-failed`, `skipped-up-to-date`, `skipped-ci-fresh` — so an operator reading the stage comment can distinguish a deliberate skip from an omission.

### Check B does not break the reported livelock

The first version of this design (reviewed pre-merge, superseded by the current text) claimed Check B alone broke the common-case livelock. That claim was wrong and has been removed rather than reworded, because it was load-bearing for the original design's justification and needs to stay visibly gone, not softened.

The livelock in the Problem section is specifically the case where the base advances faster than a required check can complete — meaning the branch is behind on *every* Validate attempt, by construction. Check B's zero-behind test can never fire true in that scenario, so it cannot be what stops the cycle. Check B is still worth keeping: it eliminates rebase cost in the (separate, also common) case where nothing has changed on the base since the last rebase. But the check that actually addresses the reported bug is Check C, which is why Check C's design got a second pass (see below) rather than being dropped once its original branch-protection premise was found to be wrong.

### Check C: from branch-protection proxy to direct CI-freshness measurement

The first version of Check C skipped the rebase when `gh api repos/{o}/{r}/branches/{b}/protection --jq .required_status_checks.strict` read exactly `false`, on the theory that a non-strict repo doesn't need up-to-date-ness to merge, so the rebase is pure cost there.

This was found to be the wrong criterion during pre-merge review, for two reasons, both confirmed empirically against `handarbeit/fabrik` itself:

1. **It conflates "GitHub won't enforce it" with "staleness is harmless."** `required_status_checks.strict: false` means being up to date is not a *merge precondition* — it says nothing about whether the last successful check run actually tested the current base. A PR can sit for days accumulating unrelated merges to `main` and still merge on a check that went green against a base state from before any of them landed.
2. **It fires unconditionally on every Validate invocation on this exact repo.** `gh api repos/handarbeit/fabrik/branches/main/protection --jq .required_status_checks` returns `{"contexts":["Analyze (go)","Verify llms-full.txt is up to date"],"strict":false}` — verified live during review. Under the branch-protection design, Fabrik would never again rebase at Validate on its own repository, a behavior change invisible from the diff and far larger than the issue asked for.

The replacement measures the actual condition directly instead of inferring it from a config proxy: has the most recent successful required-check run already completed against a base-branch state at least as new as the one currently at `origin/$base_branch`? If yes, a rebase restarts checks that already covered the same ground — pure cost, skip it. If no — including the pathological livelock case, where no successful run can ever be fresher than a base that keeps moving out from under it — the rebase still runs, exactly as it did before this issue.

This also removes ADR-933's 403 risk as an active concern for this check (see "Consequences" below): the branch-protection read is no longer part of Check C at all.

### Check C's required-check filter

The first version of the CI-freshness rewrite (above) read `gh pr view --json statusCheckRollup`, filtered to `conclusion=="SUCCESS"`, with no distinction between required and non-required checks. This was found during a second round of pre-merge review to reopen a narrower version of the same class of bug the branch-protection redesign fixed: on a PR with a fast, always-green, non-required check (a CLA bot, a license scanner, a notification job), that check's `completedAt` can be more recent than the actual required check's last successful run, making Check C read "fresh" and skip a rebase that a stale *required* check still needs — the exact pathological case this issue exists to close.

`gh pr view --json statusCheckRollup` has no field to distinguish required from non-required checks — its JSON shape (verified against the installed `gh` CLI) carries `conclusion`, `completedAt`, `name`, `status`, `workflowName`, and none of them encode required-ness. Re-adding the `branches/{b}/protection` REST read to get the list of required check-context names was considered and rejected: that is the exact call ADR-933 documented as 403-prone for tokens without admin-level repo access, and reintroducing it here for a different field would resurrect the same access risk that Check C's redesign had just removed.

The actual fix is GitHub GraphQL's `isRequired(pullRequestNumber:)` field, present on both `CheckRun` and `StatusContext` via the `RequirableByPullRequest` interface — confirmed live against `handarbeit/fabrik`'s own PR #1424: `isRequired` correctly reports `true` only for `Analyze (go)` and `Verify llms-full.txt is up to date`, matching this repo's actual `required_status_checks.contexts`, while `Test and vet`, `Analyze (actions)`, and `CodeQL` (informational, non-required jobs) report `false`. This is PR-scoped, like Check A's `isInMergeQueue` query — not repo-admin-scoped like `branches/{b}/protection` — so it carries none of ADR-933's 403 risk. Check C now filters `statusCheckRollup` contexts to `isRequired==true` before taking the latest successful run's timestamp.

### Check C: completion time vs. start time

Both versions of the CI-freshness rewrite above (unfiltered and required-filtered) compared the base commit's time against the required check's *completion* time (`completedAt`). A third round of pre-merge review found this compares the wrong pair of instants: `completedAt` tells you when a run *finished*, not what tree it *tested*. A long-running required check (the ADR's own livelock example cites 18 minutes) can be dispatched against a stale tree, still be running when a new base commit lands, and then complete successfully — the completion timestamp lands after the base commit's timestamp purely because the base moved *during* the run, not because the run tested it.

Concretely: base commit B lands at t1; the PR's already-in-flight required check — dispatched before t1, so testing a pre-B tree — completes successfully at t3 > t1; base then advances again to commit B2 at t2, with t1 < t2 < t3. Comparing `completedAt` gives `ci_epoch(t3) > base_epoch(t2)` and skips the rebase, even though the successful run tested neither B nor B2. This resurrects a narrower version of the same class of bug the branch-protection-proxy and unfiltered-`statusCheckRollup` designs were rejected for above: a signal that *correlates* with freshness (finished recently) mistaken for *proof* of it (tested the current tree).

The fix is to compare against `startedAt` instead: a check that started before the current base commit landed cannot have tested a tree containing it, regardless of when it finished, so requiring `startedAt > base_epoch` is a materially tighter bound than requiring `completedAt > base_epoch`. It is not a perfect bound — GitHub's checkout can lag slightly behind a workflow run's start event, and there remains a narrow theoretical window between a run starting and its actual checkout snapshotting the merge tree — but it closes the specific, concrete race identified above, which `completedAt` does not. Verifying the check's *tested SHA* is an ancestor of the base tip (rather than inferring freshness from either timestamp) was considered as a stronger alternative and rejected — see "Direct SHA-ancestry verification for Check C" below. `StatusContext` (the legacy commit-status type) has no `startedAt` equivalent; its `createdAt` — the timestamp of the status's first report — is used as the closest available proxy, since it is close to when the status began, not when it last changed (`updatedAt`, which would carry the same completion-time flaw).

### Check C: one fresh required check does not mean all required checks are fresh

The `startedAt` fix above still had a gap on a repo with more than one required check (this repo has two: `Analyze (go)` and `Verify llms-full.txt is up to date`): the jq filter took `sort | last` over the start times of every currently-successful required check — i.e. the single most-recently-started one. A fourth round of pre-merge review found this proves only that *a* required check is fresh, not that *every* required check is. If one required check reruns and starts after the current base lands while a sibling required check's last successful run predates it (or hasn't rerun at all since the base moved), the `last`-picked timestamp is the fresh one, `ci_epoch > base_epoch` holds, and the rebase is skipped — leaving the genuinely stale required check un-refreshed. This is the same failure shape the `isRequired` filter and the `startedAt`-vs-`completedAt` fix both exist to close (a signal that looks fresh masking one that isn't), recurring a third time, now across sibling required checks rather than required-vs-non-required or start-vs-completion.

The fix has two parts, both necessary: take the **minimum** start time across required checks instead of the maximum, since freshness must hold for the least-fresh one; and require that the count of currently-successful required checks equals the total count of required checks, since a required check that is pending, failed, or simply not in the rollup yet is not "fresh" in any sense the query can vouch for — dropping out of the `SUCCESS` filter without changing the total would otherwise let a still-pending required check silently fail to hold back the freshness verdict. The jq filter now binds `$required` to every required-check context regardless of outcome and `$ok` to the successful subset; when the two counts diverge, it emits nothing (falls through to rebase) rather than a timestamp computed from a partial view.

### Check order: harm, not requirement number

The checks run queue-safety → up-to-date → CI-freshness, ordered by the cost of getting each one wrong, not by the order the requirements happen to be numbered in the issue:

- A queued-PR push is **actively destructive** — it ejects the PR, and nothing downstream can undo that.
- A redundant rebase is **merely wasteful** — CI time spent restarting a check that would have passed anyway.
- A wrongly-skipped genuinely-required rebase is **the least-bad failure mode**, because the engine's own `fabrik:rebase-needed` path (`attemptMergeOnValidate` → `ErrNotMergeable`) catches a stale branch after Validate completes and dispatches a rebase re-invoke.

Checking up-to-date first (cheapest, no API call) was considered and rejected: it doesn't change correctness — all three still have to be evaluated before any push can happen — and it buries the highest-consequence check second, which reads worse and invites a future editor to reorder them without noticing the harm ordering was load-bearing.

### Asymmetric fail-safe defaults on detection failure

Check A defaults to **skip** on failure or ambiguity; Check C defaults to **rebase** (i.e., does not skip) on failure. This is deliberate, not an oversight, and the two checks must not be made symmetric:

- Check A guards against an **unrecoverable** action. A queued PR pushed to accidentally is ejected; nothing downstream repairs that. When the queue-membership signal is unavailable, the only safe default is to assume the worse case and skip.
- Check C guards against a **recoverable** one. A repo whose CI-freshness read fails or reports a stale/absent successful run still just gets today's rebase — the same cost the skill has always paid, caught and corrected by the engine's existing `fabrik:rebase-needed` path if it turns out to have been unnecessary. There is no plausible harm in defaulting to "still rebase" here, only redundant CI time in the false-positive case.

Applying the same default in both directions would either reopen the exact ejection risk this issue exists to close (if both defaulted to "proceed") or make Check C spuriously skip a genuinely required rebase on every transient `gh` read hiccup (if both defaulted to "skip"). The asymmetry is the point, and it survived the branch-protection → CI-freshness redesign unchanged: only the *condition* Check C evaluates changed, not its fail-toward-rebase default.

### Why FR-3 stays a SHOULD, not a MUST

Even after moving off the branch-protection read, FR-3 (the issue's original requirement covering this check) remains framed as a SHOULD rather than a MUST: the `statusCheckRollup`/`isRequired` GraphQL read can still be empty, incomplete, or transiently unavailable, and the whole point of Check C's fail-toward-rebase default is that none of those cases block the stage — they just fall through to the same rebase Fabrik has always performed. FR-3 was never a MUST because the skip is a pure optimization; that framing didn't depend on which detection mechanism backed it, so it needed no revision when the mechanism changed twice (branch-protection proxy → unfiltered CI-freshness → required-check-filtered CI-freshness).

ADR-933 documented that `GET .../branches/{b}/protection` 403s in practice for tokens without admin-level repo access. That finding no longer bears on Check C directly, since the protection endpoint isn't called there anymore — but it remains relevant background for why a branch-protection read was rejected as the detection mechanism in the first place (see "Check C: from branch-protection proxy to direct CI-freshness measurement" above).

## Alternatives Considered

### Making the engine's queue guards cover worker-initiated mutations

The engine could refuse to dispatch Validate at all for a PR it knows (from its poll-time `ProjectItem` snapshot) to be in a merge queue, extending ADR-058 D3's engine-side pattern instead of adding a parallel skill-side one. Rejected for this issue: it's a larger design question (what does the engine do instead — skip the stage entirely? defer it?), it doesn't address the up-to-date/strict-protection waste (FR-1/FR-3), and the issue's own scope explicitly carves it out as a plausible follow-up, not a blocker for the skill-side fix. It's also strictly narrower than the skill-side fix in one respect: the engine's snapshot is taken once per poll cycle before dispatch, so a PR that enters the queue mid-invocation (a live edge case for CI-fix re-invocation or `fabrik:revalidate`) would still slip past a dispatch-time check. A live, in-session `gh api graphql` read closes that staleness gap regardless of whether a dispatch-time guard is ever added later.

### Symmetric fail-safe defaults (both checks fall back to "still rebase")

This was Research's initial blanket suggestion — treat every detection failure the same way, falling back to today's unconditional behavior. Rejected specifically for Check A: the two checks guard against opposite-cost mistakes (see above), and applying the same default to both would silently reintroduce the exact merge-queue ejection risk this issue exists to close, in the narrow but real case of a flaky or malformed GraphQL response.

### Live detection for the internal merge-train `Queued` column

Considered giving FR-2's internal-queue clause an active runtime check, e.g. by exposing project-board Status to the worker via a new context file or environment variable. Rejected: `Queued` is a `holding_stage`, and current dispatch logic never runs Validate for an issue sitting in that column — the two states are mutually exclusive by construction, so there is no live case to detect. Building the check anyway would be dead code; plumbing new board-status visibility into the worker would itself be an engine change, which is explicitly out of this issue's scope.

### Branch-protection `strict` as the Check C criterion

This was the original design (see "Check C: from branch-protection proxy to direct CI-freshness measurement" above for the full account). Rejected after pre-merge review found it conflates "GitHub doesn't require up-to-date-ness to merge" with "the last check run is still fresh," and demonstrated the conflation empirically: `handarbeit/fabrik`'s own `main` branch protection has `strict: false`, so this design would have fired on every Validate invocation on this repo, disabling the rebase capability here entirely — a behavior change far larger than, and invisible from, what the issue's diff appeared to describe.

### Unfiltered `statusCheckRollup` (any successful check, not just required)

This was the second design (see "Check C's required-check filter" above for the full account) — `gh pr view --json statusCheckRollup` filtered only to `conclusion=="SUCCESS"`, with no required/non-required distinction. Rejected after a second pre-merge review round found it could make Check C read "fresh" off a fast, always-green, *non-required* check (a CLA bot, a license scanner) while the actual required check was still stale — reopening a narrower version of the exact pathological case this issue exists to close. Replaced by the `isRequired(pullRequestNumber:)` GraphQL filter, which is PR-scoped and carries none of the branch-protection endpoint's 403 risk.

### Comparing `completedAt` instead of `startedAt`

This was the third design (see "Check C: completion time vs. start time" above for the full account) — comparing the base commit's time against the required check's *completion* timestamp. Rejected after a third pre-merge review round identified a concrete race: a long-running required check dispatched against a stale tree can still be in flight when a new base commit lands, then complete successfully afterward, making its completion time read as "after" the new base purely because of wall-clock ordering, not because it tested that tree. Replaced by comparing `startedAt`, a materially tighter (though still not perfect) bound on which base state a run could have tested.

### Direct SHA-ancestry verification for Check C

Considered having Check C confirm the tested commit is actually an ancestor of `origin/$base_branch`'s tip, rather than inferring freshness from any timestamp. This would be the structurally correct check — timestamps are a proxy, ancestry is the actual fact in question. Rejected for this pass: the `statusCheckRollup` path used here reports checks against the PR's *head* commit, not the ephemeral `refs/pull/N/merge` tree a `pull_request`-triggered workflow actually tests, so there is no single exposed field giving "the base SHA this specific run's checkout resolved to" — reconstructing it reliably would need per-workflow-run introspection (e.g. correlating `checkSuite` data with the specific merge commit at dispatch time) beyond what a shell-scripted Bash tool call can reasonably do inside a stage's turn budget. The `startedAt` fix closes the concrete race identified in review without that added complexity; if a future report surfaces the residual (narrower) start-time-vs-checkout-lag gap in practice, SHA-ancestry verification is the natural next step.

## Consequences

**Positive:**
- Breaks the reported livelock (Check C alone: once a required-check run has already started against the current base, further rebases restart checks that would have passed anyway, so a converged PR stops restarting its own CI).
- Closes the merge-queue ejection gap for the skill-initiated rebase path, matching the engine-side ADR-058 D3 guarantee in spirit, on the one mutation site D3 didn't reach.
- Reduces wasted CI runs whenever the last successful check already covers the current base, regardless of the repo's branch-protection configuration.
- The stage summary's explicit outcome string gives operators a fast, unambiguous signal — no more inferring "did it skip the rebase or just forget" from a full transcript read.

**Negative / accepted costs:**
- Two additional `gh api graphql` calls per Validate invocation (Check A's `isInMergeQueue` query, Check C's `statusCheckRollup`/`isRequired` query) on top of the existing `gh pr view`/`git fetch`. Both are cheap, read-only, PR-scoped (not repo-admin-scoped), and bounded by the same rate limits the worker already operates under.
- **This changes Validate's rebase behavior on every repo where CI already ran against the current base — not just repos with unusual branch-protection config.** A reader auditing the blast radius should check Check C's GraphQL query (base commit time vs. the *earliest* start time among the PR's currently-successful required checks, requiring every required check to be successful) on an affected PR, not `branches/{b}/protection`; the two checks measure different things and the latter is no longer consulted by Check C at all.
- The internal-`Queued`-column reachability argument is unverified by a runtime check — if a future dispatch-logic change ever makes Validate reachable from that column, this ADR's reasoning would need revisiting alongside it.
- Check C's freshness comparison depends on `git log`'s local commit timestamp and `gh`'s `startedAt` both being trustworthy; a repo that rewrites base-branch history (force-push to `main`) after a check run started could produce a stale `base_epoch` that looks older than it should. This is a pre-existing category of risk for any timestamp-based comparison against a mutable branch, not new risk introduced by this design.
- The `startedAt` comparison is a bound, not a proof: it does not verify the check's actual tested SHA is an ancestor of the base tip, only that the run could not have started before that base state existed. See "Direct SHA-ancestry verification for Check C" above for why the stronger, SHA-based check was deferred rather than built now.
