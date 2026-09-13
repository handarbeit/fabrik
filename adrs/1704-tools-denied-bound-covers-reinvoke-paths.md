# ADR 1704: Tools-Denied Bound Covers Reinvoke Paths

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1704 — tools-denied bound covers only the stage path; reinvoke paths loop indefinitely on a
mode denial

## Context

ADR-1523 gave a "don't ask mode" tool denial (`decision_reason_type: "mode"` — deterministic, refuses
Bash at the mode level, short-circuiting allowlist evaluation so no retry and no allowlist entry can fix
it) a correct exemption from `max_retries` and a correct terminal state: `ToolsDeniedRetries`/
`MaxToolsDeniedRetries` (default 3), escalating via `pauseForToolsDeniedLimit`. But that bound was wired
into exactly one consumer: `finalizeStageOutcome` (`engine/item.go`), the stage-dispatch path. Every
reference to `ToolsDeniedRetries` and `pauseForToolsDeniedLimit` lived in that one file.

The reinvoke family — `dispatchCIFixReinvoke`, `dispatchReviewReinvoke`, `dispatchRebaseReinvoke` — and
ordinary comment review all route through a second, shared funnel: `processComments`
(`engine/comments.go`). Its error branch never classified `*claudeToolsDeniedError`; it fell into a
generic `warn` log and the comment circuit breaker (§4.6, ADR-1089/ADR-1555). That breaker is calibrated
for a different failure shape — N invocations within a 30-minute window (default N=10) signaling a
*runaway* comment loop — not a *slow, deterministic* denial recurring once per stage-retry cooldown. At a
`poll: 120` cadence with a 20-minute retry cooldown, a denial recurs roughly 1.5 times per 30-minute
window: structurally unreachable against a threshold of 10. The loop never converged.

Four occurrences across two community reports (#1657, 2026-08-26 and 2026-09-06, same environment)
confirmed this in practice: three of four landed on paths ADR-1523's bound did not cover, and the
sharpest was `ci-fix-reinvoke` — the *designed* CI repair path, disabled by the exact fault it exists to
recover from, retried indefinitely at ~$0.30–0.40 per attempt with no terminal state. The reporter's own
suggestion (independently corroborated during Research) named exactly the fix's shape: gate the retry on
the `fabrik:tools-denied` classification rather than only annotating it, and name `fabrik:unrestricted` as
the actionable remedy in the pause comment.

## Decision

### Classify in `processComments`, not `reinvoke.go`

`interpretClaudeResult` (`engine/claude.go`) is the single seam both invocation paths already share:
`Invoke` (stage dispatch) and `InvokeForComments` (comment path) each receive an identical
`*claudeToolsDeniedError` under the identical detection rule (`!completed && ok &&
len(resp.PermissionDenials) > 0`). The gap this issue closes is entirely in the *consumers* of that
already-unified classification, not the classification itself.

`processComments` is the single point all four invocation shapes — ordinary comment review and the three
reinvoke dispatchers, via the shared `dispatchReinvoke` scaffold — already funnel through. Adding the
classification there, rather than in each of the three `reinvokeOpts.after` callbacks or in `reinvoke.go`
itself, covers all four shapes in one place instead of three duplicated checks that would still miss
ordinary comment review. `reinvoke.go` needed no change at all: `dispatchReinvoke`'s error handling only
logs whatever `processComments` returns.

### Share the bookkeeping via an extracted helper, keep `pauseForToolsDeniedLimit` unforked

The "increment `ToolsDeniedRetries`, apply the once-per-episode label+comment, compute
`willEscalate`" block was inlined in `finalizeStageOutcome`. Extracting it into
`recordToolsDeniedDetection(item, stage, toolNames) (count int, willEscalate bool)` — callable from both
`finalizeStageOutcome` and the new `processComments` branch — removes ~20 lines of duplication risk (two
copies drifting, e.g. differing label/comment text) without touching `pauseForToolsDeniedLimit`'s
signature or behavior at all; each caller independently decides whether to invoke it once `willEscalate`
is true, exactly as before.

Because `ToolsDeniedRetries` is keyed by `(repo, issue, stageName)` only — never by which code path
incremented it — a denial on the stage-dispatch path and a subsequent denial on `ci-fix-reinvoke` for the
same stage accumulate against the identical counter and the identical `MaxToolsDeniedRetries` bound. The
outcome (pause at the bound, exemption from `max_retries`) is the same regardless of which invocation type
detected it — this is what R3 ("escalate identically wherever it is detected") requires, and it falls out
of reusing the same counter rather than needing separate bookkeeping.

### Placement bypasses the comment breaker entirely, by construction

The new branch sits in `processComments`'s `if err != nil && !completed` block, as a sibling to the
pre-existing usage-limit exclusion, ahead of the generic `warn` log and both `checkNoOpCommentCycle`/
`checkCommentBreaker` calls. It returns immediately after recording the detection (and, if the bound is
reached, pausing) — never reaching either breaker check. This is a structural exclusion, not a threshold
retune: R2 explicitly forbids lowering the comment breaker's own thresholds to compensate (doing so would
make it fire on legitimate busy-comment issues), and this placement makes that retune unnecessary — a
tools-denied cycle simply never participates in the breaker's counting at all.

### Remedy naming (R4)

`pauseForToolsDeniedLimit`'s escalation comment — the one that fires at `MaxToolsDeniedRetries`, distinct
from the once-per-episode detection comment — now names `fabrik:unrestricted` as the primary actionable
remedy, with its caveat (it removes all tool restrictions, not just the denied tool) stated alongside it,
retaining "check the permission configuration" as an alternative for operators who'd rather fix the
underlying cause. A headless worker has no interactive prompt to grant the denied tool, so the pre-#1704
text's implicit "grant Bash permission" framing was not actionable in this context; `fabrik:unrestricted`
is the one lever an operator (or Fabrik itself, via a follow-up comment/label) can actually pull. Per
Acceptance's literal wording ("the pause comment names..."), only the escalation comment's text changed —
the once-per-episode detection comment is unchanged, since it reaches the operator well before the bound
and Acceptance does not require the remedy there.

## Alternatives Considered

**Retune the comment breaker's threshold or window for this condition.** Rejected outright by the
issue's own R2: a mode denial is deterministic and needs a consecutive-detection bound, not a
frequency-based one: The comment breaker measures how often invocations happen; ADR-1523's bound measures
how many times in a row the same deterministic condition recurs. Conflating them would either make the
breaker too sensitive for legitimate high-traffic comment threads, or leave the tools-denied case
under-bounded if tuned the other way.

**Duplicate the detection/bookkeeping block in `processComments`'s new branch instead of extracting a
helper.** Rejected: the issue's scope note forbids forking `pauseForToolsDeniedLimit` specifically, but is
silent on the surrounding bookkeeping — silence is not license to duplicate ~20 lines with a real risk of
future drift between the two copies. Extraction is a strict simplification of the pre-existing code, not
new complexity.

**Add the classification inside each `reinvokeOpts.after` callback in `ci.go`/`reviews.go`/
`merge_gate.go`.** Rejected: three separate call sites would each need the identical `errors.As` check,
and none of them cover ordinary comment review (which does not go through `dispatchReinvoke` at all).
`processComments` is the one point that already sees all four invocation shapes.

## Consequences

**Positive:**
- A tool denial detected via `ci-fix-reinvoke`, review-reinvoke, or comment review now escalates at the
  same `MaxToolsDeniedRetries` bound as the stage-dispatch path, closing the exact gap #1657's four
  occurrences demonstrated — including the `ci-fix-reinvoke` case, where the designed CI repair path was
  itself disabled by the fault it exists to recover from.
- The bound is shared, not duplicated: a mode denial recurring across different invocation types against
  the same stage still converges to the same terminal state, rather than resetting per code path.
- The comment breaker's own calibration (N per window, tuned for runaway comment loops) is untouched — no
  retune, no new sensitivity to legitimate high-traffic threads.
- The escalation comment now names an actually-actionable remedy (`fabrik:unrestricted`) for a headless
  worker, rather than a "grant Bash permission" framing with no interactive prompt to act on.

**Negative / Trade-offs:**
- `reinvoke.go` and the three individual dispatchers remain unaware of tools-denied handling entirely —
  correct today (all four shapes funnel through `processComments`), but a future reinvoke-family addition
  that bypasses `processComments` would silently regress to the pre-#1704 gap. No structural guard
  prevents this beyond code review.
- The shared counter means a stage that alternates between stage-dispatch denials and reinvoke-path
  denials reaches the bound faster (in wall-clock terms) than either path would alone — the intended
  behavior per R3, but worth naming: the two paths are not independently budgeted.

## Explicitly Out of Scope

The CLI's own mode-selection logic — why some invocations start in don't-ask mode while others in the
same hour do not is unresolved and not visible from the engine log (the reporter's own open question).
This issue makes the consequence terminal; it does not investigate or change the cause.

**References:** ADR-1523 (the stage-path bound this extends), ADR-1089/ADR-1555 (the comment breaker this
bound is deliberately excluded from, not folded into), #1657 (the community report this work issue traces
to), #1691 (same environment, different failure — not investigated here).
