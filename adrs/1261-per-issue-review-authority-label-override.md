# ADR 1261: Per-issue `review-authority:<mode>` label override

**Status:** Accepted
**Date:** 2026-07-30
**Issue:** [#1261](https://github.com/handarbeit/fabrik/issues/1261)

## Context

ADR-1250 made `review_authority` (`advisory` | `authoritative`) a per-stage YAML field, consumed directly by `checkReviewGate` and `reviewGateBlocksLanding` at `stage.ReviewAuthority`. That is repo-wide, stage-scoped configuration: opting a single issue into authoritative review means editing the stage YAML, which applies to every issue that reaches that stage — there is no way to make one high-risk change binding without also making every other change on that stage binding, or vice versa.

#1258's first attempt at e2e-covering authoritative mode worked around this by inventing a parallel board column, `Review-Authoritative`, whose only purpose was to carry a different `review_authority` value than the stage's normal column. That approach was rejected during #1258's Validate discussion: `review_authority` is a property of a stage's *review policy*, not a *kind* of stage — encoding it as a column name puts a non-product concept on the board (an operator sees a column and has to know it means "same as the other column, but authoritative") and makes per-item opt-in structurally impossible (an item is in exactly one column at a time; it cannot be "authoritative for this Validate pass, advisory for others" via column alone).

Fabrik already has an established idiom for exactly this shape of problem — a per-issue override on top of stage-scoped config: `model:<name>`, `effort:<level>`, and `base:<branch>` all let a single issue diverge from repo/stage defaults via a label, with no board or YAML changes required. ADR-1250 itself anticipated this need: "a reviewer implementing #1051's risk-tiered policy on top of this mechanism should set `review_authority` per stage/repo and rely on the gate" — a risk tier expressed as a per-issue label is a more natural shape for that policy to consume than a column move, and this issue ships the mechanism (not #1051/#1177's policy) so that a future risk-tiering feature has something to attach a label to.

## Decision

**Add a per-issue label override, `review-authority:<mode>` (`advisory` | `authoritative`), that overrides the stage's configured `review_authority` for that issue only.** No board column, no stage YAML change, no new interfaces or `ProjectItem` schema fields — the label is read directly from `item.Labels` at gate-check time, the same low-level idiom every other label extractor in the codebase uses.

**Precedence mirrors `extractEffortOverride`, not `extractModelOverride`.** The codebase has two established conflict-resolution conventions for multi-value override labels:

- `extractModelOverride`: first label found wins, later ones logged and ignored.
- `extractEffortOverride`: all labels found are ranked, the highest-ranked (most restrictive-by-convention) wins, with one warning listing everything found.

This feature follows the latter. `review-authority:advisory` and `review-authority:authoritative` present together resolve to `authoritative` — the more restrictive value — with a logged warning, rather than "first wins." Silently picking the less restrictive value on ambiguous input would be the wrong default for a review-strictness control: an operator who accidentally leaves a stale `review-authority:advisory` label after intending to add `review-authority:authoritative` should get the stricter behavior, not the weaker one, while still being told about the conflict via the warning.

A label whose suffix is not exactly `advisory` or `authoritative` — a typo, wrong casing, or unknown value — is ignored with a logged warning and falls back to the stage config. This is deliberately never a hard failure and never a silent escalation to authoritative: a malformed label is far more likely to be a typo of `advisory` than of `authoritative`, and treating unrecognized input as "tighten the gate" would be a surprising, hard-to-discover behavior change for an operator who made a spelling mistake.

**One shared resolution point, consulted identically everywhere `stage.ReviewAuthority` used to be read directly.** `effectiveReviewAuthority(item gh.ProjectItem, stage *stages.Stage) string` (`engine/reviews.go`) computes `extractReviewAuthorityOverride(item.Number, item.Labels)`, falling back to `stage.ReviewAuthority` when no override is present. Three call sites — previously reading `stage.ReviewAuthority` directly — now call this instead:

1. `checkReviewGate` (the advance gate, ADR-1250's Path 2).
2. `reviewGateBlocksLanding` (the landing gate, ADR-1216/ADR-1250's Path 3).
3. `pauseForReviewTimeout`'s message-only check, which decides whether to append an authoritative-mode explanation to the timeout pause comment.

The first two are gate-*clearing* decisions and must never diverge — that is exactly the failure mode ADR-1250 already guards against for stage-YAML-authoritative mode (`reviewGateAuthorityVerdict` as one shared pure verdict function), and this issue extends the same guarantee one layer up: the *input* to that verdict function (which mode is active) must also be resolved identically, or the two gates could each read a different value of `stage.ReviewAuthority` vs. a label override and disagree about whether an issue is authoritative at all. The third call site is not a gate-clearing decision, but leaving it on the raw stage field would produce a user-visible inconsistency — a label-authoritative issue timing out would show a pause message describing advisory-mode wording, or vice versa — so it is routed through the same helper for messaging correctness, not because it needs the precedence logic for its own sake.

**The override does not change *whether* the gate applies.** `wait_for_reviews: true` on the stage is still required for either gate to engage at all — `review-authority:<mode>` has no effect on a stage that never opted into `wait_for_reviews`. The label only changes the verdict-strictness once the gate is already active, exactly as `stage.ReviewAuthority` itself does.

**`yolo`/`cruise` still never bypass whatever authority resolves to.** Because `effectiveReviewAuthority` is consulted from inside the same shared clearing branch ADR-1250 established (`checkReviewGate`'s and `reviewGateBlocksLanding`'s `len(outstanding) == 0 && hasReviews` condition), and `hasCruiseLabel` is still checked at `attemptMergeOnValidate` *before* `reviewGateBlocksLanding` is ever consulted, this issue changes nothing about ADR-1250's composition guarantee — it only changes what value `effectiveReviewAuthority` can return, never where or how autonomy controls interact with the gate. A `review-authority:authoritative` label on a `yolo` item behaves exactly like a stage-YAML-authoritative stage on a `yolo` item: `yolo` still merges immediately once the gate clears, but cannot force a still-blocking verdict through.

**Location: `engine/reviews.go`, not `engine/item.go`.** `extractModelOverride`/`extractEffortOverride` live in `item.go` because they feed `InvokeOptions` at Claude-invocation call sites (`item.go:954,958`, `comments.go:275,279`) — they answer "what model/effort should this Claude invocation use?" `extractReviewAuthorityOverride`/`effectiveReviewAuthority` have no equivalent invocation-time consumer; they are read only at gate-check time, alongside `reviewGateOutstanding` and `reviewGateAuthorityVerdict`, the other pure resolution helpers both gates already share. Colocating with those avoids splitting one feature's logic across two files for no benefit — `item.go` is mirrored here for the extractor *idiom*, not for *file placement*.

**Pre-seeded labels.** `review-authority:advisory`/`review-authority:authoritative` are added to `github/labels.go`'s `staticLabelDefs`, in the same purple (`6f42c1`) "override labels" color family as `model:*`/`effort:*`. The existing precedent — enumerable, fixed value sets get pre-seeded (`model:*`, `effort:*`); parametric ones don't (`base:<branch>`, which has unbounded branch names) — applies cleanly here: the value set is exactly `{advisory, authoritative}`, so it is pre-seeded rather than left to be created ad hoc.

## Consequences

**No behavior change for issues without the label.** `effectiveReviewAuthority` returns `stage.ReviewAuthority` unchanged when no `review-authority:` label is present, so every existing ADR-1250 test and behavior is preserved exactly — pinned by the pre-existing `TestCheckReviewGate_Advisory_ChangesRequestedReview_StillClears`/`TestAttemptMergeOnValidate_Advisory_ChangesRequestedReview_StillMerges` non-regression controls, which continue to pass unmodified.

**A single issue can now diverge from its stage's default authority in either direction** — tightening (`review-authority:authoritative` on an advisory stage, for a high-risk change) or loosening (`review-authority:advisory` on an authoritative stage, for a low-risk exception) — without touching stage YAML or moving the item to a different board column.

**Forward compatibility for #1051/#1177's risk-tiered review policy.** A future policy that computes a risk tier and wants to enforce authoritative review for high-risk issues now has a label to apply (`review-authority:authoritative`) rather than needing to invent its own mechanism or a board-column workaround. That policy is explicitly out of scope here — this issue ships only the override mechanism, consuming the same `effectiveReviewAuthority` resolution point ADR-1250's forward constraint already designated as the place authority-related changes belong.

**Forward constraint, extending ADR-1250's:** any future authority-resolution logic (a new precedence source, a new label, a policy-computed default) belongs inside `effectiveReviewAuthority`, consulted identically by every gate and message call site — never duplicated inline at a call site, and never layered on as a bypass around the gate. This is the same constraint ADR-1250 placed on `reviewGateAuthorityVerdict`, now extended to authority-mode *resolution* as well as authority-mode *verdict evaluation*.

## Alternatives Considered

**A dedicated board column (`Review-Authoritative`), #1258's first attempt.** Rejected per this issue's motivating discussion: `review_authority` is a property of a stage's review policy, not a kind of stage, so encoding it as a column name puts a non-product concept on the board and makes per-item opt-in structurally impossible (an item occupies exactly one column).

**First-wins precedence (mirroring `extractModelOverride`), instead of rank-based "more restrictive wins."** Rejected per this issue's explicit requirement: a review-strictness control should resolve ambiguous/conflicting input toward the safer (more restrictive) behavior, not toward whichever label happened to be applied or iterated first. `effort:<level>`'s existing rank-based convention already establishes this pattern in the codebase for a different "prefer the strongest signal" case.

**Silently escalating a malformed suffix to `authoritative`, treating "unrecognized" as "better safe than sorry."** Rejected: this issue's explicit requirement is that malformed input is "never a silent escalation to authoritative" — a typo should not covertly tighten a gate an operator didn't intend to change at all. Falling back to the stage's own configured value (whatever it already was) is the behavior least likely to surprise.

**Adding the override as an `InvokeOptions` field, mirroring `model:`/`effort:`'s ADR-025 plumbing.** Rejected: `model:`/`effort:` overrides affect the Claude *invocation itself* (which model runs, what thinking budget it gets), so they need to reach `InvokeOptions` before a stage's Claude process starts. `review_authority` affects only gate-check-time decisions made by the engine after a stage completes — it never needs to reach a Claude invocation at all, so adding `InvokeOptions` plumbing for it would be unused surface area.

## References

- [ADR-1250: Review authority — advisory vs. authoritative, an axis orthogonal to autonomy](1250-review-authority-orthogonal-to-autonomy.md) — the stage-YAML `review_authority` mechanism, the shared `reviewGateAuthorityVerdict` verdict function, and the `yolo`/`cruise` composition guarantee this issue extends without modifying.
- [ADR-1216: Review gate checked at the landing decision](1216-review-gate-at-landing-decision.md) — establishes `reviewGateBlocksLanding` as the landing-decision choke point `effectiveReviewAuthority` is now also consulted from.
- #1258 — e2e coverage for authoritative mode; its Validate-stage discussion rejected the board-column approach in favor of this label mechanism, and its (unmerged, at the time of this issue) e2e scenario code already relies on the exact label string `review-authority:authoritative` this ADR ships.
- #1051, #1177 — risk-tiered human review and its risk→authority policy mapping; this issue ships the label mechanism those would consume, not the policy that decides when to apply it.
- `docs/state-machine.md` §6.1.1 — as-built description of the verdict-aware clearing mechanism, extended in this issue's commit to cover the label override.
