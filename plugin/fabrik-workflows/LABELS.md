# Fabrik Label Reference

Fabrik uses GitHub issue labels as its state mechanism. Each stage skill's own
"Labels You Interact With" section documents the *hot subset* — the labels
that stage actually reads or writes on a normal run. This file is the
long-tail reference: everything else, for when you need to reason about a
label your stage doesn't touch directly (e.g. explaining engine behavior to
a user, or understanding why an issue is stuck).

**Canonical source**: `docs/state-machine.md` §1.4 "Label Semantics
Reference" is the exhaustive, authoritative as-built spec (fabrik source,
not shipped to consumers). This file is a condensed derivative, sized for
on-demand reading rather than exhaustive reference. If something here
seems to conflict with observed engine behavior, `docs/state-machine.md` —
or the fabrik source itself — wins.

## Pause & Block

Labels that suspend processing entirely.

- **`fabrik:paused`** — Processing stopped on active stages. Applied after
  retry exhaustion, `FABRIK_BLOCKED_ON_INPUT`, a review/CI/rebase timeout or
  cycle limit, a manual TUI stop, or the comment-processing circuit breaker
  tripping (too many non-advancing comments in a row). Cleared by the user
  removing it manually, or by a human (non-bot) comment arriving on the
  issue — an implicit resume. Cleanup stages ignore comments entirely while
  paused. A Validate-stage item whose PR was merged externally can still
  advance regardless of this label.
- **`fabrik:awaiting-input`** — Companion to `fabrik:paused` marking the
  specific "blocked on user input" variant (as opposed to a retry-exhaustion
  or timeout pause). Cleared the same way as `fabrik:paused`, plus
  automatically on the next stage completion (removes any orphaned label
  left over from a manual `fabrik:paused` removal).
- **`fabrik:blocked`** — Set when the issue has open `blockedBy`
  dependencies (via GitHub's Issue Dependencies feature). Cleared as soon as
  all blockers close — either immediately (a push-based observer) or within
  one dependency re-check cycle. Suspends all stage dispatch while present.

## Gates

Labels that hold an item at its current stage until an external condition
(CI, review, mergeability) resolves. Distinct from Pause & Block: gates are
part of the normal happy path, not an error condition.

- **`fabrik:awaiting-review`** — The `wait_for_reviews: true` review gate is
  active: outstanding PR reviewer requests remain, or a self-submitting bot
  reviewer (Copilot/Gemini/CodeRabbit-style — never formally "requested")
  hasn't yet posted a review. Cleared when every reviewer responds, or when
  `FABRIK_REVIEW_WAIT_TIMEOUT` elapses (the issue is then paused instead).
  Blocks auto-advance and PR landing until clear. See `review-authority:<mode>`
  and `expected-reviewers:<mode>` below for how the gate's strictness and
  reviewer expectations can be tuned per-issue.
- **`fabrik:awaiting-ci`** — The `wait_for_ci: true` CI gate is active.
  Applied the instant a stage emits `FABRIK_STAGE_COMPLETE`; the stage's own
  `stage:<name>:complete` label is deliberately withheld until CI actually
  passes (a conjunctive gate — completion and green CI both required).
  Cleared when all CI checks pass, or when GitHub reports the PR as
  mergeable via `mergeable_state`. Also (re-)applied on a confirmed CI
  failure to drive the CI-fix reinvocation path (the engine re-prompts the
  stage with failing-check details). Suppresses stage re-dispatch while
  present — only the catch-up/settle loop evaluates CI during this window.
- **`fabrik:rebase-needed`** — The linked PR no longer cleanly merges onto
  its base branch (GitHub reports `mergeable == false`, a confirmed
  conflict — not simply "still computing"). The engine re-dispatches the
  stage to resolve the conflict. Cleared when the rebase succeeds and
  GitHub reports mergeable again, or when the PR merges/closes. At
  `MaxRebaseCycles`, the issue is paused for human intervention instead.
- **`fabrik:awaiting-done`** — A `FABRIK_NO_WORK_NEEDED` decision has been
  made; the board move to Done and/or issue close is still outstanding.
  Retried every poll regardless of the item's current board column.
  Suppresses dispatch of every non-cleanup stage while present. Cleared
  once both the Done move and issue close succeed; escalates to
  `fabrik:paused` after `MaxRetries` failed settle passes.
- **`fabrik:awaiting-member-close`** / **`fabrik:awaiting-close`** — Same
  shape as `fabrik:awaiting-done`, scoped to narrower failure paths: a
  merge-train member issue whose close call failed after its PR already
  merged (`awaiting-member-close`), or a `base:<branch>` issue whose
  engine-initiated close failed after its Done-advance already happened
  (`awaiting-close`). Both retried every poll until the issue is confirmed
  closed, then escalate to `fabrik:paused` after `MaxRetries`.
- **`fabrik:awaiting-placement`** — A spawned child issue's initial
  project-board column placement failed at spawn time. Retried every poll
  until placement succeeds or the child is observed closed; escalates to
  `fabrik:paused` (on the child) after `MaxRetries`. Does not itself
  suppress dispatch — a stranded child in an unrecognized column
  (typically Backlog) never resolves to a stage there anyway.
- **`fabrik:auto-merge-enabled`** — Engine-internal marker that GitHub's
  native auto-merge has been enabled on the PR (yolo + non-cruise, at
  Validate completion). Anchors the auto-merge convergence budget and
  bypasses the legacy CI/review gates while present. Cleared when the PR
  merges or closes, the user disables auto-merge in the GitHub UI, or the
  convergence budget is exhausted (pauses the issue). Never applied when
  `fabrik:cruise` is also present.

## Operator Overrides

User-applied labels that change engine behavior for one issue. All are
applied and removed manually; the engine never adds or clears these itself
except where noted.

- **`fabrik:yolo`** — Forces auto-advance through stages even when a
  stage's YAML sets `auto_advance: false`; also triggers GitHub-native
  auto-merge of the linked PR once Validate completes.
- **`fabrik:cruise`** — Forces auto-advance without auto-merge; the
  pipeline stops (issue stays open, PR unmerged) once Validate completes.
  When both `cruise` and `yolo` are present, **cruise wins** for both
  end-of-Validate decisions.
- **`fabrik:unrestricted`** — Passes `--dangerously-skip-permissions`
  instead of `--permission-mode dontAsk`, bypassing the default tool
  allowlist entirely. Use sparingly — it removes all tool restrictions.
- **`fabrik:extend-turns`** — Pre-grants a 2× turn budget for every stage
  and comment-processing invocation while present. Persists across stages;
  removed only at the Done cleanup stage, or manually. No-op when a stage's
  `max_turns == 0` (already unlimited).
- **`model:<name>`** — Selects a specific Claude model for this issue
  (e.g. `model:opus`). First label wins if more than one is present.
- **`effort:<level>`** — Overrides the stage's configured thinking effort
  (`low`/`medium`/`high`/`max`) for this issue only. Highest-ranked value
  wins if more than one is present.
- **`base:<branch>`** — Overrides the worktree base branch: Fabrik forks
  from, rebases onto, and targets PRs at `<branch>` instead of the
  repository default. Must be set before Research. Falls back to the
  default branch (with an explanatory comment) if `<branch>` isn't found
  on the remote.
- **`review-authority:<mode>`** (`advisory`/`authoritative`) — Overrides a
  `wait_for_reviews: true` stage's `review_authority` for this issue only.
  `advisory` (the unset default) clears the gate once reviewers have
  responded, whatever they said. `authoritative` additionally requires no
  outstanding CHANGES_REQUESTED and satisfied required approvals. Both
  labels present → resolves to `authoritative` (more restrictive), with a
  logged warning. Unrecognized suffix → ignored, falls back to stage
  config.
- **`expected-reviewers:<mode>`** (`none`/`declared`) — Overrides a
  `wait_for_reviews: true` stage's `expected_reviewers` for this issue
  only. `none` enables an immediate fast-advance when nothing was actually
  requested. `declared` resolves to a fixed synthetic test identity (never
  posts a real review — expect the full wait-timeout ladder to run out).
  Both present → resolves to `declared` (imposes waiting), with a logged
  warning. Unrecognized suffix → ignored, falls back to stage config.
- **`fabrik:revalidate`** — Forces re-entry into the Validate stage: the
  engine strips `stage:Validate:{complete,failed}`, `fabrik:paused`,
  `fabrik:awaiting-input`, `fabrik:awaiting-ci`, `fabrik:auto-merge-enabled`,
  and the trigger label itself, then Validate dispatches on the next poll.
  Applied to a non-Validate issue: only the trigger label is removed, with
  a warning. Safe to apply mid-flight — held until the active worker exits.
- **`fabrik:clear-claude-limit`** — One-shot operator command: clears an
  active account-wide Claude usage-limit suspension without an engine
  restart. Consumed and removed from every carrying item in the same poll
  pass. Not scoped to the issue it's applied to — the suspension it clears
  is account-wide.

## Engine-Internal & Informational

Labels a skill will rarely need to act on directly, but may see on an issue
and should recognize.

- **`fabrik:locked:<user>`** — Held while a specific Fabrik instance is
  actively processing this issue; prevents other instances (or other
  worker slots) from racing on the same item. Released when the stage
  invocation ends, whatever the outcome.
- **`fabrik:editing`** — Held for the duration of comment processing;
  prevents a fresh stage dispatch from racing an in-flight comment reply.
- **`stage:<name>:in_progress`** — Informational: shows which stage is
  currently running on the item. Mirrors `fabrik:locked:<user>`'s
  lifecycle.
- **`stage:<name>:complete`** — Marks a stage as finished. Never removed
  in the ordinary case (exception: `stage:Validate:complete` is stripped
  if the linked PR's HEAD SHA changes after completion, since the
  mergeability determination it certified no longer applies). Prevents
  re-invocation of that stage and drives catch-up advancement to the next
  one.
- **`stage:<name>:failed`** — Applied after a stage exhausts its retry
  budget; always paired with `fabrik:paused`. Cleared only when the user
  manually removes `fabrik:paused` (signaling they've intervened).
- **`fabrik:sub-issue`** — Applied to a child issue created by Plan's
  `FABRIK_SPAWN_CHILD_*` decomposition mechanism. Purely informational —
  carries no gating semantics of its own.
- **`fabrik:children-spawned`** — Applied to the *parent* issue once all
  of its declared sub-issues have been created, board-placed, and linked
  as `blockedBy` dependencies. Acts as an idempotency guard — while
  present, the pre-Implement spawn step is a no-op. Remove manually (and
  close any orphaned children) to force a fresh spawn.
- **`fabrik:claude-limit`** — Set when a Claude invocation exits because
  the account's usage limit was hit (detected structurally from the CLI's
  own result payload, never from output text). The stage attempt still
  counts toward the normal dispatch cooldown, but not against
  `max_retries` — the stage never actually ran. Cleared on the issue's
  next invocation that isn't itself a usage-limit exit, or account-wide
  once the suspension lifts. Distinct from GitHub's own API rate limiting.

## Notes for skill authors

- A label's *presence* on an issue is authoritative for engine behavior;
  don't infer state from label *absence* plus board column alone — several
  gate labels (the `awaiting-*` family) are retried independently of
  column position.
- When in doubt about a label not covered here, or about current, precise
  semantics, treat this file as a starting point and defer to
  `docs/state-machine.md` (fabrik source) or ask the operator — don't
  guess at gating behavior.
