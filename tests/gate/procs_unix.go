//go:build !windows

package gate

import (
	"os"
	"path/filepath"
	"syscall"
)

// Alive reports whether a live process holds pid. The gate launches the bed
// engine detached and releases it without ever waiting, so once that engine
// exits it stays a zombie of the gate: kill(pid, 0) still succeeds on it. A
// non-blocking wait4 reaps it (and reports it dead); for any pid that is not our
// child it fails with ECHILD and the plain liveness probe decides.
func (osProcs) Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	var ws syscall.WaitStatus
	if got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); err == nil && got == pid {
		return false
	}
	return pidAlive(pid)
}

func (osProcs) Signal(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// LockHeld probes the engine's instance flock with a shared, non-blocking lock:
// the engine holds it exclusively for the whole of Run(), so the probe fails
// exactly while an engine is up, and succeeds (and is released at once) when the
// file is merely a stale record of a dead one.
func (osProcs) LockHeld(bed string) bool {
	f, err := os.Open(filepath.Join(bed, ".fabrik", "fabrik.lock"))
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return err == syscall.EWOULDBLOCK
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // closing releases it anyway
	return false
}
