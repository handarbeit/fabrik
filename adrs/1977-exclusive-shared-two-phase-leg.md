# ADR 1977: An exclusive / shared classification and a two-phase leg — raising in-leg parallelism safely

## Status

Accepted (#1977). Builds on ADR-1933 (the per-test registry the new fields extend), ADR-1972 (the ledger, whose keys and required set are unchanged), ADR-1973 (INCONCLUSIVE retries, which now preserve classification), ADR-1975 (the cell model; its "isolated cell" wording is superseded here) and ADR-1648 (merge-train partitions per (repo, base)). **ADR-1454 is unchanged:** every live scenario still runs live in every (test, leg) pair the ledger requires. Nothing here moves coverage to the sim; what changes is how many tests run at once inside a leg.

## Context

Within a leg, `go test -parallel` was 4 (train off) or 2 (train on) against a bed engine whose worker cap is 5, and most of a live test's time is spent waiting on GitHub, CI and review bots — so the limits left the host idle. They could not simply be raised, because some tests take over the whole bed (they stop, restart or reconfigure it, or deliberately trip a bed-wide guard) and merge-train tests compete for one (repo, base) partition.

Serialisation was an accident of `t.Parallel()` placement: Go runs a test without `t.Parallel()` to completion before releasing the parallel ones, and nine tests relied on that. The only explicit mechanism was `TrainIsolatedRE`, a hard-coded one-test regexp that forced an extra "isolated" cell — and with it an extra bed restart — for the runaway-guard test.

Reading the code also showed the problem was wider than the issue text assumed:

- Three tests (`TestMergeTrainHappyPathLanding`, `…BisectionEjectsPoisoner`, the main half of `…TwoBasesConcurrent`) were all `t.Parallel()` on `RepoAlpha/main`, so "no two default-base train tests overlap" was already false.
- Under train `on` the shared yolo pipeline tests also enqueue on `RepoAlpha/main`, so a default-base test asserting exact batch composition is exposed to *every* shared test, not only to its train peers.
- The engine logs `merged integration PR #N for <repo>` and `opened draft CI PR #N for <owner/repo>` per **repo**, not per partition, so a test that counts those lines cannot share a repo with a concurrent train on another base.

## Decision

### 1. Two additive registry fields, no second list

`registry.json` entries gain `exclusive` + `exclusive_reason` and `default_base_train` + `default_base_train_reason` (additive, `Version` stays 1). A reason is required, one line, iff its flag is set; a test is at most one class. `Entry.Isolation()` derives `shared | default-base-train | exclusive`. Two bools rather than an enum keeps the issue's "an `exclusive` field" literal and makes R3.2's default-base group checkable.

### 2. A leg is one restart and up to three phases

After the leg's single bed restart the gate runs, in this order: **shared** at the cell's `-parallel` (`E2E_PARALLEL` / `E2E_PARALLEL_ON`), **default-base-train** at `-parallel 1`, **exclusive** at `-parallel 1`.

- **Exclusive last, no restart between phases.** A shared test always inherits a freshly restarted bed, never state an exclusive test left behind (the runaway guard poisoning RepoBeta's counters for an hour is the motivating case). A restart between phases would cost one per cell and buy little, since test cleanups already close leftover Queued members. The one exception is the INCONCLUSIVE retry: it runs after every first-run phase, so when the first run included an exclusive phase the bed is restarted before each retry attempt that re-runs a shared or default-base-train test (an exclusive-only retry needs none). A failed restart stops the retries and leaves the tests inconclusive.
- **Default-base-train after shared.** Its exact-composition assertions then never overlap the shared yolo tests that enqueue on the same partition.
- **The split sits after every selection mechanism** — a caller `-run`/`-skip`, a `--resume` rewrite, the sparse plan's narrowing — as a pure function of (selection, registry), via `SelectedTests` and `narrowArgs` (a subtest suffix survives). The ledger's required set and keys are untouched, and one recorder takes every phase's stream, so a killed leg keeps what had finished.
- **Every phase runs even after a red one**; the first non-zero exit code is the leg's. It stops early only on cancellation, an incomplete phase (a timeout kill leaves the bed in an unknown state) or the rate-limit backoff (the cell is void anyway). RUN INVALID, the post-suite watchdog and budget handling stay once per leg.
- **Retries preserve classification** (R2.3): the inconclusive set is regrouped through the same builder.
- `TestSwitchTrainMode` is `exclusive` in the registry but is never a phase member — it is the leg's own restart step.
- No readable registry → one undivided `go test`, as before.

### 3. Classification is checked, not hoped for

In plain `go test ./...`: every live test that reaches `StopFabrikTestBed`, `StartFabrikTestBed`, `RestartFabrikTestBed` or a bed `.env` rewrite — directly, from a `t.Cleanup` closure or through a same-package helper — must be `exclusive` (`ScanBedLifecycleCallers`, the closure logic of ADR-1975's identity scan). A test that trips a bed-wide guard is not statically detectable and is classified by hand (`TestMergeTrainRunawayGuardPausesBatch`). And `t.Parallel()` must match the class (`ScanParallelTests`): a shared test that forgot it would silently run serially at the head of the shared phase.

### 4. Merge-train tests: what moved and what did not

Own throwaway `base:<branch>` partition, shared: `TestMergeTrainBisectionEjectsPoisoner`, `TestMergeTrainRedSingletonReroutesOffQueued`, `TestMergeTrainSingletonFastPathLandsExactlyOnce`. Exclusive: `TestSwitchTrainMode`, `…RestartSafety`, `…ColdCacheBaseMember`, `…RunawayGuardPausesBatch`. Default-base-train: `TestMergeTrainHappyPathLanding` (kept on protected main as the production-shaped landing proof — a private base has no branch protection, and ADR-1648's default partition keeps its bare key), `…TwoBasesConcurrent` (its subject), and three tests that were **not** moved for a recorded reason: `…QueuedDeeperThanBatchCap` and `…ConflictBisectPrefixRerere` read engine log lines that are per repo, not per partition, and `TestQueuedMemberCommentEjection` needs the `slow-gate` check, which cannot be confirmed to fire on a non-default base from this repository.

The harness gained `PrepareMemberExactPathOnBase`, `QueueMemberPausedOnBase`, a partition-aware `staleQueuedMembers` and `WaitForNoStaleTrainArtifactsOnBase`: the repo-wide forms would have made a test on its own base wait on, or be failed by, another partition's artifacts. The three moves have had no live pass yet; the fallback is to put a test back in `default_base_train` with its reason, not to force it.

### 5. Measure, then choose

Each leg prints and archives (`phases.json`) per-phase wall-clock, peak concurrent tests (derived from stream order: `run`/`cont` +1, `pause`/terminal −1), GraphQL spend (a budget probe at each phase boundary) and the host's peak 1-minute load (a sampler on the archive cadence; `load.json` gains `peak_1m`). `E2E_PARALLEL` / `E2E_PARALLEL_ON` keep their names and now govern the shared phase; the **defaults stay at today's 4 (train off) and 2 (train on)**, now applied to the shared phase. 8 and 4 are the values to **try** (`E2E_PARALLEL=8 E2E_PARALLEL_ON=4`) once the bed's `max_concurrent` is 10, and the defaults are raised only after a measured leg with these phase metrics, judged by wall-clock, the INCONCLUSIVE rate and peak load staying near their previous levels — a judgement, not a numeric gate. Raising them unmeasured, with the bed still at `max_concurrent: 5`, would run 8 tests against 5 engine workers: an untuned timing change of exactly the kind that produces timeouts and INCONCLUSIVE outcomes. Retry spend is recorded as its own `retry` entry in `phases.json`, so the last phase's figure is not inflated by retries.

### 6. Bed concurrency

The gate cannot set the bed's `max_concurrent` (the bed is launched with no `--max-concurrent`); it resolves from the bed's `.env`, then `config.yaml`, then 5. The recommended setting is 10. Each running test needs at least one slot and the merge-train worker shares the engine semaphore (ADR-1661), so the rule is `max_concurrent ≥ shared-phase -parallel`, with headroom for tests that keep two issues in flight. The gate records the effective value (`bed-concurrency.json`) and warns — never blocks — when it is below the widest `-parallel`. `config.yaml` is part of the bed-config hash, so raising it makes the ledger's "configurations differ" warning fire once for a straddling SHA.

## Consequences

- `TrainIsolatedRE`, `Cell.Isolated`, `isolatedRunArg`, the `-skip`/`-run` pairs in `PlanCells` and the `-isolated` archive-cell suffix are removed; the runaway-guard test is exclusive through the registry.
- A new live test must pick a class; a misclassified shared test still produces intermittent, hard-to-attribute failures — the static scans narrow that, they cannot remove it.
- Gains are concentrated on train-`on` cells (every merge-train test self-skips under `off`), and the default-base-train phase remains serial and long, so the leg's wall-clock gain is smaller than the raw `-parallel` increase suggests. CI and review-bot throughput may become the next bottleneck.
- #1992 (seeding at the state under test) and #1978 (the poll hold seam) touch the same test bodies; classification can drift if they land in parallel.
