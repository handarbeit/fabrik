//go:build !windows

package engine

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// registerSighupHandler registers the two-phase SIGHUP handler (#2259, ADR 2259).
//
//   - First SIGHUP: begin a drain (Engine.onFirstSighup). Dispatch stops, polling
//     stops, in-flight workers keep their context and run to their own end, and
//     the main loop re-execs once none is left. Nothing is cancelled.
//   - Second SIGHUP: cancel the run context. The main loop then reaches
//     drainAndExit, which runs the bounded drain (drainDeadline) and re-execs. It
//     no longer force-quits with exit 1.
//   - Third and later SIGHUPs: no effect beyond the restart already under way.
//
// One long-lived Notify channel counts the signals, so there is no window in which
// a SIGHUP would hit the default (terminating) action. restartDone is closed by
// Run() when it returns and ends the goroutine (and the in-flight log repeater).
// SIGINT/SIGTERM are handled in Run() and are unchanged (ADR-1393).
func registerSighupHandler(ctx context.Context, cancel context.CancelFunc, e *Engine, restartDone <-chan struct{}) {
	sighupCh := make(chan os.Signal, 4)
	signal.Notify(sighupCh, syscall.SIGHUP)
	go func() {
		defer signal.Stop(sighupCh)
		count := 0
		for {
			select {
			case <-sighupCh:
			case <-restartDone:
				return
			}
			if ctx.Err() != nil {
				// A stop (SIGINT/SIGTERM, lock loss) or the second SIGHUP's cancel is
				// already under way; a further SIGHUP changes nothing.
				continue
			}
			count++
			switch {
			case e.drain.cancelOnFirst.Load():
				// Test seam: the pre-#2259 behaviour.
				e.logf(0, "signal", "received SIGHUP — restarting in place to clear in-memory state\n")
				e.sighupRequested.Store(true)
				cancel()
			case count == 1:
				e.onFirstSighup(restartDone)
			case count == 2:
				e.logf(0, "signal", "received second SIGHUP — cancelling remaining workers and restarting now\n")
				cancel()
			}
		}
	}()
}

// performSighupRestart stops the transports, releases the lockfile, and
// re-execs the current binary in place. On success, this function never returns.
// On exec failure, it logs the error and returns so Run() can exit cleanly.
func performSighupRestart(e *Engine, locks *instanceLocks) {
	e.transports.StopAll()
	// An exec skips deferred cleanup: stop taking deliveries and persist the
	// dedupe record first (#2351). Bounded, so it cannot extend the SIGHUP drain.
	closeGrace := intakeCloseGrace
	if e.faults.active(FaultNoIntakeDrain) {
		closeGrace = 0 // TEST-ONLY (#2427): lose queued deliveries to the exec
	}
	e.intake.Close(closeGrace)
	// An exec never runs deferred cleanup: close the local API (unlinking its
	// socket) first. The next process also replaces a stale socket at startup.
	e.closeLocalAPI()
	e.closeChannelEvents()

	// Release both instance locks explicitly before exec. O_CLOEXEC means the fds
	// are also closed atomically at exec time, but doing it here makes the intent
	// clear. There is a negligible window between this unlock and syscall.Exec in
	// which another instance could acquire a lock; if one does, whichever process
	// holds the flock keeps it and the loser — here, the re-exec'd image — fails
	// its acquire in Run() and exits. The PID is unchanged across exec, so the
	// recorded-pid check in per-poll verification stays valid.
	locks.Release()

	exe, err := os.Executable()
	if err != nil {
		e.logf(0, "signal", "SIGHUP restart: could not determine executable path: %v\n", err)
		return
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		e.logf(0, "signal", "SIGHUP restart: could not resolve symlinks for executable: %v\n", err)
		return
	}

	execFn := func(argv0 string, argv []string, envv []string) error {
		return syscall.Exec(argv0, argv, envv)
	}
	if e.sighupExecFn != nil {
		execFn = e.sighupExecFn
	}

	// Release the terminal before replacing the process so the shell is not left
	// in alt-screen mode. The new process re-enters alt-screen normally.
	if fn := e.cleanupHook; fn != nil {
		fn()
	}

	// FABRIK_SIGHUP_RESTART=1 mirrors FABRIK_AUTO_UPGRADED: detected at
	// cmd/root.go startup to force non-interactive plugin skills refresh
	// without prompting. Must be unset there before engine.New() so Claude
	// child processes do not inherit it.
	env := append(os.Environ(), "FABRIK_SIGHUP_RESTART=1")
	if err := execFn(exe, os.Args, env); err != nil {
		e.logf(0, "signal", "SIGHUP restart: exec failed: %v\n", err)
	}
}
