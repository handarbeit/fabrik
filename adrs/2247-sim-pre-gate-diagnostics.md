# ADR 2247: Sim pre-gate diagnostics — per-test record, bounded goroutine dump, attributed engine logs, runner-side reaper

## Status

Accepted (#2247). Amends ADR 2226 (the "external killer process" rejection) and refines ADR 1454's pre-gate (no change to what it gates).

## Context

In the v0.0.84 gate a sim pre-gate run took 2642 s and ended with only the testlife ceiling line and `FAIL tests/sim`. Nothing said which test was responsible: the run had no `-json`, the ceiling exited without a goroutine dump, engine log lines carried neither a time nor a test name, and nothing was archived. Diagnosing it meant re-running the suite by hand. Separately, two `-race` binaries then hung inside exit and the in-process self-kill timer (ADR 2226) never fired.

## Decision

1. **`go test -json` plus a decoded stream.** The sim step runs `scripts/sim/run.sh --all -json`. `simStreamWriter` decodes the stream, tracks each top-level test (start restarted on `cont`, since `-parallel` queueing is not run time; subtests fold into the parent), archives it, and feeds `crashScanner` the decoded `Output` text. Raw JSON must not reach the scanner: every JSON line begins `{"Time"`, which would defeat its line-anchored veto and let a genuine panic or data race be retried as a TSan crash. The veto also covers the testlife ceiling message and dump header.
2. **Heartbeat, advisory.** Every 3 minutes the gate prints the running tests with elapsed times and a SLOW note past `max(3× last recorded duration, 1 min)` (5 min with no history). It never changes the exit code (ADR 1676). The baseline is `pregate/sim-timings.json`, updated by passing runs only, so a hang cannot become the next threshold. The 1-minute floor is a deliberate delta from "3×" so a 0.1 s test does not warn at 0.3 s.
3. **Bounded goroutine dump.** `testlife`'s shared bail writes a goroutine profile (`debug=2`) to a file and then stderr after the self-kill watchdog is armed and the reason printed. The profile is built in a goroutine and waited for at most 3 s, inside the fixed 5 s grace; it never extends it (ADR 2226's "not operator-configurable" stands). The path is `SIM_TEST_DUMP_FILE`, else the temp directory — never the cwd, which is `tests/sim` and would dirty the tree the pre-gate records a pass against. Failures are reported and ignored; `Options.DisableDump` is the neutralisation seam.
4. **Attributed engine logs, one `engine/` seam.** `Engine.SetLogSinkForTest` (a per-Engine, nil-by-default sink; nil is byte-identical to before) lets the sim `NewEnv` prefix each `logEvent` line with a wall-clock timestamp and `t.Name()`. This is the only `engine/` edit and goes beyond the issue's stated scope ("no engine behaviour change" still holds), because per-Env attribution is impossible otherwise. Direct prints and the global `gh.Logf` stay anonymous. Lines after test cleanup are dropped rather than written via `t.Logf` (which panics after completion).
5. **Archive.** `.e2e-coverage/pregate/<head>.sim[.aN].{jsonl,log,dump.txt,timings.json}`; a TSan-crash retry is its own attempt. `PruneArchives` skips `pregate/`, so `prunePregateSimArchive` keeps the newest `CoverageKeepSHAs` heads' bulk files and never the coverage record. Ledger disabled: a temp directory, path printed.
6. **Runner-side reaper (amends ADR 2226).** The gate watches the sim's session (`Cmd.OnStart` hands it the PID, which is the SID under `Session: true`) and SIGKILLs a single `.test` member only on positive evidence (ADR 2128): same start token seen for `SIM_TEST_CEILING` + 2 min, token re-read immediately before the signal. First-seen is at most one poll after the real start, so the kill is only ever late, never early, and the in-process ceiling always gets the first chance. It is runner-side rather than a pre-armed helper process because a helper has pipe-inheritance (`go test` waits for EOF), orphan and PID-reuse problems, whereas the gate already owns the session and `sessionreap`'s seams make the rule testable with a fake table. A bare `go test` or hand-run `run.sh` is not covered.
7. **Release path.** `scripts/cut-release.sh` previously ran `run.sh --all` itself and exported `FABRIK_PREGATE_VERIFIED_SHA`, so the gate's pre-gate never ran there — the exact path of the incident. It now builds and execs `gate sim`, which runs only the sim step with all of the above. Wire-contract tests and the verified-SHA export are unchanged.

## Alternatives rejected

- Matching the veto regexes inside JSON: brittle, and still matches text a test echoed.
- Echoing every test's output to the terminal under `-json`: unreadable, and what hid the culprit before. The full stream is archived and its path printed.
- `t.Logf` as the sim sink: correct attribution in the JSON stream, but a panic after the test completes and thousands of polling lines.
- Subtest granularity in the heartbeat: the dump identifies the blocked goroutine exactly.

## Consequences

Edits under `tests/gate/` change the live-coverage drift inputs (expected). The `-race` exit hang is unreproduced; the reaper is covered by fake-table tests, not an end-to-end repro. Sharding the sim (#2228) would mean several binaries: the dump file name defaults to include the PID and the reaper iterates over all session members, so neither assumes one binary (the gate's `SIM_TEST_DUMP_FILE` is a single path, so only one binary per run writes the archived dump; the others use their PID-named temp file).
