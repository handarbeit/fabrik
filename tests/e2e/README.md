# Fabrik end-to-end tests

Scenario-driven integration tests for Fabrik. Each test drives a real Fabrik
instance (`~/dev/fabrik-test/`) against the real test repositories
(`handarbeit/fabrik-test-alpha`, `handarbeit/fabrik-test-beta`), files an
issue, and asserts on the resulting pipeline behaviour.

## What this is for

These tests catch regressions that escape `go test ./...` — bugs that only
manifest in real integration with GitHub, real Claude invocations, real
worktrees, and the real end-to-end stage pipeline. The category of bug:

- The `addBlockedBy` GraphQL mutation name (shipped broken in v0.0.66; fixed by #800)
- The pre-Implement spawn step failing on never-touched repos (#797, #803)
- The `Closes #N` getting absorbed into a nested code fence (#738)
- The CI gate `HeadSHA` resolution in poll-only mode (#779)

Every such regression that escapes a release earns a new scenario here.

## Where this lives in the release flow

```
[ go test ./... ]               unit tests; fast; run on every PR
        |
        v
[ tests/e2e/... ]               integration tests; slow; run before release
        |
        v
[ scripts/cut-release.sh ]      cuts a release
```

Suggested integration with `cut-release.sh` (not yet wired):

```bash
scripts/cut-release.sh v0.0.67                       # default — does NOT run e2e
scripts/cut-release.sh v0.0.67 --integration-check   # run e2e before tagging
scripts/cut-release.sh v0.0.67 --skip-integration    # explicit skip when iterating
```

We'll flip the default to `--integration-check` once the suite is stable.

## Test bed prerequisites

These tests assume:

1. **`~/dev/fabrik-test/` exists** with `.fabrik/config.yaml`, `.env`
   (containing `FABRIK_TOKEN` for `@arbeithand`), and a built `fabrik` binary.
2. **`handarbeit/fabrik-test-alpha`** and **`handarbeit/fabrik-test-beta`**
   are reachable with the token.
3. **`handarbeit/projects/2`** ("Fabrik Test") exists with stage columns
   (Backlog, Specify, Research, Plan, Implement, Review, Validate, Done).
4. **No other Fabrik instance is using the `@arbeithand` token's GraphQL
   budget concurrently** (or use `--max-concurrent 1` if you have to share).

See `~/fabrik-oss-launch-notes.md` (under "Files and where they live") for
the canonical setup.

## GraphQL budget

The bed token has 5,000 GraphQL points/hour, shared by the suite and the
test-bed engine. Measured 2026-07-28:

| Operation | Cost |
|---|---|
| `gh project item-list --limit 200` | ~101 pts |
| `gh project field-list` | ~106 pts |
| `fetchBoardItems` / `fetchStatusField` (harness.go) | ~2 pts |
| Engine board poll, warm cache | ~4 pts |
| Engine board poll, cold bootstrap | ~349 pts |

**The harness must not read the board through `gh project` subcommands.** They
resolve field values with a follow-up query per item, so cost scales with the
requested limit rather than with the data needed. When the wait-helpers used
them, a single scenario polling on a 10-15s interval cost ~24,000 pts/hour —
nearly 5x the whole budget — and a full `E2E_PARALLEL=4` run exhausted the
token in ~14 minutes. Everything after that fails with `gh` exit 1, which
looks like a pile of engine regressions but is not.

Use `fetchBoardItems` / `fetchStatusField` instead; they return the same data
for ~2 points by resolving Status inline via `fieldValueByName`. At that cost a
parallel-4 run is ~1,900 pts/hour, plus ~480 for the engine — roughly 2x
headroom. `gh project item-add` / `item-edit` are one-shot mutations and are
fine as-is.

`E2E_PARALLEL`'s default of 4 is the same value #971 originally picked, but is
now re-justified against this budget math rather than carried over
unrevisited — see "How the timeout/parallelism defaults are derived" below.
Raising `E2E_PARALLEL` (or raising `E2E_TIMEOUT` while holding parallelism
fixed, which widens the window of concurrent activity) should be re-checked
against this ~2x headroom before changing either default.

Once board reads were cheap, the residual cost became the issue/PR wait-helpers
(`gh issue view --json`, `gh pr list --json` — also GraphQL, ~1 pt each) polling
on behalf of a dozen parallel scenarios. Those run on `pollBase()`, default 30s,
overridable:

```bash
E2E_POLL_INTERVAL=15s scripts/e2e/run.sh -run TestCruiseFullPipeline
```

Shortening it is safe for a single scenario in isolation, where budget is not a
constraint; leave it at the default for a full parallel run.

If you hit the limit anyway, check the reset time and wait it out:

```bash
gh api rate_limit --jq '.resources.graphql'
```

### Additional prerequisites for `TestCIFixReinvoke` and `TestCIFixReinvokeCycleLimit`

5. **`ci-fix-sentinel` enrolled as a required status check** on
   `handarbeit/fabrik-test-alpha/main`. Both tests skip gracefully (via
   `t.Skip`) if this check is not enrolled — safe to merge before the sibling
   sub-issue adds the sentinel CI job.
6. **`FABRIK_MAX_CI_FIX_CYCLES=2` in the test bed `.env`** (for
   `TestCIFixReinvokeCycleLimit` only). The test skips with an instructional
   message if the value is `> 3`. After editing `.env`, restart the test-bed
   Fabrik instance so the new value takes effect.
7. The default `E2E_TIMEOUT=4h` already covers `TestCIFixReinvoke` run in
   isolation (inner waits alone total ~75–90 min). Use a *smaller* override
   to fail faster while iterating on just this scenario, e.g.:
   ```bash
   E2E_TIMEOUT=1h scripts/e2e/run.sh -run TestCIFixReinvoke
   ```

### Additional prerequisites for `TestPausedMergedPRRecovery`

8. **Gate labels seeded** in `handarbeit/fabrik-test-alpha`: `fabrik:awaiting-ci`,
   `fabrik:awaiting-review`, `fabrik:paused`, and `fabrik:awaiting-input` are
   production labels that must exist. `AddLabel` fatals immediately if a label is
   absent — create them manually in the repo if needed.
9. The default `E2E_TIMEOUT=4h` already covers `TestPausedMergedPRRecovery`
   run in isolation (three sequential cruise pipelines, Specify → Implement
   each, total ~60–90 min). Use a *smaller* override to fail faster while
   iterating on just this scenario, e.g.:
   ```bash
   E2E_TIMEOUT=1h30m scripts/e2e/run.sh -run TestPausedMergedPRRecovery
   ```

### Additional prerequisites for `TestConjunctiveCIReviewGate`

10. **`slow-gate` enrolled as a required status check** on
    `handarbeit/fabrik-test-alpha/main`. The test skips gracefully (via
    `t.Skip`) if not enrolled — safe to merge before enrollment.
11. **The engine process must actually authenticate as `FABRIK_TOKEN`'s
    identity** — no shell export shadowing it. `config.Token()`'s precedence
    is `FABRIK_TOKEN > GITHUB_TOKEN`, and `godotenv.Load(".env")` does not
    override a variable already set in the process environment, so this only
    breaks if `FABRIK_TOKEN` itself (not merely `GITHUB_TOKEN`) is exported in
    the shell that launches the test-bed Fabrik instance, shadowing the value
    in `.env` with a different identity (see handarbeit/fabrik#925 Confound 1
    for the incident this generalizes from). If this drifts, PRs come out
    authored by the wrong identity and `RequestPRReviewer` silently no-ops
    (GitHub forbids requesting a review from the PR author). The test's
    `AssertPRAuthorIsExpectedIdentity` preflight check catches this in
    seconds rather than after a full 60–100 min run — if it fails, check the
    launching shell's environment for a stray `FABRIK_TOKEN` export.
12. **One of the following for R5 (joint-clear verification)**:
    - **`FABRIK_REVIEWER_TOKEN` in the test bed `.env`** — a GitHub PAT for a
      non-`@arbeithand` account with write access to `fabrik-test-alpha`. The
      test uses this token to submit an approving PR review from a second
      identity (GitHub forbids self-approval). This exercises the full
      approval-path joint-clear (R5). Combined with a generous
      `FABRIK_REVIEW_WAIT_TIMEOUT` (see next bullet), the test defers
      requesting this reviewer until `fabrik:awaiting-ci` is observed (R1) —
      not until `stage:Review:complete`, which is applied immediately when
      Review's Claude invocation finishes, simultaneously with
      `fabrik:awaiting-review` and before Review's own gate has actually
      cleared. `fabrik:awaiting-ci` only appears once Validate has been
      dispatched, which only happens after Review's own `wait_for_reviews`
      gate has genuinely cleared first via the incidental
      `gemini-code-assist` review — so only Validate's gate blocks on the
      real reviewer.
    - **`FABRIK_REVIEW_WAIT_TIMEOUT` left at a generous value (e.g. the
      15-minute default)** when running the approval path — a short timeout
      (e.g. `2`) risks Review's own gate timing out before
      `gemini-code-assist` submits its incidental review (documented
      30s–10m behind PR-ready), which would break the Review-then-Validate
      sequencing the approval path depends on. The test skips with an
      instructional message if `FABRIK_REVIEWER_TOKEN` is set and this value
      is `< 10`. Use `FABRIK_REVIEW_WAIT_TIMEOUT=2` only for the
      timeout-fallback path below (no `FABRIK_REVIEWER_TOKEN`), so the
      review-timeout fallback path (R5 reduced scope) completes in a
      reasonable wall-clock budget. If this value exceeds 5 and no reviewer
      token is present, the test skips with an instructional message. After
      editing `.env`, restart Fabrik. Note: the timeout-fallback path has no
      second identity to request, so it remains exposed to the
      `gemini-code-assist` bot clearing the gate before the timeout fires
      (residual flakiness, not fixed by this redesign — see #925).
13. The default `E2E_TIMEOUT=4h` already covers this test run in isolation
    (worst case ~60–90 min for the approval path). Use a *smaller* override
    to fail faster while iterating on just this scenario, e.g.:
    ```bash
    E2E_TIMEOUT=1h30m scripts/e2e/run.sh -run TestConjunctiveCIReviewGate
    ```

### Additional prerequisites for the merge-train scenarios (ADR-059)

`TestMergeTrainHappyPathLanding`, `TestMergeTrainBisectionEjectsPoisoner`,
`TestMergeTrainRestartSafety`, and `TestMergeTrainRunawayGuardPausesBatch` need
one-time bed setup. They **skip cleanly** (`requireTrainBed`) if the `Queued`
column is absent, so they are safe to merge before the bed is set up. They
also skip cleanly under train mode `"off"` — these scenarios place issues
directly in `Queued` via the GitHub API, which succeeds regardless of mode,
but nothing drains `Queued` when `merge_train: off` (no per-item dispatch,
no batch handler), so without this check they'd hang to their full 10–50 min
timeout instead of skipping. Only run in the `on` leg of the two-mode gate.

14. **`Queued` board column** on `handarbeit/projects/2`, positioned between
    `Validate` and `Done` (ADR-059 D1 — the durable train queue). Add it in the
    Project's Status field options.
15. **`queued.yaml` holding stage** in the bed's `.fabrik/stages/`, e.g.:
    ```yaml
    name: Queued
    order: 8            # after Validate, before Done
    holding_stage: true # engine-managed; no Claude invocation
    ```
    Copy from `stages/examples/queued.yaml` (`fabrik init` / `fabrik refresh-stages`).
16. **Train-capable binary** in the bed, built from `main` (the release does not
    yet carry ADR-059). Run it **without `--auto-upgrade`** so it is not reverted
    to a release mid-suite:
    ```bash
    (cd ~/dev/fabrik && go build -o ~/dev/fabrik-test/fabrik .)
    # on macOS/Apple Silicon a copied binary may be SIGKILL'd; build in place or:
    #   xattr -cr ~/dev/fabrik-test/fabrik && codesign --force --sign - ~/dev/fabrik-test/fabrik
    ```
17. **`train-poison-guard` required check** on `fabrik-test-alpha` — only for
    `TestMergeTrainBisectionEjectsPoisoner`. Commit
    `tests/e2e/testdata/train-poison-guard.yml` to the repo as
    `.github/workflows/train-poison-guard.yml` and mark the `train-poison-guard`
    check REQUIRED on branch protection, so the combined-Validate poll gates on it.
    The bisection test skips this check indirectly — if the guard is absent the
    combined batch is green and no bisection occurs, failing the `bisecting`
    log-line wait; run it only after the guard is enrolled.
18. The default `E2E_TIMEOUT=4h` already covers these run in isolation
    (happy/bisect: 20–40 min; restart — two sequential landings: 25–50 min).
    Use a *smaller* override to fail faster while iterating, e.g.
    `E2E_TIMEOUT=1h`.
19. **`train-poison-guard` required check on `fabrik-test-beta`** — only for
    `TestMergeTrainRunawayGuardPausesBatch`. Commit
    `tests/e2e/testdata/train-poison-guard.yml` to `handarbeit/fabrik-test-beta`
    as `.github/workflows/train-poison-guard.yml` and mark the
    `train-poison-guard` check REQUIRED on branch protection (same steps as for
    Alpha in prerequisite #17, targeting Beta instead). The runaway test skips
    cleanly until this is enrolled.
    **`FABRIK_MAX_TRAIN_TRIALS_PER_WINDOW=6`** must also be set in the bed's
    `.env` before launching the Fabrik instance for this test. At the default
    (20), the guard would require ~20 red trials — the 4-member all-poison batch
    generates only ~7–10, so the test would time out. A cap of 6 sits above
    Alpha's bisect-scenario max (~4 trials) with comfortable margin. Wall-clock:
    ~10–20 min; ~6 trials × 2 required checks ≈ 12 Actions runs.

### Additional prerequisites for `TestReviewAuthority*` scenarios

`TestReviewAuthorityBlocksAndPausesOnChangesRequested`,
`TestReviewAuthorityClearsOnApproval`, and `TestReviewAuthorityYoloDoesNotBypassBlock`
cover ADR-1250's `review_authority: authoritative` mode. All four `TestReviewAuthority*`
scenarios, including `TestReviewAuthorityAdvisoryRegressionGuard`, run against the bed's
existing `Review` column/stage (default, untouched config) — **no bed column or stage-YAML
setup is required**, beyond `FABRIK_REVIEWER_TOKEN` and the `review-authority:authoritative`
label (both below).

**Mechanism: a per-issue label, not a bed column.** Authoritative mode is applied per item
via the `review-authority:authoritative` label, passed as an extra label at seed time
(`seedReviewGateItem`'s `extraLabels`). Engine support for that label is tracked separately
in #1261 — the three authoritative scenarios above cannot pass until both #1261 and this
issue's PR are merged; `TestReviewAuthorityAdvisoryRegressionGuard` has no such dependency
and ships green regardless.

An earlier design applied authority via a bed-local `Review-Authoritative` board column +
matching stage YAML (mirroring the `Queued`/`queued.yaml` precedent). That was rejected:
`review_authority` is a property of a stage's config, not a distinct kind of stage, so it
doesn't belong on the board as a column name — and requiring a bed prerequisite the operator
hadn't set up yet meant three of the four scenarios silently skipped, letting the suite go
green having validated zero authoritative behavior. Tests gating a release should fail loudly
when they can't run their intended assertion, not pass vacuously. See
`adrs/1258-e2e-review-authority-coverage.md` for the full rationale.

**Why these scenarios can't cover the landing/auto-merge gate:** `reviewGateBlocksLanding` is
only reachable through a stage literally named `Validate` — `engine/stages.go`,
`engine/poll.go`, and `engine/pr_terminal_advance.go` all hard-gate on `stage.Name ==
"Validate"`. Applying the authority label to an item on `Review` cannot reach that
stage-name-gated path, and authoritative-izing the bed's real `Validate` stage would violate
"no change to the bed's default stage config" and risk corrupting concurrently-running
advisory scenarios on the shared bed. This is a documented, accepted e2e gap — the three
scenarios below therefore assert the gate *clears* (`fabrik:awaiting-review` disappears,
`fabrik:paused` never applied), not that the item merges.

20. **`FABRIK_REVIEWER_TOKEN` in the test bed `.env`** — same non-author PAT documented
    in prerequisite #12 above. All four `TestReviewAuthority*` scenarios skip with an
    instructional message if it is unset; there is no timeout-fallback path here (unlike
    `TestConjunctiveCIReviewGate`) because these scenarios exist specifically to assert
    on a deterministic verdict, not on gate-timeout behavior alone.
21. **`review-authority:authoritative` label seeded** in `handarbeit/fabrik-test-alpha`
    (the only repo these scenarios use). `FileIssue` passes it straight through to
    `gh issue create --label`, which — like `AddLabel` (prerequisite #8) — fatals
    immediately if the label doesn't already exist as a label object in the repo; `gh`
    does not auto-create labels on issue creation. Create it manually
    (`gh label create review-authority:authoritative -R handarbeit/fabrik-test-alpha`)
    if needed. This is independent of #1261: #1261 adds the engine code that
    *interprets* the label on an issue it already carries, not the GitHub label object
    itself — the object must exist before any of these scenarios can even file their
    seed issue.
22. **Why the bed reviewer (`claude-review.yml`) stays COMMENT-only, and is not used
    for verdict assertions here**: `.github/workflows/claude-review.yml` submits
    `gh pr review --comment` in both its agent path and its fallback path — it can
    never produce `APPROVE` or `CHANGES_REQUESTED`, so it cannot exercise authoritative
    mode's blocking or clearing paths. Switching it to a real reviewer bot (e.g. pruefer)
    was explicitly rejected for issue #1258: non-determinism (verdict depends on Claude's
    severity classification of a synthetic diff), latency (pruefer polls, default 120s,
    vs. an Action firing on PR-open), cost (a real Claude invocation per test PR), and
    coupling (Fabrik's release gate depending on pruefer's health). All verdict assertions
    in `TestReviewAuthority*` instead use `SubmitPRReview` + `FABRIK_REVIEWER_TOKEN` —
    deterministic, harness-posted formal reviews from a non-author identity.
23. The default `E2E_TIMEOUT=4h` already covers
    `TestReviewAuthorityBlocksAndPausesOnChangesRequested` in isolation — its
    worst-case wall-clock is `FABRIK_REVIEW_WAIT_TIMEOUT + ~30 min`
    (10 min initial block-confirmation wait + `FABRIK_REVIEW_WAIT_TIMEOUT`+10 min for the
    pause wait itself + two trailing 5 min waits for `fabrik:awaiting-input` and the pause
    comment), though it typically completes much faster in practice. With the 15-minute
    default this worst case is ~45 min, still within `E2E_TIMEOUT=1h`. **Do not use a very
    short value like `FABRIK_REVIEW_WAIT_TIMEOUT=2` here**: `TestReviewAuthorityYoloDoesNotBypassBlock`
    runs concurrently against the same bed setting and needs the timeout comfortably above
    ~2 minutes, since its 90s "block persists under yolo" window starts shortly after
    `fabrik:awaiting-review` first appears — a too-short timeout risks a legitimate
    review-wait-timeout pause landing inside that window, which the test detects and fails
    on explicitly (distinct message, not misreported as a yolo bypass) rather than passing.
    A moderate value (e.g. `FABRIK_REVIEW_WAIT_TIMEOUT=5`) balances both tests' needs.
24. **Note on scope**: neither test bed repo has a branch-protection review requirement
    configured (only required *status checks* are documented as enrolled), so
    `FetchPRReviewDecision` returns `""` for every scenario here and `reviewGateAuthorityVerdict`
    exercises its Fabrik-computed fallback branch, not GitHub's native `reviewDecision`
    branch. A verdict-fetch-failure / unrecognized-`reviewDecision` scenario (issue #1258's
    optional scenario 6) was excluded for the same reason — producing `REVIEW_REQUIRED` or
    an unrecognized value would require new branch-protection bed setup, which is not
    "cheaply expressible" per the issue's own bar for that scenario.

### Additional prerequisites for `TestExpectedReviewers*` scenarios

`TestExpectedReviewersFastAdvance`, `TestExpectedReviewersDeclaredWaitsAndReprompts`,
`TestExpectedReviewersPrecedenceGuard`, and
`TestExpectedReviewersFastAdvanceComposesWithAuthoritative` cover ADR-1283's
`expected_reviewers` (declared unrequested reviewers for the review gate).
`TestExpectedReviewersUndeclaredRegressionGuard` is the fifth scenario and has no
dependency described below. All five run against the bed's existing `Review`
column/stage (default, untouched config) — **no bed column or stage-YAML setup is
required**, beyond `FABRIK_REVIEW_WAIT_TIMEOUT`/`FABRIK_REVIEWER_TOKEN` (already
documented above) and the two labels below.

**Mechanism: two per-issue labels, not a bed column.** A declared `expected_reviewers`
value is applied per item via one of two labels, passed as an extra label at seed
time (`seedReviewGateItem`'s `extraLabels`):

- `expected-reviewers:none` → `expected_reviewers: []` (fast-advance path)
- `expected-reviewers:declared` → `expected_reviewers: [e2e-synthetic-declared-reviewer]`
  (waiting/re-prompt-ladder path)

This is the same mechanism `TestReviewAuthority*` uses for
`review-authority:authoritative` (see above), applied to a second, list-shaped
stage-config field. Engine support for reading these two labels is tracked as a
separate, decoupled follow-up issue (not yet filed as of #1298's PR — see that
PR's description for the exact spec: the four call sites to update, the
`declared` > `none` precedence rule, and the `github/labels.go` pre-seeding step).
`TestExpectedReviewersFastAdvance`, `TestExpectedReviewersDeclaredWaitsAndReprompts`,
`TestExpectedReviewersPrecedenceGuard`, and
`TestExpectedReviewersFastAdvanceComposesWithAuthoritative` cannot pass until that
follow-up merges and both labels exist on the bed repo — they either run for real
or fail loudly (not skip) if the follow-up hasn't landed yet, mirroring #1258's
rejection of silent skips. `TestExpectedReviewersUndeclaredRegressionGuard` sets
neither label, exercises the bed's untouched default (`nil`) config, and ships
green regardless.

A bed-local `wait_for_reviews`-bearing stage/board-column variant (mirroring the
`Queued`/`queued.yaml` precedent) was considered and rejected for this feature too
— see `adrs/1298-e2e-expected-reviewers-coverage.md`. Its blast radius is worse
than the `Review-Authoritative` design #1258 already rejected: a normal (non-
`HoldingStage`, non-`Unmanaged`) stage gets no board-column-alignment exemption at
startup (`engine/startup.go` `checkStageColumnAlignment`), so a missing column
would stop the shared bed from starting entirely — not just skip a handful of
scenarios — taking every other in-flight parallel scenario down with it.

**Why these scenarios can't cover the landing/auto-merge gate:** same reason as
`TestReviewAuthority*` above — `reviewGateBlocksLanding` is only reachable through
a stage literally named `Validate`, and seeding on `Review` cannot reach it. This
is a documented, accepted e2e gap.

25. **`FABRIK_REVIEWER_TOKEN` in the test bed `.env`** — same non-author PAT as
    prerequisite #20, required only by `TestExpectedReviewersPrecedenceGuard`
    (the others don't need a submitted review). Skips with an instructional
    message if unset.
26. **`expected-reviewers:none` and `expected-reviewers:declared` labels seeded**
    in `handarbeit/fabrik-test-alpha` (the only repo these scenarios use).
    `FileIssue` passes extra labels straight through to `gh issue create --label`,
    which — like `AddLabel` (prerequisite #8) and prerequisite #21's
    `review-authority:authoritative` label — fatals immediately if a label doesn't
    already exist as a label object in the repo; `gh` does not auto-create labels
    on issue creation. Create both manually if needed:
    ```
    gh label create expected-reviewers:none -R handarbeit/fabrik-test-alpha
    gh label create expected-reviewers:declared -R handarbeit/fabrik-test-alpha
    ```
    This is independent of the follow-up engine issue: that issue adds the code
    that *interprets* a label an issue already carries, not the GitHub label
    object itself — the objects must exist before any of these scenarios can even
    file their seed issue.
27. **`TestExpectedReviewersDeclaredWaitsAndReprompts`'s wall-clock is long**
    (~2×`FABRIK_REVIEW_WAIT_TIMEOUT` + buffer, similar to
    `TestReviewAuthorityBlocksAndPausesOnChangesRequested`) — Phase 1 and Phase 2
    of the bot re-prompt ladder are folded into one continuation. The same
    moderate `FABRIK_REVIEW_WAIT_TIMEOUT` value recommended in prerequisite #23
    (e.g. `5`) applies here too; a very short value risks a legitimate Phase 1/2
    transition racing this test's own bounded-window assertions in
    `TestExpectedReviewersFastAdvance`/`...ComposesWithAuthoritative`.
28. **The synthetic declared-reviewer name (`e2e-synthetic-declared-reviewer`)
    must never resolve to a real, active GitHub account** on the bed's org —
    same rationale as prerequisite #22's warning against reusing a real installed
    bot: an unrelated real actor submitting a review would race the deterministic
    re-prompt-ladder assertions in `TestExpectedReviewersDeclaredWaitsAndReprompts`.

## Running

The recommended entrypoint is the runner script, which sets sensible defaults:

```bash
# Full two-mode validation gate — off, then on (slow: two full runs)
scripts/e2e/run.sh

# Single scenario, both modes
scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch

# Subset by name pattern, both modes
scripts/e2e/run.sh -run 'Smoke|NoWork'
```

Anything after the script name is passed through to `go test`. Override the
overall test timeout with `E2E_TIMEOUT` (default `4h`) — see "How the
timeout/parallelism defaults are derived" and "Timeout & failure reporting"
below for what backs that number and what happens if it's still hit.

The `e2e` build tag keeps all of this out of the default `go test ./...` run.

Set **`E2E_JITTER_SEED`** (a `uint64`) to make the harness's poll-interval
jitter (see below) reproducible — useful for locally reproducing a
flaky-looking failure. Leave it unset for normal runs; the jitter self-seeds
randomly by default.

#### Two-mode validation gate — `merge_train: off` and `merge_train: on`

`FABRIK_MERGE_TRAIN` is read once, at Fabrik startup, so exercising both
landing paths requires restarting the bed between them — it cannot be flipped
mid-run while `t.Parallel()` scenarios are in flight. By default `run.sh`
drives this itself: for `off` then `on`, it runs a narrow `go test` invocation
of `TestSwitchTrainMode` (stops the bed, edits `FABRIK_MERGE_TRAIN` in its
`.env`, restarts it — a wholly separate process from the suite invocation
that follows, so the restart is always complete before any scenario starts),
then the full suite with `E2E_TRAIN_MODE` exported. `off` runs first because
it's the path nearly all real usage takes; a regression there surfaces before
spending time on the less-common train-on run.

Force a single mode instead of the two-mode default with `E2E_TRAIN_MODE`:

```bash
E2E_TRAIN_MODE=off scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch
E2E_TRAIN_MODE=on  scripts/e2e/run.sh
```

A two-mode run is roughly double the single-mode GitHub API cost — see #1219
for the budget headroom this assumes, and merge it before attempting a full
two-mode run.

Scenarios resolve mode via `resolveTrainMode` (`harness.go`): `E2E_TRAIN_MODE`
takes precedence when set (an invalid value is a hard test failure), falling
back to a lenient read of the bed's own `.env` for ad-hoc/manual runs where
the switch step never ran. The "Mode" column in the Scenarios table below
records which scenarios assert a mode-specific contract.

#### Parallelism cap — the shared bed oversubscribes easily

16 of the 17 scenarios are `t.Parallel()`, but they **all drive one shared
Fabrik bed** (5 workers by default) against **one shared board and one shared
GitHub API budget**. Go's default `-parallel` is `GOMAXPROCS` (~8–12 cores), so
an unbounded full run fires ~16 scenarios at once, floods the 5-worker bed, and
saturates the API — producing cascading `transient gh error … (will retry)`
timeouts **even though every scenario passes standalone** (see issue #971).

`run.sh` therefore caps concurrency with `-parallel`, defaulting to **4**
(`E2E_PARALLEL`):

```bash
E2E_PARALLEL=2 scripts/e2e/run.sh   # tighter cap for a heavy/merge-train-heavy run
E2E_PARALLEL=6 scripts/e2e/run.sh   # looser, only if the bed's --max-concurrent is raised too
```

Lower values reduce oversubscription at the cost of wall-clock. The long
merge-train and CI-fix scenarios (see their notes above) are still best run in
isolation. **Do not** run the full suite unbounded expecting a clean pass — the
failure will be timeouts, not real regressions.

On top of the `-parallel` cap, every GitHub-polling wait helper's retry
interval is jittered ±20% (see `pollSleep` in `harness.go`) so concurrent
scenarios' polls desynchronize instead of converging into lockstep bursts
against the shared API budget (see #1104).

#### How the timeout/parallelism defaults are derived

Both `E2E_TIMEOUT=4h` and `E2E_PARALLEL=4` are backed by data already
committed in this file and by a real two-mode gate run, not chosen
arbitrarily — they're documented here so future drift (new scenarios, bed
resizing, GitHub API changes) is visible and the numbers can be revisited
deliberately rather than silently going stale.

**`E2E_TIMEOUT`: 90m → 4h.** A real two-mode gate run was killed by the
original 90m timeout while `TestCIFixReinvokeCycleLimit` was still executing
at 1h26m26s — already past its own documented 30–60min ceiling (see the
per-scenario table below). Two scenarios with paired off/on timings from that
run showed a ~1.55–1.61x contention multiplier under load:
`TestConjunctiveCIReviewGate` 1335s → 2152s, `TestPausedMergedPRRecovery`
1382s → 2146s. Applying that multiplier to the heaviest documented
per-scenario ceilings (`TestCIFixReinvoke` 75–90min, `TestPausedMergedPRRecovery`
60–90min) puts the contended worst case in the ~93–145min range (60min ×
1.55x low end, 90min × 1.61x high end). `4h` leaves ~95–147min of margin
above that for bed-restart and pipeline-setup overhead.
This is a reasoned extrapolation from two paired data points plus one
partial-kill observation, not a fresh full-suite measurement under the new
default — treat it as provisional, and re-derive it (repeating this
arithmetic with fresh paired timings) after any run that gets meaningfully
closer to 4h than the numbers above predict.

**`E2E_PARALLEL`: kept at 4, not lowered.** The available contention data
doesn't clearly indict 4 as an oversubscribed cap: the bed has 5 workers, so
4 already reserves headroom, and the "on" leg's slowdown is at least partly
explained by it having strictly more real work to do (17 scenarios vs. 13 —
the four Train-only scenarios skip near-instantly under "off"), not
necessarily by 4 being too high a concurrency cap. The one scenario failure
plausibly linked to bed starvation in the observed run
(`TestReviewAuthorityClearsOnApproval` timing out waiting for
`fabrik:awaiting-review` alongside a 5½-minute processing gap in the bed log)
is explicitly unconfirmed — its sibling `TestReviewAuthorityBlocksAndPausesOnChangesRequested`,
same helper, same assertion, passed in the same leg. Lowering `E2E_PARALLEL`
without stronger evidence would itself be an unmeasured, guessed change, and
risks masking real `t.Parallel()` interleaving defects for no demonstrated
benefit. Instead, the risk this requirement is aimed at — a scenario failing
purely because it was bed-starved — is addressed by the timeout increase
above: a starved scenario now has enough wall-clock room to actually finish
rather than racing a too-tight deadline. See `E2E_PARALLEL=2` /
`E2E_PARALLEL=6` above for the documented escape hatches if you observe a
repeated starvation pattern in practice.

#### Timeout & failure reporting

On a non-zero exit, `run.sh` classifies every top-level test by the last
action it emitted in the `go test -json` stream, and prints a labeled report
before failing:

```
== suite FAILED (leg: off, exit 1) — classifying test outcomes ==
JSON log: /tmp/fabrik-e2e-off-12345.json
completed - pass (11): TestBaseBranchPipeline, TestBlockedOnInput, ...
completed - fail (0):
completed - skip (2): TestMergeTrainHappyPathLanding, TestMergeTrainRestartSafety
still running at kill time (2): TestCIFixReinvokeCycleLimit, TestConjunctiveCIReviewGate
never started - queued behind -parallel cap (2): TestConvergenceRace, TestCruiseFullPipeline
```

This distinguishes three states a bare `FAIL` can't: **completed** (actually
ran to pass/fail/skip), **still running at kill time** (executing when the
process died — the only state Go's own built-in `-timeout` panic dump
reports), and **never started** (parked waiting for a free `-parallel` slot,
which the built-in panic dump omits entirely). The full JSON log is kept for
follow-up debugging at the path printed above.

#### Teardown on kill

A run killed by `E2E_TIMEOUT` (or an external signal) skips every in-flight
scenario's `t.Cleanup` — this is a hard Go-runtime constraint (the timeout
panic fires from a separate timer goroutine that crashes the whole process
before any test goroutine's deferred cleanup runs; an external signal doesn't
invoke Go-level defers at all), not something fixable in the test code.

When `run.sh` detects Go's own timeout-panic text in the JSON log (i.e. this
was specifically an `E2E_TIMEOUT` kill, not a normal scenario failure), it
automatically runs `scripts/e2e/reset.sh` (the plain form) as best-effort
teardown — closing stray PRs/issues, deleting leftover `fabrik/*` branches,
and draining the board, so the next run starts no dirtier than after a
completed one. A normal scenario `FAIL` does **not** trigger this — an
operator debugging a real regression needs the board/issue state left
intact.

This runs immediately and unattended, without giving an operator a chance to
inspect the stranded PRs/issues first — worth knowing if you're debugging why
a scenario hung rather than just re-running the gate. In practice, the two
most useful artifacts for that survive teardown anyway: the classification
report above (printed *before* teardown runs) already names the exact
still-running/never-started tests, and `close_open_issues_in`/
`close_open_prs_in` **close** rather than delete — their full comment
history, labels, and timeline stay inspectable afterward via
`gh issue view --repo <alpha|beta> <n>` / `gh pr view --repo <alpha|beta> <n>`
even after this teardown runs. What does *not* survive: `close_open_prs_in`
deletes each PR's head branch, and `drain_board` deletes (not just moves)
every project-board item, so board-column position at kill time is lost
unless you happened to capture it live.

**Worktrees are the one exception — they are not auto-cleaned.** The only
worktree-cleanup path (`scripts/e2e/reset.sh --worktrees`) nukes *all*
worktrees and bare clones bed-wide and requires stopping the bed first; it
cannot be scoped to just the interrupted run's artifacts, and running a
destructive full-bed operation automatically from a kill-detection path would
risk firing against a bed that isn't actually safe to stop at that moment.
If a run was killed by `E2E_TIMEOUT`, run this manually before the next
release-gate run if you need full parity with a completed run:

```bash
# stop the test-bed Fabrik instance first, then:
scripts/e2e/reset.sh --worktrees
```

### Reset between runs

**Run this as part of test prep** — before a clean suite, so the bed starts from a
known-empty state. Stale closed issues linger as **project-board items** and leftover
`fabrik/*` branches otherwise pollute the next run's merge-train snapshots and make
results hard to read. `run.sh` also runs the plain form of this automatically on a
detected `E2E_TIMEOUT` kill — see "Teardown on kill" above; a manual run is still
needed after such a kill if you want worktrees cleaned up too.

```bash
scripts/e2e/reset.sh             # full clean: PRs + issues + branches + board items (alpha + beta)
scripts/e2e/reset.sh --worktrees # ALSO wipes Fabrik's worktrees + bare clones (destructive)
```

The plain form resets to a clean slate: closes open PRs (deleting their branches),
closes open issues, deletes leftover `fabrik/*` branches, and **removes every item
from the "Fabrik Test" project board** (board items survive an issue close, so this
is what an earlier issues-only reset missed). Overridable via `FABRIK_TEST_PROJECT_OWNER`
/ `FABRIK_TEST_PROJECT_NUMBER` (default `handarbeit` / `2`).

The `--worktrees` form is for when the test bed itself is wedged — stop Fabrik first,
it will refuse otherwise.

> Do **not** run reset while a suite is in flight — it will drain the board out from
> under the running tests.

## Scenarios

"Mode" records each scenario's classification from the #1217 mode audit (FR-2/FR-3/FR-4):
**Both** — mode-invariant, single assertion set, passes under both `merge_train`
settings unmodified. **Both (mode-aware)** — genuinely differs by mode; the
scenario branches internally (via `resolveTrainMode`) and asserts the
mode-appropriate contract in each. **Train-only (on)** — exercises the merge
train directly; skips cleanly (`requireTrainBed`) under mode `"off"` or when
the `Queued` column is absent, so it only runs in the gate's `on` leg.

| Test | What it verifies | Mode | Approx wall-clock | Cost |
|---|---|---|---|---|
| `TestSmokeSingleRepoDispatch` | Worker dispatches on a trivial issue; Specify completes | Both | 3–5 min | $0.10–0.20 |
| `TestSmokeSingleRepoFullPipeline` | Full single-repo pipeline (Specify → … → Done with merged PR) | Both | 20–40 min | $0.50–1.50 |
| `TestNoWorkNeeded` | `FABRIK_NO_WORK_NEEDED` short-circuit closes issue without PR | Both | 10–15 min | $0.30–0.50 |
| `TestBlockedOnInput` | `FABRIK_BLOCKED_ON_INPUT` pause + comment-driven resume | Both | 10–15 min | $0.30–0.50 |
| `TestCrossRepoSpawn` | Cross-repo decomposition (spawn child in beta, gate parent, resume on close) | Both | 45–60 min | $1.00–2.00 |
| `TestYoloAutoMergeLabel` | `fabrik:yolo` auto-advance to Done; mode-appropriate landing contract (native auto-merge + `fabrik:auto-merge-enabled` under "off"; train close-not-merge + label never applied under "on") | Both (mode-aware) | 20–40 min | $0.50–1.50 |
| `TestConvergenceRace` | Deterministic post-Validate auto-merge race (#829): two conflicting yolo PRs; mode-appropriate `fabrik:auto-merge-enabled` contract, both land within budget, neither ends `fabrik:paused` | Both (mode-aware) | 80–100 min | $2–4 |
| `TestCruiseFullPipeline` | `fabrik:cruise` auto-advances to Validate-complete without auto-merge; PR merged by human closes issue | Both | 30–50 min | $0.80–2.00 |
| `TestBaseBranchPipeline` | `base:<branch>` non-default base branch: throwaway branch created off main, PR targets it (not main), pipeline does not falsely pause at end of Implement, review gate clears via the base-independent REST feed | Both | 35–55 min | $0.80–2.00 |
| `TestCIFixReinvoke` | CI-fix reinvoke positive path: sentinel fails on first push, Claude fixes, CI passes, issue closes | Both | 75–90 min | $1.00–3.00 |
| `TestCIFixReinvokeCycleLimit` | CI-fix reinvoke negative path: unfixable sentinel exhausts MaxCiFixCycles, issue pauses | Both | 30–60 min | $0.50–1.50 |
| `TestPausedMergedPRRecovery` | paused + gate-label at Validate with merged PR heals to CLOSED (3 sequential sub-tests: awaiting-ci, awaiting-review, no-gate-label); regression guard for #874 class | Both | 60–90 min (3 sequential sub-tests, ~20–30 min each); covered by the default `E2E_TIMEOUT=4h` | $1.50–4.50 |
| `TestConjunctiveCIReviewGate` | Conjunctive CI∧review gate: fabrik:awaiting-ci holds before CI, PR comment during CI-await not dropped, fabrik:awaiting-review holds before approval, advance suppressed until both gates clear | Both | 60–90 min (approval path) / 30–50 min (timeout path) | $1.00–2.50 |
| `TestReviewAuthorityBlocksAndPausesOnChangesRequested` | ADR-1250 authoritative mode (via `review-authority:authoritative` label, requires #1261): CHANGES_REQUESTED verdict blocks the gate (fabrik:awaiting-review); verdict never clears → pauses at ReviewWaitTimeout with the authoritative reason in the comment, not the generic "no reviews submitted yet" | Both | ~`FABRIK_REVIEW_WAIT_TIMEOUT` + 30 min (worst case) | ~$0.05 (no Claude) |
| `TestReviewAuthorityClearsOnApproval` | ADR-1250 authoritative mode (via `review-authority:authoritative` label, requires #1261): APPROVED verdict clears the gate; fabrik:paused never applied | Both | 2–5 min | ~$0.02 (no Claude) |
| `TestReviewAuthorityYoloDoesNotBypassBlock` | ADR-1250 composition guarantee (via `review-authority:authoritative` label, requires #1261): fabrik:yolo does not bypass an authoritative gate — blocked while CHANGES_REQUESTED stands, clears once approved | Both | 5–10 min | ~$0.03 (no Claude) |
| `TestReviewAuthorityAdvisoryRegressionGuard` | Regression guard: advisory (default) mode still clears on any submitted review regardless of verdict — proves the additive authoritative check didn't narrow the default path | Both | 2–5 min | ~$0.02 (no Claude) |
| `TestExpectedReviewersFastAdvance` | ADR-1283 `expected_reviewers: []` (via `expected-reviewers:none` label, requires follow-up engine issue): nothing requested/reviewed → gate fast-advances instead of waiting out the review timeout (the #1080 stall this feature fixes) | Both | 2–5 min | ~$0.02 (no Claude) |
| `TestExpectedReviewersDeclaredWaitsAndReprompts` | ADR-1283 `expected_reviewers: [<name>]` (via `expected-reviewers:declared` label, requires follow-up engine issue): declared-but-unrequested reviewer holds the gate open, Phase 1 re-prompt ladder fires with an @mention comment, Phase 2 pauses for human when no response arrives | Both | ~2×`FABRIK_REVIEW_WAIT_TIMEOUT` + buffer | ~$0.05 (no Claude) |
| `TestExpectedReviewersPrecedenceGuard` | ADR-1283 precedence guard (via `expected-reviewers:none` label, requires follow-up engine issue): a genuinely requested reviewer holds the gate open despite `expected_reviewers: []` being declared | Both | 5–10 min | ~$0.03 (no Claude) |
| `TestExpectedReviewersUndeclaredRegressionGuard` | Regression guard: undeclared (`nil`) `expected_reviewers` still never fast-advances — pins the `expected != nil` check and proves the shipped default (FR-5) is unchanged | Both | 2–5 min | ~$0.02 (no Claude) |
| `TestExpectedReviewersFastAdvanceComposesWithAuthoritative` | ADR-1283 composition guard (via `expected-reviewers:none` + `review-authority:authoritative` labels, requires follow-up engine issue + #1261): fast-advance still fires ahead of the authority-verdict branch, since it only activates once hasReviews is true | Both | 2–5 min | ~$0.02 (no Claude) |
| `TestMergeTrainHappyPathLanding` | ADR-059 internal train: 3 clean Queued members → one integration PR → all advance Queued→Done, PRs closed, no O(N²) per-member retests | Train-only (on) | 10–25 min | low (no Claude) |
| `TestMergeTrainBisectionEjectsPoisoner` | ADR-059 D4: red combined batch → halving bisection isolates the poison member → ejected → survivors land. Needs the `train-poison-guard` required check | Train-only (on) | 20–40 min | low–moderate |
| `TestMergeTrainRestartSafety` | ADR-059 D5 / #960: after a landing, a restart with the historical merged integration PR present does NOT stall the next batch (reconstruct proceeds fresh). **Not parallel** — restarts the bed | Train-only (on) | 25–50 min | low |
| `TestMergeTrainRunawayGuardPausesBatch` | ADR-059 D8 (#964/#965): persistently-red 4-member batch trips the runaway guard at cap=6, pauses all Queued members, no member reaches Done. Runs on RepoBeta for counter isolation | Train-only (on) | 10–20 min | low (no Claude) |

Approximate single-mode suite total: ~685 min wall-clock, $10.30–30 in Claude
tokens. A full two-mode gate run is roughly double this, minus the
near-instant skip of the four Train-only scenarios in the `off` leg — the
default `E2E_TIMEOUT=4h` and the contention data behind it (see "How the
timeout/parallelism defaults are derived" above) assume this full two-mode
shape, not just the single-mode total.

### Regression coverage map

| Scenario | Issues / fixes it protects |
|---|---|
| `TestSmokeSingleRepoDispatch` | General pipeline breakage |
| `TestSmokeSingleRepoFullPipeline` | Full pipeline regression |
| `TestNoWorkNeeded` | #733 (marker), #742 (close-on-no-work) |
| `TestBlockedOnInput` | `FABRIK_BLOCKED_ON_INPUT` marker, ed46b7fc (awaiting-input label clear) |
| `TestCrossRepoSpawn` | #797 / #803 (on-demand spawn-target init), v0.0.66 spawn machinery, #800 (addBlockedBy mutation name) |
| `TestYoloAutoMergeLabel` | #829 (GitHub native auto-merge for yolo), #831/#835/#871 (convergence regression cascade) |
| `TestConvergenceRace` | #829 (post-Validate auto-merge race, Story 2/SC-002); regression guard for the production failure on example-org/example-repo#82 (spurious CI-fix-cycle-limit pause); #1217 (mode-aware `fabrik:auto-merge-enabled` assertion) |
| `TestCruiseFullPipeline` | #898 (cruise/yolo gate at Validate, `engine/poll.go`); ensures cruise never triggers `checkAutoMergeConvergence` |
| `TestBaseBranchPipeline` | #1046 (report: base:<branch> GraphQL data gap), #1047 (`verifyAndHealLinkageByBody` linkage fix), #1050 (base-independent review-gate REST data feed) |
| `TestCIFixReinvoke` | #888 ADR-056 D1 (settling primitive reinterprets CI-gate signals); CI-fix reinvoke loop (engine/ci.go) |
| `TestCIFixReinvokeCycleLimit` | CI-fix cycle limit (`pauseForCIFixCycleLimit`), `MaxCiFixCycles` exhaustion path |
| `TestPausedMergedPRRecovery` | #874 (paused+merged PR recovery class), #887 (settle-owner structural fix, `runValidatePRTerminalAdvance`), ADR-056 D2 (single-owner for PR-terminal → Done) |
| `TestConjunctiveCIReviewGate` | ADR-056 D2 (conjunctive gate joint-clear), #887 (settle-owner), #895 (this scenario), #925 (identity/dual-gate/bot-reviewer redesign) |
| `TestReviewAuthorityBlocksAndPausesOnChangesRequested` | ADR-1250 (`review_authority: authoritative`), #1258 (this scenario), `checkAwaitingReviewTimeout`'s `authorityReason` pause-message path |
| `TestReviewAuthorityClearsOnApproval` | ADR-1250, #1258 |
| `TestReviewAuthorityYoloDoesNotBypassBlock` | ADR-1250's yolo/cruise composition guarantee, #1258 |
| `TestReviewAuthorityAdvisoryRegressionGuard` | ADR-1250 additive-check regression guard, #1258 |
| `TestExpectedReviewersFastAdvance` | ADR-1283 (`expected_reviewers`), #1298 (this scenario), the #1080 stall the feature exists to eliminate |
| `TestExpectedReviewersDeclaredWaitsAndReprompts` | ADR-1283, #1298 — first e2e coverage of the bot re-prompt ladder (`fabrik:bot-reprompted`, Phase 1/2 of `checkAwaitingReviewTimeout`) |
| `TestExpectedReviewersPrecedenceGuard` | ADR-1283, #1298 — declared reviewers narrow waiting for unrequested reviewers only, never bypass a genuinely requested one |
| `TestExpectedReviewersUndeclaredRegressionGuard` | ADR-1283 FR-5 regression guard, #1298 — pins `reviewGateFastAdvance`'s `expected != nil` check |
| `TestExpectedReviewersFastAdvanceComposesWithAuthoritative` | ADR-1283, #1298 — fast-advance independence from `review_authority` (ADR-1250) |
| `TestMergeTrainHappyPathLanding` | ADR-059 D1/D3 (#946, #947, #948) — Queued column, trial-branch build, integration-PR landing + member lifecycle |
| `TestMergeTrainBisectionEjectsPoisoner` | ADR-059 D4 (#949) — halving bisection, ejection, one-at-a-time fallback |
| `TestMergeTrainRestartSafety` | ADR-059 D5 (#950) + PR #960 (reconstruct must not stall on a historical merged PR) |
| `TestMergeTrainRunawayGuardPausesBatch` | ADR-059 D8 (#964) — runaway guard trial cap, per-repo counter isolation |

Every escape-from-release regression earns a new scenario in this table.

## Adding a scenario

1. Pick a name like `cross_repo_spawn_test.go`. Use `Test<DescriptiveName>` for the
   function so `-run` filtering is clean.
2. Use the helpers in `harness.go` to file the trigger issue and watch for
   expected events.
3. Always clean up at the end — close opened issues, remove the worktree from
   the test bed (`t.Cleanup` is your friend).
4. Document what regression the scenario protects against. Reference the
   originating issue or PR.

## Design notes

- Scenarios do **not** start or stop the Fabrik instance — the instance is
  expected to be already running. Two exceptions, both using the
  `StopFabrikTestBed`/`StartFabrikTestBed` helpers in `lifecycle.go`:
  `TestMergeTrainRestartSafety` (restarts mid-scenario to exercise
  restart-safety) and `TestSwitchTrainMode` (restarts to flip
  `FABRIK_MERGE_TRAIN` for the two-mode gate — not itself a scenario, run
  only via `run.sh`'s mode-switch step). Both are deliberately **not**
  `t.Parallel()`.
- Assertions are on **observable outcomes**, not internal state. We check
  GitHub for label changes, comments, PR creation, etc. — not the engine's
  internal `worktreeManagers` map.
- Log-line assertions are deliberately last-resort. Prefer GitHub state. Logs
  are only useful when the observable outcome is "Fabrik logged something
  specific" (e.g., the spawn error from #797/#803).
- Scenarios should be **idempotent** — running twice in a row should produce
  the same result. If a scenario depends on starting state that prior runs
  modify, normalize it at the start of the test.

## Known limitations

- **Cost per run is non-trivial.** A full cross-repo scenario costs $1–3 in
  Claude tokens. The suite is not for casual local iteration.
- **CI integration is not wired yet.** Initially the suite is operator-only —
  run before cutting a release. Future work: a GitHub Actions runner that
  exercises the suite on a schedule.
- **GitHub rate-limit pressure.** Shared with `~/dev/fabrik/` (the dev
  instance) under the `@arbeithand` token. Stop the dev instance if running
  the full suite.
