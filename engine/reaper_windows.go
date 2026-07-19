//go:build windows

package engine

// reapWorktreeProcesses is a no-op on Windows, mirroring killProcGroup's
// existing Windows precedent (procattr_windows.go): process enumeration by
// cwd uses Unix-specific primitives (/proc, lsof) with no direct Windows
// equivalent wired up here.
func reapWorktreeProcesses(wtDir string, issueNumber int, logf func(int, string, string, ...any)) {}
