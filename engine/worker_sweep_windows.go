//go:build windows

package engine

import "context"

// The registry-independent worker-session sweep (#1814) relies on POSIX
// getsid(2), like the #1798 descendant reaper; all entry points are no-ops on
// Windows (see descendant_reap_windows.go).

func beginWorkerRecord(pid, issueNumber int, repo, stage string) workerRecord {
	return workerRecord{PID: pid}
}

func backfillWorkerFingerprint(ctx context.Context, id string, pid int) {}

func sweepWorkerSessionAtInvocationEnd(rec workerRecord) (reaped int) { return 0 }

func sweepOrphanedWorkerSessions() (records, reaped, skipped, pruned int) { return 0, 0, 0, 0 }
