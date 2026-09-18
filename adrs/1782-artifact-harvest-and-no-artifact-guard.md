# ADR 1782: Assistant-Turn Artifact Harvest and No-Artifact Completion Guard

**Date**: 2026-09-18
**Status**: Accepted

## Context

`interpretClaudeResult` (`engine/claude.go`) trusted the CLI's terminal
`result` field (`resp.Result`) verbatim as "the stage's output." That field
is whichever text the agent emitted in its *very last* turn — not
necessarily the turn that produced the stage's real artifact. When the agent
made one more tool call after emitting its output (e.g. a final verification
command before signaling done), `resp.Result` carried only the wrap-up,
sometimes nothing but the bare `FABRIK_STAGE_COMPLETE` marker itself.

Reported in #1632 and observed twice in one day on v0.0.80:
`liminis-editor#69`'s Plan ran 38 turns for $1.55, pushed the branch, logged
`stage "Plan" complete`, and wrote neither the Plan comment nor
`.fabrik-context/stage-Plan.md`. The transcript's terminal `result` line
carried the bare string `"FABRIK_STAGE_COMPLETE"`; the actual 12,147-character
artifact sat in an earlier assistant turn, followed by a `tool_use` call the
model made before its final wrap-up.

`stageCompleteRE.MatchString(text)` was still true, so `completed` was
`true` — the stage was labelled `stage:<name>:complete` with nothing behind
it, discovered only when a later stage found no context to read (the same
symptom `recoverMissingPlanComment`, #982, exists to recover — but from a
different root cause: a stale-cache race, not a harvest defect).

A narrower instance of the correct fix shape already existed in the same
function: immediately after the `resp.Result` assignment, a fallback scanned
all assistant turns for a `FABRIK_ISSUE_UPDATE_BEGIN`/`END` block missing
from `result` — but scoped only to that one marker pair, not the general
case (Plan/Research/Review/Validate prose, which carries no self-delimiting
block of its own).

## Decision

### Harvest: generalize the existing narrow fallback, don't add a parallel mechanism

`extractLastSubstantialAssistantTurn` reuses the same `forEachAssistantText`
NDJSON line-scanner that already powers the `FABRIK_ISSUE_UPDATE_BEGIN`
fallback. It is gated by `artifactMissingOnComplete`, which fires only when
`resp.Result` (after the existing ISSUE_UPDATE fallback has already run)
carries `FABRIK_STAGE_COMPLETE` but nothing beyond Fabrik's own bare control
markers (`hasArtifactContent`, computed by stripping the same five marker
lines `finalizeStageOutcome` already strips before posting). When it fires,
the recovered turn is prepended to `text` — mirroring the ISSUE_UPDATE
fallback's own composition shape exactly.

This keeps the fix a single choke point: both invocation paths
(`InvokeClaude`/stage-dispatch and `InvokeClaudeForComments`/comment-review)
bottom out in this same `interpretClaudeResult` call, so a fix here closes
the gap for both without duplicating harvest logic in `engine/item.go` and
`engine/comments.go` separately.

### Selection rule: last turn with real content, not "the turn with the marker"

The reported transcript's marker-bearing turn contained *only* the marker —
anchoring selection on "the turn containing `FABRIK_STAGE_COMPLETE`" would
still have failed against the actual reported data. `hasArtifactContent` is
therefore evaluated per-turn, independent of which turn (if any) happens to
carry the completion marker, and the **last** turn satisfying it wins — the
same "last occurrence wins" convention `extractIssueUpdateFromAssistantTurns`
already established for the narrower ISSUE_UPDATE case.

### Guard: never label complete with no artifact, gated on the same predicate

`artifactMissingOnComplete` also gates both places `interpretClaudeResult`
converts "marker present" into `completed = true` — the marker-found-despite-
a-trailing-error path and the ordinary clean-exit path — re-evaluated against
the post-harvest text. If harvest found nothing, `completed` is forced
`false` and the invocation falls through to each caller's existing
non-completion handling: for a stage dispatch, the retry/escalate machinery
in `finalizeStageOutcome`; for comment review, the existing no-progress/
no-op-cycle detection in `processComments`. No new error type and no new
label were introduced — `completed=false, err=nil` is exactly the shape both
callers already handle correctly for an ordinary incomplete run.

### Retry classification: counts against `MaxRetries`, not exempted

The closest existing precedent is the degenerate-output guard (#1065,
`isDegenerateOutput`): a stage output that is nothing but a bare `@file`/
absolute-path reference is likewise forced to `completed=false` and counts
against `MaxRetries`. The alternative shape — `claudeUsageLimitError`/
`claudeToolsDeniedError`, exempted from `MaxRetries` because "the stage
never really ran" — does not fit here: the reported invocation ran 38 turns,
cost $1.55, and pushed a branch. The work happened; only the harvest of its
output failed. This guard therefore follows the degenerate-output shape:
it counts against `MaxRetries`, posts a one-time first-detection comment
before the limit is reached, and escalates via the existing
`escalateFailedStage` path at the limit — reusing exactly the machinery that
already exists for this classification, rather than inventing a third shape.

### `escalateFailedStage`'s reason parameter becomes a pre-formatted `causeNote`

`escalateFailedStage`'s existing `reason` parameter hardcoded the
bare-file-reference narrative internally ("the model likely wrote its
output to a file..."). Reusing that verbatim for this guard's distinct cause
(no artifact anywhere, not a dangling file reference) would misdescribe it
to the operator reading the pause comment. The parameter is now a
pre-formatted `causeNote` (the complete `\n\n**Cause:** ...` paragraph, or
`""`), constructed at each call site — `engine/item.go`'s two causes
(degenerate output, no artifact harvested) now build their own accurate
message rather than sharing one narrative that only fit one of them.

### `FABRIK_NO_WORK_NEEDED` exclusion

A stage that legitimately produces no artifact — `FABRIK_STAGE_COMPLETE`
co-occurring with `FABRIK_NO_WORK_NEEDED` — is a documented, existing
completion shape (the engine marks all remaining stages complete and moves
the issue straight to Done, no PR). Both the harvest scan and the completion
guard check `!CheckNoWorkNeeded(text)` before doing anything at all, so this
path never triggers a scan (which could otherwise pull unrelated
earlier-turn reasoning into a completion that should carry nothing) and is
never treated as this defect.

## Consequences

- A stage whose artifact lands in an earlier assistant turn than the CLI's
  own terminal result — the reported #1632/#1782 shape — is now harvested
  correctly instead of silently discarded.
- A genuinely artifact-free `FABRIK_STAGE_COMPLETE` (no earlier substantial
  turn, no content in the result) is no longer mislabelled complete — it
  retries and, if the condition persists, escalates with a comment naming
  this specific cause, distinguishing it from a degenerate bare-file-
  reference failure.
- The ordinary case (artifact already present in the terminal result) is
  unaffected: `hasArtifactContent(resp.Result)` is true, the scan never
  runs, and `text`/`completed` are byte-identical to before this fix.
- `recoverMissingPlanComment` (#982) is unmodified and remains a valid
  safety net for its own, unrelated root cause (a stale `item.Comments`
  cache snapshot racing a fresh comment post). This fix addresses the
  harvest defect directly, so new occurrences of the #1632 shape should no
  longer reach that recovery path at all — but #982's cause is independent
  and can still occur on its own.
- The comment-review path (`processComments`/`publishCommentOutput`) needed
  no code changes: it already treats `completed=false` correctly for any
  other incomplete run, and this fix's guard lives entirely upstream in the
  shared `interpretClaudeResult`.

## Alternatives Considered

- **A second, parallel scanning mechanism dedicated to this issue.** Rejected
  — the existing `FABRIK_ISSUE_UPDATE_BEGIN` fallback already solved the same
  problem for one marker; generalizing it (same scanner, broader predicate)
  avoids two independently-maintained mechanisms doing the same kind of scan.
- **A dedicated exempted error type (`claudeArtifactMissingError`), mirroring
  `claudeUsageLimitError`.** Rejected — the invocation here did real,
  billable work; exempting it from `MaxRetries` would treat a defect that
  produced a pushed branch as though the stage never ran at all, which
  overstates how benign the condition is relative to the degenerate-output
  precedent it otherwise matches exactly.
- **Anchoring turn selection on "the turn containing the marker."** Rejected
  — falsified directly by the reported transcript, whose marker-bearing turn
  contained nothing else.
