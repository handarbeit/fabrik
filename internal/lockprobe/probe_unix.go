//go:build !windows

package lockprobe

import (
	"os"
	"syscall"
)

// flockHeld probes path with a shared, non-blocking flock. EWOULDBLOCK means
// another descriptor holds it exclusively. Success is released at once.
func flockHeld(path string) bool {
	f, err := os.Open(path) // read-only, never O_CREATE
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
