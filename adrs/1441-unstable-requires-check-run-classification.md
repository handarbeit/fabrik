# ADR 1441: `unstable` No Longer Shortcuts the CI Advance Gate — Both Known Call Sites

**Date**: 2026-08-09
**Status**: Accepted
**Issue**: #1441 — CI gate: `mergeable_state=unstable` clears `wait_for_ci`, so non-required checks never gate an advance

## Context

ADR-033 established that `mergeable_state ∈ {clean, unstable}` short-circuits `settlePRMergeState`/`checkCIGate` (the single-PR CI advance gate) straight to "ready," skipping per-check classification entirely. The stated rationale was that GitHub's own branch protection already decided the PR is mergeable, so replicating that logic via raw check-run analysis is redundant and error-prone.

This conflated two different `mergeable_state` values that ADR-033's own table already documented as meaning different things:

| Value | Meaning |
|-------|---------|
| `clean` | All checks — required or not — have completed successfully |
| `unstable` | Required checks satisfied, but a **non-required check is failing or still pending** |

`clean` is a genuine "nothing to see here" signal. `unstable` is not — it means something concluded differently than green, and Fabrik has no way to tell whether that something is the repository's entire test suite (as on `handarbeit/fabrik`, where only `Analyze (go)` and the llms-full.txt drift check are required on `main`) or a truly cosmetic job. Treating the two identically meant the CI gate could clear — and Validate could complete, and a merge-train batch could accept a member — while a concluded `failure` sat on the PR's head SHA, as long as branch protection didn't happen to mark that specific check required.

This was observed live on **#1428 / PR #1429** (2026-08-08): `Test and vet` (the check that runs `go test ./...` and `go vet ./...`) failed on three successive heads, each concluding well before the gate was evaluated. `fabrik:awaiting-ci` engaged for two seconds, then cleared as if CI were green, because `mergeable_state` read `unstable` throughout. #1428 then advanced into the merge-train's Queued column with a red PR, where it was ejected three times with a diagnosis that pointed away from the real cause (a separate, companion defect).

**The identical shortcut existed in two places.** `engine/merge_train.go`'s `pollForMergeable` (the merge-train *landing* step, distinct from `pollTrainCI`, the train's *CI-poll* step) contained the same `gh.MergeableStateAccepted(mergeableState) → return true` predicate, with **no check-run fallback at all** — not even the partial classification `settlePRMergeState` already had for other `mergeable_state` values. ADR-1153 (#1153, 2026-07-27) already fixed this exact defect class for `pollTrainCI`, and its own "Alternatives Considered" section explicitly flagged `pollForMergeable` as a "candidate fast-follow" — but no follow-up issue was ever filed. Left unfixed, the engine would reach opposite conclusions about the same PR's CI health depending on whether the merge train was on for that repo.

### Why the "file a follow-up" path was rejected for `pollForMergeable`

The obvious alternative — fix `settlePRMergeState` now, file a separate issue for `pollForMergeable` — was considered and explicitly rejected during Specify, for one decisive reason: **it had already been tried, for this exact code, and failed.** ADR-1153 flagged `pollForMergeable` as a fast-follow candidate and no issue was ever created for it. That is not a hypothetical risk of deferring; it is the observed outcome of deferring, for this specific shortcut. #1460 (filed the same day as this issue) independently documents the general cost of this pattern: a single defect reached five call sites because each was fixed in isolation without a sweep for siblings, and one fixed instance was later cited as *design precedent* for a sixth, unrelated site. Both call sites are fixed in this change.

## Decision

### R1/R2/R5 — `settlePRMergeState`'s shortcut narrows to `clean` only

`engine/pr_settle.go`, the ADR-033 shortcut:

```go
if mergeableState == "clean" {
    return PRSettleResult{Status: PRMergeReady, ...}
}
```

`unstable` no longer appears in this condition. It falls through, unmodified, into the classification chain that already exists below the shortcut and is already exercised today by every other non-accepted `mergeable_state` value (`"blocked"`, `""`, etc.):

1. **ADR-933's required-context classifier** (`classifyRequiredContexts`) runs first — it can catch a confirmed failure on a classic commit status with no check-run footprint at all, which check-run classification alone would never see.
2. **`gh.ClassifyCheckRuns`** classifies the observed check runs: a confirmed `failure`/`timed_out`/`action_required` conclusion → `PRMergeBlocked` (CI failure, `fabrik:awaiting-ci` applied, CI-fix reinvoke dispatched); a check still `queued`/`in_progress` → `PRMergeUnsettled` (blocked-not-failed, re-evaluated next poll under the existing `ciWaitTimeout`/`ciBackstopTimeout` liveness-dwell backstop from ADR-1410); `skipped`/`neutral`/`cancelled` conclusions never count as failures (R5 — this is `gh.ClassifyCheckRuns`'s existing behavior, unchanged).
3. **Zero check runs** (e.g. a repo whose CI is entirely classic-commit-status-based) falls back to the pre-existing ADR-933 zero-check-runs branch, which is unconditionally reached for `unstable` after this change exactly as it already is for `"blocked"` — `settlePRMergeState`'s `hadChecks`/post-push-dwell handling sits entirely below the shortcut and required no changes.

`checkCIGate` (`engine/ci.go`) and `checkMergeabilityGate` (`engine/merge_gate.go`) needed **no code changes**: both already interpret `PRMergeReady`/`PRMergeBlocked`/`PRMergeUnsettled` correctly regardless of *why* `settlePRMergeState` produced that status. This is why the fix is narrow — a single classification input changed, and both downstream gates fall into already-correct, already-tested paths for free. Only their doc comments, which described the old shortcut's scope, needed correcting.

A behavior change worth naming explicitly, beyond the headline "failing check now blocks": a PR with `unstable` and **literally zero check runs** now sits blocked (as `PRMergeUnsettled`, via the R3/branch-protection-signal branch) for up to `ciWaitTimeout` before the existing dwell logic concludes "no CI configured," rather than clearing instantly. This is strictly more conservative — it is the exact same treatment `"blocked"` already received before this change — but it is a real, if narrow, behavioral shift for that specific shape of PR.

### R3 — The merge decision is a deliberate, separate policy choice: unchanged

The shortcut is consumed by more than the CI advance gate. `MergePR`'s own ADR-072 self-gate (`github/prs.go`) and `checkMergeabilityGate`'s conflict-only concern are **not** touched by this change — they keep deferring to `gh.MergeableStateAccepted({clean, unstable})`, per ADR-072's own explicit operator note:

> "whether a given check actually refuses an engine merge is controlled by the repository's required status checks list in branch protection, not by this allowlist. If an operator wants a different check to gate engine merges, it belongs in branch protection's required-checks list, not in `MergePR`."

This is a standing operator decision from ADR-072, not a fresh inference made for this issue. It resolves cleanly with no observable divergence: whenever `wait_for_ci: true`, the (now-tightened) advance gate sits strictly upstream of `MergePR` in the control flow, so an item can no longer reach the merge call with a known-red non-required check in that mode — the merge path's code is unchanged, but it is never exercised with the input this issue used to allow through. `wait_for_ci: false` stages (which skip the CI gate by design) are unaffected by this issue in either direction, as before.

Test coverage pins this split explicitly (AC5): the advance-gate behavior (`checkCIGate`) and the merge-path behavior (`MergeableStateAccepted`, `MergePR`'s self-gate) are asserted independently, so a future change cannot make them silently disagree.

### R4 — Degenerate-coverage warning: log-only, config-vs-observed, not a branch-protection read

ADR-933 already established, twice, that a live read of GitHub's branch protection API is not viable — `GET /repos/{owner}/{repo}/branches/{branch}/protection` returns 403 for non-admin classic PATs, and Fabrik documents only `repo` scope as required. `RequiredStatusContexts` (`config/config.go`, ADR-933's opt-in per-`owner/repo` config) is therefore Fabrik's *only* notion of "required check" — R4's warning is necessarily a comparison against that config, not against GitHub's actual branch protection.

`warnIfCIGateCoverageDegenerate` (`engine/ci.go`) fires from `checkCIGate`, gated on `len(settle.CheckRuns) > 0` (data already fetched this settle pass — no new API call), whenever `RequiredStatusContexts[owner/repo]` is empty, or none of its entries match a check name actually observed on the PR. Deduplicated per `"owner/repo|stage"` via a new `ciGateCoverageWarnedSet sync.Map` on `Engine`, mirroring the existing `baseBranchWarnedSet` precedent — logs once per process lifetime per repo/stage, not every poll; re-warns after a restart (acceptable, arguably desirable: an operator who just restarted wants visibility, not silence).

**Known, accepted limitation**: gating on `len(settle.CheckRuns) > 0` means this warning never fires for a repo whose CI is 100% classic-commit-status-based with zero check runs. Closing this gap would require an unconditional extra `FetchCombinedStatus` call purely to power a warning — not worth the added API cost for a config-visibility nicety. Since this fix's strict-failure policy (below) does not depend on the required-context distinction anyway, the gap does not weaken the actual gate, only the warning's coverage.

### R5 — Strict policy: any confirmed check-run failure blocks, required or not

This issue makes the same choice ADR-1153 already made for `pollTrainCI`, reached independently for the single-PR path: Fabrik has no general way to distinguish "confirmed non-required failure" from "confirmed required-but-unconfigured failure" beyond the opt-in `RequiredStatusContexts` config, and for the common **unconfigured** case (`handarbeit/fabrik` itself has no entries) that distinction cannot be made at all — a permissive policy would have to default to strict anyway for the repo that motivated this issue, buying nothing. `gh.ClassifyCheckRuns` already implements this correctly (only `failure`/`timed_out`/`action_required` block; `skipped`/`neutral`/`cancelled` do not) and required no changes — R5's "do not regress ADR-033's original motivation" concern (the #716/#717 incident that caused a genuinely non-blocking `Cleanup artifacts` job to stall an advance) is about a *pending* check blocking indefinitely, which the liveness-dwell backstop (ADR-1410) already governs independently of this change, not about a *concluded* failure being tolerated.

### R6 — `pollForMergeable` fixed in this issue via a new composition function, not a literal shared function with `pollTrainCI`

`engine/merge_train.go` gains `classifyLandingCI`, a new function that mirrors `pollTrainCI`'s existing per-iteration composition — dirty → red (checked by the caller before this is reached); a confirmed check-run failure or required-context failure → red, unconditionally; a check-run still pending → pending, keep polling; zero check runs → the ADR-933 fallback (`mergeableAccepted && RequiredContextsSatisfied → green`) — built from the same shared primitives `pollTrainCI` already uses: `gh.ClassifyCheckRuns`, `e.classifyRequiredContexts`, `describeCheckRuns`, `gh.MergeableStateAccepted`. It reuses `TrainCIResult` (`TrainCIGreen`/`TrainCIRed`/`TrainCIPending`) as its verdict type rather than introducing a parallel enum, since the three outcomes mean exactly the same thing in both places.

`pollForMergeable` is rewritten to call `e.client.FetchPRDetails` (replacing `FetchPRMergeableFields`) each iteration — needed to get `HeadSHA` alongside `MergeableState` in the same single REST call, at the same API cost as before — and to fetch and classify check runs via `classifyLandingCI` instead of the bare `gh.MergeableStateAccepted` check.

**Why not one function literally shared with `pollTrainCI`?** `pollTrainCI` is explicitly out of scope for this issue — already fixed by ADR-1153 — and refactoring it to call a new shared function would mean editing out-of-scope code for a cosmetic DRY gain, with no behavioral difference to show for it (the two functions' compositions are already, and remain, semantically identical). This is R6's own escape-hatch clause exercised on its own terms: "one shared mechanism where possible" is satisfied at the level of shared *classification primitives*, composed once into `classifyLandingCI` and reused by `pollForMergeable`, while `pollTrainCI`'s own inline composition (which happens to be structurally identical) is left untouched. The reason is scope, not technical infeasibility — a future unification, if ever wanted, is a pure refactor with no behavior change, since both are built from the same primitives in the same order today.

This resolves R6's stated condition for diverging from "fix both via one mechanism": the reason is recorded here explicitly, and — critically — **both call sites are fixed in this same change**, which is the actual requirement R6 imposes; it is the *literal-function-sharing* question, not the *both-sites-fixed* question, where this ADR diverges from a maximally literal reading.

## Alternatives Considered

### Read GitHub's branch protection API to distinguish required from non-required checks

Rejected, for the third time in this codebase's history (ADR-033 rejected it originally; ADR-933 confirmed the rejection with a cited real-world 403 for non-admin classic PATs). No new information changes this conclusion.

### Tolerate `unstable` + confirmed non-required failure (permissive policy)

Rejected for the same reason ADR-1153 rejected it for `pollTrainCI`: without a live branch-protection read, Fabrik can only distinguish required from non-required via the opt-in `RequiredStatusContexts` config, which is absent on the repo that motivated this issue. A permissive policy would have to default to strict for the unconfigured case anyway, so it buys nothing while adding classification complexity and re-opening the exact silent-pass failure mode this issue exists to close.

### Defer `pollForMergeable`'s fix to a follow-up issue

Rejected — see "Why the 'file a follow-up' path was rejected" above. This is not a hypothetical; it is the observed outcome the last time this exact deferral was tried (ADR-1153).

### Literally share one function between `pollForMergeable` and `pollTrainCI`

Considered, rejected in favor of the composition-function approach above: `pollTrainCI` is out of scope for this issue, and touching it for a cosmetic refactor risks regressing an already-correct, already-tested function for no behavioral gain.

## Consequences

**Positive:**
- Closes the #1428/#1429 failure mode: a confirmed `failure` check run on the head SHA can no longer clear the CI advance gate merely because `mergeable_state` reads `unstable`.
- Both known instances of this shortcut are fixed together, closing the specific gap ADR-1153 identified and left open.
- `classifyCIFromRequiredContexts`/`classifyCIFromCheckRuns` (the CI gate's existing classification chain) and `classifyLandingCI` (the new landing-path classifier) now agree on every input — the engine can no longer reach opposite conclusions about the same PR's CI health depending on whether the merge train is enabled for that repo.
- A degenerate `wait_for_ci: true` configuration (no coverage from `RequiredStatusContexts`) is now visible in the logs instead of silent.
- `clean`'s fast path (no check-run round-trip) is unchanged and still tested.

**Negative / Trade-offs:**
- One additional `FetchCheckRuns` round-trip for the common `unstable` case in `settlePRMergeState`, and one in `pollForMergeable`. Both immaterial — the single-PR path polls far less frequently than the merge-train's 30-second interval, which ADR-1153 already accepted this exact cost for.
- A PR with `unstable` and zero check runs now sits blocked for up to `ciWaitTimeout` before resolving to "no CI configured," instead of clearing instantly — narrow, but a real behavior change beyond the headline fix.
- R4's warning has a known gap for classic-commit-status-only repos with zero check runs (see R4 above) — accepted rather than paying for an unconditional extra API call.

## Predecessor Context

- **ADR-033** (`033-mergeable-state-over-check-runs.md`, #716/#717): the shortcut this issue narrows. Its Prong 2 (`checkCIGate`) description is superseded for `unstable` by this ADR; Prong 1's framing (mergeable_state consulted before per-check classification) is preserved for `clean`. Amended with a short pointer to this ADR rather than rewritten in place, preserving its own historical Decision record — mirroring how ADR-1153 amended it by addition rather than edit for `pollTrainCI`.
- **ADR-933** (`933-required-status-context-config.md`, #933): added `classifyRequiredContexts`/`gh.ClassifyRequiredContexts` and explicitly declined to touch the ADR-033 shortcut itself ("reopening that decision is explicitly out of scope for #933"). This issue is the one that reopens it for the single-PR path — the direct analogue of how ADR-1153 reopened it for `pollTrainCI`. ADR-933's zero-check-runs branch and required-context classifier are reused unmodified throughout.
- **ADR-1153** (`1153-train-ci-completeness-over-mergeable-state.md`, #1153): the direct predecessor and template. Its Strict policy (§4) is adopted here independently for the single-PR path (R5); its explicit flagging of `pollForMergeable` as an unfixed "candidate fast-follow" is exactly what R6 closes in this same change, rather than repeating the deferral that left it open.
- **ADR-072** (`072-mergepr-self-gates-on-mergeable-state.md`, #1094): established `MergeableStateAccepted({clean, unstable})` as `MergePR`'s own self-gate and the operator note this issue's R3 decision rests on. Not revisited —`MergePR` and the merge path stay exactly as ADR-072 left them.
- **ADR-1410** (`1410-ci-gate-liveness-not-elapsed-time.md`): defines the `ciWaitTimeout`/`ciBackstopTimeout`/`ciProgressStalledSince` liveness-dwell machinery that governs a merely-*pending* check under this ADR's classification — unchanged, and the mechanism R2's "do not let a false pass become a 30-minute stall" concern already resolves to.
- **#1460** (filed 2026-08-09, the same day): documents the general cost of deferring a fix to one of several identical call sites — direct motivation for R6's "fix both now" default, cited above.
