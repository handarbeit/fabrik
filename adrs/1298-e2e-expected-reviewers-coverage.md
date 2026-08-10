# ADR 1298: e2e coverage for expected_reviewers via a per-issue label pair

**Status:** Accepted
**Date:** 2026-08-01
**Issue:** [#1298](https://github.com/handarbeit/fabrik/issues/1298)

## Context

#1283 shipped `expected_reviewers` (declared unrequested reviewers for the review
gate) with zero e2e coverage. The e2e test bed's `review.yaml`/`validate.yaml` leave
`expected_reviewers` undeclared (`nil`), which correctly preserves the pre-#1283
default (FR-5) — so the existing suite exercises only the undeclared path.
Additionally, every existing review-gate scenario requests a reviewer explicitly,
making `outstanding` non-empty, which short-circuits `reviewGateFastAdvance` before
the declaration is even consulted. Both the declared-empty and declared-non-empty
paths were unreachable from the suite as configured — the exact gap #1258 closed
for `review_authority: authoritative`, recurring for a second stage-config field.

This is a direct sequel to #1258/ADR-1258: same class of gap (a stage-config-scoped
review-gate feature shipped without e2e coverage), same two constraints (no
production code changes to `engine/reviews.go`/`stages/stages.go`; no change to the
bed's default stage config for existing scenarios), and the same "landing gate is
reachable only through a stage literally named `Validate`" limitation traced in
ADR-1258's Context section — unchanged here, since neither issue touches that
hardcoding.

The one material difference from #1258: `review_authority` is a 2-value enum
(`advisory`/`authoritative`), which is what made it cleanly encodable as a single
boolean-style label. `expected_reviewers` is list-valued — an arbitrary reviewer-name
list — so the same 1:1 label-to-config-value mapping doesn't transfer directly.

## Decision

**Apply a declared `expected_reviewers` value per issue via one of two fixed labels,
against the bed's existing `Review` column/stage (default, untouched config).** No bed-
local board column or stage-YAML variant is introduced.

- `expected-reviewers:none` → `expected_reviewers: []` (the fast-advance path)
- `expected-reviewers:declared` → `expected_reviewers: [e2e-synthetic-declared-reviewer]`
  (the waiting/re-prompt-ladder path)

Two fixed canned values, not a parametric list-valued label. `expected_reviewers`'s
value shape (an arbitrary string list) doesn't collapse to a single boolean-style
label the way `review_authority`'s enum did, but the five scenarios this issue's
requirements specify only ever need two concrete values: empty, and a single
declared name. Inventing a general CSV-in-label-value scheme (e.g.
`expected-reviewers:name1,name2`) to cover list shapes no test here exercises would
be speculative generality with no consumer — the same "keep it minimal" judgment
Research's own open question flagged, mirroring #1261's narrow two-value scope for
`review_authority`.

Both labels are passed as `extraLabels` to the existing `seedReviewGateItem` helper
(`tests/e2e/review_authority_helpers.go`) — no changes needed to that helper, since
it already accepts arbitrary extra labels generically. This reuses #1258's harness
pattern verbatim rather than inventing a second one: `CreateMemberPR` (zero Claude
cost), `stage:Review:complete` label-seeding so the catch-up loop's
`handleReviewGate` evaluates the gate without ever invoking Claude, and
`SubmitPRReview` + `FABRIK_REVIEWER_TOKEN` for deterministic non-author verdicts
where a scenario needs one.

**Engine support for reading these two labels is tracked as a separate, decoupled
follow-up issue**, not filed as part of #1298 and not spawned via
`FABRIK_SPAWN_CHILD` — spawning would make #1298 `blockedBy` the follow-up, stalling
test-writing (which needs no engine change) on an unrelated engine PR, inverting the
whole point of "decoupled." This exactly mirrors how #1261 was kept decoupled from
#1258. `TestExpectedReviewersFastAdvance`,
`TestExpectedReviewersDeclaredWaitsAndReprompts`, `TestExpectedReviewersPrecedenceGuard`,
and `TestExpectedReviewersFastAdvanceComposesWithAuthoritative` cannot pass until that
follow-up merges and both label objects exist on the bed repo — they fail loudly
(`FileIssue` fatal if the label object is missing; gate-timeout failure if the label
exists but the engine doesn't yet read it), never skip silently.
`TestExpectedReviewersUndeclaredRegressionGuard` sets neither label, exercises the
bed's untouched `nil` default, and has no such dependency — it ships green in this
PR alone.

### Why a bed-local stage/column variant was rejected again, more severely than for #1258

The alternative — a bed-local stage YAML declaring `expected_reviewers` directly,
with a matching board column, mirroring the `Queued`/`queued.yaml` precedent — was
considered (it's the "scenario-local stage/column variant" the issue's own Scope
section pre-approved as an option) and rejected, for both of #1258's original
reasons plus a third, specific to this feature:

1. **Wrong level of abstraction** (same as #1258): `expected_reviewers` is a
   property of a stage's config, not a distinct kind of stage. A bed-only
   `ExpectedReviewers`-named stage would be test scaffolding masquerading as a
   pipeline stage — the product itself has no such stage.
2. **Silent-skip trap** (same as #1258): a missing bed prerequisite would let some
   scenarios skip cleanly, reporting green while validating nothing.
3. **Startup blast radius is categorically worse than #1258's rejected design.**
   Tracing `checkStageColumnAlignment` (`engine/startup.go`) shows the exemptions
   that make `CleanupWorktree`/`Unmanaged`/`HoldingStage` stages safe to omit from
   the board do not extend to an ordinary `wait_for_reviews: true` stage:
   `CleanupWorktree` and `Unmanaged` stages are exempt from the column check but
   also never dispatch or evaluate gates at all; `HoldingStage` is exempt from the
   check only when `merge_train != "on"`, but `engine/poll.go`'s catch-up loop
   unconditionally skips `HoldingStage` items regardless of mode, so the exemption
   wouldn't even help reach the gate. A **normal** stage — what
   `expected_reviewers` + `wait_for_reviews: true` actually requires — gets **no
   exemption**: if its YAML exists in `.fabrik/stages/` but its board column is
   missing, Fabrik refuses to start entirely (`fmt.Errorf`, not a per-scenario
   skip). This is a materially larger failure mode than #1258's already-rejected
   `Review-Authoritative` design (whose worst case was three scenarios silently
   skipping, not the whole shared bed refusing to start) — reinforcing rather than
   merely repeating #1258's rejection.

## Consequences

**Advance-gate `expected_reviewers` coverage is real (pending the follow-up engine
issue); landing-gate coverage is not.** Same accepted, documented gap as ADR-1258
left for `review_authority` — `reviewGateBlocksLanding` is reachable only through a
stage literally named `Validate`, and neither this issue nor its follow-up changes
that hardcoding.

**Four of the five scenarios have a hard dependency on the follow-up engine issue.**
Until the engine reads `expected-reviewers:none`/`expected-reviewers:declared` and
honors them as equivalent to the stage's `expected_reviewers` config,
`TestExpectedReviewersFastAdvance`, `TestExpectedReviewersDeclaredWaitsAndReprompts`,
`TestExpectedReviewersPrecedenceGuard`, and
`TestExpectedReviewersFastAdvanceComposesWithAuthoritative` will fail (the gate
behaves as if `expected_reviewers` were undeclared regardless of the label). This is
expected and intentional, mirroring #1258/#1261's shipped state — this PR alone does
not give green `expected_reviewers` coverage; the follow-up issue must land too.

**Per-issue label, not board structure, remains the general pattern for
stage-config-scoped e2e coverage that must not touch the bed's shared default
stages** — ADR-1258's conclusion holds, now confirmed across two different
value-shaped config fields (a 2-value enum and a reviewer-name list). A future
stage-config-scoped feature should default to a label-per-value (or a small fixed
label set for a bounded value space) rather than a bed-local stage/column variant,
which should be reserved for behavior that is genuinely a distinct *stage* (like
`Queued`), not a config flag on an existing one.

**Five new zero-Claude-cost scenarios**
(`TestExpectedReviewersFastAdvance`, `TestExpectedReviewersDeclaredWaitsAndReprompts`,
`TestExpectedReviewersPrecedenceGuard`, `TestExpectedReviewersUndeclaredRegressionGuard`,
`TestExpectedReviewersFastAdvanceComposesWithAuthoritative`) run in both legs of the
two-mode gate at negligible added GitHub API cost, since none of them touch
merge-train code paths. `TestExpectedReviewersDeclaredWaitsAndReprompts` is the
first e2e coverage of the bot re-prompt ladder (`fabrik:bot-reprompted`, Phase 1/2
of `checkAwaitingReviewTimeout`) at all — that path was unconditionally unreachable
from the suite before this issue, since `reviewGateAllBots`'s declared-reviewer
branch (the only way to reach the ladder without a formally requested bot reviewer)
had no scenario declaring `expected_reviewers` to activate it.

## Revision (2026-08-01): draft-PR construction for the scenarios `RequestPRReviewer` can't fix

Issue [#1312](https://github.com/handarbeit/fabrik/issues/1312), fixing the same
incidental-bot-review race for ADR-1258's `TestReviewAuthority*` scenarios (see that ADR's own
"Revision (2026-08-01)"), found it applies here too, to four scenarios:
`TestExpectedReviewersFastAdvance`, `TestExpectedReviewersDeclaredWaitsAndReprompts`,
`TestExpectedReviewersUndeclaredRegressionGuard`, and
`TestExpectedReviewersFastAdvanceComposesWithAuthoritative`. The bed's `claude-review.yml` bot
reviews these member PRs too, typically within ~60-100s, and `expected_reviewers` is not
consulted by `checkReviewGate`'s outer `len(outstanding) == 0 && hasReviews` clearing branch at
all — a declared-but-unrequested or undeclared-nothing-requested configuration offers that
branch no protection whatsoever. An incidental bot `COMMENT` landing before the engine's first
gate evaluation clears the gate in advisory mode (or, for the authoritative-composition
scenario, can apply the label via the authoritative-verdict branch instead — the opposite-
direction failure) before these scenarios' own assertions ever run.

Unlike ADR-1258's fix, **`RequestPRReviewer` is not an option for these four scenarios**: their
properties under test are specifically "nothing was requested" (`TestExpectedReviewersFastAdvance`,
`TestExpectedReviewersFastAdvanceComposesWithAuthoritative`, `TestExpectedReviewersUndeclaredRegressionGuard`)
or "declared but *unrequested*" (`TestExpectedReviewersDeclaredWaitsAndReprompts`). A genuinely
outstanding requested reviewer would falsify the very condition the scenario exists to verify —
worse, for the declared-waits-and-reprompts case specifically, a real requested reviewer routes
to the mixed/human pause path instead of the bot re-prompt ladder (`reviewGateAllBots`), silently
defeating the scenario's actual purpose (the first e2e coverage of that ladder) rather than just
racing an assertion.

**Fix:** these four scenarios now seed via `seedReviewGateItemDraft` (`CreateMemberPRDraft`)
instead of `seedReviewGateItem`, opening the member PR as a draft. Direct inspection of the bed's
actual `claude-review.yml` confirmed its review job is guarded by
`if: github.event.pull_request.draft == false` and triggers only on `opened`/`ready_for_review` —
a draft PR that is never marked ready is therefore permanently invisible to it, removing the
incidental review entirely rather than racing it. `engine/reviews.go` and the rest of the
gate/dispatch path never inspect `IsDraft`, so this is purely a bed-workflow-triggering property
with zero engine-behavior impact. `TestExpectedReviewersPrecedenceGuard` needed no such
treatment — like ADR-1258's fixed scenarios, it already calls `RequestPRReviewer` before its
wait, which is unaffected by this gap.

This technique is bed-workflow-dependent in one sense worth flagging for future readers: if a
future change to `claude-review.yml` removes the `draft == false` guard, these scenarios would
silently become racy again with no compile-time signal. `tests/e2e/README.md` prerequisite #29
points at the guard's exact condition for this reason.

## Alternatives Considered

**A bed-local `ExpectedReviewers`-declaring stage/column variant.** Rejected — see
"Why a bed-local stage/column variant was rejected again, more severely than for
#1258" above. Materially worse blast radius than #1258's already-rejected
`Review-Authoritative` design: a missing column doesn't skip scenarios, it stops the
shared bed from starting.

**A parametric, arbitrary-list-encoding label** (e.g.
`expected-reviewers:name1,name2`, or a label whose suffix is parsed as a
comma-separated list). Rejected as speculative generality: the five scenarios this
issue's requirements specify only need two concrete values (empty, and one
declared name), and a general encoding scheme has no test consumer today. Two fixed
labels, mirroring `effort:<level>`'s discrete-label idiom, cover everything needed
with less surface to validate and document.

**A per-issue label override for `ExpectedReviewers`** (mirroring `model:<name>`/
`effort:<level>`/`base:<branch>`, and directly reusing #1261's `review-authority:<mode>`
pattern). This is the mechanism ultimately adopted for the *test* side — see Decision
above. It does require a production engine change to read the labels, which is why
that change is tracked and merged as a separate, decoupled follow-up issue rather
than folded into this test-coverage-only PR, exactly as #1261 was kept decoupled
from #1258. `ExpectedReviewers` on `stages.Stage` remains stage-scoped per ADR-1283;
the labels are an independent per-issue override layered on top by the follow-up
issue, not a change to `ExpectedReviewers`'s own type or the stage config schema.

## References

- [ADR-1283: Declared unrequested reviewers for the review gate](1283-declared-unrequested-reviewers.md) — the shipped behavior this issue adds e2e coverage for.
- [ADR-1258: e2e coverage for `review_authority: authoritative` via a per-issue label](1258-e2e-review-authority-coverage.md) — the direct precedent this issue follows and extends to a list-shaped config field; its Context/Decision/Consequences structure and its column/stage-variant rejection rationale are reused and reinforced here.
- [ADR-1261: per-issue `review-authority:<mode>` label override](1261-per-issue-review-authority-label-override.md) — the engine-side decoupled-follow-up template the (not-yet-filed) `expected_reviewers` label-override issue should mirror.
- `engine/reviews.go:379` `reviewGateFastAdvance`, `:144`/`:654` call sites, `:151-152`/`:1060-1062` declared-reviewer consumption — the production code this issue's scenarios exercise without modifying.
- `stages/stages.go:106-127` `Stage.ExpectedReviewers`, `:288-325` `validateExpectedReviewers`.
- `engine/startup.go:28-177` `checkStageColumnAlignment` — traced to establish the startup blast-radius argument against a bed-local stage/column design.
- `tests/e2e/expected_reviewers_test.go` — the implementation.
- `tests/e2e/review_authority_helpers.go` (`seedReviewGateItem`) — reused as-is, no changes.
- `tests/e2e/README.md` §"Additional prerequisites for `TestExpectedReviewers*` scenarios".
- `tests/sim/expected_reviewers_test.go` (#1450) — deterministic sim-bed port of this ADR's five live scenarios (`TestExpectedReviewersFastAdvance`, `DeclaredWaitsAndReprompts`, `UndeclaredRegressionGuard`, `FastAdvanceComposesWithAuthoritative`, `TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation`), zero token cost, seconds instead of minutes. `simgh` never synthesizes an incidental bot review, so the live suite's draft-PR-vs-`RequestPRReviewer` determinism split (#1312) has no sim analogue — the sim port gets the same determinism for free by construction. Inherits the same landing-gate scope limitation ADR-1258 documents. Surfaced and fixed a `simgh` fidelity gap in the same port: `SeedReview` did not clear a matching outstanding review request the way real GitHub does on a review landing (see `tests/sim/simgh/FIDELITY.md`). Does not replace the live suite named above — see `tests/sim/README.md`'s "permanently blind to" section — but is the pre-gate regression coverage for this ADR's state-machine behavior going forward.
