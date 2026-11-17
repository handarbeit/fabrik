# ADR 2593: A zero-check trial is held while member PRs show the repository runs CI

## Status

Accepted. Amends ADR 1153 (the statement that `mergeable_state` alone is the green basis when
a head has no check-run footprint), builds on ADR 933, 1441, 1822, 2052 and 2296.

## Context

Every zero-check-runs green path (`evalTrialCI`, `classifyLandingCI` via `settleAndMerge`, the
stage gate's `clean` shortcut) reads "no check runs, no workflow runs, no suites, accepted
`mergeable_state`, required contexts satisfied" as "this repository has no CI". During a
GitHub Actions delay in workflow creation (0.0.93 release gate, `fabrik-test-alpha#11365`:
no `pull_request` runs created for ~9 minutes) every one of those signals is absent for a
repository that does run CI, so the trial and then its landing were judged green and the
poisoned batch merged; the CI workflows were created 23 seconds after the merge and
`train-poison-guard` failed. The three members' own PRs had green check runs at admission:
the engine had positive evidence and did not consult it.

## Decisions

1. **Positive evidence: any batch member's own PR head carrying check runs.** Any run counts
   (cancelled, stale and non-Actions runs included, ADR 2296). It is read live and uncached
   (`FetchCheckRuns` at each member's head, stopping at the first member with runs), per
   decision, never from a formation-time value. No wider evidence (workflow files, history)
   is introduced.
2. **While such evidence exists, zero check runs on the trial or the landing head is
   pending, not green.** `evalTrialCI` returns `TrainCIPending` (undecided); `settleAndMerge`
   converts a would-be green zero-run verdict to its existing pending branch. Both are bounded
   by the existing `CIBackstopTimeout`; no timeout is added or changed.
3. **The rule is layered at the two callers, not inside `classifyLandingCI`.** The classifier
   has five callers; the fast path, admission, the catch-up wait and the CI status file keep
   byte-identical behaviour (ADR 1441's "one mechanism at the primitive level"). The landing
   re-judges independently of the trial because it reads the same PR a second time and the
   incident's merge was logged there.
4. **A failed evidence read holds.** If no member shows runs but a read failed, the evidence is
   unknown and the head is pending (and the reason is logged); never land unvalidated code on a
   repository that may run CI.
5. **The genuine no-CI repository is unchanged.** When every member read succeeds with zero
   runs the rule does not hold and the trial/landing lands on the zero-check path exactly as
   before (ADR 933, the #933 local-CI shape).
6. **Uncharged.** A hold is a pending evaluation: no trial failure, ejection, retry or pause is
   recorded for it. If the backstop is reached, the trial follows the ordinary
   pending-past-backstop handling unchanged (which includes that handling's existing runaway
   accounting) — FR-011 is bounded to the per-poll hold; changing the backstop path is out of
   scope. A landing that expires ends `outcomeBackstopExpired` (keepable, ADR 2297) with a
   reason saying CI had not started rather than "checks were still pending".
7. **Members are not persisted.** `trialCIState.members` is set by the caller before each
   evaluation (like `probe`) and is not part of the persisted record; a restart rebuilds it
   from the run record's member numbers.
8. **Members' formation heads are read.** The question is whether the repository runs
   check-run CI, and the formation head is the commit the trial contains; a fresh
   `FetchPRDetails` per member per poll was not worth doubling the read count.
9. **Stage gate (R3): the same reading through `HasHadChecks`.** `settlePRMergeState`'s `clean`
   shortcut for a `wait_for_ci` stage returns `PRMergeUnsettled` for a zero-run head whose
   item's PR has had check runs, matching what the classified path already returned for
   `blocked`/`unstable`; it records `PRChecksObserved` whenever it reads runs.
10. **Sim fidelity.** Trial PRs are drafts and simgh reports `draft` (never accepted), so a
    zero-check trial could not reach the green path in the sim and a "does not land" test
    would pass without the fix. `Sim.SetDraftMergeableState` (default unchanged; `dirty` still
    wins) models real GitHub's `clean` for the draft trial PR in the incident
    (`tests/sim/simgh/FIDELITY.md`).

## Audit of the zero-check paths (R3)

| Path | Outcome |
|---|---|
| Trial `evalTrialCI` | Rule applied (members from the caller) |
| Landing `settleAndMerge` (trial and singleton landing) | Rule applied |
| Stage `wait_for_ci` gate, `settlePRMergeState` `clean` shortcut | Rule applied via `HasHadChecks` |
| Stage gate, classified zero-run path | Already held on `HasHadChecks` (unchanged) |
| Singleton fast path | Already rejects zero runs before `classifyLandingCI`; no change |
| `admitTrainMembers` | A zero-run member is admitted (fail-open, ADR 1821); a zero-run member is not "evidence"; no change |
| `waitMemberCI` (catch-up) | Not a green path: zero runs is pending, then falls through to the trial, where the rule applies; no change |
| `ciStatusVerdictFor` | Already `none` for zero runs; no change |

## Residuals

- A repository whose members carry check runs but whose workflows never run on the
  `fabrik/merge-train/*` branches (a `branches:` filter) now waits the full `CIBackstopTimeout`
  per trial. Accepted by the spec; the hold line names the cause.
- A trial with some checks where one is stuck `in_progress`: a separate issue.
- The stage gate clearing on a partial set where only CodeQL's dynamic run exists
  (`TestLateCheckRunSuiteGate`): checks exist but are incomplete, which `HasHadChecks` does not
  address.
- `HasHadChecks` is memory-only; a PR first observed after a restart has no evidence.
- No live reproduction: the Actions delay cannot be induced on demand against the bed, so the
  live registry is unchanged and only the sim twin and unit tests prove the behaviour
  (ADR 1454; as ADR 2052).

## Test seams

`Engine.SetZeroCheckMemberEvidenceDisabledForTest` (trial and landing),
`Engine.SetStageGateHadChecksShortcutDisabledForTest` (stage gate); sim twin
`tests/sim/mergetrain_zero_check_evidence_test.go`; unit tests
`engine/train_zero_check_evidence_test.go` and `engine/pr_settle_infra_test.go`.
