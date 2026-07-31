# ADR 1283: Declared unrequested reviewers for the review gate

**Status:** Accepted
**Date:** 2026-07-31
**Issue:** [#1283](https://github.com/handarbeit/fabrik/issues/1283)

## Context

The review gate (`checkReviewGate`, `engine/reviews.go`) distinguishes two reviewer
states: `outstanding` (requested reviewers who haven't submitted, from
`ReviewRequest`) and `hasReviews` (at least one non-`DISMISSED` review exists). Its
bot-aware escalation ladder — a two-phase re-prompt-then-pause sequence, guarded by
`reviewGateAllBots` and the `fabrik:bot-reprompted` label — exists specifically to
handle review bots. But `reviewGateAllBots` computed `allBots := len(outstanding) > 0`:
unconditionally `false` whenever nothing was formally requested.

Every real-world review bot — Pruefer (Fabrik's own first-party bot), Gemini,
CodeRabbit, Copilot — is **unrequested and asynchronous**. It polls or receives a
webhook and posts a review directly; none of them is ever added to a PR's formal
`requested_reviewers` list, because none of them is requestable that way. So for every
bot that actually exists in production, `outstanding` is always empty, `allBots` is
always `false`, and the escalation ladder can never fire. Verified empirically:
`fabrik:bot-reprompted` had been applied to zero issues, ever, across hundreds of
issues run through Review with `wait_for_reviews: true` and an active bot reviewer.
Every existing test that exercised the ladder constructed its bot as a member of
`LinkedPRReviewRequests` — the one shape no real self-submitting bot ever takes.

The user-visible failure mode: "no reviewer requested, no review yet" is
indistinguishable at the gate from "a bot is two minutes from submitting." Both wait
the same `1×`/`2× ReviewWaitTimeout`. On a repo with no bot at all (community report
#1080: solo maintainer, no CODEOWNERS), every single issue burns the full timeout
before pausing, for a gate that could never have cleared. On a repo *with* a bot, the
same stall recurs whenever the bot skips a PR (e.g. an over-cap diff, #1274) — again
indistinguishable from "still processing."

**Why this can't be inferred from GitHub's API — it has to be declared:**

| Route | Why it fails |
|---|---|
| Request the bot as a reviewer | Not possible. These bots poll or receive webhooks and appear only after posting; they cannot be added to `requested_reviewers`. |
| CODEOWNERS | GitHub permits only users, teams, and email addresses with *write permissions*. A GitHub App holds installation permissions, not collaborator access — not a valid entry type. |
| CODEOWNERS via a machine user | Permitted, but a machine user is `type: User`. `IsBot` reads it as human, so `allBots` is still `false` — this converts "no reviewer requested" into "waiting on a human who never reviews," which is worse, not better. |
| Infer from history ("has a reviewer ever responded here?") | Fails open on a cold-started repo — a brand-new install with a bot present reads as bot-less and would advance *with no review posted*. Also stale-positive after an App is uninstalled, and makes the gate's clearing condition depend on a historical API read instead of current PR state. |

The fact "a reviewer will turn up here, unrequested" exists only in the operator's
head. GitHub's data model has no representation for it. It must be declared in config.

## Decision

Add a new stage-YAML key, `expected_reviewers` (`stages.Stage.ExpectedReviewers
*[]string`), that supplies the missing input to the *existing* escalation machinery —
this is explicitly a fix to make dead code reachable, not new parallel machinery.

**Type and three-state semantics**, mirroring the codebase's existing `*bool`
tri-state idiom (`WaitForReviews`, `WaitForCI`) rather than introducing a new
convention:

- `nil` (key absent) — **undeclared**. Behavior is byte-for-byte unchanged from before
  this issue: the gate waits the full `ReviewWaitTimeout` for a self-submitting bot,
  with no re-prompt ladder engaged (this is the historical, if accidental, safe
  default — see Consequences).
- `&[]string{}` (`expected_reviewers: []`) — explicitly declares that **no**
  unrequested reviewer is expected. Enables a fast-advance path: when nothing is
  requested (`outstanding` empty) and nothing has been reviewed (`hasReviews` false),
  the gate clears immediately instead of waiting out the timeout. A reviewer actually
  requested on the PR always still blocks — this narrows waiting for *unrequested*
  reviewers only, and is therefore a strictly narrower relaxation than
  `wait_for_reviews: false` (which would ignore a requested reviewer outright).
- Non-empty (`expected_reviewers: [name, ...]`) — one or more declared identities. The
  gate is satisfied when **any one** responds — not all, which would turn N declared
  bots into N independent points of failure (the same class of fragility already
  tracked for a different reason in #1071). This matches the existing `hasReviews`
  clearing predicate exactly (any non-`DISMISSED` review from any author already
  satisfies it) — the any-of-N semantics were, in effect, free.

**Load-time identity validation (`validateExpectedReviewers`, `stages/stages.go`).**
A declared identity must be a bare, `@`-mentionable handle: non-empty, no leading `@`,
no trailing `[bot]`/`[Bot]`/`[BOT]` suffix (case-insensitive). This is not a
hypothetical edge case: Fabrik's own first-party bot's actual review author carries
exactly that suffix — confirmed for this repo's live Pruefer installation via both
GraphQL (`Bot.login` omits the suffix) and REST (`user.login` includes it,
`handarbeit-pruefer[bot]`). A malformed declaration fails `LoadAll` — and therefore
engine startup — entirely, rather than silently producing a re-prompt comment that
never resolves as a mention.

**Identity matching (`reviewerIdentityMatches`, `stripBotSuffix`,
`declaredReviewersOutstanding`, `engine/reviews.go`).** Because FR-8 forbids the
`[bot]` suffix on the *declared* side but GitHub's REST API includes it on the *live*
side, matching cannot be a straight string comparison. The match strips a trailing
`[bot]` from the live author, applies the existing `botMentionHandle` copilot-collapse
to both sides, then compares case-insensitively — tolerating both the REST-suffixed
and GraphQL-unsuffixed forms the same login can arrive in depending on which API path
supplied it.

**`reviewGateAllBots` extended, not replaced.** New signature:
`reviewGateAllBots(reviewRequests, outstanding, declaredOutstanding []string) bool`.
When `outstanding` is non-empty, behavior is byte-for-byte unchanged — a real,
formally-requested reviewer's bot/human classification never depends on what's
declared. Only when `outstanding` is empty does the function now return
`len(declaredOutstanding) > 0` instead of an unconditional `false`. This is the
narrowest change that makes the ladder reachable without touching the out-of-scope
clearing predicate (`len(outstanding) == 0 && hasReviews`).

**Phase 1's re-prompt gains a direct-mention path for declared-but-unrequested
reviewers.** There is no GitHub review request to `DELETE`+`POST` for a reviewer that
was never formally requested, so that mutation is skipped entirely for this path — a
`@<name> just checking in` comment is posted directly, using the declared identity
verbatim (already validated to be mention-resolvable). Both the requested-reviewer
loop and the declared-reviewer loop share the single `fabrik:bot-reprompted`
idempotency guard, applied once per gate cycle regardless of which group(s)
triggered it — this label's semantics are explicitly *not* relaxed or duplicated,
because doing so would reopen the runaway-mention class already tracked in
#1083/#1088. This direct-mention path does not conflict with `neutralizeBotMentions`
(#1141, `engine/mentions.go`): that scoping is deliberately specific to Claude's
freeform stage output, not to this engine-authored re-prompt, which already posted a
live mention for requested bots before this issue.

**Timeout messaging names the declared reviewer(s) (`pauseForReviewTimeout`).** A
per-declared-reviewer status line (`` `pruefer` reviewed; `gemini` did not respond ``)
is appended to the pause message whenever the stage declares `expected_reviewers`,
independent of which timeout branch fired — replacing the prior, undifferentiated "no
reviewers were requested" framing, which was true but told the operator nothing
actionable.

**Startup notice for under-specified stages (`stages.WarnUndeclaredReviewers`,
`stages/drift.go`).** For every `wait_for_reviews: true` stage with no declaration,
Fabrik prints a one-line, self-limiting notice at startup — the same
`warnings.Record`/`warnings.Clear` idiom `WarnStageDrift` already uses, keyed
`"undeclared_reviewers:" + stage.Name` so it disappears the moment a declaration
(including an explicit `expected_reviewers: []`) is added. This is deliberately
**not** folded into stage-drift detection (`FilterNoOpKeys`): drift answers "is this
config outdated?", and the honest answer here is no — omission *is* the documented,
behavior-preserving default. This notice answers a different question, "is this
config under-specified?", which only the operator can resolve. Without this notice
the fix ships and is silently never adopted — including on this repo's own
`review.yaml`/`validate.yaml`, which (along with three other known Fabrik-family
instances) carried `wait_for_reviews: true` with no declaration before this issue.

## Consequences

**The historical default was accidentally, not deliberately, safe.** Before this
issue, an undeclared stage waited the full timeout and then paused for a human on
every self-submitting-bot PR that happened to be slow, and *also* on every PR where no
bot exists at all — the two were indistinguishable, and the safe outcome (pause,
don't advance without review) was a side effect of the ladder being unreachable, not a
designed behavior. This ADR does not change that default (FR-5): an undeclared stage
still waits the full timeout with no ladder. What changes is that an operator can now
*tell* Fabrik which of the two situations applies, and get the correct behavior for
each — fast-advance when nothing is coming, or an actual re-prompt-then-pause ladder
when something is.

**Declaring a reviewer is not evidence it exists (FR-6).** A declared-but-uninstalled
bot runs the identical re-prompt-then-pause sequence as a real one that's merely slow,
and still ultimately pauses, naming the bot that never appeared. The declaration is
purely an input to the *waiting* logic — it has no bearing on the gate's clearing
condition, which remains exactly `len(outstanding) == 0 && hasReviews`.

**A malformed declaration is a startup-time failure, not a silent no-op.** Because
FR-8 validation runs inside `loadOne` (`stages.LoadAll`), a typo in a declared name —
including the single most tempting mistake, appending `[bot]` — fails engine startup
with an explicit error rather than producing a re-prompt comment that mentions nobody.

**Rollout is entirely operator-driven, and slow by design.** All four known
Fabrik-family repos (`fabrik`, `fantasy`, `liminis-project`, `concept-mapping`) had
zero declarations across eight `wait_for_reviews: true` stage files at the time this
issue was filed. This ADR's fix has zero effect on any of them until an operator adds
a declaration — the startup notice is the sole adoption mechanism, so its own
correctness (fires when it should, clears when it shouldn't) matters as much as the
gate logic itself.

**Forward constraint: three call sites must never disagree.** `checkReviewGate`
(advance gate), `reviewGateBlocksLanding` (landing gate, ADR-1216), and
`pauseForReviewTimeout` (message builder) all consult the same pure functions
(`reviewGateFastAdvance`, `declaredReviewersOutstanding`, `reviewerIdentityMatches`)
rather than independent reimplementations — the same discipline ADR-1250 already
established for `review_authority`. Any future change to declared-reviewer matching or
the fast-advance condition must be made in these shared functions, not at a call site,
or the three sites will drift apart exactly as ADR-1216 exists to prevent for the
underlying clearing predicate.

## Alternatives Considered

**Infer expected reviewers from PR history or installed GitHub Apps.** Rejected — see
the Context table above. Every inference route either fails open on a cold-started
repo, goes stale after an App is uninstalled, or makes the gate depend on a
non-deterministic historical read instead of current PR state. The information does
not exist in GitHub's model; it must come from the operator.

**A single expected-reviewer field instead of a list.** Rejected. Repos routinely run
more than one review bot (this repo alone could reasonably declare Pruefer and, in the
future, others). A single-name design would recreate the SPOF already tracked
separately in #1071 — one declared bot going quiet would be indistinguishable from
"no bot is coming," reintroducing exactly the failure mode this issue fixes.

**Require all declared reviewers to respond, not just one.** Rejected. This would
match neither the existing `hasReviews` clearing semantics (any one non-DISMISSED
review already satisfies it, with no per-author accounting) nor the goal of the
feature — turning N declared reviewers into N independent points of failure is
strictly worse than accepting any one.

**Pause immediately, instead of fast-advancing, when `expected_reviewers: []` is
declared.** Rejected during Specify. Pausing still costs a human round-trip per issue
— the actual complaint in #1080 (seven issues paused overnight, each needing manual
label surgery). Fast-advancing, with the decision recorded in the log trail rather
than silent, answers the fail-open concern through explicit declaration and an
auditable trail instead of through an extra human click that doesn't change the
outcome.

**Fold the undeclared-stage notice into existing drift detection
(`WarnStageDrift`/`FilterNoOpKeys`).** Rejected. Drift's value-aware filtering is
specifically designed to stay silent when a key's default is behaviorally identical
to omission — which is exactly the case here by FR-5's own requirement. Reusing drift
for this would either require weakening `FilterNoOpKeys`'s correctness guarantee for
every other key, or the notice would simply never fire. A separate, purpose-built
notice (`WarnUndeclaredReviewers`) answers a genuinely different question and reuses
only the underlying `warnings.Record`/`warnings.Clear` idiom, not the drift-comparison
logic itself.

## References

- Community report [#1080](https://github.com/handarbeit/fabrik/issues/1080) — the
  visible symptom (seven issues paused overnight on a solo-maintainer, no-bot repo).
- [#1274](https://github.com/handarbeit/fabrik/issues/1274) — the same stall,
  reachable on a repo that *does* have a bot, when Pruefer skips a PR.
- [#1071](https://github.com/handarbeit/fabrik/issues/1071) — single-reviewer-bot SPOF
  fragility; the reason `expected_reviewers` is a list, not a single name.
- [#1083](https://github.com/handarbeit/fabrik/issues/1083),
  [#1088](https://github.com/handarbeit/fabrik/issues/1088) — the runaway-mention
  class the unrelaxed `fabrik:bot-reprompted` idempotency guard exists to prevent.
- [#1141](https://github.com/handarbeit/fabrik/issues/1141) — the outbound-mention
  incident behind `neutralizeBotMentions`; confirmed not to conflict with Phase 1's
  engine-authored re-prompt (`adrs/073-outbound-bot-mention-neutralization.md`).
- [ADR-1216: Review gate checked at the landing decision](1216-review-gate-at-landing-decision.md)
  — establishes the three-site-agreement discipline this ADR extends to declared
  reviewers.
- [ADR-1250: `review_authority` orthogonal to autonomy](1250-review-authority-orthogonal-to-autonomy.md)
  — the shared-pure-function precedent (`effectiveReviewAuthority`) this ADR's
  `reviewGateFastAdvance`/`declaredReviewersOutstanding` follow.
- `docs/state-machine.md` §6.1 — as-built description of the extended escalation
  ladder, the FR-2 fast path, and the FR-7 startup notice.
- `docs/USER_GUIDE.md` §Declaring Expected Reviewers — operator-facing documentation.
