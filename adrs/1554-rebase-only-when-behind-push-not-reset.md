# ADR 1554: Rebase Only When Behind; Push, Don't Reset, On Divergence

**Date**: 2026-08-12
**Status**: Accepted
**Issue**: #1554 — Review/Validate rebase loop: stage rebases then resets to origin, discarding its own work

## Context

`fabrik-review/SKILL.md` and `fabrik-validate/SKILL.md` each open their "Before You Start" section with an **unconditional** `git fetch origin main && git rebase origin/main` — no check for whether the branch is actually behind, and no instruction for what to do with the result.

Observed on `handarbeit/fabrik#1254` (PR #1255), Validate: five identical cycles in ~20 hours, each replaying 25 commits (originally authored 2026-07-30), producing new SHAs that don't match `origin/fabrik/issue-1254`, followed ~30 seconds later by `git reset --hard origin/fabrik/issue-1254` — discarding the rebase the stage had just performed. Final state each cycle: `HEAD == origin/fabrik/issue-1254`, clean tree, no commit landed, no `FABRIK_STAGE_COMPLETE`. The pass consumed a full invocation and produced nothing, so the stage was redispatched and repeated it.

Worse, the stage's own narration described this backwards — attributing the reset to an external reverter ("it reverted to the pre-rebase commit yet again") when the reflog shows the same stage performing both the rebase and the reset in the same invocation.

Two independent gaps caused this:

1. **No behind-check.** A branch already current with its base gains nothing from a rebase — it just replays commits and changes their SHAs. Once #1254's branch was current, every subsequent rebase was pure churn that could only diverge from the (unchanged) remote.
2. **No push instruction.** Neither skill told the agent what to do with a rebase's rewritten SHAs. Faced with local-vs-remote divergence and no guidance, the agent's own judgment reached for "make the worktree match the remote" — precisely the operation that destroys the work it just did.

`engine/merge_gate.go`'s `buildRebaseComment` (the engine's own synthetic rebase-recovery instructions, for a different trigger — post-hoc mergeability failure) already establishes the correct pattern for this repo: rebase, resolve conflicts conservatively, build/test, `git push --force-with-lease`, never reset. `engine/worktree.go`'s own push helper uses the same `--force-with-lease` idiom. Neither skill's "Before You Start" rebase step had ever been aligned with this established convention.

`fabrik-validate/SKILL.md`'s later Pre-Completion Gate (Step 1, added by ADR-1364/#1364) already solves the behind-check half of this problem — but for a *different* rebase point, addressing a *different* failure mode (CI-restart livelock and merge-queue ejection, not work-discarding reset). It predates this fix and was never revisited when the earlier "Before You Start" rebase went unaddressed.

## Decision

At the earlier "Before You Start" rebase point in both skills:

1. **Behind-check precondition.** Before rebasing, compute `behind_count=$(git rev-list --count HEAD..origin/main)`. If `0`, skip the rebase entirely — the branch is already current, so a rebase can produce nothing but churn.
2. **Push-not-reset divergence resolution.** If the rebase does run, push it immediately: `git push --force-with-lease`. State explicitly that a rebase's rewritten SHAs not matching `origin/<branch>` is expected (rebasing always rewrites SHAs), not a fault — and state an explicit prohibition on resolving that mismatch with `git reset --hard origin/<branch>` (or any reset of the branch to the remote tip), framing it as data loss.
3. **Causally-grounded narration (R5).** If the stage narrates the rebase outcome anywhere in its output, it must describe only what it actually did or observed — never assert an external cause (e.g., "the worktree reverted") without concrete evidence.

`CLAUDE.md`'s "Rebase onto the latest base branch" convention is updated to state the same precondition and divergence rule, since it is the standing documented convention both skills are following.

**`git push --force-with-lease` is safe here** because `fabrik/issue-<N>` is a branch Fabrik owns exclusively for the issue — there is no other legitimate writer to lose a race against, mirroring the existing justification in `buildRebaseComment` and `engine/worktree.go`.

**The behind-check idiom (`git rev-list --count`) is reused verbatim from Validate's own Pre-Completion Gate Check B**, for in-file and cross-skill consistency, not because the two rebase points share a failure mode — they don't (see "Why not fold into ADR-1364's gate" below).

### Why not fold into ADR-1364's Pre-Completion Gate

ADR-1364's three-check cascade (queue-safety, up-to-date, CI-freshness) exists to prevent a *different* problem — an unconditional rebase restarting CI faster than it can complete, and ejecting a PR already sitting in a merge queue — at a *different* rebase point (immediately before `FABRIK_STAGE_COMPLETE`, after all validation work is done). This issue's rebase point is the *first* thing either skill does, before any review or validation work has happened; there is no CI-restart-livelock risk yet (nothing has been validated to restart), and no merge-queue-ejection risk yet (an issue in `Queued`, a `holding_stage`, is never dispatched to Validate in the first place — the same reachability argument ADR-1364 already makes for its own gate). Extending Checks A/C to this earlier point would add queue/CI-freshness machinery this rebase point doesn't need, and the issue's own scope explicitly forbids altering the existing gate. Only Check B's up-to-date idiom is shared, by reuse of a name, not a mechanism.

## Consequences

**Positive:**
- Removes the loop's trigger: once a branch is current with its base, the behind-check permanently prevents any further rebase from running against it, closing off the divergence that led to the reset.
- A rebase that does produce new commits is preserved (pushed) rather than discarded, closing the actual data-loss bug.
- Aligns both skills' earliest rebase step with the push-not-reset convention already established elsewhere in this codebase (`buildRebaseComment`, `engine/worktree.go`), removing an inconsistency that had stood undetected until a live incident surfaced it.
- Stage narration that misattributes a self-inflicted reset to an external actor — which sent the #1254 investigation in the wrong direction — is explicitly prohibited going forward.

**Negative / accepted costs:**
- **This is a prompt-text-only fix; it does not deterministically prevent recurrence.** Unlike engine code, an instruction added to a SKILL.md file constrains a Claude Code stage agent's behavior only probabilistically — there is no code path that can *prevent* a future agent from still reaching for `git reset --hard` under some future context. This is accepted as the best available lever, mirroring the issue's own framing: the entire bug is agent-instruction shaped, since no engine call site resets a worktree to a remote branch.
- **Loop detection (R4) is explicitly out of scope**, deferred to #1555's generic no-op-redispatch-bounding mechanism. This ADR's fix removes the loop's *trigger* (per R2's own reasoning: a branch that's already current can't diverge from an unchanged remote), which is expected to prevent recurrence of this exact shape, but does not add a circuit breaker for a hypothetical future shape that isn't behind-check-shaped.
- **Validate's Pre-Completion Gate has the same missing-push gap** (confirmed by grep: no `git push` anywhere in that section, before or after this change) — a second instance of the root cause this issue fixes, in a section this issue's scope explicitly leaves untouched. Flagged here as a candidate follow-up, not fixed in this change.
- Test coverage for this fix is a content-presence regression test on the shipped skill text (mirroring `plugin/labels_drift_test.go`'s precedent), not a behavioral simulation of a Claude Code worker — no harness in this repo executes SKILL.md prompt text against a real model. It proves the instruction text changed and is non-vacuous against the pre-fix text; it does not prove a future agent invocation will comply.

## Alternatives Considered

### Extending ADR-1364's Pre-Completion Gate to cover the earlier rebase point too

Rejected — see "Why not fold into ADR-1364's Pre-Completion Gate" above. Different rebase point, different failure mode, and the issue's own scope forbids altering the existing gate.

### `git merge-base --is-ancestor origin/main HEAD` instead of `git rev-list --count`

Both are equivalent in outcome (zero-behind ⇔ ancestor). `git rev-list --count` was chosen to match the idiom Validate's own Pre-Completion Gate already uses (`behind_count` via `git rev-list --count`), for consistency within that file and, by extension, with Review's mirrored treatment — not for any functional difference.

### Engine-side enforcement (refuse to permit a worktree reset to a remote ref)

Rejected as out of scope by the issue itself: no engine call site performs this reset (confirmed by grep — the only two `reset` call sites in `engine/` are merge-train trial rollback to a captured `preMergeHEAD`, never a remote ref, and context-file unstaging). Building an engine-side guard against a Bash tool invocation the stage agent makes directly would require either sandboxing `git` inside the worktree or post-hoc reflog inspection to detect and revert it after the fact — both substantially larger changes than a prompt-text fix, and neither was asked for by this issue.
