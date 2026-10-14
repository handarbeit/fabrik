//go:build !windows

package gate

import (
	"os"
	"path/filepath"
	"syscall"
)

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
