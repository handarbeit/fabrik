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

### Production seams, all additive

None changes any existing call site's behavior; all are documented test
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
- **`Engine.RegisterObservers() (unregister func())`** (`engine/poll.go`,
  #1592/ADR-1592) — moves the reactive `itemstate.Store` observer
  registration block (`mayNeedWorkObserver`, `InvocationObserver`,
  `StageChangeObserver`, `PushUnblockObserver`, `CommentBreakerObserver`,
  the `cacheImpl`-gated `WebhookHealthObserver` subscription) out of `Run()`
  into its own method — `PollOnce` alone never registered any of them,
  which nothing in this package's coverage happened to depend on until
  #1592's dependency-unblock scenario needed `PushUnblockObserver`
  specifically. **`NewEnv`/`RestartEnv` deliberately do NOT call it by
  default** — see `NewEnv`'s own doc comment: registering it unconditionally
  changes dispatch *timing* for every scenario in this package, since
  `mayNeedWorkObserver`'s cycleSet is a fast-path that bypasses
  `itemMayNeedWork`'s cooldown-based admission gate, and wiring it in
  broke several pre-existing, unrelated scenarios whose own timing
  assumptions predate this observer being reachable from `tests/sim` at
  all. A scenario that needs it — currently `dependency_unblock_test.go`
  (gap 1, for `PushUnblockObserver` itself) and
  `reinvoke_cycle_counters_test.go` (gap 4, for a different reason: see
  that file's own doc comment on `reinvokeCycleCountersEnv` — advisory-mode
  multi-round reinvokes have no other cooldown path back to re-admission
  once `dispatchWithCycleLimit`'s `advancedItems` marking suppresses the
  periodic-re-eval stamp) — calls `env.Engine.RegisterObservers()` itself,
  scoping the timing change to exactly the scenarios that require it.
- **`Engine.PollWithBackoff(ctx, configuredInterval) (PollBackoffResult, error)`**
  (`engine/poll.go`, #1592/ADR-1592) — moves `Run()`'s `doPollCycle` closure
  body (the REST/core rate-limit hard gate, `poll()` itself, idle-timer
  bookkeeping, GraphQL rate-limit hysteresis, the resulting effective
  next-poll interval) into its own method, returning only the computed
  `NextInterval` a caller needs to reset its own ticker. `backoff.go`'s five
  pure functions had no call site outside that closure, so `PollOnce` alone
  never reached any of them. The five backoff-state locals the closure held
  became `Engine` fields (`backoffPrevMultiplier` etc.), mirroring
  `idleStart`'s own precedent, since the method must now be callable
  repeatedly across successive polls from a test.
- **`Engine.RunStartupCleanup()`** (`engine/worker_liveness.go`,
  #1592/ADR-1592) — delegates, in order, to the five one-shot startup
  recovery scans `Run()` has always run once, immediately after its first
  successful poll cycle (`runStartupCleanup` and four siblings) — all
  unexported and `Run()`-only, so `PollOnce` alone never reached them
  either. A scenario calls this once after `RestartEnv` rebuilds the
  `Engine`, mirroring where `Run()` calls it in production.

See `adrs/1592-sim-bed-backoff-and-startup-cleanup-seams.md` for the full
rationale on the three newest seams, including the one behavior widening
that rode along with `PollWithBackoff`'s extraction (the REST hard gate now
compares against `e.now()` instead of `time.Now()` — byte-identical in
production, since `e.now()` falls back to `time.Now()` when no `Clock` is
injected, and necessary for the gate to be observable at all from a
`tests/sim` scenario's injected `Clock`).

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
| `train_mode_switch_test.go` | Live-only | Per R6: restarts the bed process to force it to re-read `FABRIK_MERGE_TRAIN` at startup — a process-lifecycle property, not a state-machine one. `Engine.PollOnce` drives an in-process engine directly; there is no subprocess boundary for this package to restart. No sim analogue is possible by construction. (The adjacent state-machine question — does an "off" engine ever drain Queued, and does an "on" one land normally — is covered separately by #1452's `mergetrain_mode_test.go`, construction-time rather than restart-time; see the `mergetrain_helpers.go` row below.) |
| `marker_paths_test.go` | N/A | Per R6/the issue body: a static consistency check over the live harness's own path map. Zero live-bed content, no sim analogue needed. |
| `review_failfast_test.go` | N/A | Per R6/the issue body: a pure boundary-condition unit test of `reviewFailFastDue`, zero live-bed content per its own doc comment. |
| `mergetrain_happy_test.go` | Ported | Superseded by a broader combinatorial matrix rather than a 1:1 port: `mergetrain_assembly_test.go`'s all-green subtest covers the happy path directly, and `mergetrain_mode_test.go`'s `TestMergeTrainMode_On` is a second, minimal positive control. See #1452. |
| `mergetrain_bisect_test.go` | Ported | `mergetrain_assembly_test.go` (R2 poison matrix: real-conflict proof, all-green, single poison at first/middle/last position, two independent poisons, a member poisonous only in combination) + `mergetrain_bisect_bound_test.go` (R3: trial counts asserted against `ceilLog2`/`effectiveBisectCap`, cost-cap-honored proof via forced fallback to `landOneAtATime`) + `mergetrain_ejection_test.go` (R5 ejection half: comment wording, `MaxMergeTrainEjections` pause-after-N, no-Queued-without-comment invariant) + `mergetrain_landing_test.go` (R6: `landOneAtATime`/`landSingleton`, plus the `landMergeTrainBatch` asymmetry pin — no live counterpart). See #1452. |
| `mergetrain_restart_test.go` | Ported | `mergetrain_restart_test.go` (sim) — R7's three `reconstructTrainState` routes (orphaned-branch cleanup, resume from a live trial, deferred landing after a merge) via `RestartEnv` (ADR-1451), direct-seeded rather than goroutine-raced against a real kill signal. Building this scenario surfaced and fixed a real `RestartEnv` defect (stale `origin` remote across a restart) — see ADR-1452. See #1452. |
| `mergetrain_runaway_test.go` | Ported | `mergetrain_runaway_test.go` (sim) — `TestMergeTrainRunaway_TripsAndPausesQueuedMembers` plus its own non-vacuity "does not trip with a generous window" subtest. `recordTrial`/`isRunawayTripped` are real-wall-clock-anchored, not `Clock`-seamed, so this cannot fast-forward the rolling window — mirrors the live suite's own small-window real-time trial-cycling technique. See #1452. |
| `mergetrain_redsingleton_test.go` | Ported | `mergetrain_redsingleton_test.go` (sim) — the #1440/#1545 red-singleton-reroute shape (R2's mandatory batch-size-1 red case), plus R9's two reroute-off-holding scenarios (state-transition dispatchability via `fabrik:revalidate`; failed-reroute ordering via `Instrumented.Log().Precedes`, no comment/count on failure). See #1452. |
| `mergetrain_helpers.go` | Ported | `mergetrain_helpers.go` (sim) — `mergeTrainStages`/`mergeTrainEnv`/`QueueMember` (mirroring the live file's own idiom) plus `startTrialVerdictSeeder`, the per-SHA poison-declaration mechanism this port's real-git approach needed and the live suite does not (it observes real CI, sim scripts it) — see ADR-1452. Also carries R4's scripted-conflict-resolution `CommentScript` builders (`mergetrain_conflict_resolution_test.go`'s three failure shapes: unresolvable-eject, usage-limit-no-eject, mixed-mode-premature-commit — no live counterpart, since the live suite exercises a real Claude resolver rather than scripting its failure modes) and R8's mode on/off regression guard (`mergetrain_mode_test.go` — a construction-time property in sim, distinct from `train_mode_switch_test.go`'s bed-restart/config-reread property, which stays live-only — see that file's own row above). See #1452. |
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

**Updated for #1451's recovery-machinery scenarios** (~30 new test
functions across 8 new files — R1's 7 settle-scan escalation pairs, R2's
rebase cycle limit, R3's 4 restart kill points plus `RestartEnv`'s own
round-trip test, R4's partial-mutation scenario, R5's 2 Claude-limit
scenarios, R6's concurrency scenario): `go test -race -count=1
./tests/sim/` (this package alone) now measures at **~36s**, essentially
unchanged from the ~36–40s baseline directly above despite the added
scenario count — the direct-seed + fault-injection technique used for 6 of
R1's 7 settle scans (seed the marker label plus minimal item state, fault
one call) avoids a full pipeline dispatch for most of the new coverage, and
R6's concurrency scenario deliberately uses a single-dispatch pipeline
rather than a PR-creating one (see `concurrency_test.go`'s own doc comment
for why — two earlier drafts using pipeline depth for realism each measured
100+ real wall-clock seconds on their own before this was found).
`go test -race -count=1 ./tests/sim/...` (this package + `simgh` +
`simclaude` + `simgh/ghfault`, run concurrently) measured **~84s total**,
still bounded by `simgh`'s own ~84s runtime rather than by this issue's
additions — comfortably under the ~90s line, so the "no `sim` build tag"
decision above still holds.

**Updated for #1452's merge-train suite** (~25 new test functions across 9
new files, one shared helper file): this is the first addition to genuinely
exceed the ~90s line, and honestly, not by a small margin — each merge-train
scenario does real, repeated `git merge`/push/`git gc` work (per trial, and
several scenarios run multiple trials), which is qualitatively heavier than
anything else in this package. `go test -race -count=1 -run TestMergeTrain
./tests/sim/` (this issue's scenarios alone) measures at **~90–130s**
depending on machine load. Two mitigations were applied, both documented in
`adrs/1452-mergetrain-sim-harness-seams.md`:

- `mergeTrainEnv`'s `CIBackstopTimeout` (10s) and
  `SetTrainCIPollIntervalForTest` (15ms) give the verdict seeder headroom
  against contention without changing the common-case (sub-millisecond)
  latency.
- `mergeTrainSlots`, a package-level semaphore capping concurrent
  merge-train real-git activity at 3 scenarios regardless of how many
  `t.Parallel()`-marked merge-train test functions exist. Measured directly:
  running all merge-train scenarios with unlimited concurrency (no
  semaphore) pushed individual scenario wall-clock to 40–70s apiece under
  `-race` and — more importantly — starved *other, unrelated* package tests
  badly enough to fail four of them
  (`TestCruiseFullPipeline_NonVacuous`, `TestRestartEnv_RoundTrip`,
  `TestYoloAutoMergeLabel`, `TestSmoke_FullPipelineToDone`) via
  `workerQuiescenceTimeout`/`AdvanceUntil` exhaustion in one full-package
  `-race` run, and drove the whole package to **~660s**. With the semaphore,
  `go test -race -count=1 ./tests/sim/...` (this package + `simgh` +
  `simclaude` + `simgh/ghfault`, run concurrently) measured **~106–130s** in
  clean runs — still a real increase over the ~84s baseline directly above,
  now the actual bottleneck rather than `simgh`. One further run, under
  unusually heavy external load on the measuring machine, saw a single
  *pre-existing, unrelated* test (`TestRebaseCycleLimit`) exhaust
  `workerQuiescenceTimeout` at ~440s total — the same environment-contention
  risk class this file's "worker-quiescence fix" section above already
  documents, not a new failure mode this issue introduced, but the
  semaphore's 3-way cap is a *mitigation* of that risk, not an elimination
  of it under genuinely adverse load.

**The "no `sim` build tag" decision is revisited, not reaffirmed, by this
issue.** ~90–130s (typical) to ~440s (adverse-load outlier) is no longer
"comfortably under" anything — it is argued to remain acceptable rather than
gated behind a build tag, for three reasons: (1) merge-train is exactly the
subsystem this package exists to make safe to test at all, per this issue's
own Problem statement — gating its only sim coverage behind an opt-in tag
would mean it silently stops running in ordinary CI; (2) the semaphore
already bounds the worst realistic case to roughly 2–3x the package's
pre-existing runtime, not open-ended; (3) `tests/sim/simgh`'s own ~90–100s
is already comparable in magnitude and already ungated. If this package's
total continues to grow with future additions, revisiting the build-tag
question is a reasonable future call — but not one this issue's own
Scope (which explicitly does not include changing this package's own testing
infrastructure beyond what its scenarios need) is positioned to make
unilaterally.

**Updated for #1454 (correcting stale figures).** The "~92s total" /
"comfortably under the ~90s line" conclusion two sections up predates the
merge-train suite's addition (#1452, directly above) and has been stale
since. Current measured runtime on a dev machine, recorded here as the
honest current numbers rather than re-derived on every future edit (per this
issue's own R8 — these figures are inherently machine- and load-dependent,
as this section's whole revision history demonstrates):

```
tests/sim               42.5s     the scenarios
tests/sim/simgh        105.0s     unit tests of the model itself
tests/sim/simclaude      1.1s
tests/sim/simgh/ghfault  1.1s
                        ~107s total wall clock
```

The scenario layer alone (42.5s) is the part that matters for a pre-gate —
it's the layer that actually exercises the pipeline — and it is plenty fast
for that purpose; `simgh`'s own 105s is the model's unit-test suite, not
pipeline coverage, and dominates the `--all` total the same way it has
throughout this file's history. `scripts/sim/run.sh` (repo root) is the
frictionless on-demand entry point this issue adds (R8): with no arguments
it runs the scenario layer only (`./tests/sim`, ~42.5s); `--all` runs the
full tree above (~107s). It's both the manual "run the sim layer by hand
before committing to a full live e2e run" entry point and what
`scripts/e2e/run.sh`'s pre-gate calls — see
`adrs/1454-sim-pre-gate-not-replacement.md` for the full layering decision.

**Updated for #1592's sequence-shaped scenarios** (8 new test functions across 4 new
files — gap 1's two dependency-unblock scenarios, gap 2's two backoff scenarios, gap
3's stale-worker-reap scenario, gap 4's three reinvoke-cycle-counter scenarios): all
eight, run standalone via `-run`, measure at **~5–6s** — none of them do real git work
beyond a single `EnsureWorktree` per scenario (`preCreateWorktree`,
`reinvokeCycleCountersEnv`), and none loop more than a handful of polls. `go test -race
-count=1 -skip TestMergeTrainAssembly_PoisonMatrix ./tests/sim/...` (this package +
`simgh` + `simclaude` + `simgh/ghfault`, run concurrently) measured **~80s for this
package, ~100s for `simgh`** — both within the ranges #1454's own entry directly above
already established as the package's current baseline, so #1592 does not move the "no
`sim` build tag" needle further. (`TestMergeTrainAssembly_PoisonMatrix` itself is
excluded from that figure as a pre-existing flake unrelated to this issue — confirmed
independently reproducible on an unmodified `main` checkout, tracked separately from
#1592's own scope.)

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

- `settleQueuedReviewFindings` (ADR-1208) — merge-train-specific; #1452's own
  scope covers the *standalone-validation-failure* reroute-off-Queued cause
  (`ejectRedSingleton`/`rerouteQueuedMemberOffHolding`, R9,
  `mergetrain_redsingleton_test.go`), which is `settleQueuedReviewFindings`'s
  structural sibling but not the same scan — the *PR-review-finding* cause
  `settleQueuedReviewFindings` itself owns (`ejectQueuedMemberForReviewFindings`,
  ADR-1208) remains untested at the sim layer, left as a follow-up.
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
explicit scope — filed as follow-up issues #1582 (`MarkPRReady` failure
never retried, can permanently strand a draft PR) and #1583
(`spawnChildren` retry after an operator un-pause duplicates
already-created children), linked from the covering scenarios' own doc
comments.

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

## Fidelity-drift policy (R4, #1454)

The sim bed's entire value as a pre-gate (see
`adrs/1454-sim-pre-gate-not-replacement.md`) depends on one property: it must not pass
what live e2e would fail. A sim that does is worse than no gate at all — it manufactures
false confidence at exactly the point where confidence is being relied on to justify
skipping (or later starting) a full live run.

**Rule:** any live-e2e failure that the sim passed is a fidelity bug in the sim, and is
fixed there as well as in the engine.

**Procedure**, when this happens:

1. **File a fidelity issue** describing the divergence — what live e2e caught that the
   sim's scenarios didn't, and why the sim's model (`simgh`) let it through.
2. **Add the scenario to `tests/sim`** so the same class of bug cannot silently pass here
   again — the same porting discipline #1450–#1452 already established for every other
   scenario in this package.
3. **Update `tests/sim/simgh/FIDELITY.md`** — the permanent, deliberate ledger of every
   place `simgh` knowingly departs from real GitHub — with the divergence found and what
   it costs. If you are relying on a sim-backed test to cover something subtle, check
   that file first; if you change the model, update it in the same commit.

This is a release-checklist step — recorded in `scripts/cut-release.sh`'s own header
comment and its live-gate failure message (a platform-level write restriction on
`.claude/skills/cut-release/SKILL.md` blocks recording it there too; see
`adrs/1454-sim-pre-gate-not-replacement.md`'s R4 section) — not something that depends
on someone remembering it after a bad live run. It does not retroactively
cover every gap the sim has — `simgh/FIDELITY.md`'s "Absent"-labeled entries are known,
accepted blind spots, not violations of this rule — it covers the specific, high-signal
case where live e2e actually caught something in production use that the pre-gate should
have caught first.

## Sequence-shaped coverage (#1592)

#1451 covered settle-scan retry-and-escalation, restart recovery, and
adversarial invoker shapes. #1592 covers a different class the sim exists
for just as much: behavior that only manifests **across multiple polls or
across two interacting items**, where reading a terminal label — the
convention every earlier scenario in this package otherwise follows —
cannot distinguish the correct sequence from a wrong one that happens to
land in the same final state. `TestNoWorkNeeded_AwaitingDoneIsFirstMutation`
(pre-dating #1592) is the canonical example this issue generalizes from: the
unit suite executes the same line and cannot say it came first.

The set below is defined by property, not enumeration, matching R1's own
framing and the settle-scan table above's convention: a mechanism belongs
here if the assertion that actually distinguishes correct from incorrect
behavior is an ordering or sequence claim, not a state claim. Four
mechanisms qualified when #1592 was filed — if a fifth is ever added
without a row here, that is the gap this table exists to make visible
instead of leaving it latent in prose.

| mechanism | covering scenario | seam needed |
|---|---|---|
| Dependency blocking/unblocking (`PushUnblockObserver`, `engine/observers.go`) | `dependency_unblock_test.go`: `TestDependencyUnblock_OnBlockerClose`, `TestDependencyUnblock_EmptyEdgeListDoesNotUnblockViaObserver` | `Engine.RegisterObservers` |
| GraphQL rate-limit backoff / REST hard gate (`engine/backoff.go`) | `backoff_test.go`: `TestBackoff_GraphQLRateLimitIntervalEscalatesAndRecovers`, `TestBackoff_RESTHardGateSkipsPollUntilReset` | `Engine.PollWithBackoff` |
| Stale-worker-label reaping (`forEachStaleUnworkedItem`, `engine/worker_liveness.go`) | `stale_worker_reap_test.go`: `TestStaleWorkerReap_OrphanedLockAndEditingLabels` | `Engine.RunStartupCleanup` + `RestartEnv` |
| Review/no-op reinvoke-cycle counters and their interaction (`ReviewCycleDecremented`/#1045/ADR-1518, `NoOpCommentCycles`/#1555) | `reinvoke_cycle_counters_test.go`: `TestReviewCycleDecremented_NoCommitRefundsIndefinitely`, `TestNoOpCommentCycles_TripsBreakerAndResetsOnProgress`, `TestReviewCycleVsNoOpCommentCycle_Invariant` | no new engine seam, but reuses `Engine.RegisterObservers` — see that file's own doc comment |

### Gap 1 — dependency blocking and unblocking

`PushUnblockObserver` fires on two distinct, asymmetric paths (R2): Path 1
(a blocker's own `StateChanged`, scanning the store for dependents) and
Path 2 (`BlockedByChanged` on the dependent's own snapshot, which
deliberately declines to act when the new `BlockedBy` list is empty —
ADR-1419's protection against a stale-empty cache falsely unblocking an
issue). `TestDependencyUnblock_OnBlockerClose` drives the close-and-unblock
sequence; `TestDependencyUnblock_EmptyEdgeListDoesNotUnblockViaObserver`
proves the empty-list guard holds and that recovery instead falls to the
`dep-blocked` cooldown-gated pull path in `checkDependencies` — the
behaviour #1453's chain work hit in practice.

`dependency_unblock_test.go`'s own doc comment records a `tests/sim`-only
wrinkle surfaced while writing the close-and-unblock scenario: without
`boardcache.CacheImpl` (which `tests/sim` deliberately never wires in — see
`NewWithDeps`'s own doc comment), the store's per-item `IsClosed` field is
kept fresh only for items individually admitted for a deep-fetch, not for
every board item the way production's `runProbeAndDeepFetch` keeps it. A
blocker parked on an `Unmanaged` column that later closes can end up with a
store entry that durably reads `IsClosed()==false` despite genuinely being
closed — no production consequence (this exact race cannot occur once
`CacheImpl` is in the loop), but reachable in this harness. The scenario
routes around it by filing the blocker as a plain repo issue never added to
any project board — never touched by any settle scan, and its real
open/closed state is always available via the dependency edge's own
`State` field regardless.

### Gap 2 — GraphQL rate-limit backoff and the REST hard gate

Driven entirely through `Engine.PollWithBackoff` (ADR-1592) — the control
surface question (R3) resolved to `SeedRateLimits`
(`tests/sim/simgh/seed.go`, #1457) over fault injection: `RateLimitStats` is
documented in `simgh/FIDELITY.md` as the one interface method fault
injection cannot fail, and `backoff.go` consumes budget *ratios*, not a
failing call. `TestBackoff_GraphQLRateLimitIntervalEscalatesAndRecovers`
drives the full escalation ladder (`>=10%`: 2x, `>=5%`: 4x, `>=1%`: 6x,
`<1%`: 10x configured interval) plus the two-threshold hysteresis sticky
zone (activates below 20%, clears only above 50%) across successive
`PollWithBackoff` calls. `TestBackoff_RESTHardGateSkipsPollUntilReset`
proves the hard gate skips `poll()` entirely — no `FetchProjectBoard` call
at all — while the REST budget is exhausted, and that a fresh
`SeedRateLimits` call (modelling GitHub's own hourly rollover) resumes it;
simgh's `RateLimitStats` always reports `Reset` relative to the current
injected clock instant, not a fixed timestamp a scenario could wait out.

### Gap 3 — stale-worker-label reaping

`forEachStaleUnworkedItem` reaps an orphaned `fabrik:locked:<user>` or
`fabrik:editing` label left behind when a worker dies mid-dispatch.
`TestStaleWorkerReap_OrphanedLockAndEditingLabels` reuses `RestartEnv`
(#1451) for the genuine process-state boundary the scenario needs — the
label is seeded directly on the pre-restart `Env` (no attempt to kill a
real dispatched goroutine mid-flight, which would be inherently racy to
land deterministically and isn't what `forEachStaleUnworkedItem`'s own
precondition, `Worker() == nil` plus the label present, cares about) —
then `Engine.RunStartupCleanup` (ADR-1592) reaps it on the restarted
engine's first poll. Both seeded issues also carry `stage:<Name>:complete`
from the start, isolating `RunStartupCleanup` as the only mechanism in the
sequence that could touch either label.

Deliberately scoped to the startup-cleanup shape only, not the periodic
`runWorkerDetectorScan` sweep: that scan is real-wall-clock-bound end to
end (`time.Since`, not the injected `Clock`) on both its heartbeat-staleness
and its PID-unset fallback checks, and every `tests/sim`-dispatched worker
has `PID == 0` (`simclaude` never calls `onPIDReady`), routing it
exclusively into the also-wall-clock-bound `StartedAt` branch — reachable
only via a genuine sleep, a narrower and more fragile addition for no real
gain over the startup-cleanup path this issue already needed.

### Gap 4 — the two reinvoke-cycle counters, and the invariant between them

Two distinct, previously-uncovered mechanisms observing the same
`processComments` funnel, plus the interaction between them (R6) — this
issue's largest single piece, requiring no new engine seam (both
mechanisms are reached through the ordinary `PollOnce`/catch-up-handler-
chain path every earlier scenario in this package already exercises). It
does reuse `Engine.RegisterObservers` (ADR-1592), for a reason discovered
only once the scenario was actually driven for more than one round: see
`reinvoke_cycle_counters_test.go`'s own doc comment on
`reinvokeCycleCountersEnv`.

- **4a — `ReviewCycleDecremented`** (#1045, ADR-1518,
  `dispatchReviewReinvoke`'s `after` hook, `engine/reviews.go`): a
  review-reinvoke that lands no new commit refunds its own prior
  `ReviewCycleIncremented` — the forgive-forever guarantee that keeps a
  chatty bot reviewer's non-actionable `COMMENTED` overviews from ever
  accumulating toward `MaxReviewCycles`.
  `TestReviewCycleDecremented_NoCommitRefundsIndefinitely` drives 3 rounds
  at `MaxReviewCycles=1` and proves none of them pause. The other
  direction — a commit-landing reinvoke does *not* refund — is not
  duplicated here: `TestReviewAuthorityCycleLimitPauses`
  (`review_authority_test.go`, pre-dating #1592) already commits a real
  marker file every round via `DefaultCommentScript`, and its own
  exact-`maxCycles`-then-pause behavior is itself the structural proof.
- **4b — `NoOpCommentCycles`** (`checkNoOpCommentCycle`, #1555,
  `engine/comment_noop_breaker.go`): a separate, never-refunded counter
  over the same funnel, resetting to zero (not merely decrementing) on any
  genuine progress. `TestNoOpCommentCycles_TripsBreakerAndResetsOnProgress`
  drives a `[NoOp, NoOp, Commit, NoOp, NoOp, NoOp]` script sequence and
  proves the breaker trips exactly on round 6, not round 5 — the
  round-5-not-yet-paused check is what pins reset-to-zero specifically,
  since a decrement-instead-of-reset regression would trip one round
  earlier while still eventually pausing.
- **4c — the interaction, and this gap's primary deliverable (R6):** the
  invariant that `NoOpCommentCycles`' default (10) is deliberately higher
  than `ReviewCycles`' default (5) *because* one refunds and the other
  never does — stated only in `effectiveMaxNoOpCommentCycles`'s doc comment
  before #1592, with no terminal-state test able to notice a regression
  that collapsed the two thresholds together (the issue would still pause,
  just for the wrong reason and at the wrong count).
  `TestReviewCycleVsNoOpCommentCycle_Invariant` drives one sustained
  no-op-reinvoke sequence through both halves: past `MaxReviewCycles=3`
  without pausing (4a's refund holding), then pausing exactly at
  `MaxNoOpCommentCycles=6` via the no-op breaker specifically — naming
  which limit tripped via each pause comment's own literal prefix, not
  merely that a pause occurred.

Two fidelity notes surfaced while building gap 4, recorded in
`reinvoke_cycle_counters_test.go`'s own doc comments: `simgh`'s
`FetchPRReviews` collapses same-author `COMMENTED` follow-ups
(`latestReviewsByAuthor`, modelling GitHub's own per-author reduction), so
each round's seeded review needs a distinct author, not merely a distinct
`DatabaseID`, or every round after the first is invisible to the engine;
and `dispatchReviewReinvoke`'s `build()` hook captures `headBefore` via
`gitHeadSHA` *before* `processComments` itself calls `EnsureWorktree` (see
`dispatchReinvoke`, `engine/reinvoke.go`), so a zero-Claude-cost
`seedReviewGateItem` construction needs its worktree pre-created
(`preCreateWorktree`) or round 1's own refund is silently skipped — a
confound specific to never having dispatched a real stage for the issue
first, not a defect in the refund mechanism itself.

### Fixture additions

- **`Sim.SeedRemoveBlockedBy`** (`tests/sim/simgh/seed.go`) — removes one
  `blockedBy` edge, modelling a human deleting the dependency link directly
  (GitHub UI, or the REST dependencies API). No `GitHubClient` interface
  counterpart: production only ever *adds* an edge (`AddBlockedByIssue`, at
  spawn time), never removes one. Deliberately does not bump the
  dependent's `updatedAt` — mirroring #977's real-API gap, and exactly the
  staleness gap 1's companion scenario needs to exercise the empty-list
  guard rather than the ordinary probe-driven refresh. See
  `simgh/FIDELITY.md`'s own entry for the full rationale.
- **`simclaude.NoOpCommentReview() CommentScript`** — touches nothing in
  `workDir` and signals no progress (`completed=false`, no commit, no
  `FABRIK_ISSUE_UPDATE` body) — the exact shape `processComments`'s own
  `progressed` check treats as a no-op cycle, which both 4a and 4b's
  mechanisms key off. Named distinctly from `DefaultCommentScript`/
  `CommentReviewCompleted` (both of which commit), mirroring
  `simclaude/scripts.go`'s existing naming convention.

## Running it

```bash
scripts/sim/run.sh                     # on-demand entry point (R8): scenario layer only, ~42.5s
scripts/sim/run.sh --all               # full tree (this package + simgh + simclaude + ghfault), ~107s
go test -race ./tests/sim/...          # equivalent to --all above, invoked directly
bash tests/sim/simgh/nonvacuity.sh     # the simgh mutation sweep (needs a clean tree)
```

The tests use the real `git` binary and skip when it is unavailable, following
the repo-wide `skipIfNoGit` convention.
