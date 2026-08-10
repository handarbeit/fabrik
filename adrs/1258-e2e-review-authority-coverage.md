# ADR 1258: e2e coverage for review_authority: authoritative via a per-issue label

**Status:** Accepted
**Date:** 2026-07-30 (the label-based mechanism below was settled same-day, during
Validate-stage review of this issue's own PR — see "Revision" — so this date marks
final acceptance, not the first-draft column/stage-variant design)
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

1. **No production code changes to `engine/reviews.go`** — ADR-1250's shipped gate/predicate
   behavior is out of scope for this issue.
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
same way. This holds regardless of which mechanism applies `review_authority: authoritative`
to an item — no stage/column standing in for `Validate` under a different name can exercise
`reviewGateBlocksLanding`, and flipping the bed's real `Validate` stage to authoritative — even
temporarily — would violate constraint 2 above and risk corrupting concurrently-running
advisory scenarios.

### Revision (2026-07-30): column/stage-variant mechanism rejected in favor of a per-issue label

The first implementation of this issue (PR #1260, initial commits) built the mechanism around
a new bed-local stage variant and board column named `Review-Authoritative` — a copy of
`stages/examples/review.yaml` with `review_authority: authoritative` added, plus a matching
column on `handarbeit/projects/2`, mirroring the `Queued`/`queued.yaml` precedent (ADR-059 D1).
This was rejected during Validate-stage review for two reasons:

1. **Wrong level of abstraction.** `review_authority` is a property of a stage's *config*, not
   a distinct *kind* of stage. `Queued` is a real product stage — it ships in the binary as
   `stages/examples/queued.yaml` with `holding_stage: true` and exists independent of any test
   concern. `Review-Authoritative` would have been pure test scaffolding masquerading as a
   pipeline stage, putting a concept on the board (and in `.fabrik/stages/`) that the product
   itself doesn't have. The `Queued`/`queued.yaml` precedent does not transfer.
2. **Silent-skip trap.** The column is a one-time, external, operator-applied bed prerequisite.
   Because the bed had no such column at review time, the skip guard (`requireAuthoritativeBed`)
   would have caused three of the four scenarios to skip — and the suite would report green
   having validated zero authoritative behavior. These scenarios exist specifically to gate a
   release; a vacuous pass is strictly worse than an absent test, since it reads as coverage
   that isn't there.

The mechanism was reworked to apply authority **per issue via a label**,
`review-authority:authoritative`, set at seed time as an `extraLabel` to `seedReviewGateItem`
(the same parameter already used for `fabrik:yolo`). Engine support for reading this label is
tracked as a separate, decoupled issue — **#1261** — so that engine behavior change and this
issue's test-coverage change can be reviewed and merged independently. The three authoritative
scenarios cannot pass until both #1261 and this issue's PR are merged;
`TestReviewAuthorityAdvisoryRegressionGuard` has no such dependency (it never sets the label)
and ships green regardless.

## Decision

**Apply `review_authority: authoritative` per issue via a label, `review-authority:authoritative`,
against the bed's existing `Review` column/stage (default, advisory config, untouched).** No
bed-local board column or stage-YAML variant is introduced. `seedReviewGateItem`
(`tests/e2e/review_authority_helpers.go`) seeds the item directly via the GitHub API
(`CreateMemberPR`, zero Claude cost) and label-seeds `stage:Review:complete` so the catch-up
loop's `handleReviewGate` evaluates the gate without ever invoking Claude for that stage — the
same zero-cost construction precedent as `TestPausedMergedPRRecovery`'s R5. Because the label is
applied per item rather than encoded in board structure, it isolates cleanly on the shared
parallel bed: an item without the label runs the ordinary advisory path unaffected, regardless
of how many authoritative-labeled items are running concurrently.

**No bed prerequisite is required beyond `FABRIK_REVIEWER_TOKEN`.** Removing the column/stage
mechanism also removes its only skip path — none of the four scenarios skip for lack of bed
setup; they either run for real or fail loudly if #1261 hasn't landed yet.

`reviewGateBlocksLanding`'s authoritative wiring is **not** exercised by this mechanism and has
no e2e coverage after this issue. This is an accepted, explicitly documented gap — not a
silently missing one — because closing it would require either a production code change
(relaxing the `stage.Name == "Validate"` hardcoding) or authoritative-izing the bed's real
`Validate` stage (violating constraint 2). Consequently, scenarios 2 and 3 from the issue are
descoped from "merges under yolo" to "gate clears" — they assert `fabrik:awaiting-review`
disappears and `fabrik:paused` is never applied, not that the item actually lands.
`reviewGateAuthorityVerdict` itself — the pure predicate both gate functions share — is still
fully exercised, since it is identical code regardless of which caller invokes it; only the
landing-gate call site's own wiring around it is unreached.

**Construction is zero-Claude-cost**, reusing two established precedents: `CreateMemberPR`
(the merge-train helpers' direct-API PR builder) and the R5 pattern from
`TestPausedMergedPRRecovery` (label-seeding `stage:<Name>:complete` directly so the catch-up
loop's `handleReviewGate` — gated on `pctx.hasComplete` — evaluates the gate without ever
invoking Claude for that stage). `seedReviewGateItem` wraps this into one call: file issue
(with any extra labels, including `review-authority:authoritative` when needed) → add to
project → `CreateMemberPR` → `AddLabel("stage:<column>:complete")` → `SetIssueStatus`. This
documents these scenarios as validating gate/settle logic, not the natural
`handleStageComplete` stage-completion flow — consistent with the R5 precedent it builds on,
and cheaper than the ~$1–2.50 full-pipeline scenarios like `TestConjunctiveCIReviewGate`.

**No `RequestPRReviewer` call is needed.** `checkReviewGate`'s outer clearing condition
(`len(outstanding) == 0 && hasReviews`) is satisfied by any submitted review — outstanding
reviewers come only from `reviewRequests`, never from who actually submitted. A bare
`SubmitPRReview` is sufficient for every scenario here, simplifying the setup relative to
`TestConjunctiveCIReviewGate`'s pattern. This also means none of the four scenarios touch
`FABRIK_MERGE_TRAIN`-sensitive code, so they run identically and unmodified in both legs of
the suite's two-mode gate.

> **Correction (2026-08-01):** this paragraph reasoned about the outer clearing condition
> correctly but is otherwise wrong in practice — see "Revision (2026-08-01)" below. Three of
> the four scenarios now do call `RequestPRReviewer`.

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

**Advance-gate authoritative coverage is real (pending #1261); landing-gate authoritative
coverage is not.** Anyone extending this area should read this ADR before assuming
`reviewGateBlocksLanding`'s authoritative branch is e2e-covered — it isn't, and closing that
gap requires either relaxing the `Validate`-name hardcoding (a production change, deliberately
not made here) or accepting the cross-scenario risk of authoritative-izing the bed's real
`Validate` stage.

**Three of the four scenarios have a hard dependency on #1261.** Until the engine reads
`review-authority:authoritative` and honors it as equivalent to the stage's `review_authority:
authoritative` config, `TestReviewAuthorityBlocksAndPausesOnChangesRequested`,
`TestReviewAuthorityClearsOnApproval`, and `TestReviewAuthorityYoloDoesNotBypassBlock` will fail
(the gate will behave advisorially regardless of the label). This is expected and intentional —
the two issues are deliberately decoupled so each can be reviewed independently — but it means
this PR alone does not give green authoritative coverage; #1261 must land too.

**Per-issue label, not board structure, is the general pattern for stage-config-scoped e2e
coverage that must not touch the bed's shared default stages.** Unlike a bed-local
column/stage variant, a label requires no external operator setup, cannot silently skip, and
composes cleanly with concurrent scenarios on the shared bed since it's scoped to the item, not
the column. Future stage-config-scoped e2e coverage (e.g. a future risk-tiered review policy
per #1051/#1177) should reach for a label first, and reserve a bed-local stage/column variant
for cases where the behavior is genuinely a distinct *stage* (like `Queued`), not a config flag
on an existing one.

**Four new zero-Claude-cost scenarios** (`TestReviewAuthorityBlocksAndPausesOnChangesRequested`,
`TestReviewAuthorityClearsOnApproval`, `TestReviewAuthorityYoloDoesNotBypassBlock`,
`TestReviewAuthorityAdvisoryRegressionGuard`) run in both legs of the two-mode gate at
negligible added GitHub API cost, since none of them touch merge-train code paths.

## Revision (2026-08-01): `RequestPRReviewer` needed after all, for pre-verdict engagement proof

Issue [#1312](https://github.com/handarbeit/fabrik/issues/1312) found that the "No
`RequestPRReviewer` call is needed" rationale above, while correct about
`checkReviewGate`'s outer clearing condition in isolation, did not account for the bed's own
`claude-review.yml` bot. That bot reviews these trivial, markdown-only member PRs too (not
just full Implement-stage PRs) — the same `COMMENT`-only workflow whose *verdict* this ADR's
Context section already excluded from use, but whose mere existence as *a* submitted review
was overlooked. A `COMMENT` review satisfies `hasReviews` exactly as well as this test's own
deliberate `SubmitPRReview` call does, and it typically lands within ~60-100s of PR creation —
often faster than the engine's first `checkReviewGate` evaluation under suite load (observed
as slow as ~3.5 minutes in incident evidence). Three scenarios
(`TestReviewAuthorityBlocksAndPausesOnChangesRequested`, `TestReviewAuthorityClearsOnApproval`,
`TestReviewAuthorityAdvisoryRegressionGuard`) waited for `fabrik:awaiting-review` as a
*precondition* before submitting their own deliberate verdict, intending to prove "the gate
engaged before we reviewed it, so the later transition is genuinely observed, not a trivial
first-look pass." When the bot's incidental review landed first, the outer clearing branch
cleared the gate on the very first evaluation and never applied the label at all — a scenario
timeout against **correct** engine behavior, not a defect, and not a symptom `checkReviewGate`'s
clearing semantics need to change for (out of scope for both this ADR and #1312's fix).

**Fix:** the three affected scenarios now call `RequestPRReviewer` immediately after
`seedReviewGateItem`, reusing the `FABRIK_REVIEWER_TOKEN` identity they already require for
their own deliberate verdict. This makes `outstanding` genuinely non-empty by construction —
the exact mechanism this ADR's Decision section (above) reasoned was unnecessary — so the
gate's pre-verdict engagement no longer depends on out-running any incidental reviewer,
bot or otherwise. `TestReviewAuthorityYoloDoesNotBypassBlock`'s two
`fabrik:awaiting-review` waits needed no such fix: both occur *after* that test's own genuine
`REQUEST_CHANGES` review is already submitted, and once a genuine `CHANGES_REQUESTED` verdict
exists, an unrelated incidental bot `COMMENT` — landing before or after it — can never satisfy
the outer clearing condition on its own; that wait was already deterministic.

This does not change `checkReviewGate`'s wall-clock/API-cost profile materially (one extra
`gh pr edit --add-reviewer` call per affected scenario) and does not reopen the "silent-skip
trap" this ADR's first Revision rejected — `RequestPRReviewer` fails the test loudly
(`t.Fatalf`) if it errors, it does not skip.

## Alternatives Considered

**Point pruefer (a real Claude-backed reviewer) at the test repos instead of harness-posted
verdicts.** Rejected per the issue's own explicit rejection: non-deterministic (verdict
depends on Claude's severity classification of a synthetic diff), higher latency (pruefer
polls, default 120s, vs. an Action firing on PR-open), added cost (a real Claude invocation
per test PR on every run), `request_changes_threshold` off by default (would emit COMMENT
anyway), and coupling (Fabrik's release gate would depend on pruefer's health).

**A bed-local `Review-Authoritative` stage/column variant** (this issue's original design).
Rejected during Validate-stage review — see "Revision" in Context above: `review_authority` is
a stage-config property, not a distinct stage kind, unlike the `Queued`/`queued.yaml` precedent
it was modeled on; and the column-absent skip path meant three of four scenarios could silently
skip, letting the suite pass green with zero authoritative coverage exercised. A
`Validate-Authoritative` variant of the same design was considered and rejected for the
additional reason that no differently-named stage can reach `reviewGateBlocksLanding` regardless
(see the `stage.Name == "Validate"` hardcoding above), so it would have exercised the same
`checkReviewGate` path as a `Review`-based variant while implying landing-gate coverage it
didn't have.

**A per-issue label override for `review_authority`** (mirroring `model:<name>`/`effort:<level>`/
`base:<branch>`). This is the mechanism ultimately adopted — see Decision above. It does
require a production engine change to read the label (#1261), which is why it's tracked and
merged as a separate, decoupled issue rather than folded into this test-coverage-only PR;
`ReviewAuthority` on `stages.Stage` remains stage-scoped per ADR-1250, and the label is an
independent per-issue override layered on top by #1261, not a change to `ReviewAuthority`'s own
type or the stage config schema.

## References

- [ADR-1250: Review authority — advisory vs. authoritative](1250-review-authority-orthogonal-to-autonomy.md) — the shipped behavior this issue adds e2e coverage for.
- [ADR-1216: Review gate checked at the landing decision](1216-review-gate-at-landing-decision.md) — establishes `reviewGateBlocksLanding`/`attemptMergeOnValidate` as the landing-decision choke point this ADR's gap applies to.
- [ADR-059: Internal merge train](059-internal-merge-train.md) D1 — the `Queued`/`queued.yaml` bed-variant precedent considered and distinguished from this issue's needs (see Revision above).
- #1261 — engine support for the `review-authority:authoritative` per-issue label; a hard dependency for three of this issue's four scenarios.
- `tests/e2e/review_authority_helpers.go`, `tests/e2e/review_authority_test.go` — the implementation.
- `tests/e2e/README.md` §"Additional prerequisites for `TestReviewAuthority*` scenarios" — reviewer-token requirement and label mechanism.
- `tests/e2e/paused_merged_pr_recovery_test.go` (R5) — the zero-Claude-cost label-seeded completion precedent this issue's `seedReviewGateItem` builds on.
- `tests/e2e/mergetrain_helpers.go` (`CreateMemberPR`) — the direct-API PR construction precedent reused as-is.
