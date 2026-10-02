//go:build !windows

package gate

import (
	"os/exec"
	"syscall"
)

// setSession puts the child in a session of its own, so its PID is also its
// SID — what sessionreap.Escalate/Track key on.
func setSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// pidAlive is `kill -0 pid`: true only when the signal could be delivered. An
// EPERM (someone else's process) reads as 'not alive', exactly as the bash
// idiom did.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func termPID(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) }
