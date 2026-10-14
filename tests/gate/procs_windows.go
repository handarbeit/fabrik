//go:build windows

package gate

import (
	"os"
	"syscall"
)

// The gate drives macOS/Linux hosts; these stubs only keep cross-compilation of
// the tests/gate tree clean. LockHeld reports false, so nothing is ever stopped.
func (osProcs) Alive(pid int) bool { return pidAlive(pid) }

func (osProcs) Signal(pid int, _ syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func (osProcs) LockHeld(string) bool { return false }
