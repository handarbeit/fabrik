# ADR 1258: e2e coverage for review_authority: authoritative via a bed-local advance-gate variant

**Status:** Accepted
**Date:** 2026-07-30
**Issue:** [#1258](https://github.com/handarbeit/fabrik/issues/1258)

## Context

ADR-1250 shipped `review_authority: advisory | authoritative`, honoring a review verdict at
both `checkReviewGate` (the advance gate) and `reviewGateBlocksLanding` (the landing gate,
ADR-1216) via the shared pure predicate `reviewGateAuthorityVerdict`. It shipped with zero
e2e coverage. Worse, the gap is structural with the current test bed: `fabrik-test-alpha`/
`-beta` are reviewed by `.github/workflows/claude-review.yml`, which submits
`gh pr review --comment` in both its agent path and its fallback path — COMMENT only. It can
never produce `APPROVE` or `CHANGES_REQUESTED`, so authoritative mode's blocking and clearing
paths are both untestable with the bed's incidental reviewer. The bed's `review.yaml`/
`validate.yaml` carry `wait_for_reviews: true` with no `review_authority`, i.e. advisory —
today's suite exercises only the default path.

Two constraints shaped the design:

1. **No production code changes** — this is test-coverage-only work; `engine/reviews.go` and
   the rest of ADR-1250's shipped behavior are out of scope.
2. **No change to the bed's default stage config** — `Review`/`Validate` must stay advisory
   for every existing scenario, and the shared bed runs many `t.Parallel()` scenarios
   concurrently (`E2E_PARALLEL`), so any authoritative-mode mechanism must not leak into or
   destabilize advisory scenarios in flight.

Tracing the two gate call sites down to their callers surfaced a constraint neither the issue
nor Research had fully resolved: **the landing/auto-merge/Done-advance path is reachable only
through a stage literally named `Validate`.** `reviewGateBlocksLanding` is called exclusively
from `attemptMergeOnValidate`, and both of that function's callers hard-gate on the literal
string `"Validate"` before invoking it — `engine/stages.go` (`if yoloActive && stage.Name ==
"Validate" && !waitForCI`) and `engine/poll.go` (`if stage.Name == "Validate"`). A third site,
`engine/pr_terminal_advance.go` (`if stage.Name != "Validate"`), gates Done-advancement the
same way. No differently-named variant column can exercise `reviewGateBlocksLanding`, and
flipping the bed's real `Validate` stage to authoritative — even temporarily — would violate
constraint 2 above and risk corrupting concurrently-running advisory scenarios.

## Decision

**Build the e2e mechanism around `checkReviewGate` only**, via a new bed-local stage variant
and board column named `Review-Authoritative`: a copy of `stages/examples/review.yaml` with
`review_authority: authoritative` added and `order: 41` (deliberately far outside the real
pipeline's 1–9 range, so it is never any real stage's natural successor), plus a matching
`Review-Authoritative` column on `handarbeit/projects/2`. This is one-time, bed-local operator
setup — the same pattern already accepted for the merge-train scenarios' `Queued`/
`queued.yaml` (ADR-059 D1). Scenarios needing it skip gracefully via `requireAuthoritativeBed`
(mirroring `requireTrainBed`) if the column is absent, so this PR is mergeable independent of
when the bed is actually updated. Because dispatch is column-name-keyed, a differently-named
column structurally isolates authoritative scenarios from advisory ones running concurrently
on the shared bed — no extra guarding needed to satisfy constraint 2.

`reviewGateBlocksLanding`'s authoritative wiring is **not** exercised by this mechanism and
has no e2e coverage after this issue. This is an accepted, explicitly documented gap — not a
silently missing one — because closing it would require either a production code change
(relaxing the `stage.Name == "Validate"` hardcoding, out of scope per constraint 1) or flipping
the bed's real `Validate` stage authoritative (out of scope per constraint 2). Consequently,
scenarios 2 and 3 from the issue are descoped from "merges under yolo" to "gate clears" — they
assert `fabrik:awaiting-review` disappears and `fabrik:paused` is never applied, not that the
item actually lands. `reviewGateAuthorityVerdict` itself — the pure predicate both gate
functions share — is still fully exercised, since it is identical code regardless of which
caller invokes it; only the landing-gate call site's own wiring around it is unreached.

**Construction is zero-Claude-cost**, reusing two established precedents: `CreateMemberPR`
(the merge-train helpers' direct-API PR builder) and the R5 pattern from
`TestPausedMergedPRRecovery` (label-seeding `stage:<Name>:complete` directly so the catch-up
loop's `handleReviewGate` — gated on `pctx.hasComplete` — evaluates the gate without ever
invoking Claude for that stage). `seedReviewGateItem` (`tests/e2e/review_authority_helpers.go`)
wraps this into one call: file issue → add to project → `CreateMemberPR` → `AddLabel
("stage:<column>:complete")` → `SetIssueStatus`. This documents these scenarios as validating
gate/settle logic, not the natural `handleStageComplete` stage-completion flow — consistent
with the R5 precedent it builds on, and cheaper than the ~$1–2.50 full-pipeline scenarios like
`TestConjunctiveCIReviewGate`.

**No `RequestPRReviewer` call is needed.** `checkReviewGate`'s outer clearing condition
(`len(outstanding) == 0 && hasReviews`) is satisfied by any submitted review — outstanding
reviewers come only from `reviewRequests`, never from who actually submitted. A bare
`SubmitPRReview` is sufficient for every scenario here, simplifying the setup relative to
`TestConjunctiveCIReviewGate`'s pattern. This also means none of the four scenarios touch
`FABRIK_MERGE_TRAIN`-sensitive code, so they run identically and unmodified in both legs of
the suite's two-mode gate.

**Scenario 5 (issue #1258: "verdict that never clears → pause, no infinite spin") folds into
scenario 1's test** rather than exercising `MaxReviewCycles`/`pauseForReviewCycleLimit`.
`dispatchReviewReinvoke`'s precheck (`len(buildReviewThreadComments(item)) > 0`) requires
unresolved *inline* review-thread comments; a bare `REQUEST_CHANGES` with no inline comments
produces zero reinvoke dispatches. A persistent `CHANGES_REQUESTED` verdict resolves
exclusively via `checkAwaitingReviewTimeout` → `pauseForReviewTimeout` — the same
`ReviewWaitTimeout` path scenario 1 already exercises, not the distinct `MaxReviewCycles`
path the issue names. Building new inline-comment infrastructure to reach a cycle-limit path
that authoritative mode structurally cannot reach was rejected as disproportionate; "pause for
human, no infinite spin" is satisfied by the `ReviewWaitTimeout` pause, and
`TestReviewAuthorityBlocksAndPausesOnChangesRequested` asserts both the immediate block and
the eventual pause (with the `authorityReason`-driven message, not the generic "no reviews
submitted yet") as one continuation.

**Scenario 6 (verdict-fetch failure / unrecognized `reviewDecision`) is excluded.**
`FetchPRReviewDecision` only returns a non-empty value when the repo has a branch-protection
review requirement configured; neither bed repo has one (only required status checks are
documented as enrolled). Producing `REVIEW_REQUIRED` or an unrecognized value would require
new branch-protection bed setup, which fails the issue's own "cheaply expressible" bar for
this optional scenario. All four implemented scenarios therefore exercise
`reviewGateAuthorityVerdict`'s Fabrik-computed fallback branch (`reviewDecision == ""`), not
GitHub's native `reviewDecision` branch — documented explicitly in `tests/e2e/README.md` so a
future reader doesn't assume the native-verdict branch has coverage it doesn't.

## Consequences

**Advance-gate authoritative coverage is real; landing-gate authoritative coverage is not.**
Anyone extending this area should read this ADR before assuming `reviewGateBlocksLanding`'s
authoritative branch is e2e-covered — it isn't, and closing that gap requires either relaxing
the `Validate`-name hardcoding (a production change, deliberately not made here) or accepting
the cross-scenario risk of authoritative-izing the bed's real `Validate` stage.

**The `Review-Authoritative` bed variant is a second precedent (alongside `Queued`/
`queued.yaml`) for "isolate a gate/settle behavior behind a differently-named, bed-local stage
+ column" as the general e2e strategy for behavior that's stage-scoped in production config but
must not touch the bed's shared default stages.** Future stage-scoped e2e coverage (e.g. a
future risk-tiered review policy per #1051/#1177) should reach for this pattern before
considering a change to the bed's real pipeline stages.

**Four new zero-Claude-cost scenarios** (`TestReviewAuthorityBlocksAndPausesOnChangesRequested`,
`TestReviewAuthorityClearsOnApproval`, `TestReviewAuthorityYoloDoesNotBypassBlock`,
`TestReviewAuthorityAdvisoryRegressionGuard`) run in both legs of the two-mode gate at
negligible added GitHub API cost, since none of them touch merge-train code paths.

## Alternatives Considered

**Point pruefer (a real Claude-backed reviewer) at the test repos instead of harness-posted
verdicts.** Rejected per the issue's own explicit rejection: non-deterministic (verdict
depends on Claude's severity classification of a synthetic diff), higher latency (pruefer
polls, default 120s, vs. an Action firing on PR-open), added cost (a real Claude invocation
per test PR on every run), `request_changes_threshold` off by default (would emit COMMENT
anyway), and coupling (Fabrik's release gate would depend on pruefer's health).

**A `Validate-Authoritative` bed variant instead of `Review-Authoritative`.** Considered first,
since the issue's scenario 2/3 language ("merges under yolo") more naturally maps to Validate.
Rejected once the `stage.Name == "Validate"` hardcoding was traced: a variant named anything
other than `Validate` cannot reach `reviewGateBlocksLanding` regardless of which stage number
in the pipeline it stands in for, so a `Validate-Authoritative` variant would exercise exactly
the same `checkReviewGate` code path as `Review-Authoritative` does, with a more misleading
name (implying landing-gate coverage it doesn't have).

**A per-issue label override for `review_authority`** (mirroring `model:<name>`/`effort:<level>`/
`base:<branch>`). Rejected: explicitly out of scope per the issue ("no change to
`engine/reviews.go` or other production engine code"); `ReviewAuthority` is deliberately
stage-scoped only (ADR-1250), and adding a label override would be new production behavior,
not test infrastructure.

## References

- [ADR-1250: Review authority — advisory vs. authoritative](1250-review-authority-orthogonal-to-autonomy.md) — the shipped behavior this issue adds e2e coverage for.
- [ADR-1216: Review gate checked at the landing decision](1216-review-gate-at-landing-decision.md) — establishes `reviewGateBlocksLanding`/`attemptMergeOnValidate` as the landing-decision choke point this ADR's gap applies to.
- [ADR-059: Internal merge train](059-internal-merge-train.md) D1 — the `Queued`/`queued.yaml` bed-variant precedent this issue's `Review-Authoritative` mechanism follows.
- `tests/e2e/review_authority_helpers.go`, `tests/e2e/review_authority_test.go` — the implementation.
- `tests/e2e/README.md` §"Additional prerequisites for `TestReviewAuthority*` scenarios" — bed setup instructions.
- `tests/e2e/paused_merged_pr_recovery_test.go` (R5) — the zero-Claude-cost label-seeded completion precedent this issue's `seedReviewGateItem` builds on.
- `tests/e2e/mergetrain_helpers.go` (`CreateMemberPR`) — the direct-API PR construction precedent reused as-is.
