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

`RunPoll` pauses for a small, fixed real duration (`workerYield`, 100ms)
after every `PollOnce` call so these background goroutines get real wall-clock
time to progress — see `poll.go`'s doc comment. This is the dominant
contributor to this package's own runtime; see Runtime below.

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
| `WaitForProjectStatus(t, env, repo, issueNumber, columnName, timeout time.Duration)` | `WaitForProjectStatus(t, env, issueNumber, status string, maxPolls int)` | No `repo` param (as above). `timeout time.Duration` → `maxPolls int`: the live harness waits on real GitHub's own clock; the sim harness waits on `AdvanceUntil`'s poll count, driven by the shared `Clock` and `workerYield`, not wall-clock deadlines. |
| `WaitForIssueLabel(t, env, repo, issueNumber, label, timeout)` | `WaitForIssueLabel(t, env, issueNumber, label, maxPolls int)` | Same two divergences as above. |
| `WaitForLabelAbsent(t, env, repo, issueNumber, label, timeout)` | `WaitForLabelAbsent(t, env, issueNumber, label, maxPolls int)` | Same. |
| `IssueLabels(t, env, repo, issueNumber) []string` | `IssueLabels(t, env, issueNumber) []string` | No `repo` param (as above); otherwise identical. |
| *(none)* | `WaitForIssueClosed(t, env, issueNumber, maxPolls int)` | New — no live-harness equivalent. Added because AC1 requires driving to "a closed issue in the Done column," and every scenario in this package needs to assert that explicitly rather than infer it from a label. |
| *(none)* | `RunPoll(t, env)` / `RunPolls(t, env, n)` / `AdvanceUntil(t, env, cond, maxPolls)` | New — the live harness has no equivalent because it never drives poll cycles itself; it waits on wall-clock timeouts against a daemon it doesn't control. This is R4's deterministic poll-advancement vocabulary. |

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

Comfortably under the ~90s line R8 sets, so **no `sim` build tag** — this
package is part of the default `go test ./...` (and CI's `go test -race
-timeout 5m ./...`), guarded only by the existing `skipIfNoGit` idiom, exactly
like `tests/sim/simgh` already is.

`tests/sim/simgh`'s own pre-existing suite (~65–76s, unaffected by this
issue) runs as a separate package and is not part of this measurement, though
`go test -race ./tests/sim/...` runs both concurrently — Go parallelizes
across packages by default — so the wall-clock cost this issue actually adds
to a full run is closer to "the slower of the two," not their sum.

## Running it

```bash
go test -race ./tests/sim/...          # the suite (this package + simgh + simclaude)
bash tests/sim/simgh/nonvacuity.sh     # the simgh mutation sweep (needs a clean tree)
```

The tests use the real `git` binary and skip when it is unavailable, following
the repo-wide `skipIfNoGit` convention.
