# ADR 1375: `review_authority: authoritative` reinvokes on unresolved feedback — it never pauses first

**Status:** Accepted
**Date:** 2026-08-03
**Issue:** [#1375](https://github.com/handarbeit/fabrik/issues/1375)

**Amends:** [ADR-1250: Review authority — advisory vs. authoritative, an axis orthogonal to autonomy](1250-review-authority-orthogonal-to-autonomy.md)

## Context

ADR-1250 introduced `review_authority: authoritative` to make Fabrik's advance and
landing gates *honor* a reviewer's verdict, not just their having responded. Its
"Graceful non-clear reuses the existing pause path — no new machinery" consequence
(§Decision, penultimate paragraph) stated that a verdict which never resolves "falls
through into whatever 'still blocked' logic already existed for that outstanding
state... ultimately `pauseForReviewTimeout`/`pauseForReviewCycleLimit`." At the time
this was the correct design: the old model treated a persistently-blocking verdict as
something only a human could resolve, so falling through to the pre-existing pause
machinery was the intended outcome, not a gap.

In production this inverted the two modes. `checkReviewGate`'s clearing branch
(`len(outstanding) == 0 && hasReviews`) additionally consults
`reviewGateAuthorityVerdict` only under `authoritative`; when the verdict is
`CHANGES_REQUESTED`, that branch never returns cleared, so `checkReviewGate` keeps
returning `blocked = true` indefinitely. `handleReviewGate`
(`engine/catch_up_handlers.go`) consumed `blocked`/`timedOut` *before* ever computing
whether there was review feedback to act on — the reinvoke path
(`buildReviewThreadComments` → `dispatchReviewReinvoke`) was gated on "the gate cleared
naturally," which under `authoritative` with an unresolved `CHANGES_REQUESTED` can
never happen. The result, observed on bed issue `handarbeit/fabrik-test-alpha#4180`: a
human reviewer submitted `REQUEST_CHANGES` with an explanatory body, and the engine
never re-invoked the stage to address it — it blocked, waited out `ReviewWaitTimeout`,
and paused for a human. `advisory` mode, by contrast, already reinvokes on a
naturally-cleared gate whenever unresolved thread comments exist — so `advisory` was
*more* responsive to review feedback than `authoritative`, exactly backwards from
ADR-1250's intent of authoritative mode enforcing more, not less.

A second, independent defect compounded this even once the ordering above is fixed:
`buildReviewThreadComments` (the function that decides whether there is anything to
reinvoke on) reads only `item.LinkedPRReviewThreadComments` — inline per-line review
comments. It never consults a review's top-level body
(`item.LinkedPRReviews[].Body`), even though that field is already fetched
(`github/project.go`'s `latestReviews` GraphQL selection). Per GitHub's REST contract
for *Create a review for a pull request*, `body` is required for `REQUEST_CHANGES` and
`COMMENT` — a `CHANGES_REQUESTED` review always carries a body; inline comments are
optional and, in practice, frequently absent (this is exactly the shape the e2e
harness's `SubmitPRReview` produces). ADR-1250's own "Alternatives Considered" section
already named this as a known limitation, accepted at the time because pausing was the
correct terminal response either way. Under the reinvoke model, tolerating it is no
longer acceptable: a review with a clear written explanation and no inline comments —
the most common shape, and the only shape GitHub guarantees — would still produce zero
reinvoke dispatch even after the ordering fix.

## Decision

**`review_authority` governs merging. It never governs whether Fabrik works.**
Addressing reviewer feedback is Fabrik's job regardless of mode; `authoritative`
narrows what verdict is *acceptable to land on*, not what triggers a reinvoke. The
intended loop: changes requested → Fabrik reinvokes → pushes a fix → reviewer
re-reviews → verdict flips → merge. Pausing is the terminal fallback when that loop
fails to converge (`MaxReviewCycles`, unchanged from ADR-1250/#1083), never the first
response to a change request.

A conditional hybrid — reinvoke only when the review carries actionable text, pause
immediately otherwise — was considered and rejected, because the "immediately" arm can
never fire for the verdict that matters: GitHub requires a body for
`REQUEST_CHANGES`/`COMMENT`, so there is no genuinely-empty `CHANGES_REQUESTED` case to
route to it. (`APPROVED` with an empty body is the only empty case, and it never
blocks anything.)

**`handleReviewGate` computes actionable feedback before consuming `blocked`/`timedOut`,
not after.** `engine/catch_up_handlers.go`'s `handleReviewGate` now computes
`e.buildReviewFeedbackComments(pctx.item)` immediately after the `terminated` claim
(ADR-1223, unchanged — a direct pause from `handleBrokenReviewLinkage` still wins) and
dispatches the existing `dispatchWithCycleLimit`-bounded reinvoke whenever that set is
non-empty, *regardless of whether `checkReviewGate` reports `blocked` or `timedOut`*.
Only when there is genuinely nothing actionable — a plain "nobody has responded yet"
block, or a review whose feedback was already addressed and dedup'd away — does the
function fall through to the pre-existing cooldown-record (`blocked`) or
`pauseForReviewTimeout` (`timedOut`) tail, unchanged. `checkReviewGate`'s own return
values and side effects (label application, the bot-reprompt ladder, Phase 1/2 timing)
are untouched by this ADR — the fix lives entirely in what `handleReviewGate` *does*
with those values, not in `checkReviewGate` itself. This preserves ADR-1250's
"authority checks live inside the shared clearing predicate" discipline: nothing here
adds a second predicate for what "satisfied" means, it only changes when the *existing*
predicate's blocked state is allowed to produce a pause versus a reinvoke.

**The reinvoke set now includes review bodies, not only thread comments.**
`buildReviewBodyComments` (`engine/reviews.go`) is new, additive alongside the
unmodified `buildReviewThreadComments`: it turns each unaddressed review's top-level
body into a synthetic `gh.Comment`, skipping `APPROVED` and `DISMISSED` reviews (a body
there is not something to act on), reviews with an empty body, and reviews with
`DatabaseID == 0` (no stable ID to key dedup on).
`buildReviewFeedbackComments`/`currentHeadReviewFeedbackComments` are additive
combinators (thread comments + body comments) that `handleReviewGate` and
`dispatchReviewReinvoke` now dispatch on, in place of the thread-only functions.
`buildReviewThreadComments`/`currentHeadReviewThreadComments` are kept standalone
(unmodified) because their existing tests and the #1207 auto-merge-disable guard's
isOutdated-aware narrowing still exercise them directly.

**Idempotency key for a review body: `"review-body:" + PRReview.DatabaseID`.**
GitHub's REST reactions API has no endpoint for a top-level `pulls/.../reviews/{id}`
object — a review body has no ROCKET-reaction dedup backstop the way a real thread
comment does. `snap.CommentProcessed` keyed on this synthetic ID is therefore the
*only* idempotency guard (R7), same mechanism `markCommentsProcessed` already
provides for every comment regardless of origin. The prefix is structurally distinct
from GraphQL node IDs (what real thread comments use verbatim as `ID`), so a synthetic
body ID can never collide with a real comment ID. The synthetic `gh.Comment` carries
`DatabaseID: 0`, so the pre-existing "synthetic comment, skip reaction calls" escape
hatch in `acknowledgeComments`/`finalizeComments` (both already gate on
`DatabaseID == 0`) handles it with no further changes. `isReviewReinvoke`
(`engine/comments.go`) now recognizes a comment as review-reinvoke-eligible when
either `ReviewThreadID != ""` or its `ID` carries the `review-body:` prefix, so a
mixed or body-only reinvoke batch still posts the PR feedback summary comment.

**A review body has no thread and therefore no `isOutdated` signal (R8) — resolved as
"always current-head," an explicit, documented simplification, not an oversight.**
`PRReview.CommitID` (the SHA a review was submitted against) is populated only via the
REST path (`FetchPRReviews`); the GraphQL `latestReviews` selection the default-branch
path uses does not fetch `commit{ oid }`. Comparing a review body's target commit
against the PR's current head SHA would require extending that GraphQL query — a real
schema change (new field, new mapping, potential `boardcache` delta implications) out
of proportion to this fix. Instead, `currentHeadReviewFeedbackComments` includes every
review-body comment unconditionally, alongside `currentHeadReviewThreadComments`'s
existing outdated-thread filtering. This can, at worst, hold auto-merge for a review
body that actually targets an already-superseded commit — over-conservative, not
unsafe, and consistent with this file's established "block on unknown state" pattern
(the same posture `checkReviewGate`/`reviewGateBlocksLanding` already take on any fetch
failure).

## Consequences

**`TestReviewAuthorityBlocksAndPausesOnChangesRequested` no longer encodes correct
behavior and must be reconciled (R3).** Its name and assertion — that an authoritative
`CHANGES_REQUESTED` escalates directly to `fabrik:paused` — described the pre-fix
behavior. Under this ADR, the primary response to that same input is a reinvoke; a
pause is reachable only as the `MaxReviewCycles` terminal fallback. The reworked test
must assert the reinvoke (a stage invocation is observable in the engine log or as a
new commit on the PR branch — not merely a label transition, since other paths also
produce label changes) as the primary path, and separately exercise the cycle-limit
pause as a distinct scenario.

**No change to `checkReviewGate`'s return semantics, and no regression to R4 (never
merge past `CHANGES_REQUESTED`).** The dozen-plus existing unit tests asserting
`checkReviewGate`'s own `blocked`/`timedOut` values for authoritative mode
(`TestCheckReviewGate_Authoritative_*`, `TestCheckReviewGate_NonDefaultBase_Authoritative_*`)
pass unmodified — this ADR changes what `handleReviewGate` does with those values, not
the values themselves. `reviewGateBlocksLanding` (ADR-1216's landing-decision gate) is
a separate function this ADR does not touch at all; it still refuses to clear a
landing while an authoritative verdict is unsatisfied, so a reinvoke loop that never
converges still cannot merge — it can only reinvoke until `MaxReviewCycles`, then pause.

**Finding 2 (a related, independently-reproducible defect, fixed alongside this one):**
a stage-declared `expected_reviewers` bot (ADR-1283) must never defer a human's
authoritative escalation behind its own re-prompt ladder. `reviewGateAllBots`'s result
is now additionally gated on `authorityReason == ""` at the `checkReviewGate` call
site — `authorityReason` is only ever non-empty once a requested human has already
responded with a verdict authoritative mode does not accept, so this cannot change
behavior for the "human hasn't responded yet" case (already `allBots=false` via the
per-reviewer loop), it only prevents a declared bot from preempting an already-resolved
human verdict.

**Runaway-reinvoke risk (the #1083 shape) is the central hazard this ADR's dedup design
targets.** Making Fabrik reinvoke on every poll where an authoritative verdict is
unresolved, without airtight dedup, would reproduce the exact incident #1083/#1088
already fixed once. `buildReviewFeedbackComments` only returns *unprocessed* feedback
— the same review, once addressed and recorded via `CommentProcessed`, produces
nothing further to dispatch on, so the gate's continued "blocked" state falls through
to the ordinary cooldown/timeout tail rather than reinvoking again.

**`buildReviewBodyComments` REST-falls-back on a `base:<branch>` item (found in review
by Pruefer, fixed the same day).** The first draft of this ADR's implementation read
only `item.LinkedPRReviews`, which — like `item.LinkedPRReviewRequests` before
`checkReviewGate`'s own #1046/#1047/#1050 fix — is structurally empty on a
`base:<branch>` item (`closedByPullRequestsReferences` is empty for any PR not
targeting the repository default branch). Left unfixed, this ADR's central promise —
that an authoritative `CHANGES_REQUESTED` reinvokes instead of just blocking — would
have silently never held for `base:<branch>` stages, exactly the configuration
`TestCheckReviewGate_NonDefaultBase_Authoritative_*` already exercises. `buildReviewBodyComments`
now mirrors `checkReviewGate`'s existing REST-fallback branch: resolve the PR number
from `item.LinkedPRNumber`, or — since that's always `0` on a `base:<branch>` item — a
plain `FetchLinkedPR` call (safe without linkage-repair side effects, since
`handleReviewGate` only reaches this call after `checkReviewGate`'s own
`handleBrokenReviewLinkage` already ran this same poll without terminating), then
`FetchPRReviews` directly. `buildReviewThreadComments` (inline thread comments) is
deliberately left as-is — its `base:<branch>` gap is the pre-existing, already-documented
(`docs/state-machine.md`'s Review Gate section) limitation from #1050, out of scope
here — fixing the review-body half alone is sufficient to make this ADR's target
scenario (a `CHANGES_REQUESTED` review with a body and zero inline comments — Finding 4)
reachable on a `base:<branch>` repo too.

**`buildReviewBodyComments` stamps the synthetic comment with the review's real
`SubmittedAt`, not `time.Now()` (found in the same review pass).** The first draft used
`time.Now()` for every synthetic body comment's `CreatedAt`, since `gh.PRReview` didn't
carry a submission timestamp. `engine/claude.go` renders `Comment.CreatedAt` verbatim
into the reinvoke prompt, so a review submitted hours or days earlier would appear to
the agent as having just happened — and in a batch mixing real thread comments (real
timestamps) with a review body, the body would always read as the newest item
regardless of actual chronology. Fixed by fetching the review's actual submission time:
`submittedAt` added to the GraphQL `latestReviews` selection and `submitted_at` to the
REST `FetchPRReviews` parsing, both landing in a new `PRReview.SubmittedAt time.Time`
field; `buildReviewBodyComments` prefers it, falling back to `time.Now()` only when
genuinely unavailable (zero value — unparseable or a data source that predates this
field). Lower-risk than the `base:<branch>` fix above: no schema change to `boardcache`
delta handling was needed, since `SubmittedAt` is not consulted by any existing gating
logic (only by this new display path).

## Alternatives Considered

**Conditional hybrid: reinvoke only when the review carries actionable text, pause
immediately when it does not.** Rejected — collapses to "always reinvoke" for
`CHANGES_REQUESTED`/`COMMENT`, since GitHub requires a body for those verdicts. The
"pause immediately" arm would never take effect for the verdict this ADR exists to
address, so it is not a real alternative, just unnecessary conditional complexity.

**Leave `buildReviewThreadComments` unmodified and special-case review bodies only at
the `handleReviewGate` call site.** Rejected in favor of the additive
`buildReviewBodyComments`/`buildReviewFeedbackComments` layering: `dispatchReviewReinvoke`
needs the same combined set `handleReviewGate` computes (both call sites must dispatch
on identical feedback, or a precheck/dispatch mismatch becomes possible), and the
existing `buildReviewThreadComments`/`currentHeadReviewThreadComments` tests and the
#1207 guard's isOutdated narrowing already depend on the thread-only functions'
existing shape — rewriting them in place would have required touching call sites this
issue does not need to change.

**Extend the GraphQL `latestReviews` selection with `commit{ oid }` to give review
bodies a genuine `isOutdated` signal (R8).** Rejected for this issue's scope: a real
schema change (new field, new mapping in `github/project.go`/`github/types.go`,
potential `boardcache` delta implications) to close a gap whose current resolution
("always current-head," fail-conservative) is safe, not incorrect — it can only ever
hold a landing longer than strictly necessary, never merge past unaddressed feedback.
Documented here as the explicit, revisitable choice rather than left as a silent
oversight, per the issue's own R8 framing.

**Add a new label or config key to express "reinvoke, don't pause."** Rejected —
`review_authority: authoritative`'s intended meaning (per ADR-1250) already was "the
verdict binds"; the pre-fix pause-first behavior was a bug in what "binds" resolved to
at the reinvoke/pause fork, not evidence that a new toggle is needed. No new label,
stage field, or `MaxReviewCycles`-adjacent config is introduced by this ADR.

## References

- [ADR-1250: Review authority — advisory vs. authoritative, an axis orthogonal to autonomy](1250-review-authority-orthogonal-to-autonomy.md) — the ADR this one amends; its "Graceful non-clear reuses the existing pause path" consequence is superseded by this ADR's reinvoke-first decision. Its "Alternatives Considered" section already named the review-body gap this ADR closes.
- [ADR-1283: Declared unrequested reviewers for the review gate](1283-declared-unrequested-reviewers.md) — governs the `reviewGateAllBots` mechanism Finding 2's fix narrows; this ADR's `authorityReason == ""` gate preserves ADR-1283's "declared reviewers never interfere with formally-requested-reviewer classification when outstanding is non-empty" invariant.
- [ADR-1216: Review gate checked at the landing decision](1216-review-gate-at-landing-decision.md) — establishes `reviewGateBlocksLanding` as the separate landing-decision choke point this ADR does not modify; R4 (never merge past `CHANGES_REQUESTED`) continues to hold through that function unchanged.
- [ADR-1223: terminated claims precedence over gate-cleared inference](1223-catch-up-loop-terminated-precedence.md) — the ordering discipline `handleReviewGate`'s `terminated` check must still precede the (now-reordered) reinvoke computation.
- #1083, #1088 — the runaway bot-mention/reply-loop incident this ADR's dedup design (via `CommentProcessed`, R7) exists not to reproduce.
- `docs/state-machine.md` §6.1.1, §6.2 — as-built description updated alongside this ADR to describe the reinvoke-first model.
- `tests/e2e/review_authority_test.go` — `TestReviewAuthorityBlocksAndPausesOnChangesRequested`, reconciled per R3 to assert the reinvoke as the primary path and the cycle-limit pause as the terminal one.
