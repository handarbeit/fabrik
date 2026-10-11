package engine

import "sync/atomic"

// worktreeReapLsofDisabled is the test seam for the macOS worktree reaper's
// `lsof` enumeration (#1805, ADR 1805). When set, reapWorktreeProcessesDarwin
// returns before spawning anything. It exists so the ~50 engine test files and
// the sim bed, which reach CleanupWorktree on every scenario, stop paying for
// a real `lsof` per cleanup. It has no effect on Linux, whose reaper never
// spawns a process.
var worktreeReapLsofDisabled atomic.Bool

// SetWorktreeReapLsofDisabledForTest disables (or re-enables) the macOS
// reaper's `lsof` call process-wide. Exported because tests/sim cannot reach
// unexported package state. The real-path reaper tests in this package turn it
// back on for their own duration.
func SetWorktreeReapLsofDisabledForTest(disabled bool) {
	worktreeReapLsofDisabled.Store(disabled)
}
