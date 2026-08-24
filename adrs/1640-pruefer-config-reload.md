# ADR 1640: Pruefer Config Reload (`SIGHUP`), Without a Restart

**Date**: 2026-08-24
**Status**: Accepted
**Issue**: #1640 — reload config without a restart (`SIGHUP`), instead of read-once-at-startup

## Context

Pruefer's `Config` was read exactly once, at `Execute()`'s `LoadConfig(os.Args[1:])` call, and copied into `Daemon.Config` — a plain, unsynchronized field, read from every poll cycle, every event-triggered review dispatch, and the TUI. There was no `SIGHUP` handler, no file watcher, no periodic re-read. Every configuration change — adding a repo to `watched_repos`, raising `concurrency_cap`, tuning `max_diff_bytes` — required a full restart, which kills any review in flight, tears down and re-establishes the Hookdeck WebSocket session (missing events during the gap), and discards the operator's accumulated TUI session state.

`SIGHUP`-triggered reload is a well-established Unix daemon convention (`nginx -s reload`, `sshd`, `syslogd`, HAProxy): parse and validate the whole candidate config first, apply only on success, log a summary of what changed. This ADR adopts that shape for Pruefer.

**Not the same convention as Fabrik's own engine.** Fabrik's engine already has a `SIGHUP` handler (`engine/sighup_unix.go`), but it means something different: drain in-flight workers, release the lockfile, and re-exec the binary in place (`performSighupRestart` → `syscall.Exec`) — a graceful *restart*, picking up new code as well as new config. This issue's `SIGHUP` never restarts or re-execs Pruefer; it only mutates the running daemon's config in place. The two daemons living in the same monorepo assign the same signal two different meanings — documented explicitly in `cmd/pruefer/README.md` so an operator familiar with one doesn't assume the other's behavior.

## Decisions

### 1. Classification lives as a struct tag on `Config`, not a hand-maintained table

Every field in `pruefer.Config` carries a `reload:"live"`, `reload:"restart"`, or `reload:"skip"` struct tag (`pruefer/config.go`). `applyConfigReload` is one reflection-driven function that both merges a reload candidate against the running config (writing only `live`-tagged fields that changed) and produces the diff-style summary (R6) — a single pass over the tags does both jobs; there's no second table for either to drift against. `TestConfig_AllFieldsClassified` asserts every field has a recognized tag, so a field added to `Config` without a tag fails the build's tests rather than silently defaulting to any particular reload behavior — `applyConfigReload`'s own default case for an unrecognized/absent tag is `restart` (fail conservative, never fail open), so even a lapse in that test would never cause a field to be silently live-applied.

Rejected: a hand-maintained `map[string]reloadClass` parallel to the struct. The issue's own R2 already found the *issue text's* first-cut classification had drifted out of sync with the actual field list (four fields — `auto_upgrade`, `hookdeck.api_key_env`, `hookdeck.webhook_secret_env`, `reconciliation.startup` — were unclassified) before implementation even began; a parallel table in code has the same failure mode with no compiler or test to catch it.

### 2. `WatchedRepos` is diffed and applied specially, at repo granularity

Every other field is a plain scalar or slice compared by `reflect.DeepEqual` and, if changed, either applied whole or reported whole. `WatchedRepos` gets its own `diffRepos` pass instead, reporting individual repos added/removed (order-insensitive) rather than a single before/after slice dump — this is what lets the log summary and Acceptance Criteria (AC1, AC2, AC6) speak in terms of "which repo," not "the whole list changed."

### 3. `Daemon.Config` and `Daemon.Clients` are guarded by one `sync.RWMutex` (`cfgMu`), not three independent locks

Both fields — plus the semaphore channel `sem`, whose size is derived from `Config.ConcurrencyCap` — are guarded by a single `cfgMu`. `config()`/`client()`/`clientForOwner()` are the read accessors; `ApplyReload` is the sole write path. One lock (not `Config`, `Clients`, and `sem` each independently locked) is deliberate: R5's owner-addition needs `WatchedRepos` and its corresponding `Clients` entry to become visible as one atomic unit. Two independent locks would let a reader observe the new repo in `Config.WatchedRepos` before its owner's client exists in `Clients` — a window in which `poll()` would try to dispatch against a repo it has no client for.

`Daemon.Config`/`Daemon.Clients` stay plain exported fields (not `atomic.Pointer[Config]`): construction still happens as a struct literal (`Daemon{Config: cfg, ...}`, both in `execute.go` and the existing test suite) before any goroutine runs, so no lock is needed at construction time, and the existing test pattern needed zero changes. `atomic.Pointer[Config]` was considered and rejected — it would have required changing the field's type and rewriting every `Daemon{Config: cfg}` test literal and every `d.Config.X` dot-chain into a pointer-deref pattern, for no benefit over an `RWMutex` given reload is a rare, human-triggered event, not a hot path.

This is the **first** concurrent-write path against `Daemon.Config`/`Daemon.Clients` — both had been read-only-after-construction since Pruefer's inception (ADR-1113). Every production read site in `daemon.go` and `tui_run.go` was converted to the new accessors; a missed one would not fail to compile, only to be caught by `-race` (or, worse, produce silent staleness) — mitigated by an explicit enumerated list during implementation and `TestDaemon_ConcurrentReloadDuringActivePoll` (AC7), which is confirmed to fail under `-race` against the pre-#1640 shape.

### 4. Restart-only fields are never written into the running `Config` — not just left unread

`applyConfigReload` starts `merged` as a copy of the *old* `Config` and only overwrites fields tagged `live`. A changed restart-only field is reported in the diff, but `merged` itself keeps the old value for it. This makes AC4 ("reported clearly... never silently ignored") true of the log, and "not applied" literally true of the struct itself — not merely true because nothing happens to read the changed value before the next restart (which was verified true for every restart-only field's actual read sites, but is a more fragile property to depend on going forward as the code evolves).

A restart-only field changing does **not** block an otherwise-safe `live` field changing in the same reload — `TestApplyConfigReload_MixOfLiveAndRestartOnlyBothReported` asserts this. R2's wording ("reported clearly... never silently ignored, never half-applied") is read as "half-applied" describing a single field, not the whole reload: an operator who bundles a live change with a restart-only change in one file edit still gets the live change applied immediately, with the restart-only one clearly called out as pending a restart.

### 5. `concurrency_cap` resizes by replacing the channel; no forced eviction

`semaphore()` rebuilds `d.sem` to a freshly-sized channel (under `cfgMu`, double-checked-locked so the common no-resize case only takes a read lock) whenever the desired size (from `Config.ConcurrencyCap`) no longer matches the existing channel's capacity. This is safe without any coordination with in-flight holders because `reviewOne` already captures the channel value returned by `semaphore()` locally, once, at dispatch time (`sem := d.semaphore()`) rather than re-reading `d.sem` on release — a goroutine already dispatched keeps releasing into the channel it acquired from, regardless of what `d.sem` is swapped to afterward for the *next* dispatch.

Consequence, accepted rather than engineered around: during a shrink, a review dispatched just before the resize is still using up a slot on the old (larger) channel, while new dispatches are being admitted against the new (smaller) one — total in-flight concurrency can transiently run as high as `old_cap` (draining) alongside `new_cap` (new admissions), until the old holders finish. This is bounded (by the old cap) and self-correcting (every old holder eventually finishes and stops contributing), not a leak or an unbounded overshoot. `TestDaemon_Semaphore_ResizeDoesNotDisturbInFlightHolder` confirms a holder's release into the pre-resize channel completes cleanly rather than blocking or racing.

### 6. R5's owner addition is a two-phase mint-then-commit split on `AuthSet`

`AuthSet.mintOwnerAuth(cfg, owner)` performs the GitHub App discovery/token-mint for exactly one owner, touching no `AuthSet` state. `AuthSet.CommitOwner(owner, auth, mintedFresh)` registers an already-minted result. `handleReload` (`pruefer/execute.go`) mints every newly-watched owner in the reload's batch first, aborting the *entire* reload — before touching `Daemon` or `AuthSet` at all — on the first owner that fails to mint (R5, AC5). Only once every owner in the batch has minted successfully does it commit all of them and start their refresh goroutines, then call `Daemon.ApplyReload`.

This mirrors `BootstrapMulti`'s own all-or-nothing bootstrap contract, but scoped to just the new owners in one reload rather than the whole `watched_repos` set — `BootstrapMulti` itself is not reused for reload because re-running it would re-derive and re-mint tokens for every *already*-watched owner too, needlessly (and, for a pinned installation shared across many owners, redundantly re-registering an already-running refresh loop).

`mintedFresh` distinguishes a genuinely new `*Auth` (appended to `AuthSet.auths`, so `RunRefreshLoops`/`InstallationCount` pick it up) from the pinned-installation case, where a newly-watched owner shares the App's single already-minted `*Auth` (`CommitOwner` registers the client mapping only, appending nothing) — `TestAuthSet_CommitOwner_NoDuplicateAuthOnPinnedOwner` confirms several new owners sharing one pinned installation in the same batch never produce more than one `*Auth`, and therefore never start more than one redundant refresh goroutine for it.

**Accepted, documented trade-off**: if `mintOwnerAuth` succeeds for one owner but a *later* owner in the same batch fails, the first owner's freshly-minted token is minted but never committed — it is simply never referenced by anything and expires unused (~1h later, per `tokenRefreshMargin`'s neighborhood). This is not a leak requiring cleanup; a retried reload re-mints cleanly. `TestHandleReload_NewOwnerNoInstallation_FailsWholeReloadAllOrNothing` asserts the property that actually matters — no owner from a failed batch is ever observable through `Daemon`/`AuthSet` — rather than asserting a zero mint count for the earlier owner, which this trade-off makes untrue by design.

### 7. `PollInterval`, `AutoUpgrade`, and `ReconciliationFallbackInterval` needed small mechanism changes to actually be live, not just reclassified

Tagging a field `live` only means `applyConfigReload` will write it into `Daemon.Config`; whether the running daemon's *behavior* actually changes depends on whether anything still reads that field only once, before a long-lived loop begins. Three fields needed a small change to stay true to their tag:

- `runPollOnly`'s loop used to capture `PollInterval` and check `AutoUpgrade` once per iteration already, but read them via the (then-unguarded) `d.Config` directly captured into a local outside the loop for `PollInterval` specifically — changed to re-read `d.config()` fresh each iteration, so a reload lands starting with the very next cycle rather than only after a restart.
- `runEventDriven`'s fallback-poll `ticker` was built once, from `ReconciliationFallbackInterval` read at loop start. Changed to detect a value change after each tick and call `ticker.Reset`.

Without this, tagging these fields `live` would have been cosmetic — correct on paper, ineffective in practice, and exactly the kind of drift-from-documentation this ADR's Decision 1 is trying to prevent for the classification itself.

### 8. `ReconciliationStartup`, `HookdeckAPIKeyEnv`/`HookdeckWebhookSecretEnv`, and `AutoUpgrade` — resolving the issue's four originally-unclassified fields

- `ReconciliationStartup` → **restart-only**: read exactly once, synchronously, before `runEventDriven`'s loop even begins. By the time any `SIGHUP` could be handled concurrently with a running daemon, that read has already happened — a reload can structurally never observe or affect it.
- `HookdeckAPIKeyEnv`/`HookdeckWebhookSecretEnv` → **restart-only**: both are read once in `Execute()` to resolve concrete secret values baked into the already-constructed `hookdeck.Config`. Changing which env var to read would require reconstructing the whole `EventSource` — the same reasoning that makes `event_source` itself restart-only.
- `AutoUpgrade` → **live**: a plain boolean consulted at a safe poll-boundary check each cycle; once `runPollOnly` re-reads config per iteration (Decision 7), it's live for free.

### 9. Reload re-resolves the full flag > env > YAML precedence chain, not just the YAML file

`handleReload` calls `LoadConfig(args)` — the same function `Execute()` calls at startup, re-run against the same captured `args` slice — rather than a YAML-only re-parse. This was the simpler, less divergence-prone option (one parsing path, not two), and `LoadConfig` was already confirmed safe to call repeatedly (it builds a fresh `flag.FlagSet` internally and touches no package-level mutable state). The trade-off, documented in `cmd/pruefer/README.md`: an operator's environment drifting since startup (rare, but possible under some supervisors) is also picked up by a `SIGHUP`, not just the YAML file edit the issue's own wording focuses on.

## Consequences

**Positive:**
- A `watched_repos` edit — the most common config change in practice (per the issue's own motivating incident) — no longer requires a restart, and therefore no longer kills in-flight reviews, drops the Hookdeck session, or resets TUI session state.
- The reloadable/restart-only classification is structurally enforced (`TestConfig_AllFieldsClassified`), not a document that can silently drift from the code the way the issue's own first-cut list already had.
- The concurrency-safety boundary this issue introduces (`cfgMu`) is confined to `Daemon`; `Config` itself remains an ordinary value type used identically everywhere except the daemon's own long-lived state.

**Negative / Trade-offs:**
- `Daemon.Config`/`Daemon.Clients` gained a locking discipline that didn't exist before — every future production read site must remember to go through `config()`/`client()`/`clientForOwner()` rather than the field directly. Nothing enforces this at compile time (the fields are still plain exported fields); a lapse here degrades to a `-race` finding rather than a build failure.
- A `concurrency_cap` shrink has a bounded, transient overshoot window rather than an immediate hard cap (Decision 5) — acceptable, but a real (if small) behavior nuance an operator tuning capacity down under load should know about.
- Reload re-resolves environment variables and flags, not just the YAML file (Decision 9) — a minor surprise-potential for an operator whose environment has drifted since startup.
- A reload-batch mint failure can leave one earlier owner's token minted-but-unused until it expires (Decision 6) — harmless, but worth knowing if `access_tokens` API traffic is being audited.

## Related Work

- ADR-1113 (`adrs/1113-pruefer-v1-architecture.md`) — establishes the poll loop and `Daemon` shape this issue adds a concurrency-safety layer to.
- ADR-1233 (`adrs/1233-pruefer-multi-installation-auth.md`) — establishes `AuthSet`/`BootstrapMulti` and the "only ever touch installations for owners actually in `watched_repos`" security property; this issue's `mintOwnerAuth`/`CommitOwner` extend that property to a reload-discovered owner rather than only a startup-time one.
- ADR-1254 (`adrs/1254-event-driven-hookdeck-ingestion.md`), ADR-1563 (`adrs/1563-hookdeck-drop-accounting-and-signature-drift-escalation.md`) — establish the Hookdeck `EventSource`, dedupe ring, and reconciliation-fallback machinery R3 requires this issue not disturb; classifying `event_source`/`hookdeck.*`/`reconciliation.startup` restart-only is what keeps this issue from having to touch any of it.
- `engine/sighup_unix.go` — Fabrik's own, differently-scoped `SIGHUP` handler (graceful restart via re-exec), discussed in Context above as prior art that this issue deliberately does *not* follow, and cross-referenced from `cmd/pruefer/README.md` to avoid operator confusion.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
