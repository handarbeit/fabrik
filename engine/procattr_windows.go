//go:build windows

package engine

import "os/exec"

// setCmdProcAttr is a no-op on Windows: process groups work differently and
// the Setpgid/SIGKILL approach is Unix-specific.
func setCmdProcAttr(cmd *exec.Cmd) {}

// killProcGroup is a no-op on Windows.
func killProcGroup(cmd *exec.Cmd) {}

// killProcGroupGraceful is a no-op on Windows.
func killProcGroupGraceful(pid, issueNumber int, label string) {}

// isProcessAlive returns true on Windows — process liveness via signal 0
// is Unix-specific. The stale-lock detector is conservative on Windows.
func isProcessAlive(pid int) bool { return true }
