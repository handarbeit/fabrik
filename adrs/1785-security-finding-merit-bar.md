# ADR 1785: A security finding needs an exposure assessment, not a conformance citation, before it can be dismissed or its thread resolved

**Status:** Accepted
**Date:** 2026-09-18
**Issue:** [#1785](https://github.com/handarbeit/fabrik/issues/1785)

## Context

A worker can dismiss a **security finding** on spec-conformance grounds without ever
assessing the risk. Reported in #1643 (human-owned community thread; the diagnosis
there stands as filed), reproducible publicly at `verveguy/liminis-diagrams#4`.

`handarbeit-pruefer` flagged a missing `persist-credentials: false` and pre-empted the
obvious rebuttal ("the PR's rationale explains the design choice but doesn't mitigate
the exposure"). The worker dismissed it twice and resolved the review thread with no
code change, reasoning that it contradicted FR-020 — a spec requirement whose own
justification was only "the reference's equivalent job doesn't set it either." The
outcome was benign only because the finding happened to be low-severity; nothing in the
reasoning chain established that. The same chain applied to a genuine vulnerability
dismisses it with equal confidence, and resolves the thread so no human ever sees it.

Three distinct, separable failures compose into this outcome:

1. **The rebuttal is circular.** FR-020's sole justification was "the reference does it
   this way" — precisely what the bot challenged as non-responsive. Citing FR-020 back
   restates the premise under dispute; provenance is not mitigation.
2. **Risk was never assessed.** The dismissal contained no exposure analysis — no
   statement of what was reachable, under which trigger, or with which mitigations
   already in place. It happened to reach a defensible conclusion (this instance had
   `default_workflow_permissions: read` and a pnpm version that doesn't run lifecycle
   scripts without an allowlist) by an argument that never touched the question. Had the
   defaults been different, the dismissal would have read identically.
3. **Spec provenance was laundered into human authority.** "an explicit spec decision
   reviewed twice by a maintainer" was inaccurate: FR-020 originated in an automated
   review comment answering a scoping question, never a maintainer security judgement.
   Attributing it to a human gave a machine-generated scoping call the standing of a
   maintainer ruling, which then justified overriding a security bot.

The worker behaved correctly by its instructions at every individual step — the spec is
the source of truth, the issue forbade improving on the reference, and a repeat bot
comment adds no new maintainer direction. The composition of locally-sound steps is
what produces "conforms to spec" standing in for "is safe." Under `fabrik:cruise` this
ran unattended to Validate and self-reported READY TO MERGE with the finding closed; a
human happened to be watching. Under `fabrik:yolo` this merges.

No engine-side signal distinguishes a security finding from any other: `[Bot Review
Finding]` (ADR-1045) marks bot-authored content generically, and nothing in the engine
classifies a finding's *subject matter*. Adding that classification mechanically was
explicitly out of scope for this issue (R4) — a keyword match ("security") is both
under- and over-inclusive, and the issue's own framing requires substance-based
judgment ("not on whether the reviewer used the literal word 'security'"). This is
therefore, and can only be, a prompt-level fix.

## Decision

**Add a security-relevance bar to the four skills that see review findings**:
`fabrik-review`, `fabrik-review-comment`, `fabrik-validate`, and
`fabrik-validate-comment`. The bar has three parts, each addressing one of the three
composing failures above.

**R1 — no dismissal on conformance grounds alone.** A finding concerning security
(credential/secret exposure, injection, authn/authz gaps, permission escalation,
insecure defaults, or similar), judged by substance rather than keyword, cannot be
answered with a conformance argument until the worker first states: what's
reachable/exploitable, under which trigger or condition, and what mitigations already
reduce the exposure. Citing a spec requirement, a reference implementation, or
"already reviewed" provenance alone — without addressing those three — does not
satisfy this bar, however many times the spec point was previously discussed.
Provenance (where a requirement came from) and mitigation (whether the resulting state
is safe) are answers to different questions, and the first must not stand in for the
second. Each of the four skills states the FR-020/`persist-credentials` reasoning chain
by name as the concrete example of insufficient grounds — not just descriptively, but
at the actual enforcement point (see R3), so it can't be edited out of one file without
disturbing language reviewers are likely to diff for consistency.

**R2 — a genuine spec/security tension escalates, it does not get arbitrated.** When
the R1 exposure assessment genuinely conflicts with an explicit spec decision — the
finding is real and the spec forbids the fix — resolving that conflict is a human's
job, not the worker's. The worker states the tension plainly (both the spec requirement
and the security exposure it creates) and leaves the thread unresolved. This is
deliberately non-blocking at the stage level: an unresolved thread already keeps the
existing review-reinvoke loop engaged on it every poll, and if the tension never
converges, the existing `MaxReviewCycles`/`pauseForReviewCycleLimit` terminal fallback
(ADR-1518) already escalates to a human-paused issue. No new label, marker, or pause
path is introduced — the existing non-convergence mechanism already does the job R2
needs.

**R3 — `resolveReviewThread`'s existing exception is conditioned for security-tagged
threads.** All four skills already carry a "review thread resolution is permitted,
comment creation is not" exception under "What You Do NOT Do." Each now adds: resolving
a *security-tagged* thread additionally requires that, in this turn, either a code
change addressing the finding was made, or an explicit risk rationale satisfying R1 was
stated. This closes the literal incident mechanism — the worker resolved Pruefer's
thread by invoking exactly this exception, reasoning from FR-020 each time. The
condition applies identically whether the impetus is the worker's own autonomous
evaluation of a `[Bot Review Finding]`-marked comment, or an explicit human "dismiss"/
"resolved" instruction relayed through a `-comment` skill: an instruction to dismiss is
not itself a risk rationale. If a human's instruction gives only conformance grounds,
the worker asks for a one-line risk rationale rather than resolving on the instruction
alone — the same standard R1 applies to the worker's own reasoning applies to what it
will accept as grounds to act on someone else's.

**Anchored at the point each file actually evaluates findings, not uniformly
duplicated.** Two code paths deliver a finding to a worker today:

- **Main-stage discovery** (`fabrik-review` only): the agent runs `gh pr view
  --comments` itself and reads bot content as plain text, with no `[Bot Review
  Finding]` marker at all — the worker's own judgment is the only thing distinguishing
  a security finding from an aside. The R1/R2 bar and the R3 condition both go directly
  after that step.
- **Comment-reinvoke discovery** (`fabrik-review-comment` and `fabrik-validate-comment`,
  via `dispatchReviewReinvoke` → `processComments`): the mechanically precise place,
  since the engine already marks bot content with `[Bot Review Finding]` and the skill
  already carries the ADR-1045 autonomous-evaluation carve-out this bar sits on top of.
  Both the Dismiss bullet (`fabrik-review-comment`) and the "Issue is resolved" bullet
  (`fabrik-validate-comment`) get the same one-line-rationale requirement.

`fabrik-validate` has no independent finding-evaluation step of its own — confirmed:
all of Validate's finding-handling happens through its comment-reinvoke path. Rather
than inventing prose for a code path the file never exercises, it gets a short pointer
subsection under "What You Validate" that states the bar applies and cross-references
`fabrik-validate-comment`'s full treatment, existing purely so its own
`resolveReviewThread` exception note (duplicated across all four files, per Scope) has
something local to point back at.

**No engine-side change of any kind.** No new label, marker, gating mechanic, or
severity classification. This is prose-only across the four `SKILL.md` files, plus one
new regression test (`TestBuildCommentReviewPrompt_SecurityFindingPreemptsConformanceRebuttal`,
`engine/build_prompt_test.go`) that documents the incident shape reaching the prompt
correctly — an FR-020/`persist-credentials`-shaped bot finding, including its
conformance-rebuttal pre-emption, delivered verbatim with the `[Bot Review Finding]`
marker. It proves plumbing delivery only, the same ceiling ADR-1045's own tests accept:
Go cannot assert that a model complies with prose guidance.

**Does not narrow #1045/ADR-1045's actionability widening.** The No-Op Contract and the
fix/dismiss/defer/clarify flow for *non-security* findings are unchanged verbatim —
every edit here is additive, gated on a finding first being judged security-relevant by
substance. Each of the four new/amended sections closes with an explicit line that the
bar applies only once a finding is so judged, specifically to prevent a superficially
security-adjacent comment (e.g. "missing input validation" on a value that's actually a
hardcoded constant) from triggering a mandatory exposure-assessment ritual and
manufacturing busywork toward `MaxReviewCycles` exhaustion on an otherwise benign PR.

## Consequences

**Quality depends entirely on how reliably a model applies substance-based judgment to
free-form prose — this is accepted, not fixed.** There is no engine-side signal to fall
back on if a worker fails to recognize a finding as security-relevant, and R4 rules out
adding one. The fixture added alongside this ADR regression-tests only that the
incident's literal content reaches the prompt; it cannot prove a worker responds to it
correctly, now or against a future model version. This is the same limitation the issue
itself calls out in its own Risks section.

**A security-tagged thread that never converges now visibly occupies the existing
review-reinvoke loop and eventually the `MaxReviewCycles` pause, exactly like any other
unresolved feedback — this is not a new cost, but it may occur more often.** Before this
change, the worker's incentive was to resolve the thread as fast as possible (dismissing
on conformance grounds satisfied that); after this change, a worker facing a genuine
spec/security tension is expected to leave the thread open and restate the tension each
cycle rather than closing it. An operator should expect more issues reaching the
non-convergence pause specifically when a real spec/security conflict exists — which is
the intended behavior (R2: a human should see and decide these), not a regression.

**Four files must be kept in recognizable alignment by hand.** The R1 three-part
exposure-assessment structure, the R2 escalation language, and the R3 condition on
`resolveReviewThread` are written near-identically (not byte-identical, since
surrounding context differs) across all four skills. Nothing mechanically enforces this
parity — a future edit to one skill could drift from the other three without any test
catching it. A maintainer changing this guidance in one file should check the other
three.

**No change to `review_authority` semantics (ADR-1250/ADR-1375).** That lever still
governs only whether an unresolved `CHANGES_REQUESTED` blocks *merging*; it says nothing
about a worker resolving a thread rather than disagreeing openly, which is the failure
this ADR addresses. The two are orthogonal, as before.

## Alternatives Considered

**Engine-side keyword or label-based severity classification.** Rejected by the issue's
own R4: the engine cannot reliably classify a finding's severity or subject matter from
its text, and a keyword match ("security") is both under-inclusive (a
credential-exposure finding that never uses the word) and over-inclusive (a comment
that mentions "security" in passing). Left entirely to the worker's own substance-based
judgment, consistent with how `[Bot Review Finding]` itself only ever distinguished
*authorship* (bot vs. human), never *subject matter*.

**A new pause/escalation label for R2**, distinct from the existing
`MaxReviewCycles`/`pauseForReviewCycleLimit` path. Rejected: the issue is explicit that
this is non-blocking at the stage level and the existing non-convergence fallback
already covers it — a genuine spec/security tension that never resolves is
indistinguishable, from the engine's perspective, from any other review thread that
never converges, and both should land on the same terminal escalation.

**Adding a full "Security-Tagged Findings" section to `fabrik-validate/SKILL.md`
itself**, duplicating the comment-skill's treatment rather than pointing at it.
Rejected: Validate's main stage has no code path that reaches full finding-evaluation
logic (confirmed in Research) — a fully duplicated section there would document
behavior that file never exercises, while the pointer subsection satisfies the same
readability goal (a human reading this file learns the bar exists and where it's
enforced) without inventing dead prose.

## Related

- [ADR-1045](1045-review-body-comment-actionability-and-noop-budget.md) — the
  actionability widening (`[Bot Review Finding]`, the No-Op Contract, the
  fix/dismiss/defer/clarify flow) this ADR builds on top of and does not narrow.
- [ADR-1250](1250-review-authority-orthogonal-to-autonomy.md) /
  [ADR-1375](1375-review-authority-reinvoke-not-pause.md) — `review_authority` governs
  merging, never working; explicitly orthogonal to this ADR's concern (a worker that
  resolves a thread rather than disagreeing openly).
- [ADR-1518](1518-review-gate-non-convergence-terminal-check.md) — the
  `MaxReviewCycles`/`pauseForReviewCycleLimit` non-convergence fallback R2 relies on
  instead of introducing new engine machinery.
- #1643 (human-owned community thread) — the source report; the diagnosis there stands
  as filed. Reproducible at `verveguy/liminis-diagrams#4`.
- #1045 — the actionability-widening issue behind ADR-1045.
