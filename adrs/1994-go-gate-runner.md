# ADR 1994: The live e2e gate runner is a Go program; `run.sh` and `reset.sh` are shims

## Status

Accepted (#1994). Companion to ADR-1454 (the sim is a pre-gate, never a replacement), ADR-1624 (pre-gate dedup, process-group reap), ADR-1676 (hang watchdogs), ADR-1933 (the per-test registry) and ADR-1989 (session-wide reap). It does not supersede them: their intent is unchanged, and this records how their *mechanisms* moved.

## Context

The live release gate was orchestrated by `scripts/e2e/run.sh` — about 2,200 lines of bash with 29 functions — plus `reset.sh` and nine bash tests that `source` `run.sh` to call its functions. A large share of that code worked around bash rather than describing the gate: a named-pipe `tee | jq` consumer and the logic to drain it, a post-suite watchdog with a checkpoint file, process-group reaping of descendants reparented to PID 1, and `with_timeout`/`run_reaped` deadline guards (the biggest bash test, `hang_hardening_test.sh`, exists to keep those from hanging). The rest parsed `go test -json` with `jq`, scraped engine logs, and passed state between steps in files and environment variables.

The follow-on chain (#1972 coverage ledger, #1973 INCONCLUSIVE, #1974 probes, #1975 sparse matrix, #1976 multi-bed, #1977 two-phase legs) adds a great deal to this orchestrator. Building that in bash would have made the most fragile piece of the gate bigger at exactly the point it most needs to be reliable. In Go, most of it is ordinary code: `go test -json` events decode into structs, concurrency is goroutines, deadlines are contexts, and process trees are reaped by #1989's `internal/sessionreap`.

## Decision

1. **Port, don't redesign.** `scripts/e2e/run.sh` and `reset.sh` are replaced by a Go program — library `tests/gate`, `main` in `tests/gate/cmd/gate` — that does what they did, with no intentional behaviour change. The exit-code contract `scripts/cut-release.sh` interprets (3 budget exhausted, 4 preflight failed, 5 pre-gate failed, 6 post-suite watchdog, 7 precondition failed, otherwise the suite's own code) and every flag and `E2E_*`/`FABRIK_*`/`PRUEFER_DIR` environment variable keep their meaning. `tests/gate/README.md` lists every `run.sh` function and where it went, and every bash test case and its Go counterpart.

2. **Location: `tests/gate`, not `tests/e2e/gate`.** Live legs run `go test -tags=e2e … ./tests/e2e/...`. Anything beneath that pattern would be swept into every leg — including each of the two 3-minute `TestSwitchTrainMode` invocations — and into the per-leg `-json` outcome report, which is the thing the acceptance criterion compares. Placing the runner beside `tests/e2e` leaves the live legs' package set and report unchanged. (`tests/e2e/registry` already rides along today and is untouched.) The runner compiles with no build tag, so ordinary `go test -race ./...` builds and tests it on every PR; it is not part of the `fabrik` binary.

3. **Shim mechanism: build, then `exec`.** `scripts/e2e/run.sh` runs `go build -o <dir>/gate.$$ ./tests/gate/cmd/gate`, renames it into place, and `exec`s `gate run "$@"`; `reset.sh` does the same with `gate reset`. Not `go run`: `go run` starts the program as a child and does not forward INT/TERM to it, so the runner — the thing that must reap its `go test` trees — would never hear the signal, which is precisely the orphaned-`sim.test` class #1624 and #1989 closed. Build-then-exec is always fresh (it builds from the tree in front of you, never a stale cached binary), uses the Go build cache so an unchanged tree costs about a second, needs only the Go toolchain `cut-release.sh` users already have, and leaves the runner as the signal recipient. The rename is atomic, so a concurrent or still-running gate is never disturbed by a rebuild. `E2E_GATE_BIN_DIR` overrides the (per-user, under `$TMPDIR`) build directory.

4. **Every subprocess goes through a `Commander`.** `OSExec` is the real one; tests substitute a fake, so none of the runner's tests need a bed, a network or a `gh` login. A command that can fork a tree (`go test`, `gh`, the pre-gate) runs with `Cmd.Session`: its own session, so its PID is its SID, tracked by `sessionreap.Track` and reaped by `sessionreap.Escalate` on cancellation or timeout. Short `git`/build commands stay in the gate's own session so a tty prompt (an ssh passphrase) still works.

5. **Mapping of ADR-1624/1676/1989's mechanisms.**

   | bash | Go |
   |---|---|
   | `run_reaped` (backgrounded job, `kill -TERM -pgid`, INT/TERM traps) | `OSExec.Run` with `Session`; `main` turns INT/TERM into a cancelled context, which reaps every child, then exits 128+signum |
   | `with_timeout` (backgrounded command + `sleep` watcher + marker file; the `$(…)` pitfall) | `Cmd.Timeout`; `Result.TimedOut` replaces exit 124 |
   | named-pipe `tee`-to-`jq` consumer, `drain_output_consumer` (#1694) | the runner copies the child's output pipe itself and bounds the wait for EOF with `Cmd.WaitDelay`; `Result.PipeWedged` replaces the abandoned-drain return |
   | post-suite watchdog (`sleep` watcher signalling `$$`, checkpoint file, `_post_suite_watchdog_signal`) | a timer in `RunLeg`'s `select` over the tail goroutine; the checkpoint is an `atomic.Value`; firing prints the same diagnostic, cancels the tail (reaping what it waits on) and returns exit 6 |
   | stall detector (`stat` mtime poll in a subshell) | a ticker comparing the time of the last output write |
   | `jq`/`column -t` reports | typed `test2json` events (`Event`, `Classify`, `Timings`), golden-tested against the original output |

6. **Seams for the chain (structure only).** `Cell` (a leg) and a `Scheduler` that runs them (`SerialScheduler` today); `LegResult`, delivered to `Gate.OnLeg`, carrying the decoded event stream; `Classification`, where #1973 adds INCONCLUSIVE; an ordered `[]Preflight` for #1974's probes; and an injectable `Commander`, environment, clock and sleep. None of the chain's features exist yet.

## Deliberate behaviour deltas

The port is behaviour-preserving except where bash's process model forced otherwise or a bash quirk was a hazard. Each is listed so nothing is silent (the full list, including quirks kept on purpose, is in `tests/gate/README.md`):

- **Kill reaches the whole session and ends in SIGKILL.** The bash guards sent SIGTERM to a process group. `sessionreap.Escalate` (SIGTERM → `KillGrace`, default 10s → SIGKILL) reaches Bash-tool-style `setsid` command sessions too (ADR-1989) and cannot be ignored. This is the one intended improvement on bash behaviour.
- **Preflight failures that bash let escape as git's/go's own exit code** (`git checkout`, `rev-parse`, the bed `go build`) now exit 4, matching `cut-release.sh`'s "the suite never ran" reading.
- **An uncompilable `FABRIK_PREGATE_ALLOWED_DIRTY_REGEX` fails closed** (every change counts as dirty → the full pre-gate runs). Bash's `grep -Ev … || true` failed open, vouching for any tree — the TOCTOU gap ADR-1624 exists to close.
- **A wedged output pipe is detected for a failing suite too** (`exec.Cmd.WaitDelay` reports it only on a successful exit).

## Consequences

- The gate's orchestration is compiled, vetted, race-tested and unit-tested on every PR; the seven-file, ~1,500-line bash test suite and its nine CI steps are gone.
- Hang protection is contexts and `internal/sessionreap` instead of job control, which removes the `$(…)`/`set -m` pitfalls the bash comments spent pages on.
- Two launch sites for the bed now exist in Go (`tests/gate/bed.go` and the `e2e`-tagged `tests/e2e/lifecycle.go`). The runner is untagged and cannot import the tagged package, and extracting a shared package would modify live-test code, which is out of scope. They are pinned to the three shared contracts — `-notui -poll`, the isolated gitconfig, the unset `FABRIK_GITHUB_APP_*` — by reciprocal comments and by `TestBedStartCmdContracts`.
- `scripts/e2e/run.sh` now needs a Go toolchain at gate time. `scripts/cut-release.sh` already does.
- **Live parity is not proven by unit tests.** Everything after the pre-gate needs a real bed. The acceptance check — one live leg through the shim reporting the same outcomes as the old `run.sh` at the same SHA — is performed by an operator (or in Validate) and recorded on the PR; the report formats are pinned meanwhile by golden tests generated from the original `jq`/`column` code.
