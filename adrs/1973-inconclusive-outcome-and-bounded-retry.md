# ADR 1973: A third live-test outcome, INCONCLUSIVE — declared by the test, retried a bounded number of times, never coverage

## Status

Accepted (#1973). Builds on ADR-1972 (the per-SHA ledger and its open `Outcome` enum, where INCONCLUSIVE was reserved) and ADR-1994 (the Go gate runner and its `Classify`/`RunLeg` seams). Consistent with ADR-1454: nothing here moves live coverage to the sim, and an INCONCLUSIVE test is **uncovered** — it must still pass live before a release.

## Context

Several live tests need the engine to pass through a particular intermediate state, and they already detect when it never did (a poll boundary straddled the release; the cold-cache line never appeared; the landing decision was never reached with the comment pending). They fail with "this is a harness race, not an engine regression — re-run". Failing is right in one sense — a vacuous test must never pass — but it files a harness race in the same bucket as an engine regression, and each costs a manual triage and a manual re-run. On 0.0.83 all three fired and the engine was correct every time.

The pre-gate has a sibling problem: under host load the `-race` sim suite sometimes dies from the known TSan fork/exec crash (ADR-1624, ADR-1677). That is not an engine verdict either, yet it hard-stopped the whole gate.

The usual cure — retry failures — is the wrong one: it masks real regressions (`gotestsum --rerun-fails`, Bazel flaky retries, pytest `rerunfailures`). The test itself knows the difference between "the engine did the wrong thing" and "the engine was never put in the situation under test".

## Decision

### 1. The test declares it; nothing is retried because it failed

`e2e.Inconclusive(t, format, args...)` ends a test with `t.Skip("E2E-INCONCLUSIVE: <reason>")`. The marker is a string both the tagged live tests and the untagged gate runner must agree on, so it lives in `tests/e2e/inconclusive` (untagged; `tests/e2e/registry` is the precedent) rather than being duplicated. `IsMarked` matches only a skip message that **starts** with the marker, so a log line quoting it, or an ordinary skip mentioning it, is not a declaration. The helper must run on a top-level test's own goroutine (`t.Skip` ends a test via `runtime.Goexit`, and the runner reads only a top-level test's own output — subtests fold into their parent), so a call from a subtest is a loud `t.Fatalf` rather than an invisible marker.

A skip, rather than a new test status, because `go test -json` has none: the marker rides in the output that `skipMessage` already extracts for the ledger. One predicate, `isInconclusiveSkip`, is shared by `Classify` and the ledger recorder so they cannot disagree.

### 2. Convert only precondition guards; keep every assertion a failure

A guard converts only when it fires **before any assertion about engine behaviour** and means the precondition the scenario needs never arose. Assertions about what the engine did stay `t.Fatalf`. Guards that ask the operator to fix bed *state* ("clear the Queued column and re-run") are not converted — a retry would hit the same state. A setup *error* is not "a precondition that never arose" (it may be a permanent harness bug a retry would turn into "uncovered"), and the landed-comment post failure is a known engine defect (#1275) a retry would mask; both stay failures. The full per-guard disposition is in `tests/e2e/README.md`.

Where the awaited log line sits inside the shared `waitForLogMatch`, a second entry point, `waitForLogMatchInconclusive`, converts only the *timeout*; `waitForLogMatch` itself keeps `t.Fatalf`, because for most callers "the line never appeared" is the regression. Cleanups that key on `t.Failed()` (`repauseOnFailure`) now also run for a skip, so the retry does not inherit un-paused Queued members.

The pre-existing `INCONCLUSIVE:` fatals (`label_events.go`, `checkrun_timing.go`, `mid_stage_label_test.go`) were the same idea under a second, conflicting meaning of the word. They are migrated at the call sites (`failOrInconclusive`) so the word has one meaning; the pure checkers keep their error prefix, which their unit tests pin and which `failOrInconclusive` switches on.

`tests/e2e/inconclusive/guards_test.go` pins the structure statically (it parses the tagged sources): the three named guards use an inconclusive helper, each still has assertion calls after it, and `Inconclusive` is never reached from a goroutine or subtest. A live neutralised-assertion check cannot run without a bed and is recorded manually in the PR.

### 3. Bounded retry, in-leg, before the watchdog and the RUN INVALID scan

At the end of a leg, `retryInconclusive` re-runs only that leg's inconclusive tests — one anchored `-run` per attempt (`rewriteSelection`), same cell, `-parallel`, env and bed, no restart — at most `E2E_INCONCLUSIVE_RETRIES` (default 2; 0 disables) times, each attempt in its own `go-test.retry-N.json`. The same recorder stays attached, so the append-only ledger's "latest record wins" makes a retry PASS supersede the INCONCLUSIVE and a retry FAIL supersede it as FAIL, with no format change.

It lives inside `RunLeg`, between the first suite exit and the post-suite tail, because (a) the 300 s post-suite watchdog bounds seconds of bookkeeping and would kill a legitimately long retry, and (b) the RUN INVALID scan must see throttling that happens *during* a retry. A wrapper that re-entered the suite step would have to repeat the bed restart. No retry follows a timeout kill (the bed state is unknown), for a subtest-filtered cell (rewriting to top-level names would widen it), or once the engine's rate-limit backoff has engaged. A retry that goes red stops the loop and the leg's exit code is that retry's.

### 4. Uncovered is not failed — and never reads as success

What stays inconclusive is recorded `INCONCLUSIVE`: `Snapshot.Covered` is false, `ResumeCells` re-runs it, the summary's `inconclusive` column counts it. Because it makes `go test` exit 0 (it is a skip), a leg that ends with leftovers would otherwise let the invocation read as success. `Gate.withLeftInconclusive` turns "otherwise clean, but N tests stayed inconclusive" into `ExitCoverageIncomplete` (8) — with or without `--resume`, ledger on or off — and names them. A genuine failure keeps its own, more specific, code. Exit 8 already exists in `scripts/cut-release.sh`'s `interpret_e2e_exit_code`, and `cut-release.sh` re-checks coverage after its resume run, so a release cannot be cut on INCONCLUSIVE; the 3/4/5/6/7 contract is untouched (only the exit-8 message gains a sentence).

Each leg prints the count and the **names** of its inconclusive tests — including those that passed on retry — and, above `E2E_INCONCLUSIVE_WARN` (default 3), a warning pointing at #1974, which fixes the underlying races. Retries spend live budget; they fall inside the leg's budget-before/after window, so the budget line reports their sum.

### 5. The pre-gate crash is retried once, on a narrow, vetoed signature

`runPregateStep` scans each pre-gate step's output as it streams (`crashScanner`, per line, one partial line held — the sim suite is verbose and nothing needs it buffered). A failing step is re-run once iff the output carries a TSan `CHECK failed … tsan_*.cpp` abort, or a line naming `git` with `signal: segmentation fault`, **and** no veto: any `panic: `, `fatal error: `, `WARNING: DATA RACE`, `goroutine N [` or Go's own `[signal SIGSEGV` (an engine crash) anywhere in the output. A bare "SIGSEGV" or "fatal error" never matches. The signature fails closed — an unrecognised crash is the same hard stop (exit 5) as before — and a false match costs at most one wasted run, because the step must pass *in full* the second time: a retry can never turn a genuine failure green.

The ledger is not open when the pre-gate runs (it needs the engine SHA, resolved at bed preflight), so the retry is recorded under the existing `pregate/` directory: a note — step, signature, load average, outcome — appended to `pregate/<head>.retries.jsonl` whether or not the retry passed (a retry that fails again writes no pass record, so it needs a home of its own), and `retried`/`load_avg` on the pass record, shown in the coverage summary's pre-gate line.

## Consequences

- A harness race no longer costs a manual re-run, and no longer pretends to be an engine failure; but it is also **never green** — it is uncovered until it passes live.
- The retry re-runs a scenario against a bed that holds the first attempt's leftovers (landed members on `main`, closed issues). A retry that trips a stale-state pre-flight is a FAIL, not a hidden transient — visible, and the reason stale-state guards are not converted.
- Over-conversion would mask regressions. The defences are the "before any assertion" rule, the per-guard audit in the README, the AST pin test, the retry cap, and exit 8.
- **Fixtures for the crash signature are synthetic** (`tests/gate/testdata/pregate/README.md`): the repository holds no recorded real log, only the ADR prose. The signature fails closed, so an unrecognised real crash stays a hard stop; a real log, if captured, should be added as a fixture.
- The marker is a convention enforced by tests on both sides, not by the type system; drift would make INCONCLUSIVE tests classify as ordinary skips (uncovered as "missing", never green).
