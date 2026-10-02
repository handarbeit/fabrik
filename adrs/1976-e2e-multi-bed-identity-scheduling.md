# ADR 1976: Multi-bed live e2e gate — the GitHub identity, not the bed, is the scheduling unit

## Status

Accepted (#1976). Builds on ADR-1994 (the Go gate runner and its `Cell`/`Scheduler`/`Preflight`/`Commander` seams), ADR-1972 (the per-SHA coverage ledger), ADR-1975 (the sparse plan and its baseline cell) and ADR-1713/ADR-1893 (App installation auth and identity). #1684 (competing token consumers), #1678 (the GraphQL budget), #1861 (auth-mode legs) and #1975 frame it. **ADR-1454 is unchanged:** every live scenario still runs live before every release, the pre-gate still runs first and spends nothing, and nothing moves to the sim. This ADR covers the script side only; standing up the second bed (a second App `fabrik-bed-2`, its repos and board) and certifying a live two-bed run is #1990.

## Context

There was one live test bed — one engine directory, board `handarbeit` #2, the `fabrik-test-alpha`/`-beta` repos, the App `fabrik-bed` and arbeithand's PAT — and every cell reconfigured that one bed, so cells ran strictly in series. Almost all of a cell's wall-clock is spent waiting on GitHub, CI and review bots, so a second bed would roughly halve a gate's wall-clock without competing for much CPU. After #1975 the baseline cell (`app/on`, every live test) dominates; the other three cells are short.

The constraint a second bed must respect is GitHub's rate limiting, which is **per authenticated identity**: every PAT of one user shares that user's 5,000 points/hour, and an App installation has a budget of its own — and an App installs only once per org. Two beds whose legs overlap on one identity spend one budget, which is exactly the throttling (RUN INVALID) the gate exists to avoid.

And the runner assumed one bed everywhere: the lock, the bed log, the archive's engine-log sampler, the pre-gate marker, bed start/stop, the board drain and reset, the competing-daemon check and the scheduler were all implicitly single-bed.

## Decision

### 1. A bed is a view of the gate

`E2E_BEDS` is a comma-separated list of bed directories, the first being bed A; unset, it is the single `FABRIK_TEST_DIR` bed. Every bed-dependent path in the runner already derived from four sources (`Cfg.TestBed`, `Cfg.EngineLog`, `Cfg.BedToken`, `resetConfig()`), so a bed is a **per-bed view** of the invocation's `Gate` carrying its own values of those, built field by field (`newBedGate`). Shared mutable state is reached through a `parent` pointer (the INCONCLUSIVE list) or a shared pointer (the coverage ledger). A bed's repo pair and board come from its own `.env`, else the environment, else today's defaults; each leg's `go test` invocations receive them, and the live harness now honours `FABRIK_TEST_PROJECT_NUMBER`. No new file format.

### 2. Identity sets, acquired atomically

The scheduling unit is the **identity**. A cell charges a *set*: its engine identity — `app:<installation id>` (from the bed's `.env`, so it is known before the bed restarts) on an app leg, the bed token's `user:<login>` (one `GET /user` per distinct token) on a pat leg — plus the user of the **harness** token it runs with, which is the same bed `.env` `FABRIK_TOKEN`. The harness token counts because the harness's own GraphQL calls draw on its budget too. The set is deduplicated and sorted, and a registry under one mutex grants a cell its whole set at once or not at all. With no partial holds there is no hold-and-wait, so no deadlock; every wait is logged once, naming the identity and the leg holding it.

After each app leg's restart the bed's startup banner is cross-checked: the `identity: GitHub App installation <N>` line of its last startup must name the installation the leg was scheduled on, or the leg fails with exit 7 rather than running on an identity nobody scheduled. The parser is pinned to the engine's format strings by a test, so a banner change breaks CI, not a release.

### 3. Assignment

Under the sparse matrix with the baseline in the plan, bed A runs exactly the baseline cell (`app/on`) and the other beds serve a shared queue of the remaining cells in `sparseOrder` — with two beds, bed B runs `app/off`, `pat/on`, `pat/off` in sequence. With the documented setup (bed B on its own App and a different harness user), no two concurrent cells then share an identity, and no second machine-user PAT is needed. With no baseline in the plan (a `--resume`, a filtered or partial run) and always under `E2E_MATRIX=full`, every bed serves the shared queue. A shared-queue bed takes the first cell whose whole set is free right now and waits only if none is; bed A's own queue is strict. Beds start in order, each after the previous has claimed a cell or registered a wait, so bed A — the long pole — gets first claim on any identity it shares; and while bed A's next cell waits, it reserves that cell's identities, so a shared-queue bed that keeps freeing and re-acquiring a shared identity cannot starve it.

A bed's engine is stopped while the bed waits for an identity and once it has no cells left. An engine polls with its identity whether or not a leg is using it, so an idle bed's engine would otherwise spend the budget of an identity another bed's leg holds — the #1684 incident shape, between two beds. Every leg's restart step starts its bed again, so nothing is lost. This is what makes it safe for the competing-consumer check (§5) to exclude the beds' own engines.

### 4. Failure semantics

A failing leg lets every other bed's **running** leg finish (maximising ledger coverage) but no new cell starts on any bed. RUN INVALID — the engine's own rate-limit backoff — is narrower: that bed's records for the cell are voided, its **engine** identity is marked exhausted (the backoff line is the engine's, so only the engine identity is evidenced), cells that would charge it are not started, and cells on other identities continue. The exit code is the first failure's, by time — not the "highest severity" the issue's assumptions suggested: the first failure is the one that changed what ran. A closing summary lists every bed's cells with their outcome, every cell that never started and why, and any exhausted identity. The exit-code contract `scripts/cut-release.sh` keys on is unchanged.

### 5. Refuse only what can never be scheduled

A static `bed-topology` preflight (before the pre-gate, exit 7) refuses two beds with the same App installation when an app leg is planned, two beds on the same board or sharing a repo (each bed's reset and scenarios would drain the other's state), and a bed with no token. Two beds sharing a harness login are **not** refused: their overlapping cells serialize. The #1684 competing-consumer check runs per bed with every configured bed's directory excluded — another bed's engine is the scheduler's business — while any other process on a bed's token, the dev daemon above all, is still refused. Beds that would run different engine SHAs are refused before any leg, because the ledger is per SHA.

### 6. One ledger, one process

Every bed writes the one per-SHA ledger; a (test, leg) pair covered on any bed counts, and records, void lines and archive entries name their bed (an `omitempty` field, so the format version is unchanged). Appends are serialized by the ledger's in-process mutex, so **one gate process drives every bed**; two gate processes on one ledger are unsupported. The pre-gate record stays repo-scoped and runs once per invocation.

### 7. One bed is today's gate

With one bed — the default — there are no views, `SerialScheduler` stays (its RUN INVALID still ends the run), output is unprefixed and legs gain no bed variables. The only additions are R5's per-identity budget lines (each leg logs every identity's remaining GraphQL budget and reset time at start and end, minting an installation token for App identities), the `bed-identity` live preflight (which only warns on one bed), the banner cross-check on app legs, and the bed attribution in records and archives.

## Alternatives rejected

- **A `BedContext` parameter threaded through `RunLeg`, `PreflightBed`, `Reset` and the archive.** It would touch every signature and every existing test, in the most fragile code the runner has. The per-bed view gets the same isolation by derivation.
- **The bed as the scheduling unit, refusing any overlap.** Beds do not own budgets; identities do. Refusing would forbid valid configurations (a shared harness token, `E2E_MATRIX=full`'s overlapping PAT cells) that serializing runs safely.
- **One gate process per bed.** Two processes would need a cross-process ledger lock, a cross-process identity registry and a merged exit code — three new failure modes for no gain over goroutines in one process.
- **A fixed `E2E_BED_ASSIGN` schedule.** The default A/B split plus the greedy fallback covers every plan the gate builds; a knob would be one more way to configure an unschedulable run.

## Consequences

- The wall-clock gain depends on bed B carrying a different harness user (and its own App). With a shared harness token every cell shares that user and the beds serialize — correct, but no faster. #1990 measures whether two beds pay off.
- Concurrency is new in the runner: per-bed views share only read-only fields or explicitly synchronized state, and every scheduler test is channel-driven and `-race`-clean. A field added to `Gate` must be classified in `newBedGate`.
- The single-bed run makes one more REST call per invocation (the token's login) and a few GraphQL `rateLimit` reads per leg — negligible against a leg's ~4,000 points.
- The second App's grants and rate limits, and real two-bed timing, are only provable live, in #1990.
