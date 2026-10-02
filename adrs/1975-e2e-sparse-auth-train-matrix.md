# ADR 1975: A sparse auth × train matrix for the live e2e gate — explicit per-test sensitivity, one baseline cell, focused auth tests

## Status

Accepted (#1975). Builds on ADR-1933 (the per-test registry the new fields extend), ADR-1972 (the ledger, whose required set follows the plan), ADR-1994 (the `PlanCells`/`Scheduler` seams) and ADR-1846/ADR-1893 (what auth mode changes). **ADR-1454 is unchanged:** every live scenario still runs live before every release. Nothing here moves coverage to the sim. What narrows is only *which cells* each test runs in.

## Context

The 0.0.83 gate ran every live test in all four legs of auth (`pat|app`) × train (`off|on`): 44 behaviours, 176 executions. Most of those executions re-proved something already proven in another leg, and the cost was real — wall-clock, and GraphQL spend on the PAT identity (one full leg ≈ 4,000 points of arbeithand's 5,000/h bucket, #1678).

The two axes change much less than the matrix assumes.

**Auth mode changes three things.**
1. *Access.* The token source and its rotation, the git credential helper for pushes (ADR-1846), the `GH_TOKEN` handed to workers, and which rate-limit budget is charged.
2. *Identity.* Who "we" are: self-recognition (a Fabrik comment never lifts a pause, own reviews are suppressed, the blocked comment is edited in place), the lock-label shape (ADR-1893), the @mention and assignee targets, commit identity and PR authorship (which limits what the review gate can request).
3. *Permissions.* A PAT carries the user's scopes; an installation token carries only the App's grants.

**Train mode changes only the landing path:** the Queued holding stage, batch assembly, trial branches, bisection, the singleton fast path, integration PRs and the train settle scans. Under train `off`, landing is a direct merge on Validate. Everything up to the landing decision is shared: dispatch, markers, comment processing, pause/resume, the CI/review/feedback gates, spawning, breakers.

So most tests are neutral on at least one axis, and many on both. A second problem was how auth got tested: an identity check usually sat inside a long pipeline scenario, so making that scenario "auth-sensitive" would run a whole pipeline in the other auth mode to re-check one assertion.

## Decision

### 1. Two explicit sensitivity fields, no default

`registry.Entry` (ADR-1933) gains `auth` and `train`, each `sensitive` or `neutral`, plus `auth_reason`/`train_reason`. The registry completeness test (plain `go test ./...`) fails for a live test with a missing or other value, for a `sensitive` axis without a one-line reason, and for a reason on a `neutral` axis. Storing the reason in the registry makes "a reason for every sensitive mark" machine-checked rather than a PR-description promise.

Two rules pin the identity-bearing tests so a misclassification cannot hide:
- any test whose name contains `SelfRecognition` must be `auth: sensitive`;
- any test that reaches `AssertPRAuthorIsExpectedIdentity` or `AssertPRAuthorIsEngineIdentity` must be `auth: sensitive`. `registry.ScanIdentityAssertCallers` finds direct calls and takes the transitive closure over same-directory non-test functions by name, so a helper cannot hide the assertion from the rule.

`auth: sensitive` covers anything in the three categories above. `train: sensitive` covers any test whose subject is, or whose run ends in, the landing path — which includes every yolo scenario that waits for the issue to close.

### 2. The sparse plan

One **baseline** cell runs every live test; each other cell runs only the tests sensitive to whatever it changes:

| Cell | Runs |
|---|---|
| `app/on` (baseline) | every live test |
| `app/off` (other train, same auth) | `train: sensitive` |
| `pat/on` (other auth, same train) | `auth: sensitive` |
| `pat/off` (diagonal) | sensitive on **both** axes |

A test therefore runs once, plus once per axis it is sensitive to. Cells are planned baseline-first and grouped by auth (the scheduler's auth banner assumes grouping).

`PlanCells` takes an optional `SparseInput{Live, Entries}`; `nil` plans today's full matrix byte-for-byte. Narrowed cells carry one anchored `-run ^(…)$`; the baseline keeps `-skip TrainIsolatedRE` plus the isolated cell, and a narrowed cell gets an isolated cell only when the isolated scenario is in its selection. An empty cell is dropped, which also skips its bed restart. A caller `-run` is intersected with each cell's selection (a subtest filter survives on the narrowed `-run`); `E2E_AUTH_MODE`/`E2E_TRAIN_MODE` filter the finished plan, so `E2E_AUTH_MODE=pat` or `E2E_TRAIN_MODE=off` omit the baseline and are **partial runs** (the ledger keeps them incomplete).

The (test, cell) pairs a test's `skip_ok_legs` marks as structurally skipped are pruned from every non-baseline cell: otherwise `app/off` would be mostly merge-train self-skips and `pat/on` would restart the bed to watch the App-only tests skip. `skip_ok_legs` is therefore *complementary* to the sensitivity fields, not subsumed by them (ADR-1972 predicted otherwise): sensitivity says where a test should run, `skip_ok_legs` says where a skip is expected. A test with no registry entry is planned in every cell — the safe failure; the completeness test makes it unreachable.

The plan is the **single source of the required set.** `Gate.run` and `gate coverage` build their `PlanInput` through one function, and the ledger's required set is `RequiredTests(PlanCells(…))` with no caller arguments, so the ledger, `--resume` and the coverage report need no logic change and the gate cannot require pairs it never runs. The registry is loaded whether or not the ledger is enabled.

### 3. Baseline: `app/on`

App auth is where the engine is heading (the dev daemon is moving to its own App; ADR-1893 makes App identity first-class). It charges the bed App's own installation bucket rather than arbeithand's PAT, so the PAT cells shrink to the auth-sensitive set — which largely settles #1678's arithmetic. Train `on` is what Fantasy and the community deployment run. #1972's archived measurements may justify a different baseline; changing it is one pair of constants (`baselineAuth`/`baselineTrain`).

### 4. Focused auth tests

The auth-sensitive set is kept small by extracting identity and permission assertions out of long pipeline scenarios into tests that seed the minimum state:

| Focused test | Source scenario(s) |
|---|---|
| `TestAuthEnginePRAuthorIdentity` | the `AssertPRAuthorIsEngineIdentity` call in `TestConjunctiveCIReviewGate` |
| `TestAuthReviewGateIdentity` | the ten `AssertPRAuthorIsExpectedIdentity` calls in `TestReviewAuthority*` and `TestExpectedReviewers*` |
| `TestAuthLockLabelShape` | new — the lock-label shape per mode (ADR-1893) |
| `TestPATSelfRecognition{OwnMarkedCommentNeverResumes,BlockedCommentUpdatedInPlace,DurableReviewSuppression}` | PAT counterparts of the three `TestAppSelfRecognition*` cases (the A2/A3 bodies are shared) |

The originals are marked `auth: neutral` only because the call is gone from them. The removed `AssertPRAuthorIsExpectedIdentity` calls checked a property that is true *by construction* for harness-seeded PRs (the harness account authors them in every mode); the real #925 guard — the reviewer is not the PR author, which is what breaks `RequestPRReviewer` — stays at each site that requests a reviewer.

Honest limits of the PAT counterparts: under a PAT the engine *is* the harness account, so a marker-free comment from it reads as human in both modes (`filterHuman` has no `cfg.User` comparison). PAT A2 and A3 discriminate `selfLogin()`; the PAT form of A1 guards the `🏭 **Fabrik` prefix exclusion under a User-typed identity and does **not**. Their non-vacuity against a neutralised self-login check can only be shown on a live PAT leg.

### 5. `E2E_MATRIX`

`sparse` (default) or `full`; any other value is rejected at startup (`ExitUsage`) rather than silently falling back. `full` restores the four complete legs. A `full` ledger is a superset of a sparse one (ledger keys are `(leg, test)`), so it satisfies a later sparse `gate coverage`. The coverage summary and the release-notes line print the mode and the required-pair count against the full 2×2.

## Accepted risk

A real auth × train **interaction** in a test marked `neutral` on either axis goes undetected by the default gate. Mitigations, in order of strength:

- classification is explicit per test with a recorded reason, so the decision is reviewed rather than defaulted;
- credentialed merge-train pushes are sensitive on **both** axes: `TestMergeTrainHappyPathLanding` is `auth: sensitive` and runs in `pat/on`, and `TestSmokeSingleRepoFullPipeline` runs in all four cells. *This narrows the issue's wording:* only the HappyPath train test carries `auth: sensitive`, because the trial-branch push, integration-PR and landing code is the same in every train test, and marking all ten would put the whole train suite in the PAT cell and spend the PAT budget #1678 is about;
- `E2E_MATRIX=full`.

Nothing enforces that a classification *stays* right as the engine changes. The two name/caller rules above catch the identity-bearing cases mechanically; the rest rely on review.

## When to use `E2E_MATRIX=full`

For a release that changes the auth layer (App auth, token handling, credentials, identity), the landing path (merge train, auto-merge, the landing gates), or this classification itself (any edit to the `auth`/`train` fields). `scripts/cut-release.sh` is unchanged — it asks `gate coverage` under the default environment, so a sparse ledger satisfies it — which is why the instruction lives here and the release-notes line records the matrix mode: the release author, not the script, decides.

## Consequences

- The default gate requires 76 (test, leg) pairs over the 50 live tests (the 44 existing plus the 6 added here), against 200 for a full 2×2 over the same tests (176 for the original 44): baseline 50, `app/off` 12, `pat/on` 11, `pat/off` 3. The count is recomputed from the registry on every run and printed in the coverage summary.
- New live tests must declare both fields. A new test that calls an author-identity assertion is forced `auth: sensitive`.
- The operator-measured numbers — the auth-sensitive set's runtime as a fraction of the baseline, and executions, wall-clock and per-identity GraphQL spend against 0.0.83 — need a live bed and are reported on the PR as pending the first sparse gate run.
