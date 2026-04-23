//go:build !windows

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// setCmdProcAttr starts cmd in its own process group so grandchild processes
// (e.g. tail -f from the Monitor tool) can be cleaned up after cmd exits.
func setCmdProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcGroup sends SIGKILL to cmd's entire process group, cleaning up any
// grandchild processes that outlived the Claude process. ESRCH (no such process)
// is silently ignored — the group may already be gone. Unexpected errors are
// logged to stderr so cleanup failures are diagnosable.
func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Negative PID targets the process group (PGID == Claude's PID when Setpgid is set).
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "[engine] killProcGroup: unexpected error killing process group %d: %v\n", cmd.Process.Pid, err)
	}
}
