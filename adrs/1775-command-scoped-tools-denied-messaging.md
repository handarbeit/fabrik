# ADR 1775: Tool-Permission Denials Are Command-Scoped, Not Session-Scoped

**Date**: 2026-09-18
**Status**: Accepted
**Issue**: #1775 — tools-denied messaging names the tool, not the command, and asserts retry is
futile — workers abandon recoverable stages
**Supersedes (in part):** ADR-1523 (Exempt Tool-Permission Denials from `max_retries`) — specifically
its "by strong inference" conclusion that a real denial "most plausibly involved a hook," and the
downstream premise (repeated in ADR-1704's Context) that a denial reflects a session-wide permission
mode rather than a per-command allowlist mismatch. ADR-1523's detection mechanism
(`classifyToolsDenied`, the structural `permission_denials` signal), its `max_retries` exemption
rationale, and ADR-1704's reinvoke-path coverage of the same counter are all unchanged and remain in
force — this ADR narrows *why* a denial happens and *what Fabrik says about it*, not whether it counts
against `max_retries` or which invocation paths it bounds. Neither ADR-1523 nor ADR-1704 is rewritten;
both stand as accurate records of the decisions made with the evidence available at the time.

## Context

ADR-1523 established that a Claude Code tool-permission denial is not a stage failure and should not
consume a `max_retries` slot. Its reasoning for *why* a denial happens leaned on one inference, stated
in its own text as "by strong inference, never observed": a tool missing from `--allowedTools`
"self-reports it unavailable, with no `permission_denials` entry," so a real denial with a
`permission_denials` entry "most plausibly involved a hook" — implicitly, a session-wide condition.
ADR-1704 (which extended the same counter to cover the comment-review and reinvoke dispatch paths)
repeated a related framing in its Context section, citing `decision_reason_type: "mode"` as short-
circuiting allowlist evaluation entirely — a field never actually captured anywhere in this codebase,
carried only in that ADR's prose.

Both readings pointed toward the same downstream behavior: Fabrik's operator-facing comments described
the denial as applying to **the tool**, named `fabrik:unrestricted` (which removes all tool
restrictions) as *the* actionable remedy, and stated outright that "no retry can fix this on its own."

A community report (#1741, @jmatthewpryor) directly contradicts the inference with a captured
observation from a single session: turn 2's `git status && …` (`Bash`) **ran**; turn 4's
`base_branch=$(gh pr view …)` (also `Bash`) was **denied**. That is impossible if a hook or "mode" had
disabled `Bash` session-wide — the tool plainly still worked, twice, before and structurally consistent
with working again after. The reporter's independent 745-denial census attributes denials to *command
shape*, not to tool or session: roughly a third were `/tmp` redirects the sandbox doesn't cover, ~17%
binaries with no matching `allowed_tools` rule, ~14% environment-prefix/variable-assignment forms
(`base_branch=$(...)`), ~9% `cd`-chains, ~3% conditionals — every one of these is a shape mismatch
against Fabrik's own allowlist patterns (`Bash(gh:*)` does not match a `cd sub && gh ...` compound, or
an env-prefixed invocation, or a redirect into `/tmp`), not a session-wide lockout.

Fabrik's own worker logs offered no independent evidence either way: a scan of all 492 recorded
invocations containing a `permission_denials` field found **zero non-empty arrays** in this
installation — its allowlist happens to cover what its stages run. That is a gap in this
installation's evidence, not evidence against #1741's.

The consequence of believing the wrong model was concrete and measured: five consecutive Implement
comment-review invocations on the reporter's issue quit after a single denial (1, 1, 1, 4, 5 turns
respectively) — each one had Fabrik's own prior comment in context, telling it the tool was gone and
retrying was futile. The one run with **no prior tools-denied comment to read** hit five denials,
re-ran the same steps as separate, simpler commands, and completed normally. Fabrik's own messaging
was actively teaching workers to give up on a condition that resolves itself.

A related, independently-reported defect (#1743, same reporter) compounded this: the tools-denied log
line asserted "stage did not make progress," derived purely from the absence of
`FABRIK_STAGE_COMPLETE` — never from checking the worktree — contradicting this repo's own shipped
`plugin/fabrik-workflows/LABELS.md`, which already correctly documents that a denied invocation "may
have made real progress before the denial." A reported instance: an Implement comment-review that made
three commits (the entire fix, pushed), spent 80 turns and $18.38, and was logged as having made no
progress at all — the next invocation opened by declaring the review comment already handled and doing
nothing. Both defects land on the same line (`engine/claude.go`'s tools-denied branch in
`interpretClaudeResult`), so #1743 is folded into this issue rather than filed and fixed separately.

## Decision

**Denials are command-scoped.** A tool-permission denial reported by the CLI's `permission_denials`
array describes one specific tool call that didn't match Fabrik's `--allowedTools` patterns or was
blocked by a `PreToolUse` hook rule keyed on that call's shape — not a session-wide state change to the
tool itself. Other calls to the same tool, shaped differently, are unaffected and can be expected to
succeed. Fabrik's messaging, both to the worker (via the explanatory comment it reads on its next
invocation) and to the operator, now reflects this:

- `classifyToolsDenied` (`engine/claude.go`) decodes each denial's `tool_input` best-effort (`Bash`
  only — no captured evidence exists for other tools' `tool_input` shapes) and returns per-denial
  `[]toolDenial{ToolName, Command}` alongside the pre-existing deduplicated tool-name list. Absent,
  non-Bash, or malformed `tool_input` degrades silently to an empty `Command`, never a panic or a
  fabricated value.
- `claudeerr.ToolsDeniedError` gains an additive `Denials []ToolDenial` field; the pre-existing
  `ToolNames` field, its zero-value defaults, and every existing construction site are unchanged.
- Both operator-facing comments (`recordToolsDeniedDetection`'s initial detection,
  `pauseForToolsDeniedLimit`'s escalation at the `MaxToolsDeniedRetries` bound) name the first
  available denied command — sanitized (embedded newlines collapsed, backticks replaced) and truncated
  via the existing `truncateMiddle` convention (`engine/merge_train.go`), sized for a single command
  line rather than that helper's CI-log-sized defaults — and state plainly that the denial is scoped
  to that command, not the tool for the rest of the session.
- "No retry can fix this on its own" is removed from both comment templates and from
  `ToolsDeniedError`'s doc comment. Remediation is reordered: try re-running the step as separate,
  simpler commands, or add a matching `allowed_tools` rule for the specific command, first;
  `fabrik:unrestricted` remains available as a last resort, with its existing trade-off note (it
  removes all tool restrictions, not just the denied one) retained — never presented as the headline
  fix.
- The stage-facing skills (`plugin/fabrik-workflows/skills/*/SKILL.md`, all 12 — the 6 primary stages
  and their 6 comment-reinvoke counterparts, since both can hit a denial via ADR-1704's reinvoke-path
  coverage) each carry one line: a denied command is scoped to that one command, not the tool — re-run
  the step as separate, simpler commands and continue, rather than abandoning the stage.

**The `MaxToolsDeniedRetries` bound is unchanged.** #1704's counter, its accounting, and its
reinvoke-path coverage remain exactly as built — this ADR corrects what Fabrik *says* about a denial,
not what it *counts* or how many consecutive detections it tolerates before pausing for a human. A
handful of cycles is still a reasonable point to ask for help even though a denial is usually
self-recoverable, since the worker itself decides how to reshape a command and isn't guaranteed to
converge.

**Progress wording reflects only what was measured (#1743).** `runClaude` (`engine/claude.go`)
captures the worktree's `HEAD` immediately before starting the Claude process and again immediately
after `cmd.Wait()` returns (best-effort, mirroring the existing `headBefore`/`headAfter` pattern
`dispatchReviewReinvoke` already uses for review-reinvoke, #1045), and computes a commit count between
them when both captures succeeded and differ. `interpretClaudeResult`'s tools-denied branch reports
"N commit(s) pushed, stage did not signal completion" when a positive count was measured, or "stage
did not signal completion" — precisely what the absence of `FABRIK_STAGE_COMPLETE` establishes, and
nothing more — when it wasn't. The string "did not make progress" no longer appears anywhere in the
engine.

## Consequences

- A worker that hits a tools-denied classification and later reads its own prior comment (or the
  skill guidance) is told to keep working on the same stage with a reshaped command, not that the
  tool is gone — the change most directly expected to reduce the abandon-the-stage pattern #1741
  measured.
- Operators reading a tools-denied comment see the actual denied command and a remediation path that
  starts with the cheap, usually-sufficient fix (reshape the command or add an allowlist rule) before
  the blunt one (`fabrik:unrestricted`).
- `ToolsDeniedError`'s exported surface grows additively (`Denials`); no existing consumer (engine's
  own test suite, `tests/sim`) requires a change to keep compiling.
- This ADR does not widen `defaultAllowedTools` — the census in #1741 and any equivalent telemetry
  from other installations is exactly the kind of evidence that question would need, and is left for
  a separate decision.
- ADR-1523 and ADR-1704 remain unedited. A reader following either forward in time needs this ADR to
  learn that the allowlist/mode inference they describe was later contradicted by direct observation.

See #1775, #1741, #1743, ADR-1523, ADR-1704.
