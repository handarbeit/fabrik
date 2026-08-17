# ADR 1624: Sim Suite Concurrency Bounds and Pre-Gate Dedup

**Date**: 2026-08-17
**Status**: Accepted
**Issue**: #1624 — sim suite intermittently hangs in git fork/exec, stranding gitMu and
leaking 20-hour CPU-burning orphans

## Context

During the v0.0.80 release gate, the sim suite (`tests/sim`) failed three distinct ways in
one day, on the same 28-core machine, all tracing to one root cause:

1. **A wedged `fork/exec`** — a goroutine stuck inside `syscall.forkExec` for 9 minutes,
   which strands the repo's `gitMu` for every other goroutine waiting on it (the lock
   holder never releases it, because it never got far enough to), until the whole suite
   dies at the 10-minute test timeout.
2. **A child `git` process killed outright** (`signal: segmentation fault`) — tests that
   pass cleanly standalone.
3. **A `-race` (ThreadSanitizer) runtime abort** — `CHECK failed: tsan_rtl.cpp:94`, the
   child process exiting 66.

All three share a common factor: `go test`'s default `-parallel` is `GOMAXPROCS` — on this
machine, 28 — and nearly every one of the suite's ~84 `t.Parallel()` test functions spawns
real `git` subprocesses (directly, or via `NewEnv`'s unconditional `SeedRepo` → `initBare`
call during setup). High-concurrency `fork/exec` from a heavily multi-threaded Go process is
a known source of exactly this trio of failure modes. None of it is proportional to how much
real work the suite is doing — it is purely a function of host core count, which means the
gate's reliability depended on which machine happened to run it. A bigger machine made the
same suite *less* reliable, which is backwards.

Separately, orphaned `sim.test` processes were found alive at ~97% CPU each, with ages up to
20+ hours — nothing had ever reaped a `go test` invocation's children when the runner
itself was killed mid-run.

Also separately, but compounding the exposure: `scripts/cut-release.sh` runs the sim +
wire-contract pre-gate as its own Step 3, then invokes `scripts/e2e/run.sh` at Step 5, whose
first act re-runs the identical pre-gate against the same unchanged tree — meaning a release
had to win the flaky suite's coin flip twice in a row to publish.

## Decision

### R1 — an explicit, host-core-count-independent `-parallel` cap

`scripts/sim/run.sh` — the sole entry point both manual runs and `scripts/e2e/run.sh`'s
pre-gate go through, so there is exactly one place this number lives — now passes
`-parallel "$SIM_PARALLEL"` (default **8**, env-overridable) instead of leaving `go test` to
inherit `GOMAXPROCS`. This is the actual root-cause mitigation: fewer concurrent scenarios
means fewer concurrent `fork/exec` calls, which is what makes the wedge, the SIGSEGV, and
the TSan abort rare in the first place.

**8 is a reasoned starting point, not an empirically re-tuned value.** It sits comfortably
below the 28-core host that produced the incident, close to (but not below) the interim
`GOMAXPROCS`-reduction mitigation used in production, and above the pre-existing
`mergeTrainSlots` semaphore (cap 3) that already throttles a narrower, heavier real-git
subset (merge-train scenarios specifically) — this cap is a suite-wide analogue of a pattern
that already existed in miniature. If a future high-core-count run still shows contention at
this value, or if it turns out to over-constrain a low-core CI runner, the number itself is
cheap to change; nothing else depends on its exact value.

This does not touch CI's own `go test -race -timeout 5m ./...` (unscoped `-parallel`) — CI
runners are 2-4 cores, well below where this class of contention appears, which is also why
the flake was effectively unreproducible there.

### R2/R4 — bounded, diagnostic git subprocess calls, with an explicit acknowledged gap

`tests/sim/simgh`'s three git-invoking helpers (`runGit`, `runGitAllowFail`, and
`runGitStdin` — the third, less-used sibling not named in the issue's own text but treated
identically since it's the very first git call every scenario makes) now run through a
shared `newGitCmd` builder: `exec.CommandContext` bound by a 30s `gitCommandTimeout`, with a
`WaitDelay` bounding how long `Wait` spends on I/O-pipe closure after the process is killed.
A timeout produces a diagnostic naming the git subcommand and its working directory (R4) —
before this, learning which invocation wedged required reading a goroutine dump.

**This does not, by itself, close the exact incident that Symptom 1 describes**, and that is
a deliberate, documented trade-off rather than an oversight. Go's `os/exec.Cmd.Start()` calls
`os.StartProcess` — the fork/exec syscall itself — **synchronously**, and only wires up the
context-cancellation watcher goroutine *after* `Start()` returns. Symptom 1's own goroutine
dump shows the hang *inside* that synchronous `Start()` call, before the watcher exists to
kill anything. So:

- `context.WithTimeout` + `exec.CommandContext` fully protects the post-`Start()` phase — a
  process that starts and then hangs (a stub `git` that sleeps, or a genuinely wedged real
  `git` invocation once it has begun running) is killed and reported. This is what AC1's
  proof scenario exercises, and what `tests/sim/simgh/git_timeout_test.go` pins as a
  permanent regression test.
- It does **not** preempt a hang during the fork/exec syscall itself — the literal failure
  point in the production incident.

We considered racing the entire `Start()`+`Wait()` call in a goroutine against the timeout,
so a caller could return early (releasing `gitMu` for other waiters) even while the
underlying OS-level fork stays stuck. We rejected this: it satisfies AC1 exactly as well as
the simpler fix, it does not actually kill the stuck fork/exec (it only relocates the leak
from "a stuck git process" to "a stuck git process plus a leaked goroutine holding a
reference to it," unprovable by any test), and it adds a more complex concurrency primitive
around `gitMu`'s already fragile "no `Sim.mu` while holding this" convention for a benefit
that is real but narrower than it looks. R1 (fewer concurrent forks, so the syscall-level
wedge becomes rare) and R3 (below) together are what actually address the residual case: R1
prevents it, R3 bounds the blast radius the rare time it still happens.

**Do not read "R2 is done" as "the Symptom 1 incident cannot recur."** It closes the
post-start hang class outright and gives every timeout a useful diagnostic; the pre-start
fork/exec wedge is addressed by R1 and R3, not by R2.

### R3 — process-group reaping in both runner scripts

Both `scripts/sim/run.sh` and `scripts/e2e/run.sh` now launch their `go test` invocations as
backgrounded jobs under bash job control (`set -m`), with an `INT`/`TERM` trap (`sim/run.sh`
also traps `EXIT`, since it has no competing trap installer; `e2e/run.sh`'s shared
`run_reaped` helper deliberately does not, since its own test-harness scripts `source` it and
install their own `EXIT` trap in the same shell) that sends `kill -TERM` to the negative PID
— the whole process group, not just the immediate child. Killing the runner (Ctrl-C, a CI
job cancellation, a plain `kill`) now reaps `go test`'s own children (concretely, the
compiled `*.test` binary — the literal orphans in the issue's evidence) instead of leaving
them running headless.

**Portable bash job control, not `setsid`.** `setsid(1)`, the usual Linux idiom for giving a
child its own session/process group, is not shipped on macOS by default (this environment,
and the machine that produced the incident, are both Darwin). `set -m` plus a backgrounded
job achieves the same thing — the job becomes its own process group leader, and any further
children it forks inherit that group — without an external dependency, on both platforms.

`scripts/e2e/run.sh`'s main suite invocation (`switch_and_run`) is the one call site that
needs a different shape: it is a multi-stage pipe (`go test -json | tee | jq`) whose exit
code capture already depends on `pipefail`, and naively backgrounding only the pipe's last
stage would make `$!` resolve to `jq`'s PID, not `go test`'s — silently breaking both the
reap and the exit-code capture. It uses process substitution (`exec 3> >(tee ... | jq ...)`,
`go test ... >&3 2>&1 &`) instead, so `$!` (captured right after backgrounding `go test`) is
`go test`'s own PID, preserving the existing log-capture and `wait ... || rc=$?` exit-code
discipline unchanged. (This exact file has a prior incident from getting a similar
restructuring wrong — see #1547 — which is why this shape was chosen deliberately rather
than by the more obvious but fragile "just background the pipeline" route.)

The process substitution is opened on an explicit fd (3) rather than inline on the `go test`
command specifically so its own PID can be captured and `wait`ed on separately from `go
test`'s: `wait "$suite_pid"` only blocks until `go test` exits, not until the `tee`/`jq`
consumer reading its now-closed pipe has finished flushing `$jsonlog` to disk. During review,
the first version of this fix backgrounded `go test` with the process substitution written
inline (`go test ... > >(tee ... | jq ...) 2>&1 &`) and captured only `go test`'s PID — this
reproducibly raced `report_test_timings`/`report_test_outcomes`/the `E2E_TIMEOUT` grep
against a still-writing consumer (confirmed with a deliberately slow consumer: the log file
was empty or truncated immediately after `wait` returned, and only complete after an
additional, unbounded delay). Opening the substitution on fd 3 and `wait`ing on its PID too,
after `wait "$suite_pid"`, closes that gap.

### R5 — a tree-scoped pre-gate dedup signal, not a blanket opt-out

`scripts/cut-release.sh` already runs the sim + wire-contract pre-gate unconditionally as
its own Step 3. Step 5 then invokes `scripts/e2e/run.sh`, whose own `run_pregate` re-ran the
identical checks against the same, unchanged tree — doubling the release's exposure to
whatever flake the pre-gate might hit, for zero additional coverage (HEAD is provably
unchanged between the two steps; `cut-release.sh` commits nothing until well after Step 5).

The fix is `FABRIK_PREGATE_VERIFIED_SHA`: an environment variable, exported by
`cut-release.sh`'s `export_pregate_verified_sha` immediately after its own Step 3 pre-gate
passes, carrying the exact `git rev-parse HEAD` that was just verified. `run.sh`'s
`run_pregate` checks this against its own freshly-resolved `HEAD` before running anything; a
match skips the pre-gate, anything else (unset, or a mismatch) falls through to the full
run.

This was chosen over two alternatives:

- **Reusing `E2E_SKIP_PREGATE`** — the existing, deliberately blanket escape hatch that
  `cut-release.sh`'s own header comment says it "never sets." Doing so would conflate "I
  know this pre-gate is redundant for this specific tree" with "skip the pre-gate,
  unconditionally, for whatever reason" — the latter is meant for interactive iteration
  only, and giving it a second, silent caller inside the release path would erode that
  distinction.
- **A file marker on disk** — introduces staleness and cleanup concerns (what deletes it? on
  what trigger? what if a prior failed run left one behind?) that an env var doesn't have.
  `cut-release.sh` invokes `run.sh` as a direct child process, so an exported env var is
  naturally scoped to exactly this invocation — it cannot leak into a later, unrelated
  invocation the way a file could.

Critically, `run_pregate` **re-resolves `HEAD` itself** rather than trusting the caller's
claim — a stale or mismatched value always falls through to the full pre-gate, never a
silent false skip. A standalone `scripts/e2e/run.sh` invocation (outside `cut-release.sh`)
never sees this variable at all, so it continues to pay the full pre-gate price exactly as
before this existed — the original policy for that case is unchanged.

## Consequences

- The sim suite's reliability no longer depends on the host's core count — a developer on a
  high-core-count machine gets the same `-parallel` bound as CI, rather than a worse one.
- A wedged git subprocess now fails the specific test it belongs to, with a diagnostic
  naming the command and directory, rather than hanging the entire suite to its 10-minute
  timeout — for the class of hang that occurs after the process has started. A hang inside
  the fork/exec syscall itself is not directly preemptable by this fix; R1 makes it rare, R3
  bounds its cost when it still happens.
- Killing either runner script mid-suite leaves no surviving `*.test` processes, closing the
  20-hour-orphan failure mode.
- A release cut through `cut-release.sh` now pays the sim + wire-contract pre-gate's cost,
  and its flake exposure, exactly once instead of twice — but this is purely an efficiency
  and exposure win layered on top of R1-R4, not a substitute for them. A standalone
  `scripts/e2e/run.sh` invocation is unaffected.
- `SIM_PARALLEL=8` and `gitCommandTimeout=30s` are both reasoned defaults, not values proven
  optimal by exhaustive testing — see the R1 and R2/R4 sections above for the reasoning
  behind each, and revisit them if future evidence (a still-flaky very-high-core-count run,
  or a genuinely slow git operation timing out falsely) suggests otherwise.
