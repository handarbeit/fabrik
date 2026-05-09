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
func registerSighupHandler(ctx context.Context, cancel context.CancelFunc, e *Engine) {
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
			os.Exit(1)
		case <-ctx.Done():
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

	// Release the lock explicitly before exec so the new process can acquire it.
	// O_CLOEXEC would release it on exec anyway, but being explicit matches the spec.
	syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	lockFile.Close()

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

	if err := execFn(exe, os.Args, os.Environ()); err != nil {
		e.logf(0, "signal", "SIGHUP restart: exec failed: %v\n", err)
	}
}
