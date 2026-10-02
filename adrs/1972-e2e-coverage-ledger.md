# ADR 1972: The live e2e gate is coverage-based — a per-SHA ledger, `--resume`, and an archive of every leg's logs

## Status

Accepted (#1972). Builds on ADR-1994 (the Go gate runner, whose `Gate.OnLeg`/`LegResult`/`Cell`/`PlanCells` seams this uses) and ADR-1933 (the per-test registry, from which the live set and the one new per-test field come). Consistent with ADR-1454: **every live scenario still runs live before every release** — nothing here moves coverage to the sim. What changes is *when* a pass counts, not *whether* it is required. ADR-1676's RUN INVALID and watchdog legs are given an explicit ledger reading below.

## Context

The gate was invocation-based: one ~10-hour run had to pass end to end. GitHub outages, harness races, an operator suspending the run, or the quota running out threw away hours of passes that were still valid for the same engine SHA. For 0.0.83 the operator worked around it outside the repo — a scratchpad driver, a log parser keeping a per-SHA ledger, `-run` regexes over the tests that still lacked a pass — and declared the gate once the ledger was complete across several partial runs. That worked. This ADR makes it the gate's own mechanism.

The same release exposed a diagnostic gap. A stall on `app/off` could not be diagnosed because the evidence was gone. The issue blamed `bed-run.log`; the log that is actually erased is `.fabrik/fabrik.log`, which the engine opens with `O_TRUNC` on **every** start (`engine/poll.go`). `bed-run.log` is truncated once per invocation (`StartBed`) and appended to by every later harness restart. So a single copy at leg end would not have helped either: a scenario that restarts the engine mid-leg erases the log's earlier run.

## Decision

All of it is Go in `tests/gate` (compiled untagged, so `go test -race ./...` covers it on every PR); `scripts/e2e/run.sh` stays a shim.

### 1. The ledger

`E2E_COVERAGE_DIR` (default `<repo>/.e2e-coverage`, git-ignored), keyed on the **full engine SHA** — what `E2E_BED_REF` (default `origin/main`) resolves to at bed preflight (or the bed's HEAD under `E2E_SKIP_PREP`):

```
<sha>/meta.json  invocations.jsonl  bedconfig.json
<sha>/outcomes/<auth>-<train>.jsonl        append-only, one file per leg LABEL
<sha>/archive/<cell>/<invocation>/…        bulky logs (pruned by retention)
pregate/<head>.json
```

- **Outcome records are append-only JSONL, fsynced per record**, one file per leg label, so a leg killed mid-way keeps every test that had reached a terminal event, and per-leg files plus appends are ready for #1976/#1977's concurrency (a second *process* writing the same leg file is not covered). Each record carries a `v` field; a record of another version makes that leg's file **corrupt** and nothing on it counts. An unparseable line (a torn tail) is skipped with a loud warning — it can only reduce coverage — and the next append terminates a torn fragment so it never glues onto it. Known residual: a torn *void* line would leave the records it meant to discard; a void is one small fsynced write.
- **`Outcome` is an open string enum** (PASS, FAIL, SKIP, and INCONCLUSIVE reserved for #1973). A reader treats any value it does not know as **not covered**.
- **Covered ⇔ the latest non-voided record is a PASS recorded against the test's current source hash**, on a trusted leg file. Latest wins, so a later FAIL or SKIP supersedes an earlier PASS.
- **Leg = `<auth>/<train>`; cell ≠ leg.** The train-`on` leg is two cells (main + the isolated `TestSwitchTrainMode`-class cell) under one label. Outcomes key on the label; the archive directory and `void` scoping use a cell name that carries `-isolated`. `--resume` therefore builds `-run` per **cell**.
- **Streaming ingestion, not `OnLeg`.** `suiteWriter` gets an event sink; a `legRecorder` records on a top-level terminal `pass`/`fail`/`skip` event. `OnLeg` fires only for a leg that reached a normal post-suite result and stays untouched for other observers. Subtests roll into their parent. A test without a terminal event is never recorded.
- **Not recorded at all** when the caller's `-run`/`-skip` narrows to *subtests* (`TestX/case`): `go test` reports the parent PASS for a run that executed part of its body. `--resume` refuses such arguments.

### 2. The required set follows `PlanCells`

Required (test, leg) pairs = for each cell of the plan the gate would build with **no caller arguments**, the live tests the cell's own `-run`/`-skip` selects (Go's top-level matching rules), unioned across cells sharing a label. A caller's `-run` narrows what an invocation *runs*, never what the gate *requires*. The live set is `registry.ScanLiveTests` (#1933); nothing hard-codes the 2×2 matrix, so #1975's sparse plan changes the required set with no change to the ledger.

### 3. Drift (R2)

`git diff --name-only <engineSHA>` plus untracked files, run in the repo root. The ledger stays valid iff every changed path is under `tests/e2e/`, `scripts/e2e/` or `tests/gate/` — the last was added to the issue's wording because the runner this ledger lives in is `tests/gate` (#1994 introduced it after the issue was written) and #1973–#1977 all change it; under the literal rule each would invalidate in-progress coverage. An engine SHA that is not a commit in the checkout is **invalid** (fail closed), with the `git fetch` fix in the message. Release flow: `cut-release.sh` step 4 leaves release notes and `plugin/known_embedded_versions.go` uncommitted, so paths matching the caller-declared `FABRIK_PREGATE_ALLOWED_DIRTY_REGEX` are excused **only while uncommitted** (a path also changed by a commit since the SHA still invalidates). At run time an invalid verdict warns and the run proceeds (the ledger certifies the engine SHA); at acceptance (`gate coverage`) it means not accepted.

### 4. A changed test loses its PASS (R3)

The hash covers the test function, **the whole file it lives in, and every package-level declaration in `tests/e2e` it transitively references** (resolved by identifier name, no type checking; method names match any method of that name), plus every `init`. That deliberately exceeds the issue's "function plus file" floor, which the issue itself flags as leaving a gap: most scenarios depend on helpers in the 2,000-line `harness.go` and `mergetrain_helpers.go`, so a changed assertion there would otherwise leave stale PASSes certifying new behaviour — the highest-consequence failure here, a false green. Over-approximation costs reruns, never a false certification. **Not hashed** (accepted gap): non-Go inputs (testdata, embedded assets) and anything outside the package. A source file that cannot be parsed fails the run at preflight (exit 4), before any spend.

### 5. `--resume`, acceptance, exit code 8

- `--resume` and `--clean` are leading flags, either order. `--resume` turns each cell into one anchored `-run '^(A|B|…)$'` over the pairs not yet *resolved*, drops cells with nothing left (skipping the bed restart too), and keeps the isolated cell's own regex. A regex over 100 kB is a loud usage error (about 44 names is ~2 kB; macOS `ARG_MAX` ~1 MB), not chunked.
- `SerialScheduler` still stops at the first failing leg: continuing would change the exit-code contract. Passes already recorded survive.
- **Exit 8 (`ExitCoverageIncomplete`)**: every leg this invocation ran passed under `--resume`, but required coverage is still incomplete. Non-resume invocations keep the suite's own exit code, so `run.sh -run X` still returns 0; they print the coverage summary only.
- `gate coverage [--sha S] [--format notes]` (also `run.sh coverage …`) is the read-only acceptance check: exit 0 complete, 8 not. It never creates a ledger directory. `cut-release.sh` step 5 asks it first, runs `run.sh --clean --resume` only if incomplete, asks again, and records the engine SHA and the **invocation count** in the release notes. An invocation counts only if it started at least one leg, so a zero-leg resume is not counted. `--skip-integration=<reason>` stays the one loud, recorded escape hatch; coverage gating is not a quiet path around it.
- **Pre-gate record.** `pregate/<head>.json` is written on a pass with a clean tree and consulted under `--resume`. It is keyed on the repo's HEAD, not the engine SHA: the pre-gate tests the checkout, and the engine SHA is not known until bed preflight, which ADR-1454 orders after it. Same fail-closed bar as `FABRIK_PREGATE_VERIFIED_SHA` (same HEAD, clean tree), never weaker; that signal is unchanged.

### 6. Skips (R6) and the `skip_ok_legs` registry field — **deviation from the issue**

A skip citing `#N` (read from the `go test -json` output; the last `file.go:N:` log line before the skip is the `t.Skip` message, and `README.md#17`-style anchors are not citations) is a **known skip** while **at least one** cited issue is open in `E2E_ISSUE_REPO` (default `handarbeit/fabrik`); a skip citing no issue, only closed issues, or whose issue state cannot be read is **missing**. State is read with `gh issue view` through the `Commander`, bounded by `GHAPITimeout`, cached per run, and only when a summary or a resume plan is built — never inside the post-suite tail, which has a watchdog.

The issue's literal rule — an uncited skip is missing — cannot be satisfied for skips that are **structural**: every merge-train scenario self-skips under train `off` (`requireTrainBed`), and `TestSwitchTrainMode` skips in every suite leg because `E2E_TRAIN_SWITCH` is unset. Counted as missing, the gate would be permanently unsatisfiable. The R6 example is also stale (`TestCIFixReinvoke` was restored by #1991 and cites no `#N`; today no `t.Skip` in `tests/e2e` cites an issue). So `registry.Entry` gains an optional `skip_ok_legs` (leg patterns such as `*/off`), validated in the registry completeness test. A skip on a matching leg is **structural**: listed separately, does not block, is not counted as covered. This follows ADR-1933's rule that per-test metadata is a registry field, and is a minimal slice of #1975's sensitivity fields, which will subsume it. It may be incomplete on first use; the summary prints each unexplained skip's message so the fix is a one-line registry edit.

### 7. What counts from an interrupted leg

Terminal PASSes from killed, suspended, cancelled and watchdog (exit 6) legs **count**. A leg that ends in the RUN INVALID backoff banner (exit 3) writes a `void` record that discards every outcome that cell recorded in that invocation — the verdict "cannot be trusted", read fail-closed. A failed ledger write is logged loudly and the test stays uncovered.

### 8. Archive and retention (R7)

Per leg, under `archive/<cell>/<invocation>/`: `go-test.json` (written there directly, which also ends the two `on` cells overwriting one `$TMPDIR` log), `fabrik.log.<n>`, `bed-run.log`, `preflight.txt`, `load.json` (1-minute load at start and end), `bed-config.sha256`.

- **Engine log**: a sampler copies `fabrik.log`'s growth every `ArchiveLogInterval` (10 s) and starts a new numbered segment when it sees a restart — the file shrank, or its opening bytes changed — with a final sample on every exit path. A restart between two samples can lose up to one interval of tail; accepted. `bed-run.log` is append-only, so a copy at leg end suffices.
- **Bed config hash**: `.fabrik/stages/*` and `config.yaml`, never `.env`. A different hash from an earlier invocation of the same ledger warns.
- **Load average**: `vm.loadavg` via `x/sys/unix` on darwin, `/proc/loadavg` on linux, "unavailable" elsewhere; a failed probe never fails a leg.
- **Retention**: `E2E_COVERAGE_KEEP_SHAS`-many (default 5) newest SHAs by `meta.last_used` keep their `archive/`; older ones lose only that subtree, and the SHA in use is never pruned. Outcome records, `meta.json`, `invocations.jsonl`, `bedconfig.json` and `pregate/` are never pruned.

## Consequences

- A gate interrupted at any point resumes with only the missing pairs; `cut-release.sh` accepts the gate on accumulated coverage and says in the release notes how many invocations that took.
- The first real gate run is the integration check for all of this: no ten-hour live run is possible during implementation, so the unit and fixture tests (two recorded partial runs fed through `Gate.run`, a killed stream, void, drift, hashing, archive rotation, retention) carry the verification.
- The ledger is local to one operator's checkout; `E2E_COVERAGE_DIR` lets a clean checkout share one. It is never committed and never shared between machines.
- Costs of over-invalidation (a harness edit reopens its dependents, runner changes do not) are accepted over any chance of a false green.
- Out of scope, unchanged: INCONCLUSIVE classification and retry (#1973), the sparse matrix (#1975), parallelism (#1977), multi-bed (#1976), harness races (#1974), and any change to what a test asserts or to the 3/4/5/6/7 exit codes.
