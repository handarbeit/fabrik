# ADR 2259: SIGHUP drains in-flight workers before restarting

**Status:** Accepted
**Issue:** #2259
**Amends:** ADR-1393 ("SIGHUP inherits the bounded wait"), for the first SIGHUP only

## Context

SIGHUP is documented, and used by operators and the TUI's `ctrl+r`, as the graceful restart. The handler cancelled the run context on the first signal, which killed every in-flight worker; `drainAndExit` then waited at most `drainDeadline()` (30 s) and re-exec'd. On 2026-10-13 a SIGHUP cancelled #2210's comment pass 14 turns in (about $5.76 of Claude usage); the comment kept its 👀 without 🚀 and the whole pass ran again. A second SIGHUP force-quit with exit 1.

## Decision

SIGHUP becomes two-phase.

1. **First SIGHUP drains.** It sets a draining flag and cancels nothing. Every dispatch site refuses to start work (stage runs, comment passes, reinvokes, merge-train workers and steps, conflict resolutions, polls); the main loop stops polling and waits, with no deadline, until no worker is in flight, then takes the ordinary `drainAndExit` tail (lock cleanup, webhook/local API/channel hub close, lock release, exec). Workers are already bounded by `max_wall_time` and the inactivity timeout, so the wait is bounded.
2. **Second SIGHUP restarts now.** It cancels the run context; `drainAndExit` runs the existing `drainDeadline()`-bounded wait and execs. The force-quit `os.Exit(1)` for SIGHUP is gone. Later SIGHUPs do nothing.

SIGINT/SIGTERM are unchanged. Because the run context is now live during a drain, the signal goroutine clears `sighupRequested` before the clean stop, so a stop never turns into an exec.

## Alternatives considered

- **Split contexts** (workers on a context only a stop or the second SIGHUP cancels). It would stop dispatch through existing `ctx.Err()` checks, but re-parents four dispatch contexts and also stops the janitors, reconcile loop and slot publisher the status surfaces need. A flag checked at each choke point is a smaller diff and keeps those running.
- **`e.wg.Wait()` for the drain.** It can race an `Add` from zero (the ADR-1393 hazard); the wait reads the store's worker records instead, which also feed the log, health and TUI.

## Details

- *In flight* is the union of item workers (queued ones too), merge-train conflict resolutions and train worker/step goroutines. A train run waiting on CI is none of these, so it does not hold the restart; its persisted run (ADR 2051) resumes after the exec.
- A reinvoke queued for a slot has run nothing and is abandoned; the new process re-dispatches it.
- A refused conflict resolution returns `errDrainingNotAttempted`, treated by `trainCancelled` like a cancelled slot wait: no ejection, no charge, no dissolve.
- No pause: SIGHUP never applies `fabrik:paused` (unchanged). Follow-up a finishing worker would trigger is left to the new process.
- Visibility: the in-flight set is logged on the first SIGHUP and every minute; the second-SIGHUP hint once; `fabrik_health.draining` and the TUI title report the count left. The poll verdict is unknown while draining, so the stopped poll is not published as a pipeline stall.
- Untouched: the idle-only self-upgrade re-exec, SIGINT/SIGTERM, Windows.

## Consequences

A graceful restart can take as long as the longest running worker; the log, TUI title, `fabrik_health` and the second SIGHUP are the operator's tools for that. As-built description: `docs/state-machine.md` §7.20.
