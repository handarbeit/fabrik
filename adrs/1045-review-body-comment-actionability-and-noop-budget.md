# ADR 1045: Review-body comments are actionable regardless of state; a no-op reinvoke doesn't spend the cycle budget

**Status:** Accepted
**Date:** 2026-08-09
**Issue:** [#1045](https://github.com/handarbeit/fabrik/issues/1045)

**Amends:** [ADR-1375: `review_authority: authoritative` reinvokes on unresolved feedback — it never pauses first](1375-review-authority-reinvoke-not-pause.md)

## Context

ADR-1375 added `buildReviewBodyComments` (`engine/reviews.go`) to route a
`CHANGES_REQUESTED` review's top-level body into the reinvoke path, deliberately
excluding `COMMENTED` reviews: "automated reviewers (Copilot, Gemini) routinely submit
a `COMMENTED` review whose body is a generic 'Pull request overview' summary... treating
that as actionable feedback would trigger a reinvoke/Claude invocation on every
`wait_for_reviews` stage... far beyond a `CHANGES_REQUESTED` verdict's scope." That
reasoning was correct for Copilot and Gemini, and remains correct for them.

It stopped being correct once `handarbeit/fabrik` itself started running Pruefer
(`handarbeit-pruefer`) as its review bot. Pruefer submits **exclusively** `COMMENTED`
reviews by design — #1251 shipped its `REQUEST_CHANGES` as severity-gated and
raise-only, and `request_changes_threshold` is unconfigured on this instance, so every
finding set lands as `COMMENTED`. The gate itself clears normally (a `COMMENTED` review
is a non-`DISMISSED` review from an author, so `wait_for_reviews` is satisfied), but
`buildReviewBodyComments`'s `State != "CHANGES_REQUESTED"` filter meant the findings in
that body were never routed to the fixer. An unaddressed Pruefer finding on PR #1388
was caught only by hand review before merge — #1207's "yolo merges past unresolved
feedback" failure shape, arriving through a filter instead of a race.

A second, independent gap existed alongside this: a bot delivering its findings as a
**plain PR body or issue comment**, with no formal review submission at all (`@copilot
review`'s observed behavior on other repos), was never covered by
`buildReviewBodyComments` in the first place — that function only ever sees
`item.LinkedPRReviews`, which GitHub populates only for formal review submissions. This
comment reaches `processComments` through the ordinary "new comment arrived" path,
indistinguishable at the engine level from a human's comment — and the Review comment
skill's "act only on what was explicitly decided" framing caused it to refuse: *"This is
new feedback from a bot, not an explicit decision from you."* The review gate then timed
out and paused the issue with the findings never addressed.

An earlier design considered for the first gap — discriminating `COMMENTED`-body
actionability by review *author* via `expected_reviewers` (ADR-1283), since
`.fabrik/stages/validate.yaml` already declares `handarbeit-pruefer` — was proposed and
withdrawn during this issue's own drafting. It makes gate correctness depend on an
operator having correctly declared every substantive reviewer; an undeclared reviewer's
`COMMENTED` findings would be silently dropped exactly as before, which is #1407's
failure shape ("silently drops an undeclared identity's findings") relocated from one
subsystem into another rather than fixed.

## Decision

**Discriminate on review *state*, not author.** `buildReviewBodyCommentsFromReviews`
now admits any review with a non-empty body whose state is not `DISMISSED` or
`PENDING` — `COMMENTED` and `APPROVED` bodies are both treated as potentially
actionable (approve-with-nits is real feedback), alongside the pre-existing
`CHANGES_REQUESTED` case. No author allow-list, no dependence on `expected_reviewers`
or any other operator-declared identity. `reviewGateAuthorityVerdict` — the *merge/
landing* decision, a separate function — is unchanged: it still treats only
`CHANGES_REQUESTED` as blocking, exactly as ADR-1250/ADR-1375 established.
`review_authority` continues to govern merging, never working.

Removing the filter reopens the exact cost ADR-1375's exclusion was hedging against: a
Copilot/Gemini `COMMENTED` overview now dispatches a real reinvoke on every
`wait_for_reviews` stage, not just on the reviewers that matter. Two further decisions
pay that cost down rather than accepting it as-is:

**A reinvoke that lands no new commit does not spend the review-cycle budget.**
`dispatchReviewReinvoke` now mirrors `dispatchCIFixReinvoke`'s existing `gitHeadSHA`
before/after pattern (`engine/ci.go`): it snapshots `HEAD` in `build()`, before
`processComments` runs, and compares it again in a new `after` hook. When they match —
and only when the "before" snapshot was itself readable, see the scoping note below —
it applies a new compensating mutation, `itemstate.ReviewCycleDecremented` (floored at
0), against the `ReviewCycleIncremented` that `handleReviewGate` already applied
synchronously before dispatch. This is `dispatchWithCycleLimit`'s pre-dispatch
increment left exactly as-is — rebase and CI-fix cycle dispatch share that scaffold,
and restructuring *when* it fires was rejected as unnecessarily broad for a change
requirement 2 only asks of the review-cycle counter specifically. A decrement, applied
post-hoc from a hook that already exists (`reinvokeOpts.after`) and already runs
synchronously inside the goroutine before the deferred `WorkerExited` fires, is the
smaller, better-isolated shape: "a cycle that changed nothing was not an attempt," the
same principle #1199 established for turn-cap preemption against `max_retries`, applied
here to a different budget with no shared code path — the two features solve
structurally different problems (#1199 routes a *known* non-failure outcome to a
separate counter at increment time; this ADR *retroactively exempts* an outcome that
can only be known after the fact).

The no-op check is scoped to **"no new commit," uniformly** — it does not special-case
"Claude was invoked and did nothing" versus "the batch was filtered to empty before
Claude ever ran" (the pre-existing #1221 bot-service-notice chokepoint). In practice it
cannot reach the #1221 case at all: `processComments`'s notice filter
(`engine/comments.go`) runs before `wm.EnsureWorktree`, so when every candidate comment
is filtered out, the worktree is never touched that cycle. `gitHeadSHA(workDir)` then
fails identically in `build()` and in `after()` — there is no `HEAD` to compare, and the
`headBefore != ""` guard means no decrement fires. This is deliberate, not an
oversight: the mechanism only compensates a cycle it can *positively prove* made no
PR-visible change; it never guesses on a cycle it simply couldn't measure. #1221's
chokepoint therefore remains its own, separately-tracked, unfixed-by-design trade-off
(`docs/state-machine.md` §6.2) — `TestCatchUpLoop_ReviewReinvoke_AllNoticeThread_NoInvocation`'s
assertion (`ReviewCycles == 1`) is unchanged by this ADR.

A batch that resolves a review thread but lands no commit is also decremented under
this scoping — "PR mutation" is read as "commit landed," matching CI-fix's existing
definition exactly, not extended to cover thread resolution or the reinvoke summary
comment. A pure-resolution reinvoke is a small genuine attempt in principle; treating it
as a no-op is an accepted trade-off, revisited only if it proves to matter in practice.

**A plain bot body/issue comment is marked as bot review content at render time.**
`buildCommentReviewPrompt` (`engine/claude.go`) now renders a `[Bot Review Finding]`
marker on any non-thread comment (`c.Path == ""`, i.e. lacking the `**File:**`/`**Diff
context:**` headers a real inline thread comment gets) whose author matches
`gh.IsBotLogin` — the same structural bot-detection helper `isBotServiceNotice` already
uses. This covers both delivery shapes uniformly: a genuinely plain PR body/issue
comment with no review submission (the original report; its `ID` carries no
`review-body:` prefix), and a synthetic review-body comment `buildReviewBodyCommentsFromReviews`
produces for a `COMMENTED`/`APPROVED` review (its `ID` does carry that prefix) — both
render with `c.Path == ""`, and the marker's job is identical for either: tell the
comment-processing skill "this is bot review content to evaluate and act on
autonomously," not "this is a human's decision to interpret." The marker text
deliberately does not claim "no formal review submitted," since that would be false for
the second shape.

`gh.IsBotLogin` only recognizes a self-submitting review bot whose login carries a
`[bot]`/`-bot` suffix or matches one of a handful of literals — and GitHub's GraphQL API
(`item.LinkedPRReviews`, the default ingestion path for a synthetic review-body comment)
reports that suffix *differently* than its REST API does: REST's `user.login` includes it
(`handarbeit-pruefer[bot]`), GraphQL's `Bot.login` omits it (`handarbeit-pruefer`) — see
`stripBotSuffix`'s doc comment (`engine/reviews.go`), a distinction ADR-1283 already had
to account for on the `expected_reviewers` side. Without correcting for this, the marker
would silently fail to apply to a synthetic review-body comment sourced via the default
GraphQL path (base:<branch> items are the only ones that hit the REST fallback,
`FetchPRReviews`) whenever the bot's login has no other recognizable pattern — which is
exactly Pruefer's shape, the reviewer this ADR's motivating scenario is about. `github/
project.go`'s `applyLinkedPRs` therefore now queries `__typename` on a review's `author`
(mirroring the pattern `reviewRequests` already used for the same purpose) and
normalizes a `Bot`-typed author to the REST-shaped, suffix-carrying form at ingestion, so
`PRReview.Author` is canonically `[bot]`-suffixed regardless of which API populated it —
letting `gh.IsBotLogin` (and every other consumer of `PRReview.Author`, e.g.
`reviewerIdentityMatches`) behave identically either way.

`fabrik-review-comment/SKILL.md` gains an explicit carve-out: a `[Bot Review
Finding]`-marked comment is evaluated and fixed on its merits, the same as an inline
thread comment, rather than gated behind the skill's "act only on what was explicitly
decided" rule — the literal wording that caused the original repro's refusal.

**Both comment skills state an explicit no-op contract.** Removing ADR-1375's filter and
routing plain bot comments to the fixer both raise the same risk: a reinvoke handed
content with nothing actionable (a generic "Pull request overview" summary, most
commonly) could confabulate a speculative change rather than correctly doing nothing. A
pushed commit in response to noise can draw a *fresh* bot review with a new
`DatabaseID` — dedup does not help, since dedup keys on the ID the noise already
consumed — consuming another review cycle on feedback that was never real. Both
`fabrik-review-comment/SKILL.md` and `fabrik-validate-comment/SKILL.md` (Validate also
runs `wait_for_reviews: true` and shares the same `dispatchReviewReinvoke` →
`processComments` → comment-skill path) now state: no actionable findings means change
nothing and complete, not manufacture a fix. This is skill text only, not engine
logic — the engine cannot verify a model actually complied with a prose instruction, so
requirement 2's counter exemption (a structural `gitHeadSHA` signal) is what actually
bounds the cost of a model that ignores this contract; the no-op contract is what makes
ignoring it the wrong choice to make in the first place, not what enforces the outcome.

## Consequences

**Blast radius: every `wait_for_reviews` stage using Copilot or Gemini gets more
reinvoke dispatches than before, for every generic `COMMENTED` overview.** This is
exactly what ADR-1375's exclusion was hedging against, and the hedge does not become
wrong here — it becomes a cost this ADR accepts, bounded to tokens rather than budget by
the no-op exemption. Operators running `wait_for_reviews` with either bot should expect
one additional reinvoke per PR that previously would have been silently skipped; it
should self-resolve as a no-op (no commit, cycle counter unchanged) as long as the
comment skill's no-op contract holds. Call this out in release notes for the version
this ships in.

**The blast radius is not limited to bots.** The state filter this ADR removes did not
distinguish bot authors from human ones, and neither does its replacement — any
non-`DISMISSED`/non-`PENDING` review's body is now actionable regardless of who
submitted it. A **human** reviewer's `APPROVED` review carrying a note ("LGTM, minor
nit: rename `foo` to `bar`") now also triggers a reinvoke dispatch, where before this
ADR it did not (`APPROVED` was excluded outright by ADR-1375, for any author). This is
intentional — Requirement 1 states "approve-with-nits is real feedback" explicitly, and
the no-op exemption bounds its cost identically to the bot case — but it is a materially
wider behavior change than "Copilot/Gemini `COMMENTED` overviews" alone, and should be
described that way in release notes and to operators, not narrowed to the bot framing
that motivated this ADR (Pruefer review finding, PR #1473).

**`TestBuildReviewBodyComments_SkipsCommented` is inverted, not merely deleted.** The old
test asserted `COMMENTED` produces zero comments; the new
`TestBuildReviewBodyComments_CommentedIsActionable` asserts the opposite for the same
input shape, and a manual check (documented in the PR) confirms that restoring the old
`State != "CHANGES_REQUESTED"` filter turns the new test red — the acceptance criterion
this ADR's requirement 1 names explicitly.
`TestBuildReviewBodyComments_SkipsApprovedAndDismissed` is likewise split: DISMISSED
stays skipped (`TestBuildReviewBodyComments_SkipsDismissed`), APPROVED is now actionable
(`TestBuildReviewBodyComments_ApprovedIsActionable`). A new regression test,
`TestBuildReviewBodyComments_CommentedActionableRegardlessOfAuthor`, asserts an
undeclared reviewer's `COMMENTED` body is still picked up — guarding against
accidentally reintroducing the withdrawn `expected_reviewers`-based discriminator.

**The no-op exemption is only genuinely exercisable against a real git worktree.** A test
harness using a fake (never-cloned) `WorktreeManager` cannot exercise the no-op check at
all: `gitHeadSHA` fails identically before and after regardless of what actually
happened, and the `headBefore != ""` guard means no decrement ever fires — the test
would trivially "pass" without exercising the mechanism it claims to. AC3
(`TestHandleReviewGate_NoOpReinvoke_LeavesCycleCounterUnchanged`) and AC4
(`TestHandleReviewGate_FiveNoOpReinvokes_DoNotExhaustBudget_SixthGenuineFindingAddressed`)
both use a real git-backed engine (`testEngineWithRepoAndStages`, `skipIfNoGit`) and
pre-create the worktree before dispatching, matching the real "Implement already ran"
precondition every genuine reinvoke has in production.

**No new label, no new engine-visible state beyond the counter itself.** `ReviewCycleDecremented`
mirrors `ReviewCycleIncremented`'s shape exactly (floored at 0 in the store) and is
consulted nowhere except through `Snapshot.ReviewCycles`, the same read path every
existing cycle-limit check already uses.

## Alternatives Considered

**Author-based discriminator via `expected_reviewers` (withdrawn).** See Context above —
relocates #1407's failure shape rather than fixing it, since gate correctness would then
depend on an operator having declared every substantive reviewer.

**Restructure `dispatchWithCycleLimit` so the increment itself moves into the `after`
hook**, rather than a post-hoc compensating decrement. Rejected: `dispatchWithCycleLimit`
is shared by review, rebase, and CI-fix cycle dispatch; moving the increment would
require either touching all three call sites' semantics or forking the scaffold for
review alone, and the pre-dispatch synchronous check that guards against double-dispatch
(`snap.Worker() != nil`) already gives a decrement-after-increment shape the same
race-safety a restructured increment would need to establish from scratch.

**Special-case the #1221 zero-invocation chokepoint as an explicit second no-op
signal**, so a filtered-to-empty batch is exempted the same way an invoked-but-no-op one
is. Not implemented: the two cases already behave identically today for a different
structural reason (no worktree to compare `HEAD` against), so adding an explicit branch
would duplicate a distinction the `gitHeadSHA` guard already draws for free. Left as a
documented non-goal rather than an oversight.

## Related

- [ADR-1375](1375-review-authority-reinvoke-not-pause.md) — the exclusion this ADR
  removes, and the reinvoke-not-pause principle this ADR continues unchanged.
- [ADR-1199](1199-slice-budget-separate-from-failure-counter.md) — the "an attempt that
  made no progress should not be charged" principle this ADR applies to a different,
  structurally unrelated counter.
- [ADR-1283](1283-declared-unrequested-reviewers.md) — `expected_reviewers`, the
  discriminator proposed and withdrawn for this ADR's first decision.
- [ADR-1250](1250-review-authority-orthogonal-to-autonomy.md) — `review_authority`
  governs merging, never working; unchanged by this ADR.
- #1207 (closed) — yolo merging past unresolved review threads; the failure shape this
  ADR's first gap reproduced through the `COMMENTED`-exclusion filter instead of a race.
- #1407 — "silently drops an undeclared identity's findings," the failure shape the
  withdrawn `expected_reviewers` discriminator would have relocated into the review
  gate.
- #1251 (closed) — Pruefer's severity-gated, raise-only `REQUEST_CHANGES`; the reason
  its findings land as `COMMENTED` exclusively on this instance today.
