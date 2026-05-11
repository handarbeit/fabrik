//go:build !windows

package engine

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// registerSighupHandler registers a SIGHUP signal handler. On receipt, it logs,
// sets the sighupRequested flag, and cancels the context so the main loop drains
// workers before re-execing. A second SIGHUP during the drain window force-exits.
// restartDone is closed by the caller after performSighupRestart returns (exec
// failure path); on exec success the process is replaced before it can be closed.
// Using restartDone instead of ctx.Done() in the second select keeps the goroutine
// alive for the full drain window, because ctx.Done() fires immediately when
// cancel() is called on the first SIGHUP.
func registerSighupHandler(ctx context.Context, cancel context.CancelFunc, e *Engine, restartDone <-chan struct{}) {
	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)
	go func() {
		select {
		case <-sighupCh:
			e.logf(0, "signal", "received SIGHUP — restarting in place to clear in-memory state\n")
			e.sighupRequested.Store(true)
			cancel()
		case <-ctx.Done():
			signal.Stop(sighupCh)
			return
		}
		signal.Stop(sighupCh)
		// Listen for a second SIGHUP during the drain window and force-exit.
		sighup2Ch := make(chan os.Signal, 1)
		signal.Notify(sighup2Ch, syscall.SIGHUP)
		select {
		case <-sighup2Ch:
			fmt.Fprintln(os.Stderr, "\nSecond SIGHUP received during drain — force-quitting...")
			// Release the terminal before exiting so the shell is not left in
			// alt-screen mode. See also: #692 — any new os.Exit added there must
			// invoke this hook too.
			if fn := e.cleanupHook; fn != nil {
				fn()
			}
			os.Exit(1)
		case <-restartDone:
			signal.Stop(sighup2Ch)
		}
	}()
}

// performSighupRestart stops the webhook manager, releases the lockfile, and
// re-execs the current binary in place. On success, this function never returns.
// On exec failure, it logs the error and returns so Run() can exit cleanly.
func performSighupRestart(e *Engine, lockFile *os.File) {
	if e.webhookMgr != nil {
		e.webhookMgr.Stop()
	}

	// Release the lock explicitly before exec. O_CLOEXEC means the fd is also
	// closed atomically at exec time, but doing it here makes the intent clear.
	// There is a negligible window between this unlock and syscall.Exec during
	// which another instance could theoretically acquire the lock; in practice
	// this is harmless because exec is called immediately after and the PID is
	// unchanged, so any competing instance would fail on its next poll cycle.
	syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	if err := lockFile.Close(); err != nil {
		e.logf(0, "signal", "SIGHUP restart: could not close lock file: %v\n", err)
	}

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
	// NOTE: any new syscall.Exec added by issue #692 must also invoke this hook.
	if fn := e.cleanupHook; fn != nil {
		fn()
	}

	if err := execFn(exe, os.Args, os.Environ()); err != nil {
		e.logf(0, "signal", "SIGHUP restart: exec failed: %v\n", err)
	}
}
