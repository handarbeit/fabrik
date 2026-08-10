# ADR 1449: Sim Harness Engine Seams

**Date**: 2026-08-10
**Status**: Accepted
**Issue**: #1449 — Wire `tests/sim/simgh` into a real `Engine`, alongside a scripted
`ClaudeInvoker` and a deterministic poll-advancement harness

## Context

`tests/sim/simgh` (#1457) is a complete, standalone `engine.GitHubClient` implementation —
stateful, git-backed, with fault injection and a mutation log — but nothing constructed an
`Engine` against it. Doing so needed two things the engine did not yet expose to an external
test package: a way to run one poll cycle deterministically, and a way to construct the
failure shapes the engine classifies specially from a Claude invocation. Building a scripted
`ClaudeInvoker` that could act on a real worktree, and a harness that could drive `Engine.Run()`
without wall-clock waiting, was blocked on both.

## Decision

### Two production seams, both additive

**`Engine.PollOnce(ctx) error`** (`engine/poll.go`) is a thin delegation to the existing
unexported `poll`. The only other driver, `Engine.Run()`, loops on a real ticker until its
context is cancelled — looping `Run()` against a tiny `PollSeconds` and cancelling after a
while would reintroduce exactly the wall-clock nondeterminism a poll-advancement harness exists
to eliminate. `PollOnce` deliberately does **not** replicate `Run()`'s preamble:

- the exclusive `.fabrik/fabrik.lock` flock (cross-process mutual exclusion — replicating it
  would make parallel test scenarios contend with or deadlock each other);
- opening `.fabrik/fabrik.log` and assigning the package-level `pollLogFile` global (unsafe
  the moment two scenarios run in parallel);
- startup stage-drift/env warnings (diagnostics with no bearing on a single poll cycle).

`poll()` itself depends on none of this — confirmed by reading its body before making the
change. `PollOnce` returns `error` only, never the unexported `pollResult`: the harness's
diagnostics (board state, mutation log) come from `simgh`'s own state and the `Instrumented`
wrapper it's already holding, not from anything `poll()` returns, so exposing `pollResult`
would widen the seam for no benefit.

**`internal/claudeerr`** holds the four Claude failure-classification error types
(`UsageLimitError`, `TurnLimitError`, `APIErrorExit`, `ResumeFailureError`), moved out of
`engine/claude.go`. It mirrors `tests/sim/simgh/ghfault`'s precedent from #1457, with one
difference in kind: `ghfault` constructs synthetic GitHub-side faults for test fixtures; this
package holds the real error types the engine raises and classifies via `errors.As` in
production. That distinction is also why it lives under `internal/` rather than under
`tests/`: `engine/item.go` imports these types unconditionally in production, not under a test
build tag, so putting them under the test tree would make the production build depend on
`tests/`.

**The extraction is a pure move, using type aliases, not a rename.** `engine/claude.go` keeps

```go
type claudeUsageLimitError = claudeerr.UsageLimitError
type claudeTurnLimitError = claudeerr.TurnLimitError
type claudeAPIErrorExit = claudeerr.APIErrorExit
type claudeResumeFailureError = claudeerr.ResumeFailureError
```

A Go type alias makes the two names identical types, so every existing construction site and
every existing `errors.As(err, &claudeUsageLimitError{})` assertion in `engine`'s own test
files — roughly 74 references across eleven files — keeps compiling and behaving unchanged,
confirmed by running the full `go test -race ./engine/...` suite with **zero edits to any
existing test file**. This is a stronger guarantee than a mechanical rename would have given,
and it is the technique worth reusing for a future leaf-package extraction under similar
constraints: when the goal is "move the type's location without being able to touch its
existing call sites," alias the old (possibly unexported) name to the new one rather than
updating every reference.

`tests/sim/simclaude` constructs the exported `claudeerr.XxxError{...}` form directly;
`errors.As` in `engine/item.go` matches it transparently because it is the same type by alias.

### One engine-local clock seam — narrower than it first looked, then one call site wider

`Engine.Clock`/`SetClock`/`now()` (`engine/clock.go`) is structurally identical to
`simgh.Clock` (`Now() time.Time`), so a scenario can share one clock instance across both via
`simgh.WithClock` and `Engine.SetClock`, keeping GitHub-anchored and engine-local timing in
lockstep under a single `Advance(d)` call.

Research enumerated the scope as exactly `itemstate.CooldownAt`'s stamping/reading call sites
(`poll.go`, `catch_up_handlers.go`, `item.go`, `spawn.go`) — every other named gate
(`FABRIK_REVIEW_WAIT_TIMEOUT`, the CI settle scan, the Done-archive scan) is anchored on a
`GitHubClient`-read timestamp (`FetchLabelAppliedAt`) that `simgh` already controls with no
engine change. That enumeration was re-confirmed by grep during Implement and found to be
short by three sites within the same `CooldownAt` family (`item.go`'s "dep-blocked" and
"periodic-re-eval" writes, and `poll.go`'s cooldown-check read) — all folded into the same
seam, since they were always the intended scope, just under-enumerated.

**A fourth, genuinely new site was discovered empirically while building the AC6 review-wait-
timeout scenario**: `recordLabelAppliedAtNow` (`engine/mutate.go`, #1314) — an in-memory
write-through cache that `labelAppliedAt` prefers over a live `FetchLabelAppliedAt` call
whenever it already holds a value. That cache was stamped with real `time.Now()`, so a
scenario backdating `simgh`'s `FetchLabelAppliedAt` response had no effect: the engine's own
cache, recorded fresh the instant it first applied `fabrik:awaiting-review`, always won. The
mutation log made this directly diagnosable — `checkAwaitingReviewTimeout` kept evaluating
"not yet elapsed" indefinitely despite a 24-hour-backdated `simgh` timestamp, because the cache
had already recorded a real, current timestamp first.

This is treated as a natural extension of the already-approved seam, not a new one: it is one
more call site (grepped for exhaustiveness — `LabelAppliedAtRecorded{...At: time.Now()}`
appears nowhere else) using the same `e.now()` mechanism the seam already provides, converting
real time to injected time at a point R3 named as a timing-gate anchor
(`FABRIK_REVIEW_WAIT_TIMEOUT`) without research having anticipated the caching layer sitting in
front of it. Confirmed byte-identical to prior behavior (unset clock) by the full
`go test -race ./engine/...` suite.

### Git origin: a local clone of `simgh`'s bare repo, not `simgh`'s bare repo directly

Production's `WorktreeManager.baseDir` is itself a bare *clone* of the real GitHub remote —
worktree pushes run `git push origin <branch>` from inside a worktree, relying on the
`remote.origin.url` that `git clone --bare <URL>` set up on that clone. The harness reproduces
this exactly: `git clone --bare <simgh-repo-bare-dir> <local-bare-dir>` (a local filesystem
path standing in for the network URL), then constructs the real `*engine.WorktreeManager`
against `local-bare-dir`. This is real git creating a real `origin` remote pointing at
`simgh`'s backing repo — no `WorktreeManager`/`ensureBareClone` production code changes, and
pushes are genuinely observable by reading `simgh`'s repo afterward. Requires
`Sim.RepoBareDir` (`tests/sim/simgh/misc.go`), an exported accessor for `simgh`'s
already-existing private path formula — test-only, additive, living entirely under `tests/`.

### What stays real wall-clock time, deliberately

Not every timing-sensitive behavior is clock-controllable, and the harness does not pretend
otherwise:

- **`acquireLockAndVerify`'s multi-instance lock-verify delay** (`engine/item.go`'s
  `lockVerifyDelay`, a real 2s `time.Sleep`) exists to let a *different Fabrik process* observe
  a competing lock. There is nothing for a Clock seam to represent here — it is about
  wall-clock scheduling across OS processes, not simulated time within one.
- **The stage retry cooldown** (`processItem`'s `time.Since(lastAttempt) < cooldown` check) is
  stamped via real `time.Now()` at `handleUsageLimitExit`/`handleAPIErrorExit` and the ordinary
  turn-limited path alike, deliberately left outside the Clock seam's scope — it is a
  dispatch-pacing safety valve, not a gate `simgh` needs to observe.
- **The account-wide Claude usage-limit suspension**
  (`claudeUsageLimitFallbackBackoff`, a real 1-hour backoff, ADR-1120) is likewise real-time
  anchored. The harness does not fast-forward it; a scenario needing to clear it applies the
  documented `fabrik:clear-claude-limit` operator escape hatch instead of waiting.

`RunPoll` (`tests/sim/poll.go`) pauses for a small, fixed real duration (`workerYield`, 100ms)
after every `PollOnce` call, so the background goroutines `poll()` dispatches get real
wall-clock time to progress before the next poll — without it, a tight `PollOnce` loop can
out-run a dispatched worker's progress indefinitely, confirmed empirically: an early version of
the smoke scenario ran 30 poll iterations in 0.16s, well under the dispatched worker's
still-in-flight 2-second lock-verify sleep. This is a deliberate, narrow, documented exception
to R4's "a scenario must never depend on real sleeping" — that requirement targets sleeping
past a GitHub-side async event the scenario cannot otherwise observe; it says nothing about the
engine's own dispatch concurrency, which is a property of driving the *real* engine rather than
a stub.

## Consequences

- `tests/sim` (harness), `tests/sim/simclaude` (scripted invoker), and `internal/claudeerr`
  are new packages; `engine/clock.go` and `Engine.PollOnce` are new, additive engine surface.
  No existing call site's behavior changes; both extraction and both new seams are confirmed
  byte-identical to prior behavior by the full existing `engine` test suite.
- A scenario now reads close to a live `tests/e2e` scenario (see `tests/sim/README.md`'s
  vocabulary-mapping table, R5) but runs in seconds, not against real GitHub or billed Claude
  tokens.
- The two "real wall-clock time, deliberately" categories above (multi-instance lock-verify,
  stage retry cooldown) mean this harness's own scenarios are not zero-real-time even where
  the *engine's* behavior under test is deterministic — this is why `tests/sim`'s own test
  functions use `t.Parallel()` and generous `maxPolls` budgets: the dominant cost is real
  wall-clock pacing, which parallelizes well since it contends on neither CPU nor shared state,
  not CPU work that would not.
- A future contributor adding a new timing gate to the engine should check whether it is
  GitHub-anchored (already `simgh`-controllable) or engine-local (needs `e.now()`) *and* check
  for an in-memory cache sitting in front of any `FetchLabelAppliedAt`-style read — `#1314`'s
  cache is precedent that such a layer can exist without being obviously part of "the gate."
