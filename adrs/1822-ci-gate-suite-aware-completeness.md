# ADR 1822: CI gate completeness is judged over check suites, not only check runs

## Status

Accepted (#1822).

## Context

On 2026-09-20, two issues on `verveguy/concept-maps` reached `Queued` with their own PR CI red. In both, the `wait_for_ci` gate cleared 11–66 seconds after `fabrik:awaiting-ci` was applied, while a `needs:`-dependent job (`E2E (Playwright)`) sat in the Actions queue for 7–18 minutes and then failed. At the moment the gate cleared, every check run present on the SHA was `completed/success`.

The gate's judgement was correct over the data it could see. A check run exists only once its job has been scheduled onto a runner, so a queued job has none, and completeness was being evaluated over *the set that happens to exist at read time*. GitHub also reports `mergeable_state: clean` whenever no *required* check is outstanding, so the `clean` shortcut (rule 9 of `settlePRMergeState`, ADR-033) cleared on the same partial view without reading check runs at all. ADR-033's premise that `clean` means "every check passed" is false for unscheduled jobs.

Both issues carried `fabrik:yolo`. Without the merge train the same race would have auto-merged a PR whose end-to-end job never ran, with no later signal.

The check **suite** is created at queue time and stays non-`completed` until every job has run (in the incident: `app=github-actions runs=5`, `completed` twelve minutes after the gate cleared). Installed-but-inert Apps (`cursor`, `claude`) sit `queued` with zero runs for hours.

## Decision

1. **A separate suite primitive.** `github.FetchCheckSuites` (paginated, `total_count`-verified, an incomplete set is an error — the #1539 shape) and the pure `github.OutstandingCheckSuites`. `ClassifyCheckRuns` and the "green and complete" classifier (ADR-1153, ADR-1441) are unchanged; the suite signal is combined at each call site through one engine helper, `ciSuiteHold`, so the gates cannot disagree on one SHA.

2. **Discriminator.** A suite is outstanding when `status != "completed"` and either `latest_check_runs_count > 0` or (zero runs and younger than the post-push dwell, measured from the suite's `created_at`). An old run-less non-completed suite is inert and ignored. The naive "every suite must be completed" rule is rejected: it deadlocks permanently on inert Apps. The rule is "any suite", never "the suite" — GitHub Actions creates one suite per workflow run (confirmed against a captured response, `github/testdata/recordings/fetch_check_suites.json`, which shows four `github-actions` suites and three inert `queued`/0-run App suites on one SHA).

3. **Post-push mitigation (Requirement 4).** A combination, requiring no new permission:
   - the suite rule, for the mid-run gap (the incident);
   - an age-bounded zero-run rule (a variant of option (b)) reusing `PostPushDwell` (default 90s) but anchored on GitHub's `created_at`, so — unlike the in-memory `LastHeadSHAUpdate` behind rule 15 — it survives an engine restart;
   - a scoped form of option (a): a suite reporting runs contradicts an empty run set, so "no CI configured" (rule 18) holds. A repo with genuinely no CI has no outstanding suite and still clears. Literal (a) — always require one check run — was rejected because it would stop such repos ever clearing.

   A missing or unparseable `created_at` on a zero-run suite counts as young (hold): ambiguity holds, and the backstop bounds it.

4. **Option (c) rejected.** Reading the Actions workflow run needs `actions: read`, which the App is deliberately not granted (ADR-1756); adding it would trip the R3 startup grant refusal (ADR-1713) for existing installs and requires a manifest change. It is also Actions-only, leaving other providers exposed. `checks: read` covers suites, so nothing changes for App auth.

5. **Fail-safe.** A suite read error holds: `PRMergeUnsettled` for the advance gate, `TrainCIPending` for the train paths, "not eligible" for the singleton fast path.

6. **Verdict shape and bounding.** The advance-gate hold is `PRMergeUnsettled` with `MergeableState` omitted, preserving the invariant that keeps `checkCIGate`'s R3 pause from misfiring. `checkMergeabilityGate` claims it before `checkCIGate`, so `settleAwaitingCIScan`'s unconditional `CIBackstopTimeout` (ADR-1410) bounds it: a suite stuck `in_progress` escalates through `pauseForCITimeout` like a stuck check run. That comment now names the outstanding suite and app (one best-effort live read at escalation time). The train polls are bounded by their existing `CIBackstopTimeout` deadlines.

7. **Consulted last, only on a would-be-green path.** A confirmed check-run failure still returns Blocked/Red immediately, a pending run still holds, and an unsatisfied required context (ADR-933) is never masked by a suite hold. The extra read is therefore spent once per clear attempt plus once per poll while a suite is outstanding — the same cadence as the live check-run refresh in `settleAwaitingCIScan` — not on every poll of a red or pending SHA.

8. **Live read, no cache.** `check_suite` webhooks remain a documented no-op. `FetchCheckSuites` is a pass-through on `GitHubClient`, `boardcache.ReadClient`, `GitHubAdapter` and `CacheImpl`.

9. **Surfaces.** `settlePRMergeState` rules 9, 18 and 19; `classifyLandingCI` (both green returns — covering `pollForMergeable` and `singletonFastPathEligible`); and `pollTrainCI` (both green returns). Requirement 2 does not name `pollTrainCI`, but it has its own inline classifier and the same race on trial SHAs; leaving it out would let the gates disagree.

10. **Rule 9 with an empty `HeadSHA`** no longer takes the `clean` shortcut: no suite read is possible without a SHA, so it falls through to the existing "HeadSHA empty" Unsettled return.

## Consequences

- The incident sequence (green prefix, suite `in_progress`, failing check run later) holds the gate and then classifies red. Covered in the sim bed (`tests/sim/ci_suite_gate_test.go`), whose model (`simgh` check suites) deliberately leaves `deriveMergeableState` untouched so a green prefix still reads `clean`.
- One extra REST call per would-be-green evaluation, including every poll of a `clean` PR while its suite is open.
- **Ordinary yolo auto-merge on non-train instances** is protected transitively: `attemptMergeOnValidate` has no CI classification of its own, and `stage:Validate:complete` is conjunctive on the now suite-aware `wait_for_ci` gate. No new landing-time gate was added — it would need its own escalation owner to be bounded (`reviewGateBlocksLanding` relies on a separate Phase 1 owner for the same reason). Two limits remain: `MergePR`'s `unstable`-accepting self-gate is unchanged, and a Validate stage with `wait_for_ci: false` is out of scope.
- **Residual limitation:** no suite (or workflow-run) read can see a workflow that has not been *triggered* yet — a `workflow_run` chain, or one triggered by a later event. This window is inherent.
- A third-party suite that holds runs but never reaches `completed` holds the gate until the backstop and then pauses; the pause comment names it so an operator can tell.
- A member that goes red *after* admission to the merge train is not addressed here; that is #1821's batch-admission backstop, which is complementary.

## Supersedes / corrects

ADR-033's and §6.4 rule 9's wording that `mergeable_state == "clean"` means GitHub "has confirmed every check passed" is corrected: it means no *required* check is failing or pending.
