//go:build windows

package engine

import "context"

// trackWorkerDescendants is a no-op on Windows, mirroring reapWorktreeProcesses'
// existing Windows precedent (reaper_windows.go): process enumeration by
// session ID uses the POSIX getsid(2) syscall (via golang.org/x/sys/unix),
// which has no direct Windows equivalent wired up here.
func trackWorkerDescendants(ctx context.Context, workerPID, issueNumber int, repo, stage string) {}

// reapTrackedDescendants is a no-op on Windows.
func reapTrackedDescendants(workerPID, issueNumber int) (reaped, skipped int) { return 0, 0 }

// sweepStaleDescendants is a no-op on Windows.
func sweepStaleDescendants() (scanned, reaped, skipped int) { return 0, 0, 0 }
