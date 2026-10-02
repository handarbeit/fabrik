# ADR 1957: Verify process-group ownership before any stored-PID group kill

## Status

Accepted (#1957). Builds on ADR-1989 (`internal/sessionreap`).

## Context

`kill(-pid, sig)` signals whatever process group currently has ID `pid`. Several places do that with a PID recorded earlier, without checking the group still belongs to the process that recorded it: both `killProcGroup` copies (engine and Pruefer, run right after `cmd.Wait()` reaped the worker), `engine/webhook.go`'s `killFn` (which runs on `wm.currentCmd`, never cleared after the `gh webhook forward` subprocess exits, so `Stop()`, secret rotation and repo discovery can group-kill a long-reaped PID), the two `internal/sessionreap` listing-failure fallbacks, and `kill -TERM -"$pid"` in `scripts/e2e/run.sh` (watchers, traps, `drain_output_consumer`, post-suite cleanup) and `scripts/sim/run.sh`'s trap. Once the recorded leader has been reaped and its group has emptied, the number can be reused; if the new holder leads a group in an unrelated application, the kill lands there. A host with long uptime and heavy process churn — the gate host — is where that is most likely. No harm from these sites was observed; the guard is cheap, so this is hardening.

## Decision

One ownership-checked primitive in front of every stored-PID group signal, in two implementations that apply the same rules: `sessionreap.SignalGroup` (Go; both `killProcGroup` copies, the webhook manager via `killProcGroup`, and the two `sessionreap` fallbacks) and `scripts/lib/killgroup.sh`'s `kill_own_group`/`kill_own_pid` (bash; every group kill in `run.sh` and `sim/run.sh`).

1. **R4 first.** A target of `0` or `1` (so `-0`/`-1` can never be formed; PID 1 is init), a non-numeric value, or the caller's own process group is refused and logged, before ownership is looked at. An empty/unset PID in bash stays a silent no-op (the trap-before-assign idiom relies on it).
2. **A live process holds the PID.** It must lead its own group (`pgid == pid`) and be the process we started: its start token equals the one recorded at spawn (`Tracker.workerStart` for workers; `sessionreap.RecordStart` for the webhook subprocess), or — when no token was recorded — its parent is this process. A recorded token that does not match is a skip even when the parent check would pass. Bash has no recorded token; it checks the parent only (see below).
3. **Nothing holds the PID (the leader was reaped).** The group is still ours only while it has a live, non-zombie member: POSIX forbids reusing a PID while a process group with that ID exists, so a populated group is necessarily the original one. This is the normal state for `killProcGroup`'s post-exit grandchild cleanup, and a literal "leader must be alive" rule would have silently disabled it and re-opened the orphan leak ADR-1989 fixed. An empty group is a harmless no-op; unreadable membership is a skip.
4. **Never widen.** Every failure to confirm ownership skips the signal and logs why (`group kill of PGID <n> skipped: <reason>`).

### Choices

- **`kill(-pgid)` after the check, not per-member signalling.** For a SIGKILL cleanup one atomic kill also catches a process forking mid-enumeration. Session reaping (ADR-1989) enumerates and re-checks each member because a session has no single kill; a group does. The check-then-kill gap is microseconds wide and is **not closed**: a group that empties and is reused in that window is an inherent race. The guard shrinks the exposure from "any stored PID, indefinitely" to that window; it is not a guarantee.
- **Shared implementation in `internal/sessionreap`.** It already carries the start-token, zombie and `Options`-seam machinery, and both `engine` and `pruefer` import it, so the two intentional `procattr_unix.go` copies (ADR-1113) cannot drift on the kill body.
- **`killProcGroup` stays.** `Sweep` covers a superset of it, but it refuses a recycled leader outright and the group kill is what the existing tests pin; the webhook subprocess has no sweep behind it at all.
- **Webhook: guard the signal site, don't clear `currentCmd`.** Five readers use `currentCmd`; R3 asks only for the check at the signal site. The start token is recorded right after `cmd.Start()` and kept past the subprocess's exit (so a later kill on the stale cmd can tell a recycled PID from ours), forgotten when the next subprocess is recorded and when supervision ends. Clearing `currentCmd` at exit is a possible follow-up.
- **Bash checks the parent, not a start time.** Tracked PIDs are children of the script's shell (a `set -m` background job), so `ppid == $$` holds while they live. A watcher `( sleep n; kill_own_group "$pid" … ) &` runs in a subshell, so the helper also accepts `$BASHPID` and that subshell's own parent. Recording `lstart` per PID would touch every call site without materially tightening the check. A PID that never led its own group (the `$(…)` / non-`set -m` subshell caveat in `run.sh`) fails `pgid == pid` and is now skipped with a stderr line; before, `kill -TERM -"$pid"` failed silently with "No such process", so behaviour is unchanged.
- **Bash fails closed on `ps` errors, and is tested not to over-skip.** A helper that silently stopped timeout kills would recreate the 17-hour hang class of ADR-1676. `scripts/lib/killgroup_test.sh` and `scripts/e2e/hang_hardening_test.sh` (now run in CI) assert that a live own group is still signalled, through `with_timeout`, the traps and `drain_output_consumer`.
- **Zombies.** A zombie leader still reserves its PID, so it is treated as a live holder and goes through the same pgid/start-token/parent check — it is never assumed ours merely for being a zombie.

## Out of scope

- Single-PID signals in general, except the `drain_output_consumer` fallback that sits next to a group kill (it uses `kill_own_pid`, the same parent check). Those already carry their own guards where they matter (the `lstart` fingerprint of ADR-1798/ADR-1814).
- `tests/e2e/lifecycle.go`'s `p.Signal(SIGTERM)` on the PID read from the bed lock file: a single PID, not a group.
- Windows, where `killProcGroup` is already a no-op.
- Session-tree and descendant reaping (ADR-1989).
- Whichever of #1957 and #1994 (Go port of `run.sh`) lands second must route its group kills through `SignalGroup` and must not reintroduce a bare `kill(-pid)`.

## Consequences

- A skipped signal leaves a process running that would have been killed; that is the intended trade for a stored PID we cannot vouch for, and it is logged.
- New log lines: `group kill refused: …`, `group kill of PGID <n> skipped: …`.
