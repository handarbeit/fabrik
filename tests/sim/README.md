# `tests/sim` — the simulation test layer

This directory holds a simulated GitHub that Fabrik's engine can be driven
against in an ordinary `go test` run.

It exists because Fabrik had two test layers and nothing between them.

## The three tiers

| | `engine/mocks_test.go` | **`tests/sim`** | `tests/e2e` |
|---|---|---|---|
| What it is | Per-call function hooks | Stateful model, real git backing | Real daemon, real GitHub |
| State across calls | None | Full | Real |
| Multi-poll sequences | No | Yes | Yes |
| Adversarial paths | Hard | On demand | Barely reachable |
| Cost per run | Milliseconds | Seconds | Claude tokens, GraphQL budget, wall clock, a maintained live test bed |
| Catches wire bugs | No | **No** | Yes |

`engine/mocks_test.go` hands each test a set of hooks — `fetchProjectBoardFn`,
`fetchLinkedPRFn`, `fetchCheckRunsFn` — and the test hand-builds the exact
snapshot it wants the engine to see. Nothing carries across poll cycles, so no
test in `engine/` can assert a *sequence* of transitions. The engine is a state
machine; that layer can only photograph one frame at a time.

`tests/e2e` drives a real fabrik daemon against real GitHub, real Claude
workers, and real review bots. It is the only place a multi-poll sequence runs
end to end, and it costs real money and wall-clock time to find out.

The consequence was that every state-machine behaviour — label lifecycles,
conjunctive gates, settle-scan escalation and its `MaxRetries` cutover, cycle
limits, restart recovery — was either asserted one frozen frame at a time, or
cost a live e2e run. Adversarial paths (confirmed CI failure, non-mergeable PR,
API error, engine killed mid-stage) were barely covered at all, because the live
bed cannot be made to produce them on demand.

`tests/sim` is the missing middle: faithful enough that a transition *sequence*
means something, cheap enough to run on every commit.

## What is here

- **[`simgh/`](simgh/)** — a stateful model of a GitHub project (issues, project
  items, PRs, check runs, commit statuses, reviews, labels, comments, blockedBy
  links) implementing the full `engine.GitHubClient` interface, backed by a real
  local bare git repository for everything git can genuinely answer.
- **[`simgh/FIDELITY.md`](simgh/FIDELITY.md)** — every place the model knowingly
  departs from real GitHub, and what each divergence costs. **Read it before
  trusting a sim-backed test to cover a subtle behaviour.**
- **[`simgh/nonvacuity.sh`](simgh/nonvacuity.sh)** — a mutation sweep that
  neutralises each modelled behaviour and asserts the suite goes red.

## The instrumentation layer

A faithful model is a GitHub stand-in; it is not yet a test *instrument*. Three
capabilities turn it into one, and they are what let a scenario reach behaviour
the live bed cannot be made to produce on demand:

- **Clock-driven schedules** (`simgh/schedule.go`). `SeedCheckRunsAfter`,
  `SeedReviewsAfter` and their siblings enqueue a mutation for a future instant
  on the injected clock — "CI goes red thirty minutes in", "the reviewer
  responds on the third poll". Sequencing is driven by the clock rather than by
  a read count, because **one poll is not one read**: `settleAwaitingCIScan`
  reads a SHA's check runs twice in a single poll, and whether it does depends
  on harness configuration. A read-count sequence would not correspond to poll
  boundaries at all.
- **Fault injection** (`simgh/fault.go`). Any `engine.GitHubClient` method can
  be made to fail once, N times then succeed, always, on the Kth call, or only
  for calls matching a predicate. The five settle scans' retry loops and
  `MaxRetries` escalation cutovers are reachable no other way. The error shape
  is the caller's choice and it matters — `simgh/ghfault` supplies the
  rate-limit and abuse-detection shapes the engine classifies specially.
- **An ordered mutation log** (`simgh/mutationlog.go`). Several engine
  guarantees are *ordering* claims rather than state claims, and reading
  terminal labels cannot assert one. It records every intercepted call with its
  outcome — attempts, not just mutations — so "N failures then the success" is
  expressible, and doubles as the harness's debugging output.

Fault injection and the log live in a decorator, `simgh.Instrument(sim)`, which
holds its `*Sim` as a **named field and never an embedded one** — embedding
would satisfy the interface assertion by method promotion and let a forgotten
wrapper silently bypass both instruments.

`Sim.Snapshot`/`Restore` (`simgh/snapshot.go`) round-trip the model, the git
repositories, the fault schedule and the log, so a restart scenario can discard
the engine and rebuild against exactly the state GitHub would have retained.

The seam this layer targets is `engine.NewWithDeps(cfg, GitHubClient,
ClaudeInvoker, *WorktreeManager)`, which accepts substituted dependencies.
`simgh` alone only fills the first of those three — the sections below cover
the rest: a scripted `ClaudeInvoker` (`simclaude/`) and the scenario harness
(`tests/sim`, this package's own top-level files) that wires everything
together and drives it with deterministic poll advancement (#1449).

## Wiring a real Engine against it (`tests/sim`, `simclaude/`)

`simgh` is a faithful GitHub stand-in; the other live dependency
`engine.NewWithDeps` takes is the Claude worker, and a Claude worker does not
merely return a string — it runs inside a real worktree, writes files,
commits, and returns marker-bearing output the engine parses. **`simclaude`**
(`simclaude/invoker.go`) is a scripted `engine.ClaudeInvoker` that does the
real thing: `Invoke`/`InvokeForComments` calls dispatch to a `Script`,
selected by stage name and invocation count, that runs inside the real
`workDir` it's handed. `simclaude.DefaultScript` — used for any stage a
scenario doesn't script — writes a small per-stage marker file, commits it
for real via the `git` binary, and signals completion; `simclaude.ForStage`/
`ForStageComments` let a scenario override that for the stage(s) it cares
about. `simclaude/scripts.go` supplies constructors for the failure shapes
the engine treats specially — `TurnLimitExhausted`, `UsageLimitExit`,
`APIErrorExit`, `PartialOutputThenKilled`, `CommentReviewCompleted` — each
returning the exact `(output, completed, usage, err)` contract
`RealClaudeInvoker` documents for that exit condition, reachable from this
external test package only because `internal/claudeerr` (below) exports the
four error types the engine classifies via `errors.As`.

**`Env`/`NewEnv`** (`env.go`) is the harness: it seeds a repo and project in
`simgh`, wraps it with `simgh.Instrument` (load-bearing for `AdvanceUntil`'s
mutation-log diagnostics below, not just for scenarios that inject faults),
reproduces production's git topology by cloning `simgh`'s backing repo as a
local "origin" — exactly as `ensureBareClone` bare-clones the real GitHub
remote, just with a local filesystem path standing in for the network URL —
and boots a real `engine.Engine` via `NewWithDeps` plus the two production
seams below.

**`RunPoll`/`RunPolls`/`AdvanceUntil`** (`poll.go`) drive the engine
deterministically: each call advances a shared `Clock` by one poll interval,
then calls the engine's `PollOnce` seam. `AdvanceUntil(t, env, cond,
maxPolls)` polls until `cond` holds or `maxPolls` is exhausted, in which case
it fails with the board state and the `simgh.Instrumented` mutation log
attached — a failing scenario is diagnosable from CI output alone, no
re-run needed.

**`FileIssue`/`WaitForProjectStatus`/`WaitForIssueLabel`/`IssueLabels`/
`WaitForLabelAbsent`** (`assertions.go`) are named after
[`tests/e2e/harness.go`](../e2e/harness.go)'s vocabulary — see the mapping
table below for where they diverge.

### Two production seams, both additive

Neither changes any existing call site's behavior; both are documented test
seams, not new capabilities:

- **`Engine.PollOnce(ctx) error`** (`engine/poll.go`) — a thin delegation to
  the existing unexported `poll`, added because the only other driver,
  `Engine.Run()`, loops on a real ticker until its context is cancelled.
  Looping `Run()` against a tiny `PollSeconds` and cancelling after a while
  would reintroduce the wall-clock nondeterminism this harness exists to
  eliminate. `PollOnce` deliberately does not replicate `Run()`'s preamble
  (the cross-process `.fabrik/fabrik.lock` flock, the `pollLogFile` package
  global, startup drift warnings) — see its doc comment for why each of
  those would actively break a parallel test run rather than help one.
- **`internal/claudeerr`** — the four Claude failure-classification error
  types (`UsageLimitError`, `TurnLimitError`, `APIErrorExit`,
  `ResumeFailureError`), moved out of `engine/claude.go` into a leaf package
  that `engine` and `tests/sim` both import and that imports neither —
  mirroring `simgh/ghfault`'s precedent. `engine/claude.go` keeps unexported
  type aliases to the moved types, so every existing construction site and
  every existing `errors.As` assertion in `engine`'s own tests keeps
  compiling and behaving unchanged.

Plus one **engine-local clock seam** (`Engine.Clock`/`SetClock`/`now()`,
`engine/clock.go`), structurally identical to `simgh.Clock` so a scenario can
share one `tests/sim.Clock` instance across both via `simgh.WithClock` and
`Engine.SetClock`. Its scope is narrow and specific: most timing gates
(`FABRIK_REVIEW_WAIT_TIMEOUT`, the CI settle scan, the Done-archive scan) are
anchored on a `GitHubClient`-read timestamp (`FetchLabelAppliedAt`) that
`simgh` already controls with no engine change — this seam covers only the
genuinely in-memory timing `simgh` cannot reach: `itemstate.CooldownAt`
(dispatch-suppression cooldowns) and the `#1314` label-applied-at
write-through cache (`recordLabelAppliedAtNow`, discovered while building the
AC6 review-timeout scenario — see `engine/clock.go`'s doc comment for the
full story). See `adrs/1449-sim-harness-engine-seams.md` for the complete
rationale on all three seams.

### What this harness cannot avoid: real wall-clock time

Not everything is clock-controllable. `poll()` dispatches each item's work to
a background goroutine and returns immediately — part of that goroutine's
real behavior is genuinely real-time-bound, independent of the injected
Clock:

- **The multi-instance lock-verify delay** (`engine/item.go`'s
  `lockVerifyDelay`, a real 2s `time.Sleep`) exists to let a *different
  Fabrik process* observe a competing lock — there is nothing for a Clock
  seam to represent here, since it's about wall-clock scheduling across
  processes, not simulated time.
- **The stage retry cooldown** (`processItem`'s `time.Since(lastAttempt) <
  cooldown` check, `cfg.PollSeconds*10` = 10s at this package's default) is
  stamped via real `time.Now()`, deliberately outside the Clock seam's scope
  (see `failure_shapes_test.go`'s `retryCooldownPolls`).

`RunPoll` originally paused for a small, fixed real duration (`workerYield`,
100ms) after every `PollOnce` call so these background goroutines got real
wall-clock time to progress. **Finding (#1450 follow-up, PR #1538/CI run
31459112834):** a fixed pause is not a reliable proxy for "the worker made
progress" under CPU contention — a CI runner failed `TestCIFixReinvokeCycleLimit`
by exhausting its 80-poll `AdvanceUntil` bound, while the same test passed
reliably in local runs. A starved worker goroutine needs more real time per
poll than a fixed sleep assumes, and a poll-count bound has no way to detect
that; it was never a scenario problem, it was a harness problem — the layer
built to remove wall-clock nondeterminism had wall-clock nondeterminism in
its own advance primitive.

The fix is a new engine test seam, `Engine.HasInFlightWorker()` (the same
liveness signal production's own idle-upgrade and shutdown-pause logic treat
as authoritative — `engine/poll.go`'s `dispatched == 0` branch, `shutdown.go`'s
`inFlightSnapshot`), and `RunPoll` now waits for the *dispatched worker's own
completion* rather than a guessed duration whenever a poll cycle actually
dispatched something: `waitForWorkerQuiescence` polls
`Engine.HasInFlightWorker()` at a 5ms interval, bounded by a 60s safety
timeout that fails the test loudly (not a silent proceed) if a worker is
genuinely stuck. This restores the poll-count bound's meaning regardless of
runner load, and tends to be *faster* for the common case (a scripted
`simclaude` response returns synchronously, so most dispatches quiesce in
well under 100ms). A poll cycle that dispatches nothing still takes the old
fixed `workerYield` path — several existing waits (`retryCooldownPolls`, the
real 10s stage-retry cooldown) depend on that wall-clock floor to make a real,
non-Clock-seamed cooldown elapse across "nothing to do" polls, the same way
it would in production; only the previously load-sensitive dispatched-worker
case changed. (The safety timeout started at 15s and was raised to 60s after
an initial validation pass on this same machine — already under severe
external load, see below — produced two spurious timeouts of its own when
the full package's ~30 `t.Parallel()` scenarios ran concurrently under
`-race`; both passed cleanly in isolation, confirming scheduling contention
rather than a logic bug, and a "generous" bound has to survive the condition
it exists to be robust against.) See `poll.go`'s doc comments for the full
detail. This is still the dominant contributor to this package's own
runtime; see Runtime below.

## Vocabulary mapping: `tests/e2e` ↔ `tests/sim` (R5)

The goal is that a reader of a live scenario can read a sim scenario without
relearning. Names and parameter order match [`tests/e2e/harness.go`](../e2e/harness.go)
wherever the semantics permit; every place they don't is listed here, once,
rather than re-explained per function.

| `tests/e2e` | `tests/sim` | What differs and why |
|---|---|---|
| `FileIssue(t, env, repo, title, body, labels...) int` | `FileIssue(t, env, title, body, status string, labels...) int` | No `repo` param — a sim `Env` manages exactly one repo (`env.OwnerRepo`), where the live harness's `Env` manages two (`RepoAlpha`/`RepoBeta`) and must be told which. Folds in `status` — `simgh.SeedIssue`'s `Status` field places the issue on the board in one seeding call, where live `FileIssue` only creates the issue and needs two more calls (`AddIssueToProject`, `SetIssueStatus`) to place it. |
| `AddIssueToProject(t, env, repo, issueNumber) string` | *(none)* | Folded into `FileIssue` above — no separate step needed. |
| `SetIssueStatus(t, env, itemID, columnName)` | *(none)* | Folded into `FileIssue` above. |
| `WaitForProjectStatus(t, env, repo, issueNumber, columnName, timeout time.Duration)` | `WaitForProjectStatus(t, env, issueNumber, status string, maxPolls int)` | No `repo` param (as above). `timeout time.Duration` → `maxPolls int`: the live harness waits on real GitHub's own clock; the sim harness waits on `AdvanceUntil`'s poll count, driven by the shared `Clock` and each `RunPoll`'s worker-quiescence wait, not wall-clock deadlines. |
| `WaitForIssueLabel(t, env, repo, issueNumber, label, timeout)` | `WaitForIssueLabel(t, env, issueNumber, label, maxPolls int)` | Same two divergences as above. |
| `WaitForLabelAbsent(t, env, repo, issueNumber, label, timeout)` | `WaitForLabelAbsent(t, env, issueNumber, label, maxPolls int)` | Same. |
| `IssueLabels(t, env, repo, issueNumber) []string` | `IssueLabels(t, env, issueNumber) []string` | No `repo` param (as above); otherwise identical. |
| *(none)* | `WaitForIssueClosed(t, env, issueNumber, maxPolls int)` | New — no live-harness equivalent. Added because AC1 requires driving to "a closed issue in the Done column," and every scenario in this package needs to assert that explicitly rather than infer it from a label. |
| *(none)* | `RunPoll(t, env)` / `RunPolls(t, env, n)` / `AdvanceUntil(t, env, cond, maxPolls)` | New — the live harness has no equivalent because it never drives poll cycles itself; it waits on wall-clock timeouts against a daemon it doesn't control. This is R4's deterministic poll-advancement vocabulary. |

## Coverage matrix: `tests/e2e` → `tests/sim` (R4, #1450)

One row per entry currently in `tests/e2e/` (30 total: 28 files, `README.md`,
`testdata/`). Status is one of **Ported** / **Partially ported** / **Live-only**
for scenario files, or **N/A** for support/harness/pure-logic files that were
never scenarios to begin with. No row is blank.

| `tests/e2e` file | Status | Sim counterpart / reason |
|---|---|---|
| `smoke_test.go` | Ported | `smoke_test.go` — `TestSmoke_FullPipelineToDone`. Ported before #1450 as part of the harness-building chain (#1449); the reference pattern every later scenario in this package follows. |
| `cruise_test.go` | Ported | `cruise_test.go` — `TestCruiseFullPipeline` (R1–R5). |
| `blocked_on_input_test.go` | Ported | `blocked_on_input_test.go` — `TestBlockedOnInput`. |
| `no_work_needed_test.go` | Ported | `no_work_needed_test.go` — `TestNoWorkNeeded`, carrying forward the `HasPrefix`-not-`Contains` skip-comment discriminator `no_work_needed_marker_test.go` protects live. |
| `no_work_needed_marker_test.go` | N/A | Pure unit test of a string-matching helper (`hasNoWorkNeededSkipComment`), zero live-bed content per its own doc comment. Nothing to port as a separate sim test — its discriminator is carried into `no_work_needed_test.go`'s sim port directly instead of duplicated as a second no-op unit test. |
| `basebranch_test.go` | Ported | `basebranch_test.go` — `TestBaseBranchPipeline`'s core assertions (PR targets the throwaway base branch, no false pause at Implement, review gate clears naturally). Not regression coverage for the #1046/#1047/#1050 GraphQL-asymmetry defect class itself — see `simgh/FIDELITY.md`'s "Linkage and review data are base-branch-independent" entry; `tests/e2e/basebranch_test.go` remains the only coverage of that. |
| `auto_merge_test.go` | Partially ported | `auto_merge_test.go` — `TestYoloAutoMergeLabel`'s train-mode-**off** leg only (train-on is merge-train, separate issue in the chain). The sim proves the engine's *reaction* to a completed native auto-merge (the scenario itself calls `MergePR` to simulate GitHub finishing the async merge) — it cannot prove GitHub would have completed the merge spontaneously, since `simgh`'s native auto-merge is flag-only. See `simgh/FIDELITY.md`'s "Native auto-merge" entry, amended with this port's concrete finding. |
| `ci_fix_reinvoke_test.go` | Ported — **exceeds live coverage** | `ci_fix_reinvoke_test.go` — `TestCIFixReinvoke` (positive fail-then-pass path) and `TestCIFixReinvokeCycleLimit`, carrying forward #1320's `HasPrefix` comment discriminator and #1323's "force a genuine commit every cycle" non-vacuity design. The live positive path is permanently *skipped* (pending #916 — a capable live Claude agent tends to make CI green before the first push, so the induced-failure premise never fires there). Sim scripts Claude directly, so that constraint doesn't exist here — this is the one place in the port where sim provably exercises a path the live suite structurally cannot. |
| `ci_fix_reinvoke_marker_test.go` | N/A | Pure unit test of a string-matching helper, zero live-bed content per its own doc comment. Discriminator carried into `ci_fix_reinvoke_test.go`'s sim port, same reasoning as `no_work_needed_marker_test.go` above. |
| `conjunctive_ci_review_gate_test.go` | Ported | `conjunctive_ci_review_gate_test.go` — `TestConjunctiveCIReviewGate` (ADR-056 D2) and `TestConjunctiveCIReviewGate_ReviewNeverArrives` (this port's non-vacuity proof for the review-gate half). R3 (a PR comment posted during CI-await still receives its 👀 reaction) is not ported — it is a property of `itemNeedsWork`'s general new-comment admission, orthogonal to the CI/review conjunction under test, and `simgh`'s comment model does not distinguish a PR's own conversation thread from the linked issue's. |
| `review_authority_test.go` | Ported | `review_authority_test.go` — all 5 scenarios (`TestReviewAuthorityReinvokesOnChangesRequested`, `CycleLimitPauses`, `ClearsOnApproval`, `YoloDoesNotBypassBlock`, `AdvisoryRegressionGuard`), label-driven via `review-authority:<mode>`. Reinvoke-dispatch observability substitutes `env.Claude.CommentCallCount` for the live suite's `WaitForLogLine` (sim has no log-line-waiting primitive — see the "no log-scraping primitive" note below). Scope limitation carried from ADR-1258: only the advance gate (`checkReviewGate`) is exercised, never the landing gate (`reviewGateBlocksLanding`), reachable only through a stage literally named `Validate`. |
| `expected_reviewers_test.go` | Ported | `expected_reviewers_test.go` — all 5 scenarios (`TestExpectedReviewersFastAdvance`, `DeclaredWaitsAndReprompts`, `UndeclaredRegressionGuard`, `FastAdvanceComposesWithAuthoritative`, `TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation`), label-driven via `expected-reviewers:<mode>`. `DeclaredWaitsAndReprompts` uses the backdated-`StartTime` technique (`timeout_test.go`'s precedent) to fire both bot-reprompt-ladder phases without waiting out real minutes. Surfaced the `SeedReview`/review-request fidelity fix below. |
| `paused_merged_pr_recovery_test.go` | Ported | `paused_merged_pr_recovery_test.go` — `TestPausedMergedPRRecovery`'s 3 sub-cases as `t.Run` subtests (gate=`fabrik:awaiting-ci`, gate=`fabrik:awaiting-review`, no gate). The live test's fixed 45s real-wall-clock sync-barrier sleep has no port — `RunPoll`/`AdvanceUntil` drive the engine directly against synchronous sim state, so there is no board-cache-refetch race to guard against (R2: sim asserts *more*, deterministically). |
| `convergence_race_test.go` | Live-only | Not in R1's target set (R6 assessment only). Its genuine subject — two concurrently-dispatched engine workers racing a server-side atomic auto-merge — is structurally unreproducible: `simgh`'s native auto-merge is flag-only and never resolves spontaneously (see "Native auto-merge" in `simgh/FIDELITY.md`), and its own compare-and-swap window (`FIDELITY.md`'s "A PR retargeted after the gate cleared it") models a different, sim-only mechanism, not GitHub's server-side atomicity. The conflict-*detection-and-recovery* mechanism it also exercises (`checkMergeabilityGate` → `fabrik:rebase-needed` → `dispatchRebaseReinvoke`) was confirmed exercisable in sim in principle — `simclaude.DefaultScript`'s same-path-different-content collision reproduces a genuine same-anchor git conflict for free, with no custom scripting needed to provoke it — but the planned `convergence_recovery_test.go` (a new, honestly-named, narrower scenario for the recovery mechanism only, per R6) was not completed within this port's time budget. Recorded as a follow-up opportunity, not a regression against R1 (this file was never an R1 obligation). |
| `cross_repo_spawn_test.go` | Ported | `cross_repo_spawn_test.go` — `TestCrossRepoSpawn` (positive path) and `TestCrossRepoSpawn_RefusesUnservedTarget` (this port's AC3 non-vacuity proof, the ADR-1419 board-servability companion). Required extending `Env` with opt-in `EnvOptions.SecondRepo` and a new engine test seam, `Engine.RegisterWorktreeManagerForTest` (`engine/engine.go`) — see the harness note below. The parent's Plan-stage spawn declaration is seeded directly (`SeedComment`) rather than produced by a scripted live Plan invocation — see that file's own doc comment for the git-worktree corruption this sidesteps (below). |
| `train_mode_switch_test.go` | Live-only | Per R6: restarts the bed process to force it to re-read `FABRIK_MERGE_TRAIN` at startup — a process-lifecycle property, not a state-machine one. `Engine.PollOnce` drives an in-process engine directly; there is no subprocess boundary for this package to restart. No sim analogue is possible by construction. |
| `marker_paths_test.go` | N/A | Per R6/the issue body: a static consistency check over the live harness's own path map. Zero live-bed content, no sim analogue needed. |
| `review_failfast_test.go` | N/A | Per R6/the issue body: a pure boundary-condition unit test of `reviewFailFastDue`, zero live-bed content per its own doc comment. |
| `mergetrain_happy_test.go` | Out of scope | Merge-train scenario — separate issue in this chain (issue Scope section). |
| `mergetrain_bisect_test.go` | Out of scope | Same. |
| `mergetrain_restart_test.go` | Out of scope | Same. |
| `mergetrain_runaway_test.go` | Out of scope | Same. |
| `mergetrain_helpers.go` | Out of scope | Support code exclusively for the 4 merge-train scenario files above. |
| `doc.go` | N/A | Package doc comment only, no scenario content. |
| `harness.go` | N/A | Support/harness code (the live assertion vocabulary this package's `assertions.go`/`poll.go` are deliberately named after), not a scenario itself. |
| `lifecycle.go` | N/A | Support/harness code (bed start/stop, `.env` readers), not a scenario. |
| `harness_test.go` | N/A | Pure unit tests of `jitterWithRand` (`TestJitterWithRand*`) — no GitHub or Claude involved. |
| `review_authority_helpers.go` | N/A | Support code for `review_authority_test.go`/`expected_reviewers_test.go` (`seedReviewGateItem`/`seedReviewGateItemDraft`). Structural analogue: `tests/sim/review_gate_helpers.go`. |
| `README.md` | N/A | Documentation, not a scenario. |
| `testdata` | N/A | Fixture data directory, not a scenario. |

### A log-scraping primitive has no sim analogue, and doesn't need one

The live suite's review-authority/expected-reviewers scenarios assert a
reinvoke *dispatch* (as distinct from its side effects) via `WaitForLogLine`
against the engine's own log output — there is no other way to observe that
decision on a live bed. `tests/sim` has no equivalent primitive, and this port
did not add one: `env.Claude.CommentCallCount(stageName)` incrementing is a
strictly stronger, race-free, GitHub-observable-equivalent signal that
`InvokeForComments` was actually called for that stage, and every scenario
that needed the live test's log-line assertion uses it instead.

### Harness note: multi-repo `Env` needs its own `WorktreeManager` per repo

`EnvOptions.SecondRepo` (added for `cross_repo_spawn_test.go`) sets
`cfg.Repo = ""` to mirror production's multi-repo instance topology. This
exposed a real gap in `NewWithDeps`: its `worktrees` parameter registers at
most one `WorktreeManager`, keyed off `cfg.Owner+"/"+cfg.Repo` — for a
single-repo `Env` that key equals the one real repo under test, so it worked
by construction; for a multi-repo `Env` the key becomes the nonsensical
`"owner/"`, matching no real repo, and *every* repo the engine touches
(including what the test considers its "primary" repo) fell through to
production's dynamic `ensureRepoReady`/`ensureBareClone` path — which tries
to reach the real network. Fixed with a new, additive engine test seam,
`Engine.RegisterWorktreeManagerForTest` (`engine/engine.go`, ADR-1449-style),
that lets a multi-repo `Env` register one pre-built, locally-backed
`WorktreeManager` per repo directly. Single-repo `Env`s are unaffected.

Separately — and still open — a multi-repo `Env` with a **custom, non-default
(non-committing) stage script** for a stage before `Implement` (e.g. a
scripted `Plan` invocation that returns marker text without touching the
worktree) was observed to trigger a reproducible git-worktree corruption
("could not fetch origin", eventually a bare-repo path resolving into
`simgh`'s own internal storage rather than the registered origin) that a
single-repo `Env` with the identical script does not exhibit, and a
multi-repo `Env` using only default (committing) scripts does not exhibit
either. Root cause not identified within this port's time budget.
`cross_repo_spawn_test.go` avoids it entirely by seeding the parent's
Plan-stage spawn declaration directly (`SeedComment`) instead of scripting a
live Plan invocation — see that file's own doc comment. Any future
multi-repo scenario that needs a custom, non-default script on an
early-pipeline stage should be aware of this and prefer the same
direct-seed pattern until the root cause is found.

## Two things make it worth having

**It is stateful.** A mutation is observable by every subsequent read, the way
GitHub behaves. Read projections are computed fresh from the model on every
call and never cached, so that is a structural property rather than something
each test re-asserts.

**It is backed by real git.** Mergeability comes from an actual trial merge of
head into base. `MergePR` writes an actual two-parent merge commit onto the base
ref, and only then flips `merged`. Commits-behind comes from `rev-list`. A fake
that lets a test *declare* mergeability cannot catch a mergeability bug.

## What this layer is permanently blind to

**GraphQL and REST wire correctness.** This is not a gap to be closed later in
this package — it is structural, and worth being blunt about.

`simgh` implements a Go interface. Query documents, mutation names, JSON field
mappings, pagination, and status-code handling are all below it and entirely
invisible.

The concrete example is the `addBlockedBy` regression that shipped broken in
v0.0.66 (see [`../e2e/README.md`](../e2e/README.md)). The bug was a wrong
mutation *name* — a string literal inside a query document. Every
interface-level implementation of `AddBlockedByIssue`, including this one, is
perfectly green against it. A sim-backed suite would have shipped that bug with
full confidence.

Two things close that gap, and neither is replaced by this layer:

- **Wire-contract tests**, covering the query documents themselves.
- **`tests/e2e`**, which is not being retired. It remains the only place the
  whole system runs against the real thing.

Do not read "the sim scenarios pass" as "this works". Read it as "the state
machine behaves, assuming the wire layer does what we think".

## Runtime and the `sim` tag decision (R8/AC9)

`go test -race ./tests/sim/` (this package alone — `Env`, `simclaude`, and
the scenarios in "What this layer is permanently blind to" above) measures at
**~21–23s** on a development machine, dominated by the real-time costs
documented above (`lockVerifyDelay`, the stage retry cooldown, `workerYield`
pacing) rather than by CPU work — every scenario in this package uses
`t.Parallel()` for exactly this reason, since none of them contend on CPU or
shared state and the dominant cost is wall-clock waiting, which parallelizes
almost for free. `tests/sim/simclaude` alone is ~2–3s.

**Updated for #1450's port** (R1's 12 target scenarios, ~30 new test
functions across 8 new files): `go test -race -count=1 ./tests/sim/` now
measures at **~35s** on a development machine — up from the ~21–23s baseline
above, still dominated by the same real-time costs (more scenarios, each
paying the same fixed per-poll `workerYield`/cooldown costs, parallelized via
`t.Parallel()` exactly as before). `tests/sim/simgh` (below) is unaffected by
this port and remains the slower of the two packages.

**Updated again for the worker-quiescence fix** (see "What this harness
cannot avoid: real wall-clock time" above): `go test -race -count=1
./tests/sim/` now measures at **~36–40s**, essentially unchanged from the
~35s figure directly above — replacing the fixed per-dispatch `workerYield`
sleep with a wait for the dispatched worker's actual completion did not
meaningfully change the package's total runtime (confirmed across several
repeated `-count=1` runs, including under sustained external CPU load on the
measuring machine: ~36–40s each time, no exhaustion). What changed is
variance and correctness under load, not the mean: previously, a poll that
dispatched a worker always paid the fixed 100ms regardless of whether the
worker actually needed that long, and a worker needing genuinely more real
time (e.g. `lockVerifyDelay`) relied on several such polls happening to add
up to enough real time — a bet that only paid off when the CPU wasn't
starved. Now a dispatched poll pays exactly as long as the worker actually
took, no more and no less, so the total is close to the same on an
unloaded machine and — the actual point of the fix — no longer prone to
exhausting a poll-count bound (`AdvanceUntil`'s `maxPolls`) on a loaded one.

Comfortably under the ~90s line R8 sets, so **no `sim` build tag** — this
package is part of the default `go test ./...` (and CI's `go test -race
-timeout 5m ./...`), guarded only by the existing `skipIfNoGit` idiom, exactly
like `tests/sim/simgh` already is.

`tests/sim/simgh`'s own pre-existing suite (~65–76s, unaffected by this
issue; measured at ~92s under this port's `-count=1` run above, within normal
machine-load variance) runs as a separate package and is not part of this
measurement, though `go test -race ./tests/sim/...` runs both concurrently —
Go parallelizes across packages by default — so the wall-clock cost this
issue actually adds to a full run is closer to "the slower of the two," not
their sum: this port's own `go test -race -count=1 ./tests/sim/...` measured
**~92s total**, i.e. still bounded by `simgh`'s pre-existing runtime, not by
this port's additions.

## Settle-scan inventory and recovery-machinery coverage (#1451)

#1451 added the sim layer's first coverage of the engine's recovery
machinery: settle-scan retry-and-escalation, cycle-limit termination, engine
restart across a real process-state boundary, partial-mutation-sequence
recovery, and the account-wide Claude-limit path — the class of behavior the
live e2e bed structurally cannot provoke on demand (a repeatedly-failing
GitHub call, an engine kill at a specific instant, a real usage-limit hit).

### R1 — settle-scan inventory

The in-scope set is defined by property, not enumeration (see the issue's
own R1 text): every per-poll settle scan that owns a `fabrik:awaiting-*`
label and escalates to `fabrik:paused` after `MaxRetries` sustained
failures of the underlying GitHub call. Seven scans match on the `main`
this issue was built against — if an eighth is ever added without a row
here, that is the gap this table exists to make visible instead of leaving
it latent in prose.

| scan function | label | ADR | covering scenario |
|---|---|---|---|
| `settleNoWorkNeeded` (`engine/no_work_needed_settle.go`) | `fabrik:awaiting-done` | ADR-060 | `settle_scan_escalation_test.go`: `TestSettleScan_AwaitingDone`; ordering guarantee: `no_work_needed_test.go`: `TestNoWorkNeeded_AwaitingDoneIsFirstMutation` |
| `settleAwaitingCIScan` (`engine/ci_settle.go`) | `fabrik:awaiting-ci` | ADR-1270 | `settle_scan_escalation_test.go`: `TestSettleScan_AwaitingCI` |
| `settleMergeTrainMemberCloses` (`engine/merge_train_member_close_settle.go`) | `fabrik:awaiting-member-close` | ADR-061 | `settle_scan_escalation_test.go`: `TestSettleScan_AwaitingMemberClose` |
| `settleNonDefaultBaseCloses` (`engine/close_nondefault_base_settle.go`) | `fabrik:awaiting-close` | ADR-1097 | `settle_scan_escalation_test.go`: `TestSettleScan_AwaitingClose` |
| `settleChildPlacement` (`engine/spawn_settle.go`) | `fabrik:awaiting-placement` | ADR-062 | `settle_scan_escalation_test.go`: `TestSettleScan_AwaitingPlacement` |
| `settleAwaitingAdvanceScan` (`engine/advance_settle.go`) | `fabrik:awaiting-advance` | ADR-1422 | `settle_scan_escalation_test.go`: `TestSettleScan_AwaitingAdvance` |
| `settleRunawayGuardAlertScan` (`engine/runaway_alert_settle.go`) | `fabrik:awaiting-runaway-alert` | ADR-1533 | `settle_scan_escalation_test.go`: `TestSettleScan_AwaitingRunawayAlert` |

Explicitly out of R1's scope, and why (mirrors the issue's own Scope
section):

- `settleQueuedReviewFindings` (ADR-1208) — merge-train-specific, belongs to
  sibling issue #1452.
- `settleClaudeLimitLabelSweep` / `settleClaudeLimitClearRequests` — no
  `MaxRetries` counter, no escalation arc; covered under R5 instead (below).
- `fabrik:awaiting-review` / `fabrik:awaiting-input` — not settle-scan-owned
  in the R1 sense; the review gate and the blocked-on-input pause own them
  respectively.

Six of the seven scans are covered via direct-seed + fault injection (seed
the marker label plus minimal item state, fault the one call the scan
retries) — no full pipeline dispatch needed. `settleAwaitingCIScan` is the
exception: it runs the full `catchUpPhase1Handlers` chain, so its coverage
uses a real `wait_for_ci` stage and a genuine linked PR instead.

### R2 — cycle limits

| cycle limit | pause function | covering scenario |
|---|---|---|
| Review | `pauseForReviewCycleLimit` | `review_authority_test.go`: `TestReviewAuthorityCycleLimitPauses` (pre-dates #1451; satisfies R2/AC3 for this gate) |
| Rebase | `pauseForRebaseCycleLimit` | `cycle_limit_test.go`: `TestRebaseCycleLimit` |
| CI-fix | `pauseForCIFixCycleLimit` | `ci_fix_reinvoke_test.go`: `TestCIFixReinvokeCycleLimit` (pre-dates #1451) |

All three share one dispatch/pause primitive, `dispatchWithCycleLimit`
(`engine/catch_up_handlers.go`) — identical off-by-one boundary semantics
(`cycleCount >= maxCycles` checked before incrementing, so exactly
`maxCycles` reinvokes dispatch and the pause fires on what would be
reinvoke `maxCycles+1`) — so a scenario proving the exact count for one is a
faithful template for the others.

### R3 — restart recovery

`RestartEnv` (`restart.go`, see `adrs/1451-sim-bed-restart-harness.md`)
discards a scenario's `*engine.Engine` and rebuilds one against a
`simgh.Instrumented.Snapshot`/`RestoreInstrumented` round-trip, reusing the
original `Env`'s `WorktreeManager`/`Clock`/`Claude` invoker — a genuine
process-state boundary, not just another poll of the same process.
`TestRestartEnv_RoundTrip` (`restart_roundtrip_test.go`) is its own
foundation test, proving the harness itself before anything is built on
top of it.

Four kill points (`restart_recovery_test.go`): before any completion-label
write; between the two halves of `addCompleteLabelAndRemoveCI`'s label
pair; after a draft PR is created but before `MarkPRReady` lands; and
mid-`spawnChildren` (`AddBlockedByIssue`, shared with R4 below).

### R4 — partial-mutation sequences

See `partial_mutation_test.go`'s own doc comment for the full sequence
table (which steps are recoverable vs. genuinely unrecoverable-as-found).
Two defects were found and pinned rather than fixed, per the issue's
explicit scope — filed as follow-up issues, linked from both the covering
scenario's doc comment and the PR description that introduced this table.

### R5 — Claude-limit path

`claude_limit_sweep_test.go`: `TestClaudeLimitLabelSweep` (the account-wide
sweep, isolated from a per-issue redispatch via a bystander issue that
never itself dispatches again) and
`TestClaudeLimitExit_NeverAccumulatesTowardMaxRetries` (2 consecutive
usage-limit hits with `MaxRetries=1` never escalate). Both use
`fabrik:clear-claude-limit` rather than real time, since
`activateClaudeSuspension`/`claudeSuspendedUntilTime` are anchored to real
`time.Now()`, not this harness's injected `Clock` — see
`claude_limit_sweep_test.go`'s own doc comment. `fabrik:clear-claude-limit`
lifting a suspension end-to-end was already covered pre-#1451 by
`failure_shapes_test.go`'s `TestFailureShape_UsageLimitExit`.

### R6 — concurrency under `-race`

`concurrency_test.go`: `TestConcurrency_MultiIssueConvergence` — 6 issues,
`MaxConcurrent` equal to the issue count, driven by one shared
`AdvanceUntil` so all six are genuinely interleaved. Deliberately a
single-dispatch pipeline (`failureShapeStages()`), not a PR-creating one —
see that file's own doc comment for two earlier drafts (a 7-stage and a
3-stage pipeline) that each measured 100+ real wall-clock seconds before
landing on the shallow pipeline that actually isolates R6's target
(dispatch/lock/worktree contention), independent of pipeline depth.

## Running it

```bash
go test -race ./tests/sim/...          # the suite (this package + simgh + simclaude)
bash tests/sim/simgh/nonvacuity.sh     # the simgh mutation sweep (needs a clean tree)
```

The tests use the real `git` binary and skip when it is unavailable, following
the repo-wide `skipIfNoGit` convention.
