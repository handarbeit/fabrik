package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

const (
	heartbeatInterval       = 30 * time.Second
	staleWorkerScanInterval = 60 * time.Second
)

// workerStaleTimeout returns the configured WorkerStaleTimeout, defaulting to
// 5 minutes when zero. Must be longer than heartbeatInterval×2.
func (e *Engine) workerStaleTimeout() time.Duration {
	if e.cfg.WorkerStaleTimeout > 0 {
		return e.cfg.WorkerStaleTimeout
	}
	return 5 * time.Minute
}

// isWorkerStale reports whether a WorkerHandle should be treated as dead.
// Returns true only when ALL of the following hold:
//   - w is non-nil
//   - w.PID > 0 (PID has been set; skip freshly-entered workers)
//   - time.Since(w.LastSignAt) > threshold (heartbeat is stale)
//   - !isProcessAlive(w.PID) (signal-0 confirms the process is dead)
//
// The "both conditions" requirement prevents spurious clearing of live workers
// whose heartbeat goroutine is merely delayed under system load (EC-2).
func isWorkerStale(w *itemstate.WorkerHandle, threshold time.Duration) bool {
	if w == nil || w.PID <= 0 {
		return false
	}
	if time.Since(w.LastSignAt) <= threshold {
		return false
	}
	return !isProcessAlive(w.PID)
}

// startHeartbeat runs a goroutine that sends WorkerHeartbeat mutations every
// heartbeatInterval until done is closed or ctx is cancelled. The goroutine
// exits cleanly when the dispatch goroutine's defer closes done.
func (e *Engine) startHeartbeat(ctx context.Context, repo string, number int, done <-chan struct{}) {
	interval := heartbeatInterval
	if e.heartbeatIntervalOverride > 0 {
		interval = e.heartbeatIntervalOverride
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.store.Apply(itemstate.WorkerHeartbeat{
					Repo:   repo,
					Number: number,
					At:     time.Now(),
				})
			}
		}
	}()
}

// startWorkerDetector starts a background goroutine that scans for stale
// workers (workers whose LastSignAt is older than staleWorkerThreshold).
// For each stale worker, it verifies liveness via signal 0:
//   - PID dead: removes lock and in-progress labels, clears Worker in store.
//   - PID alive but heartbeat stale: logs a warning; does not remove labels.
func (e *Engine) startWorkerDetector(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(staleWorkerScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.runWorkerDetectorScan()
			}
		}
	}()
}

func (e *Engine) runWorkerDetectorScan() {
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	threshold := e.workerStaleTimeout()
	for _, snap := range e.store.All() {
		w := snap.Worker()
		if w == nil {
			continue
		}
		if w.PID <= 0 {
			// PID not yet set (or invalid). Normally this is just a freshly-entered
			// worker still on its way to onPIDReady — skip this cycle and let it
			// arrive. #1303: but if the dispatch goroutine hangs BEFORE onPIDReady
			// fires (e.g. stuck in ensureRepoReady, before processComments starts the
			// child process), no PID is ever recorded — isWorkerStale's signal-0
			// liveness check can never run for it, so an unconditional skip here would
			// let the WorkerEntered marker (and the fabrik:locked:<user> /
			// stage:<name>:in_progress labels it gates) outlive that goroutine
			// indefinitely, permanently suppressing dispatch for the item. Apply the
			// same staleness threshold against w.StartedAt instead (LastSignAt never
			// starts ticking for a worker that never reached PID assignment, so it
			// cannot be used here) and clear on timeout — this can't be confirmed dead
			// via signal-0, so it is a timeout-based clear, not a confirmed-dead clear.
			if time.Since(w.StartedAt) > threshold {
				repo := snap.Repo()
				number := snap.Number()
				e.logf(number, "worker-liveness", "worker never reached PID assignment (started %v ago) — clearing, could not verify liveness\n", time.Since(w.StartedAt).Round(time.Second))
				e.cleanupStaleWorker(repo, number, lockLabel, w.StageName)
			}
			continue
		}
		if time.Since(w.LastSignAt) <= threshold {
			continue
		}
		// Stale heartbeat detected. Verify liveness via signal 0.
		repo := snap.Repo()
		number := snap.Number()
		if !isWorkerStale(w, threshold) {
			// Process is alive but heartbeat is stale. Log warning; do not kill.
			e.logf(number, "worker-detector", "stale heartbeat for PID %d (stage %q) — process alive, waiting for natural exit\n", w.PID, w.StageName)
		} else {
			// Signal 0 failed — process is dead. Clear so the item can be re-dispatched.
			e.logf(number, "worker-liveness", "stale worker handle (pid=%d last_sign=%v) — clearing\n", w.PID, w.LastSignAt)
			e.cleanupStaleWorker(repo, number, lockLabel, w.StageName)
		}
	}
}

// cleanupStaleWorker removes the lock and in-progress labels for a dead worker
// and clears the Worker and Lock entries in the store.
func (e *Engine) cleanupStaleWorker(repo string, number int, lockLabel string, stageName string) {
	e.store.Apply(itemstate.WorkerExited{Repo: repo, Number: number})
	// Also release the lock state so cleanupLockedIssues() on graceful shutdown
	// does not try to remove already-absent labels and log spurious warnings.
	e.store.Apply(itemstate.LocalLockReleased{Repo: repo, Number: number})

	owner, repoName := parseOwnerRepo(repo)
	e.removeLockLabel(owner, repoName, number, lockLabel)
	e.removeInProgressLabel(owner, repoName, number, stageName)
	e.logf(number, "worker-detector", "stale worker cleanup complete for stage %q\n", stageName)
}

// forEachStaleUnworkedItem iterates store items that have no active Worker
// (grace period for in-session workers) and carry label, invoking fn for each.
func (e *Engine) forEachStaleUnworkedItem(label string, fn func(snap itemstate.Snapshot, owner, repoName string, number int)) {
	for _, snap := range e.store.All() {
		if snap.Worker() != nil {
			continue
		}
		if !hasLabel(snap.Labels(), label) {
			continue
		}
		owner, repoName := parseOwnerRepo(snap.Repo())
		fn(snap, owner, repoName, snap.Number())
	}
}

// RunStartupCleanup runs the five one-shot startup recovery scans Run() has
// always run, in order, immediately after the first successful poll cycle:
// runStartupCleanup (stale fabrik:locked:<user>/fabrik:editing labels),
// runStartupOrphanedInProgressScan, runStartupBareInProgressScan (both
// engine/worker_liveness.go), runStartupTransientLabelScan
// (engine/startup.go), and runStartupTerminalScan (engine/terminal.go).
// Run() itself now calls this method instead of the five separate calls —
// same functions, same order, same behavior.
//
// This is a test seam (ADR-1592, mirroring PollOnce/ADR-1449 and
// PollWithBackoff/RegisterObservers above): all five scans are unexported
// and called only from Run(), so gap 3 (#1592) — stale-worker-label reaping
// N polls after the owning worker dies — is otherwise 100% unreachable from
// tests/sim, the same shape of gap the other two seams close for their own
// target code. A scenario calls this once after RestartEnv rebuilds the
// Engine, mirroring where Run() calls it in production: immediately after
// the store is first populated.
//
// Deliberately excludes the periodic runWorktreeJanitor/runLogJanitor/
// runSessionJanitor calls Run() makes alongside these scans (gated
// separately on cfg.JanitorIntervalHours > 0) — those are a distinct,
// ongoing concern, not part of the one-shot startup recovery pass this
// method names.
func (e *Engine) RunStartupCleanup() {
	e.runStartupCleanup()
	e.runStartupOrphanedInProgressScan()
	e.runStartupBareInProgressScan()
	e.runStartupTransientLabelScan()
	e.runStartupTerminalScan()
}

// runStartupCleanup scans the store for items that have a fabrik:locked:<user>
// label but no active Worker (Worker == nil). This catches the restart case where
// a prior Fabrik instance crashed, leaving stale lock labels behind.
//
// Must be called after the store is populated by the first poll cycle.
func (e *Engine) runStartupCleanup() {
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	var cleaned int
	e.forEachStaleUnworkedItem(lockLabel, func(snap itemstate.Snapshot, owner, repoName string, number int) {
		repo := snap.Repo()

		e.logf(number, "startup", "found stale lock label from prior crash — removing\n")
		e.removeLockLabel(owner, repoName, number, lockLabel)

		// Remove all stage:*:in_progress labels (StageName unavailable after restart).
		for _, label := range snap.Labels() {
			if strings.HasPrefix(label, "stage:") && strings.HasSuffix(label, ":in_progress") {
				if err := e.client.RemoveLabelFromIssue(owner, repoName, number, label); err != nil {
					e.logf(number, "warn", "could not remove stale in_progress label %q: %v\n", label, err)
				} else {
					e.logf(number, "startup", "removed stale label %q\n", label)
					if c := e.cache(); c != nil {
						c.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repoName, number), label)
					}
				}
			}
		}

		// No-op since Worker is already nil, but applied for idempotency.
		e.store.Apply(itemstate.WorkerExited{Repo: repo, Number: number})
		cleaned++
	})
	if cleaned > 0 {
		e.logf(0, "startup", "startup cleanup: removed stale locks from %d issue(s)\n", cleaned)
	}

	// Second pass: remove stale fabrik:editing labels left by prior crashes.
	var cleanedEditing int
	e.forEachStaleUnworkedItem("fabrik:editing", func(snap itemstate.Snapshot, owner, repoName string, number int) {
		e.logf(number, "startup", "found stale editing label from prior crash — removing\n")
		e.removeEditingLabel(owner, repoName, number)
		cleanedEditing++
	})
	if cleanedEditing > 0 {
		e.logf(0, "startup", "startup cleanup: attempted stale editing label removal on %d issue(s)\n", cleanedEditing)
	}
}

// runStartupOrphanedInProgressScan scans every item in the store (not just
// items with a stale lock label) for stage:<Name>:in_progress labels whose
// stage <Name> also carries stage:<Name>:complete or stage:<Name>:failed on
// the same item. That co-occurrence is stale by definition — a stage cannot
// be both finished and in progress — so the in_progress label is removed.
//
// Unlike runStartupCleanup's lock-gated sweep, this pass does NOT check
// Worker() or any lock label. This is deliberate, not an oversight: dispatch
// ordering guarantees complete/failed and in_progress never legitimately
// co-occur for the same stage during normal operation (removeFailedLabel
// runs before in_progress is re-added on retry, and complete blocks further
// dispatch entirely), so any occurrence found here is stale regardless of
// whether a worker or lock is present. This makes the predicate self-
// justifying and safe to run unconditionally at startup — see issue #1135.
//
// This sweep is startup-only. Running it mid-poll would additionally need to
// exclude items with a live in-process worker for that stage, which this
// issue's scope does not require (see docs/state-machine.md).
//
// Must be called after the store is populated by the first poll cycle.
func (e *Engine) runStartupOrphanedInProgressScan() {
	var cleaned int
	for _, snap := range e.store.All() {
		labels := snap.Labels()

		terminalStages := make(map[string]bool)
		for _, label := range labels {
			if !strings.HasPrefix(label, "stage:") {
				continue
			}
			if strings.HasSuffix(label, ":complete") {
				terminalStages[strings.TrimSuffix(strings.TrimPrefix(label, "stage:"), ":complete")] = true
			} else if strings.HasSuffix(label, ":failed") {
				terminalStages[strings.TrimSuffix(strings.TrimPrefix(label, "stage:"), ":failed")] = true
			}
		}
		if len(terminalStages) == 0 {
			continue
		}

		owner, repoName := parseOwnerRepo(snap.Repo())
		number := snap.Number()
		for _, label := range labels {
			if !strings.HasPrefix(label, "stage:") || !strings.HasSuffix(label, ":in_progress") {
				continue
			}
			stageName := strings.TrimSuffix(strings.TrimPrefix(label, "stage:"), ":in_progress")
			if !terminalStages[stageName] {
				continue
			}
			if err := e.client.RemoveLabelFromIssue(owner, repoName, number, label); err != nil {
				e.logf(number, "warn", "could not remove orphaned in_progress label %q: %v\n", label, err)
				continue
			}
			e.logf(number, "startup", "removed stale label %q\n", label)
			if c := e.cache(); c != nil {
				c.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repoName, number), label)
			}
			cleaned++
		}
	}
	if cleaned > 0 {
		e.logf(0, "startup", "orphaned in_progress scan: removed %d stale label(s)\n", cleaned)
	}
}

// runStartupBareInProgressScan (R7/#1393, ADR-1393) scans every item in the
// store for a stage:<Name>:in_progress label that carries neither a
// fabrik:locked:<any-user> label nor a stage:<Name>:complete/failed sibling —
// the "bare in_progress" shape produced when a shutdown-pause's own
// in_progress clear (R2) never landed (write failure, crash before the
// pause-write phase ran, force-quit) while the lock label was already
// removed by cleanupLockedIssues (or never applied at all, e.g. a worker
// killed between the lock-add and the in_progress-add). Neither
// runStartupCleanup (lock-gated) nor runStartupOrphanedInProgressScan
// (terminal-sibling-gated) matches this shape — see Problem gap 2 in #1393.
//
// Deliberately a third, narrow pass rather than a broadened existing one:
// the three passes' predicates are disjoint by construction (lock-gated;
// terminal-sibling-gated; neither), so each stays self-justifying and none
// silently absorbs another's responsibility.
//
// hasLock checks any fabrik:locked:<user> label, not just the current
// process's own e.cfg.User — a bare in_progress with someone else's lock
// label present is a different, already-handled shape (that lock label
// itself is what runStartupCleanup's scan is keyed on, for whichever user it
// names); this pass only owns the case where no lock label survives at all.
//
// Skips items with an active Worker() defensively, mirroring
// forEachStaleUnworkedItem's guard, even though in practice Worker() is
// always nil this early in startup — this scan runs immediately after the
// first poll cycle populates the store, before startWorkerDetector or any
// dispatch goroutine can populate Worker().
//
// Must be called after the store is populated by the first poll cycle.
func (e *Engine) runStartupBareInProgressScan() {
	var cleaned int
	for _, snap := range e.store.All() {
		if snap.Worker() != nil {
			continue
		}
		labels := snap.Labels()

		hasLock := false
		terminalStages := make(map[string]bool)
		for _, label := range labels {
			if strings.HasPrefix(label, "fabrik:locked:") {
				hasLock = true
			}
			if !strings.HasPrefix(label, "stage:") {
				continue
			}
			if strings.HasSuffix(label, ":complete") {
				terminalStages[strings.TrimSuffix(strings.TrimPrefix(label, "stage:"), ":complete")] = true
			} else if strings.HasSuffix(label, ":failed") {
				terminalStages[strings.TrimSuffix(strings.TrimPrefix(label, "stage:"), ":failed")] = true
			}
		}
		if hasLock {
			continue
		}

		owner, repoName := parseOwnerRepo(snap.Repo())
		number := snap.Number()
		for _, label := range labels {
			if !strings.HasPrefix(label, "stage:") || !strings.HasSuffix(label, ":in_progress") {
				continue
			}
			stageName := strings.TrimSuffix(strings.TrimPrefix(label, "stage:"), ":in_progress")
			if terminalStages[stageName] {
				// Already handled by runStartupOrphanedInProgressScan.
				continue
			}
			if err := e.client.RemoveLabelFromIssue(owner, repoName, number, label); err != nil {
				e.logf(number, "warn", "could not remove bare in_progress label %q: %v\n", label, err)
				continue
			}
			e.logf(number, "startup", "removed bare in_progress label %q (no lock, no terminal sibling)\n", label)
			if c := e.cache(); c != nil {
				c.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repoName, number), label)
			}
			cleaned++
		}
	}
	if cleaned > 0 {
		e.logf(0, "startup", "bare in_progress scan: removed %d stale label(s)\n", cleaned)
	}
}
