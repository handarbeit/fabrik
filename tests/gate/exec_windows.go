//go:build windows

package gate

import (
	"os"
	"os/exec"
)

// The gate drives macOS/Linux hosts; these stubs only keep `go vet` and
// cross-compilation of the tests/gate tree clean.
func setSession(*exec.Cmd) {}

func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p != nil
}

func termPID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
