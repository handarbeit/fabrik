# ADR 1419: Cross-Repo Spawn Board-Servability and Mid-flight Spawn Recognition

**Date**: 2026-08-09
**Status**: Accepted
**Extends**: [ADR 048: Engine-Side Sub-Issue Spawning via blockedBy](048-spawn-child-engine-side.md)

## Context

Two independent, live-reproduced failure modes shared the same symptom — a spawned child that never gets picked up, and a parent that believes it is unblocked when it is not:

1. **Board/assignment misrouting.** `spawnChildren` (`engine/spawn.go`) always registered a spawned child on the *parent's own* project board (`board.ProjectID`) and never assigned anyone. For a same-process, multi-repo instance this is harmless — that instance's board already accepts items from any repo it has access to. For a `repo:`-scoped instance spawning cross-repo, the child landed on a board no correctly-configured instance was ever polling: it sat correctly in a real column, unassigned, on the *wrong* board, for ~13 hours until a human noticed and manually fixed it.
2. **Engine-bypass gap.** `FABRIK_SPAWN_CHILD_BEGIN/END` was recognized only from a stored Plan-stage comment (`preImplement`, gated to `stage.Name == "Implement"`). A Review-stage agent that discovered a mid-flight blocker had no sanctioned route to declare it, and — with `gh` available and no reason not to — called `gh issue create` directly. The engine never observed that spawn: no board registration, no assignee, and critically no `blocked_by` edge on the parent. The parent had no record it was blocked at all, resumed, and reported green through Review and Validate on a tree still missing the blocker's fix — caught only because a human manually diffed a vendored artifact.

Failure mode 2 is materially more dangerous than failure mode 1: an unregistered child is at least visibly stuck; a missing `blocked_by` edge is silent in the *other* direction and can let a PR merge without the work it was supposed to be blocked on.

A third, narrower gap compounds mode 2: `checkDependencies`'s resume-from-`fabrik:blocked` live re-read (added for #977's cache-staleness workaround) only fired when the *cached* `BlockedBy` list was non-empty. A bypassed spawn's cached list is always empty — exactly the shape that skipped the live read and trusted the wrong empty list.

## Decision

Two separable fixes, sharing one hardened function, plus one narrow gate widening.

### Fix A: board-servability is a scope-check, not a route-finder

A Fabrik instance has exactly one `Owner`/`Repo`/`ProjectNum` — there is no config field anywhere for "which board serves which other repo," and building general cross-instance board discovery was explicitly ruled out of scope for the originating issue. Given that constraint, the fix cannot *locate* the correct board for a mismatched `repo:`-scoped instance; it can only refuse to guess wrong.

`spawnTargetServedByThisInstance(childOwner, childRepo)` answers exactly one question: does *this instance's own, already-known* config cover this repo?

```go
func (e *Engine) spawnTargetServedByThisInstance(childOwner, childRepo string) bool {
	if e.cfg.Repo == "" {
		return true // multi-repo instance already serves any repo its board carries
	}
	return childOwner == e.cfg.Owner && childRepo == e.cfg.Repo
}
```

Wired into `spawnChildren`'s upfront validation pass — before any `CreateIssue` call, alongside the existing `DEPENDS_ON` structural validation and the per-repo clone-readiness loop — so a routing refusal aborts the entire batch with zero orphaned issues, exactly like every other upfront-validation failure this function already had. On refusal: `fabrik:paused` + an explanatory comment naming the unservable target and this instance's own configured scope, same fail-loud shape as every other fatal step in this function.

This does not make the *original* misrouted board correct on its own — no discovery mechanism was added — it makes the failure visible instead of silent, which is what the requirement actually asked for.

**Mandatory assignment rides alongside, unconditionally.** Every spawned child — same-repo or cross-repo — is now assigned to `cfg.User`, folded into `CreateIssue`'s existing POST as an `assignees` field rather than a separate mutation. A single combined call keeps one failure point (already fail-loud) instead of adding a second, silent one; a misconfigured `user:` value fails the spawn the same way a `CreateIssue` failure always has.

### Fix B: Review/Validate get their own recognition, parsed from raw output

Extending `FABRIK_SPAWN_CHILD` recognition to Review and Validate is a *different code path* from Plan's, not a parameterization of `preImplement`. Plan's mechanism is "stage N's output, read back later as a stored comment, drives stage N+1's pre-dispatch step" (`preImplement` hardcodes `findStageComment(item.Comments, "Plan")`). Review and Validate are `post_to_pr: true` stages that run *after* Implement — there is no later dispatch step to defer to, and nothing re-reads their comment for this purpose.

Instead, `finalizeStageOutcome` (`engine/item.go`) parses `output` directly — the same in-memory string produced by *this* dispatch, before `postOutputToPR`/`stripMarkers` runs — mirroring the existing `FABRIK_PR_CREATE_BEGIN/END` handling immediately above it in the same function. This sidesteps `post_to_pr` entirely (parsing happens before posting, so it doesn't matter where the comment eventually lands) and needs no new idempotency label: `output` is this dispatch's own fresh content, never replayed across dispatches, so a block is processed exactly once by construction — the same guarantee `FABRIK_STAGE_COMPLETE` detection already relies on.

**Both fixes converge on one function.** Fix B calls the same `spawnChildren` that Fix A hardens and that `preImplement`/`recoverMissingPlanComment` already use — three callers, one implementation. This is what makes a Review/Validate spawn "not a second-class path": there is no parallel implementation of board registration, assignment, or `blocked_by` linking to drift out of sync.

On success, a present-tense receipt note (`formatMidflightSpawnReceiptNote`) is prepended to `output` — a sibling of Plan's deferred-tense `formatSpawnReceiptNote`, since these children already exist by the time the note is rendered, unlike a Plan declaration that defers creation to Implement. The raw declaration block(s) are also stripped from `output` at this point (`stripSpawnBlocks`, mirroring the existing `stripMarkers` call for `FABRIK_PR_CREATE`/`FABRIK_ISSUE_UPDATE`) — unlike Plan's own comment, which intentionally keeps its raw block visible ("N sub-issues declared above"), the mid-flight note is self-contained and already names what was spawned, so an un-stripped block would just duplicate that information verbatim in the posted PR/issue comment. `stripSpawnBlocks` shares `ParseSpawnBlocks`'s line-scanning, so it removes exactly the blocks that were actually parsed and spawned, leaves a malformed block visible, and handles multiple blocks in one dispatch correctly. On failure, `spawnChildren` has already paused the parent and posted its own comment; `finalizeStageOutcome` releases the lock and returns without posting this dispatch's own output, mirroring the `FABRIK_PR_CREATE` failure precedent exactly.

**A same-turn hazard at Validate, closed with a live dependency guard.** `attemptMergeOnValidate` (`engine/stages.go`) runs *before* `checkDependencies` is ever reached through the normal per-item admission path for that dispatch (`checkDependencies` only gates the `shouldAdvance` branch, which a successful auto-merge/direct-merge always short-circuits to `false`). Without a check of its own, a Validate output that both spawns a genuine blocker and signals `FABRIK_STAGE_COMPLETE` in the same turn could have had its PR merged before the freshly-created dependency edge was ever consulted — the same silent-merge-past-a-blocker danger this ADR otherwise closes, recreated one dispatch earlier. Rather than leaving this to prompt-level discipline alone, `attemptMergeOnValidate` now re-fetches the item live (`FetchItemDetails`, mirroring the pattern its own direct-merge fallback already used for review threads) immediately after the `fabrik:auto-merge-enabled` idempotency check, and calls `checkDependencies` against the fresh snapshot before any landing decision. A same-dispatch edge is caught: `fabrik:blocked` is applied, the usual blocked comment is posted, and no merge/enable-auto-merge/enqueue call is ever made. Both callers of `attemptMergeOnValidate` already gate on yolo and skip once `fabrik:auto-merge-enabled` is set, so the extra live fetch is bounded to the same narrow window guard 1 already re-fetches live data in. `fabrik-validate/SKILL.md`'s "If You Discover a Blocking Issue" section still instructs the agent not to complete alongside a genuine spawn — consistent with Validate's own "unmet requirement → BLOCKED" completion criteria — but this is now a defense-in-depth layer, not the only thing standing between a misbehaving agent and a premature merge. See `docs/state-machine.md` §6.7.2 for the full mechanics and the reasoning for why Review carries no equivalent hazard (its next-stage dispatch is gated by `checkDependencies`).

### Fix C: widen the resume-from-blocked live re-read

`checkDependencies`'s existing live re-read (added for #977) fired only `if alreadyBlocked && len(item.BlockedBy) > 0`. A bypassed spawn's cached `BlockedBy` is always empty — exactly the shape that condition skipped, trusting the wrong empty list and clearing `fabrik:blocked` on nothing. Dropping the `len(...) > 0` half of the condition — firing on `alreadyBlocked` alone — closes this without new machinery: the live read now runs whenever an item resumes from `fabrik:blocked`, regardless of what the cache currently claims.

This is the requirement most directly aimed at the dangerous silent-resume case: a parent must re-verify its dependency set is *actually* satisfied on resume, not merely that the cached count says so. With Fix B in place, a genuine spawn is unlikely to ever produce a stale-empty-cache-while-blocked state again — but Fix C is deliberately independent of Fix B, since it also covers any other future or historical path that could produce the same cache shape, and costs one extra API call per currently-blocked item per eligible poll, already bounded by the existing `dep-blocked` cooldown.

## Rationale

### Why not build cross-instance board discovery?

The originating issue scoped this out explicitly: "General auto-discovery of 'which board serves which repo' beyond what is already declared in instance config" is out of scope. A discovery mechanism would need either a shared registry (new infrastructure, new failure modes, new config) or runtime probing across instances Fabrik has no channel to reach. A scope-check against config the instance already has is implementable with zero new infrastructure and turns a silent wrong-board registration into a loud, actionable failure — which is what the acceptance criteria actually required.

### Why is board-servability a whole-batch failure, not per-block?

`validateSpawnDependsOn` already validates every block before any mutation and fails the whole batch together on the first violation. Extending that same upfront pass with the scope-check, and failing the whole batch on the first unservable target, keeps one failure semantics for "problem detected before any GitHub mutation" rather than two different partial-failure shapes depending on which upfront check tripped.

### Why fold assignment into `CreateIssue` rather than a separate REST call?

A separate `AssignIssue` call would need its own fail-loud handling — the requirement is "any failure... must surface visibly," and every additional mutation is another place that requirement has to be independently re-satisfied. A single combined POST keeps the number of new failure branches at zero: assignment failure and creation failure are indistinguishable from the caller's perspective, and both already fail loud through the existing `CreateIssue`-error path.

### Why not extend the comment-processing path (`processComments`) too?

Neither reproduction involved a comment cycle, and that path has a structurally different output-handling shape from the primary stage-invocation path this ADR touches. Extending it is a candidate follow-up if a gap is observed there, not folded into this change — consistent with fixing the two failure modes that were actually reproduced, not every theoretically-similar surface.

### Why is `spawnChildren`'s return signature `([]string, bool, error)` now, not just `(bool, error)`?

The Review/Validate call site needs the spawned-identifier list to render `formatMidflightSpawnReceiptNote`; `preImplement`'s Plan-driven call site never needed it, since Plan's own receipt note (`formatSpawnReceiptNote`) is rendered independently, at Plan-comment-posting time, by re-parsing the comment body rather than consuming a return value. Adding the list to the shared function's return, rather than threading it through only the new call site, keeps `spawnChildren` a single source of truth for "what actually got spawned this call," reusable by any future caller that needs it.

## Consequences

- A `repo:`-scoped instance now visibly refuses (rather than silently mis-registers) a spawn targeting a repo it doesn't serve — operators running split multi-instance topologies (one instance per repo) will see this the first time a Plan/Review/Validate spawn targets the "wrong" instance, and must either repoint that instance's `repo:`/`project:` config or run the spawn from a correctly-scoped instance.
- Every spawned child now has an assignee — a previously-silent gap closed unconditionally, with no opt-out. A misconfigured `user:` now makes every spawn fail loud rather than producing a silently-unassigned child.
- Review and Validate skills (`plugin/fabrik-workflows/skills/fabrik-review/SKILL.md`, `.../fabrik-validate/SKILL.md`) now document the sanctioned `FABRIK_SPAWN_CHILD_BEGIN/END` route and instruct against `gh issue create`. This is prompt-level steering, not a tool restriction (removing `gh` access was explicitly ruled out of scope) — whether agents actually adopt the declared route over the path-of-least-resistance `gh issue create` is not unit-testable and can only be confirmed by e2e or live observation.
- `checkDependencies` now performs one additional live `FetchItemDetails` call per currently-blocked item, once per `dep-blocked` cooldown window, regardless of the cached `BlockedBy` list's size — a small, already-bounded cost increase in exchange for closing the silent-resume hazard.
- `attemptMergeOnValidate` now performs one additional live `FetchItemDetails` call on every invocation that reaches past its idempotency guards (i.e., yolo-active, not yet `fabrik:auto-merge-enabled`) — bounded to the same narrow per-Validate-completion window its own guard 1 and direct-merge fallback already re-fetch live data in, not a new per-poll cost for items already converging toward merge.
