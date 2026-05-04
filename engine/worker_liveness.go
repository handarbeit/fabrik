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
	heartbeatInterval      = 30 * time.Second
	staleWorkerThreshold   = 2 * time.Minute
	staleWorkerScanInterval = 30 * time.Second
)

// startHeartbeat runs a goroutine that sends WorkerHeartbeat mutations every
// heartbeatInterval until done is closed or ctx is cancelled. The goroutine
// exits cleanly when the dispatch goroutine's defer closes done.
func (e *Engine) startHeartbeat(ctx context.Context, repo string, number int, done <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
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
	now := time.Now()
	for _, snap := range e.store.All() {
		w := snap.Worker()
		if w == nil {
			continue
		}
		if w.PID == 0 {
			// PID not yet set — worker just started; skip this cycle.
			continue
		}
		if now.Sub(w.LastSignAt) <= staleWorkerThreshold {
			continue
		}
		// Stale heartbeat detected. Verify liveness via signal 0.
		repo := snap.Repo()
		number := snap.Number()
		if isProcessAlive(w.PID) {
			// Process is alive but heartbeat is stale. Log warning; do not kill.
			e.logf(number, "worker-detector", "stale heartbeat for PID %d (stage %q) — process alive, waiting for natural exit\n", w.PID, w.StageName)
		} else {
			// Signal 0 failed — process is dead. Clean up.
			e.logf(number, "worker-detector", "stale worker PID %d (stage %q) — process dead, cleaning up\n", w.PID, w.StageName)
			e.cleanupStaleWorker(repo, number, lockLabel, w.StageName)
		}
	}
}

// cleanupStaleWorker removes the lock and in-progress labels for a dead worker
// and clears the Worker entry in the store.
func (e *Engine) cleanupStaleWorker(repo string, number int, lockLabel string, stageName string) {
	e.store.Apply(itemstate.WorkerExited{Repo: repo, Number: number})

	owner, repoName := parseOwnerRepo(repo)
	e.removeLockLabel(owner, repoName, number, lockLabel)
	e.removeInProgressLabel(owner, repoName, number, stageName)
	e.logf(number, "worker-detector", "stale worker cleanup complete for stage %q\n", stageName)
}

// runStartupCleanup scans the store for items that have a fabrik:locked:<user>
// label but no active Worker (Worker == nil). This catches the restart case where
// a prior Fabrik instance crashed, leaving stale lock labels behind.
//
// Must be called after the store is populated by the first poll cycle.
func (e *Engine) runStartupCleanup() {
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	var cleaned int
	for _, snap := range e.store.All() {
		if snap.Worker() != nil {
			// Worker is active in this session — skip (grace period).
			continue
		}
		hasLock := false
		for _, l := range snap.Labels() {
			if l == lockLabel {
				hasLock = true
				break
			}
		}
		if !hasLock {
			continue
		}

		repo := snap.Repo()
		number := snap.Number()
		owner, repoName := parseOwnerRepo(repo)

		e.logf(number, "startup", "found stale lock label from prior crash — removing\n")
		e.removeLockLabel(owner, repoName, number, lockLabel)

		// Remove all stage:*:in_progress labels (StageName unavailable after restart).
		for _, label := range snap.Labels() {
			if strings.HasPrefix(label, "stage:") && strings.HasSuffix(label, ":in_progress") {
				if err := e.client.RemoveLabelFromIssue(owner, repoName, number, label); err != nil {
					e.logf(number, "warn", "could not remove stale in_progress label %q: %v\n", label, err)
				} else {
					e.logf(number, "startup", "removed stale label %q\n", label)
					if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
						cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repoName, number), label)
					}
				}
			}
		}

		// No-op since Worker is already nil, but applied for idempotency.
		e.store.Apply(itemstate.WorkerExited{Repo: repo, Number: number})
		cleaned++
	}
	if cleaned > 0 {
		e.logf(0, "startup", "startup cleanup: removed stale locks from %d issue(s)\n", cleaned)
	}
}
