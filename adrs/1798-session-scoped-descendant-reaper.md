# ADR 1798: Session-Scoped Descendant Reaper

**Date**: 2026-09-18
**Status**: Accepted
**Issue**: #1798 — bug(cleanup): stage-worker test subprocesses leak as CPU-spinning orphans — invisible to both killProcGroup and the cwd-rooted reaper

## Context

Test subprocesses spawned by stage workers were leaking as orphans, busy-spinning at 98–99% CPU indefinitely, accumulating over days, and starving the host badly enough to corrupt the test signals Fabrik's own gates depend on. Two confirmed production instances (#1142, #1787) showed the same shape: a worker backgrounds a long-running command (`nohup cmd & disown`, or plain backgrounding under a job-control-enabled shell) and ends its turn without waiting on it; the command's own descendant survives the worker's own exit.

Both of Fabrik's existing cleanup mechanisms were confirmed structurally incapable of reaching these orphans, independent of when or how often they run:

1. **`killProcGroup`** (ADR-054) sends `SIGKILL` to the worker's own process group. Engine-log evidence from #1142 confirmed this is a *targeting* failure, not a *triggering* failure: the kill ran, completed, and logged success, and the detached process survived it anyway — it had already left the targeted process group (a job-control-enabled shell calls `setpgid` on a backgrounded job, moving it into a brand-new process group distinct from the worker's own PGID, confirmed by direct local reproduction: `set -m; nohup sleep 30 & disown` produces a child whose PGID differs from its parent shell's).
2. **`reapWorktreeProcesses`** (ADR-063) catches `setsid`'d descendants by cwd rooted in the worktree, but only runs immediately before worktree *directory removal* — never at ordinary invocation end — and the reported leaks' cwd is typically outside the worktree entirely (a Go test's `t.TempDir()` fixture, resolving to a path like `/private/var/folders/.../acme/widgets.git` on macOS), structurally invisible to cwd matching regardless of when it runs.

A third finding, made empirically during Plan-stage research, ruled out the most natural remaining fix: a post-hoc walk of `/proc`-style PPID ancestry performed once at invocation teardown. A `nohup cmd & disown` child's PPID is reparented to `1` **essentially immediately** — on a sub-second timescale, well before the spawning shell script's own next line runs, let alone before the worker's own eventual exit. By the time any teardown-time walk could run, the ancestry chain back to the worker is already gone. Polling more frequently doesn't fix a race against a sub-second event.

## Decision

### Session ID, not process-group membership or a PPID walk

Every Claude worker process is made a session leader at spawn time: `engine/procattr_unix.go`'s `setCmdProcAttr` now calls `setsid()` (`SysProcAttr{Setsid: true}`) instead of only `setpgid()` (`Setpgid: true`). POSIX guarantees a process's session ID (SID) is assigned once at creation and inherited unchanged across `fork()`, changing only via an explicit `setsid()` call by the descendant itself — reparenting to init does not touch it, and neither does `setpgid()` (the mechanism that moves a backgrounded job into its own process group under job control). So every process a worker transitively forks carries the worker's own PID as its SID for its entire life, regardless of how many shells deep, regardless of PGID churn, regardless of how fast an intermediate parent exits — unless the descendant itself calls `setsid()` (the one case this reaper does not catch; see Consequences).

This closes the reparenting-race a PPID walk cannot: SID membership requires no precise timing at all, since it survives exactly the event (reparenting) that erases a PPID trail.

`setsid()` also makes the caller its own process-group leader (PGID = own PID), exactly as `Setpgid: true` already provided, so `killProcGroup`/`killProcGroupGraceful`'s existing `kill(-pid, sig)` group-kill needed zero changes.

### `ps`'s own session-ID column was tried first, and found unusable

The initial design (per the Plan stage) intended to discover descendants via `ps -eo pid=,ppid=,sid=` (GNU/Linux) / `ps -eo pid=,ppid=,sess=` (BSD/macOS) — a single bulk system-wide scan, matching the existing `listProcessArgv` precedent (`sentinel_probe_unix.go`). Empirically, on the macOS release used during Implement, `ps`'s session-ID column reported `0` for **every** process unconditionally, including `PID 1` itself — evidently masked at the `ps`-output layer (plausibly a kernel-pointer-hiding security measure; the classic BSD `SESS` column has historically exposed a session-struct address).

The underlying `getsid(2)` syscall was verified directly (not masked): calling it for the same processes whose `ps` output read `0` returned real, distinct session IDs. Discovery therefore goes through `golang.org/x/sys/unix.Getsid`, called once per live PID (from the existing, already-portable `listProcessArgv` enumeration) rather than parsed out of `ps`. `pidFingerprint` (R5 identity re-verification, below) is unaffected by this — `comm=`/`lstart=` are not masked on the same host.

### Discovery happens live, during the invocation — not once at teardown

Per the PPID-reparenting-race finding above, a session-scoped descendant must be *recorded* as it is discovered, not merely walked for at invocation end. `trackWorkerDescendants` (`engine/descendant_reap_unix.go`) runs as a goroutine parallel to `runClaude`'s existing inactivity watchdog, ticking every `descendantScanInterval` (3s in production) for the life of one invocation: each tick, it lists every live process whose session ID matches the worker's PID and, for any not already seen, records an identity fingerprint (`comm`, `lstart`) and upserts it into a durable registry.

Every recorded descendant is also stamped with the *worker's own* identity fingerprint (`WorkerComm`/`WorkerLStart`), which `sweepStaleDescendants` later needs to distinguish "this worker PID is still the same worker" from "this worker PID was reused" (see "Identity re-verification" below). Fetching that fingerprint is itself subject to the same transient-`ps`-failure risk as fetching a descendant's — so it is retried every tick until it succeeds rather than attempted once, and any descendant recorded before it succeeds is backfilled with the correct fingerprint the moment it does (`pendingIdentityBackfill`), rather than being permanently stuck on the liveness-only fallback merely because discovery outpaced that one fetch. Found in review (`handarbeit-pruefer`) as a second- and third-order instance of the same "one failed `ps` call must not permanently degrade tracking" principle already applied to the descendant-side fingerprint.

### A durable, on-disk registry — not an in-memory set

Each discovered descendant is recorded into `.fabrik/state/descendants.json` (`engine/descendant_registry.go`), written to as descendants are *discovered*, not only at invocation end — a crash between discovery and reap still leaves a durable record. This mirrors the existing `os.Getwd()`-rooted `.fabrik/sessions`/`.fabrik/logs` convention (no `fabrikDir` threading needed, since `fabrikDir` is always `os.Getwd()` per this repo's own architecture). Reads/writes are serialized by a package-level mutex and written atomically (temp file + `os.Rename`, mirroring `internal/githubauth/credentials.go`'s existing `atomicWriteFile` precedent) so a reader never observes a torn file.

This durability is what makes the R3 backstop sweep able to survive an engine restart: nothing about `runClaude`'s own in-memory tracking does.

### R2 extends an already-unconditional call site

Research confirmed `killProcGroup(cmd, issueNumber, label)` already runs unconditionally immediately after `cmd.Wait()`, on every invocation end — clean exit or not, not only on timeout paths. `reapTrackedDescendants` is called immediately after it, at the same site: it loads every registry entry for the worker's PID, re-verifies each one's identity fingerprint immediately before killing it, and `SIGKILL`s the survivors. No new "on invocation end" hook was needed.

### R3: a fourth janitor, reusing the existing cadence

`runProcessSweepJanitor` (`engine/janitor.go`) is a fourth sibling to the three existing periodic janitors (`runWorktreeJanitor`, `runLogJanitor`, `runSessionJanitor`), wired into both of their existing `JanitorIntervalHours`-gated call sites in `poll.go` (startup-after-first-poll, and the hourly ticker). It calls `sweepStaleDescendants`, which loads the *entire* durable registry (every worker, including ones from a previous engine run) and, for each entry whose recorded `WorkerPID` is confirmed dead (see below), re-verifies its fingerprint and kills it. No new config knob was introduced — `JanitorIntervalHours`'s default 1h cadence comfortably beats the ~2h regrowth-to-16 window observed in the issue.

**An entry whose worker is still alive *and still the same worker* is left alone by the sweep**, even if it shows up in a sweep pass — that invocation is still in flight, and R2 will reap its descendants at its own invocation end. "Confirmed dead" is not simply `isProcessAlive(WorkerPID) == false`: a live PID whose recorded `WorkerComm`/`WorkerLStart` fingerprint (captured once by `trackWorkerDescendants` at discovery time) no longer matches the live process at that PID is treated as dead too — the original worker exited and the OS has since reused its PID for something unrelated. Without this check, such an entry would pass `isProcessAlive` forever and never be swept — a permanent leak via worker-PID reuse, not merely a delayed one, and exactly the class of mistake R5 calls out generally ("a previously-recorded PID has since been reused... identity must be reverified, not assumed from the PID number alone") applied to the worker side of the entry rather than only the descendant side. (An entry recorded before this field existed, with both fields empty, falls back to liveness-only — the original, narrower behavior — rather than being treated as a spurious mismatch.) This distinction is a safety margin beyond what any single acceptance criterion strictly required; erring toward *not* sweeping a still-owned descendant remains the fail-closed direction R5 establishes as the norm once identity is actually confirmed, not merely assumed from a live PID number.

### Identity re-verification before every kill (R5)

Both `reapTrackedDescendants` and `sweepStaleDescendants` re-check a candidate's `comm`+`lstart` fingerprint (via a single-PID `ps -p <pid> -o comm=,lstart=` query) immediately before signaling it. A mismatch — the process already exited, or the PID was reused for something unrelated since it was recorded — means the entry is dropped from the registry and **never signalled**, regardless of what it was originally recorded as. This mirrors `reapWorktreeProcessesLinux`'s existing TOCTOU re-check precedent, and is the direct answer to R5's explicit PID-reuse concern.

This same fail-open discipline applies to `sweepStaleDescendants`'s worker-side check (`workerIdentityStillMatches`, used by the "still alive and still the same worker" guard above): a transient `pidFingerprintFn` error on the *worker's own* fingerprint is treated as inconclusive ("still matches") rather than a confirmed mismatch. Found in review (`handarbeit-pruefer`) as a fourth instance of the same "one failed `ps` call must not permanently degrade tracking" principle — this one had additionally bypassed the `pidFingerprintFn` seam entirely (calling `pidFingerprint` directly), so a transient failure here would fail *closed*, toward the kill path, for a worker that was genuinely still alive and using its descendant — the exact outcome this section exists to prevent.

`comm`/`lstart` are deliberately queried in a *separate* single-PID call from the bulk discovery scan, not folded into one combined `-eo` query: `lstart`'s value contains embedded spaces (e.g. `Wed Sep 18 12:00:00 2026`), which cannot be combined unambiguously with another variable-width field via simple whitespace splitting. Since `comm` is always the line's first token, everything after it is unambiguously `lstart` in the single-PID form.

### Kill scope: single PID, not a process group

Both reap paths send `SIGKILL` to the descendant's own PID directly (`syscall.Kill(pid, SIGKILL)`), matching `killWorktreeProcess`'s existing ADR-063 precedent — not the descendant's own process group. A detached descendant that itself forks further descendants is a theoretical multi-generation case with no evidence in the issue (the reported shape is a single leaked leaf process); not solved here (see Consequences).

## Rationale

### Why not an environment-variable ownership marker?

Considered during Research as an alternative to a durable PID registry for R3's cross-restart identity problem: inject a Fabrik-owned marker into the worker's `cmd.Env`, since environment is inherited unconditionally through fork/exec regardless of detachment style and survives reparenting. Rejected: cross-process environment readability could not be confirmed on macOS during Research's own sandboxed testing (`ps eww` returned no environment data; `/proc/<pid>/environ` is Linux-only). Given the SID-based mechanism solves the *discovery* problem outright, portably, on both platforms without this assumption, the marker idea was dropped rather than spiked separately.

### Why not subsume the cwd-rooted reaper (ADR-063)?

Per this issue's own Scope, `reapWorktreeProcesses` stays unmodified: it remains the correct mechanism for a `setsid`'d dev server whose cwd is still rooted in the worktree at directory-removal time — a case this new mechanism does not cover (a descendant that calls its own `setsid()` gets a new SID, permanently distinct from the worker's, structurally invisible to `sessionScopedDescendants`). The two mechanisms are complementary, not competing: this ADR's reaper catches what ADR-063's cannot (cwd-external, still-session-scoped descendants, reaped at invocation end rather than only at worktree removal), and ADR-063's reaper catches what this one cannot (session-detached descendants, regardless of when the worktree is eventually removed).

## Consequences

**Positive:**
- The dominant real-world leak shape (a detached background command, cwd anywhere, PGID- or session-detached) is now reaped at ordinary invocation end — R2 — closing the loop at the source for the common case, rather than only mitigating it after the fact.
- A durable, on-disk registry means the R3 backstop sweep reaches orphans left behind by a *previous* engine run, breaking the self-reinforcing host-starvation feedback loop described in the issue even for already-escaped orphans.
- Every kill re-verifies identity immediately beforehand; a stale or PID-reused entry is dropped, never signalled — failing closed per R5.
- `setsid()` is a one-line, low-risk change to a single existing, narrowly-scoped function. The worker already runs fully headless (stdin is a string reader, not a tty), so losing a controlling terminal has no behavioral effect, and `killProcGroup`'s existing PGID-scoped kill needed no changes.

**Negative / Trade-offs:**
- **A descendant that calls its own `setsid()` is not caught by this mechanism.** Its SID permanently diverges from the worker's own PID the instant it does so. This is the same class ADR-063's cwd reaper was built for; the two mechanisms together cover session-detachment and cwd-rooted `setsid`'d descendants, but a `setsid`'d descendant whose cwd is *also* outside the worktree is caught by neither. No evidence of this specific combination was found in the issue's own reports (#1142, #1787 both showed plain backgrounding/`nohup`+`disown`, not the descendant's own `setsid()` call), but it remains a known gap.
- **Multi-generation detached subtrees are reaped only at the recorded leaf.** A detached descendant that itself forks further descendants is not walked recursively; killing the recorded PID may leave its own children running. No evidence of this shape in the issue (the reported leaks are single leaf processes); not solved here.
- **`ps`'s session-ID output column is unreliable on at least one macOS release** — this is *why* discovery goes through `getsid(2)` directly rather than `ps`, but it is worth recording explicitly so a future contributor does not reach for the more "obvious" `ps -eo ...,sess=` approach and silently regress discovery to always-empty on that platform.
- **R4 (the busy-spin root cause) was not conclusively resolved during this issue.** Direct reproduction (an isolated test binary orphaned the same way as the production evidence) completed cleanly without spinning; a separate, reproducible finding — `engine/reaper_unix.go`'s `reapWorktreeProcessesDarwin` shells out to `lsof` with no timeout, and that `lsof` call was directly observed at 98–99% CPU for multiple seconds per invocation under load — was filed as #1805 rather than assumed to be the same mechanism, since it explains a different process (`lsof`, not `sim.test`) and was not conclusively linked to the original report's population.

## Related Work

- [ADR 054: SIGINT Kill Escalation and Reason Propagation](054-sigint-kill-escalation-and-reason-propagation.md) — the PGID-scoped mechanism whose confirmed-insufficient targeting motivates this issue; unchanged by this ADR.
- [ADR 063: Worktree cwd-Rooted Process Reaper](063-worktree-cwd-rooted-process-reaper.md) — the cwd-rooted reaper this issue extends, not replaces. **This ADR's central finding is that ADR-063's cwd-only matching assumption does not generalize**: a descendant's cwd being outside the worktree (a `t.TempDir()` test fixture, in the reported case) is not an edge case to special-case but the dominant real-world shape, and ADR-063's own scoping to worktree-*removal* time (rather than ordinary invocation end) leaves a second, independent gap for any issue that never reaches worktree removal (e.g. a stalled/paused issue). Future work should not re-adopt cwd-only matching, or removal-time-only reaping, as sufficient on their own.
- [ADR 055: Periodic Worktree Janitor](055-worktree-janitor.md) — the periodic-sweep precedent `runProcessSweepJanitor` follows.
- #1767 — stall detection; detects the symptom (a stage stuck waiting on a background run) but not the cause this ADR addresses.
- #1142, #1787 — the two confirmed production instances of this leak.
- #1805 — the filed follow-up for R4's `lsof`-cost finding, not conclusively linked to the original busy-spin report.

**References:** [docs/stage-lifecycle.md § Subprocess Cleanup](../docs/stage-lifecycle.md), [docs/state-machine.md § Worktree Janitor](../docs/state-machine.md)
