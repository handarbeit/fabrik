//go:build !windows

package workerowner

import "syscall"

// processExists reports whether a process holds pid. EPERM means one does.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
