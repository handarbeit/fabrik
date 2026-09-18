package engine

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// bootstrapItem seeds the engine's store with a single issue so detector/cleanup
// functions have something to scan.
func bootstrapItem(t *testing.T, e *Engine, number int, labels []string) {
	t.Helper()
	e.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: number,
		FreshState: gh.ProjectItem{
			ID:     "I_001",
			ItemID: "PVTI_001",
			Number: number,
			Title:  "Test Issue",
			Repo:   "owner/repo",
			Status: "Implement",
			Labels: labels,
		},
	})
}

// setWorker applies a LocalLockAcquired mutation to set the Worker field.
func setWorker(e *Engine, number int, pid int, stageName string, lastSign time.Time) {
	now := time.Now()
	e.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     number,
		User:       e.cfg.User,
		AcquiredAt: now,
		Worker: &itemstate.WorkerHandle{
			PID:        pid,
			StageName:  stageName,
			StartedAt:  now,
			LastSignAt: lastSign,
		},
	})
}

// getWorker retrieves the Worker snapshot from the store.
func getWorker(t *testing.T, e *Engine, number int) *itemstate.WorkerHandle {
	t.Helper()
	snap, err := e.store.Get("owner/repo", number)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	return snap.Worker()
}

// removeLabelsCalled returns all label names removed from the given issue.
func removeLabelsCalled(client *mockGitHubClient, issueNumber int) []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	var labels []string
	for _, c := range client.removeLabelCalls {
		if c.issueNumber == issueNumber {
			labels = append(labels, c.labelName)
		}
	}
	return labels
}

// hasLabel checks if a label name appears in the removed calls.
func hasRemovedLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// TestCrashRecoveryRegression501 is the regression test for the operational bug
// observed on issue #501 (2026-05-04). A worker goroutine is set up with a dead
// PID and a stale heartbeat timestamp. The detector must identify it as dead,
// remove the lock and in-progress labels from GitHub, and clear Worker in the store.
func TestCrashRecoveryRegression501(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("crash recovery requires signal-0 support; not available on Windows")
	}

	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	// Start a real subprocess and kill it so we have a confirmed-dead PID.
	cmd := exec.Command("sleep", "1000")
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start subprocess: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("could not kill subprocess: %v", err)
	}
	// Wait reaps the process so the PID is not a zombie and signal-0 returns ESRCH.
	_ = cmd.Wait()

	bootstrapItem(t, e, 1, []string{"fabrik:locked:testuser", "stage:Implement:in_progress"})
	// Set Worker with the dead PID and a stale LastSignAt (>2 min ago).
	staleTime := time.Now().Add(-5 * time.Minute)
	setWorker(e, 1, pid, "Implement", staleTime)

	// Verify setup: Worker is non-nil before scan.
	if getWorker(t, e, 1) == nil {
		t.Fatal("expected Worker to be non-nil before scan")
	}

	e.runWorkerDetectorScan()

	// Worker must be nil after cleanup.
	if w := getWorker(t, e, 1); w != nil {
		t.Errorf("expected Worker == nil after detector cleanup, got PID=%d", w.PID)
	}

	// Both labels must have been removed.
	removed := removeLabelsCalled(client, 1)
	if !hasRemovedLabel(removed, "fabrik:locked:testuser") {
		t.Errorf("expected lock label to be removed; got: %v", removed)
	}
	if !hasRemovedLabel(removed, "stage:Implement:in_progress") {
		t.Errorf("expected in_progress label to be removed; got: %v", removed)
	}
}

// TestWorkerHeartbeatUpdatesLastSignAt verifies that WorkerHeartbeat mutations
// advance the LastSignAt timestamp in the store.
func TestWorkerHeartbeatUpdatesLastSignAt(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 2, nil)
	t0 := time.Now().Add(-1 * time.Minute)
	setWorker(e, 2, 999, "Research", t0)

	t1 := time.Now()
	e.store.Apply(itemstate.WorkerHeartbeat{Repo: "owner/repo", Number: 2, At: t1})

	w := getWorker(t, e, 2)
	if w == nil {
		t.Fatal("expected Worker non-nil")
	}
	if !w.LastSignAt.Equal(t1) {
		t.Errorf("LastSignAt = %v, want %v", w.LastSignAt, t1)
	}

	// Second heartbeat advances timestamp further.
	t2 := t1.Add(30 * time.Second)
	e.store.Apply(itemstate.WorkerHeartbeat{Repo: "owner/repo", Number: 2, At: t2})

	w = getWorker(t, e, 2)
	if !w.LastSignAt.Equal(t2) {
		t.Errorf("LastSignAt = %v after second heartbeat, want %v", w.LastSignAt, t2)
	}
}

// TestHeartbeatGoroutineCleanup verifies that the heartbeat goroutine exits
// cleanly when the done channel is closed, producing no goroutine leak.
func TestHeartbeatGoroutineCleanup(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})
	e.heartbeatIntervalOverride = 5 * time.Millisecond

	bootstrapItem(t, e, 3, nil)
	startedAt := time.Now()
	setWorker(e, 3, os.Getpid(), "Plan", startedAt)

	ctx := context.Background()
	done := make(chan struct{})

	// Start heartbeat and poll until at least one tick fires (up to 500ms).
	e.startHeartbeat(ctx, "owner/repo", 3, done)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		snap, _ := e.store.Get("owner/repo", 3)
		if snap.Worker() != nil && snap.Worker().LastSignAt.After(startedAt) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	snap, _ := e.store.Get("owner/repo", 3)
	if snap.Worker() == nil || !snap.Worker().LastSignAt.After(startedAt) {
		t.Fatal("no heartbeat tick fired within 500ms")
	}

	// Close done to signal the goroutine to stop.
	close(done)

	// Poll until the goroutine has exited: LastSignAt stops advancing.
	// We check twice with a gap to confirm it truly stopped.
	time.Sleep(20 * time.Millisecond)
	snap, _ = e.store.Get("owner/repo", 3)
	stoppedAt := snap.Worker().LastSignAt

	// Wait two tick intervals and confirm no more heartbeats fired.
	time.Sleep(20 * time.Millisecond)
	snap, _ = e.store.Get("owner/repo", 3)
	if snap.Worker().LastSignAt != stoppedAt {
		t.Errorf("heartbeat goroutine still firing after done closed: stoppedAt=%v finalSignAt=%v",
			stoppedAt, snap.Worker().LastSignAt)
	}
}

// TestDetectorFalsePositivePrevention verifies that the detector does NOT remove
// labels or clear Worker when the process is alive but the heartbeat is stale.
func TestDetectorFalsePositivePrevention(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-0 not applicable on Windows (always returns alive)")
	}

	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 4, []string{"fabrik:locked:testuser", "stage:Research:in_progress"})
	// Use current process PID — always alive.
	staleTime := time.Now().Add(-10 * time.Minute)
	setWorker(e, 4, os.Getpid(), "Research", staleTime)

	e.runWorkerDetectorScan()

	// Worker must remain non-nil (process is alive).
	if w := getWorker(t, e, 4); w == nil {
		t.Error("detector incorrectly cleared Worker for alive process with stale heartbeat")
	}

	// No labels should have been removed.
	removed := removeLabelsCalled(client, 4)
	if len(removed) > 0 {
		t.Errorf("detector incorrectly removed labels for alive process: %v", removed)
	}
}

// TestDetectorRace_WorkerExitIdempotent verifies that WorkerExited is idempotent:
// applying it twice leaves Worker nil without panicking or corrupting state.
func TestDetectorRace_WorkerExitIdempotent(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 5, nil)
	setWorker(e, 5, 12345, "Implement", time.Now())

	// Apply WorkerExited twice — must be idempotent.
	e.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 5})
	e.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 5})

	if w := getWorker(t, e, 5); w != nil {
		t.Errorf("expected Worker == nil after double WorkerExited, got %+v", w)
	}
}

// TestConcurrentWorkerSpawnExit spawns multiple goroutines that simultaneously
// apply Worker mutations for different issues. The race detector must see no races.
func TestConcurrentWorkerSpawnExit(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	const n = 20
	for i := 1; i <= n; i++ {
		bootstrapItem(t, e, i, nil)
	}

	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := time.Now()
			e.store.Apply(itemstate.LocalLockAcquired{
				Repo:       "owner/repo",
				Number:     i,
				User:       "testuser",
				AcquiredAt: now,
				Worker: &itemstate.WorkerHandle{
					PID:        i * 1000,
					StageName:  "Implement",
					StartedAt:  now,
					LastSignAt: now,
				},
			})
			e.store.Apply(itemstate.WorkerHeartbeat{
				Repo:   "owner/repo",
				Number: i,
				At:     time.Now(),
			})
			e.store.Apply(itemstate.WorkerExited{
				Repo:   "owner/repo",
				Number: i,
			})
		}()
	}
	wg.Wait()

	// All workers should be nil after exit.
	for i := 1; i <= n; i++ {
		if w := getWorker(t, e, i); w != nil {
			t.Errorf("issue #%d: expected Worker == nil after WorkerExited, got %+v", i, w)
		}
	}
}

// TestStartupCleanupRemovesStaleLabels verifies that runStartupCleanup removes
// fabrik:locked:<user> and stage:*:in_progress labels from items whose Worker is
// nil in the store (the restart-after-crash case).
func TestStartupCleanupRemovesStaleLabels(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	// Bootstrap item with stale lock label and in_progress label; no Worker applied.
	bootstrapItem(t, e, 6, []string{
		"fabrik:locked:testuser",
		"stage:Implement:in_progress",
	})

	// Confirm Worker is nil (no LocalLockAcquired applied).
	if w := getWorker(t, e, 6); w != nil {
		t.Fatalf("expected Worker == nil before cleanup, got %+v", w)
	}

	e.runStartupCleanup()

	// Both labels must have been removed.
	removed := removeLabelsCalled(client, 6)
	if !hasRemovedLabel(removed, "fabrik:locked:testuser") {
		t.Errorf("expected lock label to be removed; got: %v", removed)
	}
	if !hasRemovedLabel(removed, "stage:Implement:in_progress") {
		t.Errorf("expected in_progress label to be removed; got: %v", removed)
	}
}

// TestStartupCleanupSkipsActiveWorkers verifies the startup grace period:
// an item with a non-nil Worker (active in the current session) must NOT be
// cleaned up by runStartupCleanup.
func TestStartupCleanupSkipsActiveWorkers(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 7, []string{
		"fabrik:locked:testuser",
		"stage:Implement:in_progress",
	})
	// Simulate a lock acquired in the current session (Worker non-nil).
	setWorker(e, 7, os.Getpid(), "Implement", time.Now())

	e.runStartupCleanup()

	// No labels should have been removed.
	removed := removeLabelsCalled(client, 7)
	if len(removed) > 0 {
		t.Errorf("startup cleanup incorrectly removed labels for active worker: %v", removed)
	}

	// Worker must remain non-nil.
	if w := getWorker(t, e, 7); w == nil {
		t.Error("startup cleanup incorrectly cleared active Worker")
	}
}

// TestWorkerHeartbeatNoOpWhenWorkerNil verifies that WorkerHeartbeat is a no-op
// when Worker is nil — it must not create a new WorkerHandle.
func TestWorkerHeartbeatNoOpWhenWorkerNil(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 8, nil)
	// No Worker applied — store has Worker == nil.

	e.store.Apply(itemstate.WorkerHeartbeat{
		Repo:   "owner/repo",
		Number: 8,
		At:     time.Now(),
	})

	if w := getWorker(t, e, 8); w != nil {
		t.Errorf("WorkerHeartbeat created WorkerHandle when Worker was nil; got %+v", w)
	}
}

// TestWorkerPIDSetNoOpWhenWorkerNil verifies that WorkerPIDSet is a no-op
// when Worker is nil — it must not create a new WorkerHandle.
func TestWorkerPIDSetNoOpWhenWorkerNil(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 9, nil)

	e.store.Apply(itemstate.WorkerPIDSet{
		Repo:   "owner/repo",
		Number: 9,
		PID:    12345,
	})

	if w := getWorker(t, e, 9); w != nil {
		t.Errorf("WorkerPIDSet created WorkerHandle when Worker was nil; got %+v", w)
	}
}

// TestDetectorSkipsPIDZeroWorker verifies that the detector skips workers whose
// PID is 0 (not yet set by OnPIDReady) and whose StartedAt is recent — the
// normal "worker just started, on its way to onPIDReady" case. setWorker
// always sets StartedAt to time.Now(), so this exercises that path
// regardless of how stale the (unused, for a PID=0 worker) LastSignAt is.
func TestDetectorSkipsPIDZeroWorker(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 10, []string{"fabrik:locked:testuser"})
	staleTime := time.Now().Add(-10 * time.Minute)
	// PID=0 means OnPIDReady not yet called.
	setWorker(e, 10, 0 /* PID zero */, "Implement", staleTime)

	e.runWorkerDetectorScan()

	// Worker must remain non-nil — freshly-started, PID=0 means skip.
	if w := getWorker(t, e, 10); w == nil {
		t.Error("detector incorrectly cleared Worker with PID=0 (should skip)")
	}
	removed := removeLabelsCalled(client, 10)
	if len(removed) > 0 {
		t.Errorf("detector removed labels for PID=0 worker: %v", removed)
	}
}

// TestDetectorClearsPIDNeverSetAfterStartedAtTimeout is the #1303 regression:
// a dispatch goroutine that hangs BEFORE onPIDReady fires (e.g. stuck in
// ensureRepoReady) never records a PID, so isWorkerStale's signal-0 check can
// never run for it — before this fix, runWorkerDetectorScan's unconditional
// "PID <= 0 → skip" branch let such a marker (and the fabrik:locked:<user> /
// stage:<name>:in_progress labels it gates) outlive the hung goroutine
// indefinitely, permanently suppressing dispatch for the item. Once
// w.StartedAt exceeds workerStaleTimeout, the detector must now clear the
// marker via the same cleanupStaleWorker path, distinctly logged as a
// timeout-based (not confirmed-dead) clear.
func TestDetectorClearsPIDNeverSetAfterStartedAtTimeout(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 11, []string{"fabrik:locked:testuser", "stage:Implement:in_progress"})
	staleStart := time.Now().Add(-10 * time.Minute) // older than the 5-minute default workerStaleTimeout
	e.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     11,
		User:       e.cfg.User,
		AcquiredAt: staleStart,
		Worker: &itemstate.WorkerHandle{
			PID:        0, // never reached onPIDReady
			StageName:  "Implement",
			StartedAt:  staleStart,
			LastSignAt: staleStart,
		},
	})

	e.runWorkerDetectorScan()

	if w := getWorker(t, e, 11); w != nil {
		t.Errorf("expected Worker == nil after StartedAt-timeout cleanup, got PID=%d", w.PID)
	}
	removed := removeLabelsCalled(client, 11)
	if !hasRemovedLabel(removed, "fabrik:locked:testuser") {
		t.Errorf("expected lock label to be removed; got: %v", removed)
	}
	if !hasRemovedLabel(removed, "stage:Implement:in_progress") {
		t.Errorf("expected in_progress label to be removed; got: %v", removed)
	}
}

// TestWorkerStoreReflectsDispatchPaths verifies that store.Get(...).Worker() != nil
// correctly reflects worker state: non-nil after LocalLockAcquired, nil after
// WorkerExited. This is the inFlight replacement semantic test (test #10 from spec).
func TestWorkerStoreReflectsDispatchPaths(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 11, nil)

	// Initially nil.
	if w := getWorker(t, e, 11); w != nil {
		t.Fatalf("expected Worker == nil initially, got %+v", w)
	}

	// After LocalLockAcquired with Worker: non-nil.
	now := time.Now()
	e.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     11,
		User:       "testuser",
		AcquiredAt: now,
		Worker:     &itemstate.WorkerHandle{StageName: "Implement", StartedAt: now, LastSignAt: now},
	})
	snap, err := e.store.Get("owner/repo", 11)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.Worker() == nil {
		t.Error("expected Worker != nil after LocalLockAcquired with Worker field")
	}

	// After WorkerExited: nil.
	e.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 11})
	snap, _ = e.store.Get("owner/repo", 11)
	if snap.Worker() != nil {
		t.Errorf("expected Worker == nil after WorkerExited, got %+v", snap.Worker())
	}

	// LocalLockAcquired without Worker field: Worker stays nil.
	e.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     11,
		User:       "testuser",
		AcquiredAt: now,
	})
	snap, _ = e.store.Get("owner/repo", 11)
	if snap.Worker() != nil {
		t.Errorf("expected Worker == nil when LocalLockAcquired.Worker is nil, got %+v", snap.Worker())
	}
}

// TestRunStartupCleanup_StaleEditingLabel verifies that runStartupCleanup removes
// fabrik:editing from an item whose Worker is nil (orphaned by a prior crash).
func TestRunStartupCleanup_StaleEditingLabel(t *testing.T) {
	orig := editingLabelRetryDelay
	editingLabelRetryDelay = 0
	t.Cleanup(func() { editingLabelRetryDelay = orig })
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 20, []string{"fabrik:editing"})

	// Confirm Worker is nil.
	if w := getWorker(t, e, 20); w != nil {
		t.Fatalf("expected Worker == nil before cleanup, got %+v", w)
	}

	e.runStartupCleanup()

	removed := removeLabelsCalled(client, 20)
	if !hasRemovedLabel(removed, "fabrik:editing") {
		t.Errorf("expected fabrik:editing to be removed; got: %v", removed)
	}
}

// TestRunStartupCleanup_ActiveWorkerSkipsEditing verifies that runStartupCleanup
// does NOT remove fabrik:editing when the item has an active Worker.
func TestRunStartupCleanup_ActiveWorkerSkipsEditing(t *testing.T) {
	orig := editingLabelRetryDelay
	editingLabelRetryDelay = 0
	t.Cleanup(func() { editingLabelRetryDelay = orig })
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 21, []string{"fabrik:editing"})
	setWorker(e, 21, os.Getpid(), "Implement", time.Now())

	e.runStartupCleanup()

	removed := removeLabelsCalled(client, 21)
	if hasRemovedLabel(removed, "fabrik:editing") {
		t.Errorf("startup cleanup incorrectly removed fabrik:editing for active worker")
	}
}

// TestRunStartupCleanup_EditingLabelErrNotFound_Silent verifies that ErrNotFound
// during startup editing-label cleanup is treated as a no-op (idempotency).
func TestRunStartupCleanup_EditingLabelErrNotFound_Silent(t *testing.T) {
	orig := editingLabelRetryDelay
	editingLabelRetryDelay = 0
	t.Cleanup(func() { editingLabelRetryDelay = orig })
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:editing" {
				return gh.ErrNotFound
			}
			return nil
		},
	}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 22, []string{"fabrik:editing"})

	// Should not panic and should not produce a warning.
	e.runStartupCleanup()
}

// TestDetectorSkipsFreshWorker (SC-3) verifies that the detector does NOT clear
// a worker whose heartbeat is fresh (within WorkerStaleTimeout), regardless of
// the PID's liveness. A long-running stage with a healthy heartbeat must never
// be interrupted by the detector.
func TestDetectorSkipsFreshWorker(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})
	// Use the current process PID so isProcessAlive returns true, but even if the
	// PID were dead, a fresh heartbeat alone must prevent clearing.
	pid := os.Getpid()

	bootstrapItem(t, e, 23, []string{"fabrik:locked:testuser", "stage:Plan:in_progress"})
	// LastSignAt is time.Now() — well within any reasonable stale threshold.
	setWorker(e, 23, pid, "Plan", time.Now())

	e.runWorkerDetectorScan()

	// Worker must remain non-nil — heartbeat is fresh.
	if w := getWorker(t, e, 23); w == nil {
		t.Error("SC-3: detector incorrectly cleared Worker with a fresh heartbeat")
	}
	removed := removeLabelsCalled(client, 23)
	if len(removed) > 0 {
		t.Errorf("SC-3: detector removed labels for a fresh-heartbeat worker: %v", removed)
	}
}

// TestRunStartupOrphanedInProgressScan_RemovesStaleLabel verifies that the
// broadened startup sweep removes a stage:in_progress label that co-occurs
// with stage:complete for the same stage, even with no lock label present.
func TestRunStartupOrphanedInProgressScan_RemovesStaleLabel(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 30, []string{
		"stage:Implement:complete",
		"stage:Implement:in_progress",
	})

	e.runStartupOrphanedInProgressScan()

	removed := removeLabelsCalled(client, 30)
	if !hasRemovedLabel(removed, "stage:Implement:in_progress") {
		t.Errorf("expected stage:Implement:in_progress to be removed; got: %v", removed)
	}
	if hasRemovedLabel(removed, "stage:Implement:complete") {
		t.Errorf("stage:Implement:complete should not be removed; got: %v", removed)
	}
}

// TestRunStartupOrphanedInProgressScan_FailedCoOccurrence verifies that
// stage:<Name>:failed also triggers the staleness predicate, not just
// stage:<Name>:complete.
func TestRunStartupOrphanedInProgressScan_FailedCoOccurrence(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 31, []string{
		"stage:Review:failed",
		"stage:Review:in_progress",
	})

	e.runStartupOrphanedInProgressScan()

	removed := removeLabelsCalled(client, 31)
	if !hasRemovedLabel(removed, "stage:Review:in_progress") {
		t.Errorf("expected stage:Review:in_progress to be removed; got: %v", removed)
	}
}

// TestRunStartupOrphanedInProgressScan_LeavesGenuineInFlightUntouched verifies
// that an item whose in_progress stage has no complete/failed counterpart is
// left untouched — even when a Worker is active, proving the predicate itself
// (not worker-liveness) is what protects genuinely in-flight stages.
func TestRunStartupOrphanedInProgressScan_LeavesGenuineInFlightUntouched(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 32, []string{"stage:Implement:in_progress"})
	setWorker(e, 32, os.Getpid(), "Implement", time.Now())

	e.runStartupOrphanedInProgressScan()

	removed := removeLabelsCalled(client, 32)
	if len(removed) > 0 {
		t.Errorf("expected no labels removed for genuinely in-flight stage; got: %v", removed)
	}
}

// TestRunStartupOrphanedInProgressScan_DifferentStageUnaffected verifies that
// the predicate is scoped per stage name: a complete label for one stage must
// not trigger removal of an in_progress label for a different stage.
func TestRunStartupOrphanedInProgressScan_DifferentStageUnaffected(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 33, []string{
		"stage:Implement:complete",
		"stage:Review:in_progress",
	})

	e.runStartupOrphanedInProgressScan()

	removed := removeLabelsCalled(client, 33)
	if len(removed) > 0 {
		t.Errorf("expected no labels removed for different-stage in_progress; got: %v", removed)
	}
}

// TestRunStartupOrphanedInProgressScan_PreservesLockGatedSweep verifies that
// the new broader pass composes correctly with the existing lock-gated sweep
// when both run in sequence (as poll.go does): the stale lock label and the
// orphaned in_progress label are both removed, without double-counting or panics.
func TestRunStartupOrphanedInProgressScan_PreservesLockGatedSweep(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 34, []string{
		"fabrik:locked:testuser",
		"stage:Implement:complete",
		"stage:Implement:in_progress",
	})

	e.runStartupCleanup()
	e.runStartupOrphanedInProgressScan()

	removed := removeLabelsCalled(client, 34)
	if !hasRemovedLabel(removed, "fabrik:locked:testuser") {
		t.Errorf("expected lock label to be removed; got: %v", removed)
	}
	if !hasRemovedLabel(removed, "stage:Implement:in_progress") {
		t.Errorf("expected in_progress label to be removed; got: %v", removed)
	}
}

// TestRunStartupBareInProgressScan_RemovesBareLabel verifies the R7/#1393/
// ADR-1393 AC7 shape: a stage:X:in_progress label with no fabrik:locked:*
// label and no stage:X:complete/failed sibling is healed on next startup —
// the case neither runStartupCleanup (lock-gated) nor
// runStartupOrphanedInProgressScan (terminal-sibling-gated) matches.
func TestRunStartupBareInProgressScan_RemovesBareLabel(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 40, []string{"stage:Implement:in_progress"})

	e.runStartupBareInProgressScan()

	removed := removeLabelsCalled(client, 40)
	if !hasRemovedLabel(removed, "stage:Implement:in_progress") {
		t.Errorf("expected bare stage:Implement:in_progress to be removed; got: %v", removed)
	}
}

// TestRunStartupBareInProgressScan_SkipsWhenLockPresent verifies that a
// stage:X:in_progress label co-occurring with any fabrik:locked:<user> label
// is left alone — that shape belongs to runStartupCleanup's lock-gated pass,
// not this one, so the two passes don't double-handle or race on it.
func TestRunStartupBareInProgressScan_SkipsWhenLockPresent(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 41, []string{
		"fabrik:locked:testuser",
		"stage:Implement:in_progress",
	})

	e.runStartupBareInProgressScan()

	removed := removeLabelsCalled(client, 41)
	if len(removed) > 0 {
		t.Errorf("expected no labels removed while a lock label is present (owned by runStartupCleanup instead); got: %v", removed)
	}
}

// TestRunStartupBareInProgressScan_SkipsWhenTerminalSiblingPresent verifies
// that a stage:X:in_progress label co-occurring with stage:X:complete or
// stage:X:failed is left to runStartupOrphanedInProgressScan, not
// double-removed by this pass too.
func TestRunStartupBareInProgressScan_SkipsWhenTerminalSiblingPresent(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 42, []string{
		"stage:Implement:complete",
		"stage:Implement:in_progress",
	})

	e.runStartupBareInProgressScan()

	removed := removeLabelsCalled(client, 42)
	if len(removed) > 0 {
		t.Errorf("expected no labels removed while a terminal sibling is present (owned by runStartupOrphanedInProgressScan instead); got: %v", removed)
	}
}

// TestRunStartupBareInProgressScan_LeavesGenuineInFlightUntouched verifies
// that a genuinely in-flight worker (Worker() != nil) is left untouched —
// this scan is startup-only and, per its own doc comment, defensively skips
// any item with an active Worker even though that should never occur this
// early in startup.
func TestRunStartupBareInProgressScan_LeavesGenuineInFlightUntouched(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 43, []string{"stage:Implement:in_progress"})
	setWorker(e, 43, os.Getpid(), "Implement", time.Now())

	e.runStartupBareInProgressScan()

	removed := removeLabelsCalled(client, 43)
	if len(removed) > 0 {
		t.Errorf("expected no labels removed for an item with an active Worker; got: %v", removed)
	}
}

// TestRunStartupBareInProgressScan_ComposesWithOtherPasses verifies the three
// startup passes run in sequence (as poll.go does) without double-counting or
// panicking: a lock-gated item, a terminal-sibling item, and a bare item are
// each healed exactly once by their own owning pass.
func TestRunStartupBareInProgressScan_ComposesWithOtherPasses(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 50, []string{"fabrik:locked:testuser", "stage:Plan:in_progress"})
	bootstrapItem(t, e, 51, []string{"stage:Review:complete", "stage:Review:in_progress"})
	bootstrapItem(t, e, 52, []string{"stage:Validate:in_progress"})

	e.runStartupCleanup()
	e.runStartupOrphanedInProgressScan()
	e.runStartupBareInProgressScan()

	if removed := removeLabelsCalled(client, 50); !hasRemovedLabel(removed, "stage:Plan:in_progress") {
		t.Errorf("issue #50: expected stage:Plan:in_progress removed by the lock-gated pass; got: %v", removed)
	}
	if removed := removeLabelsCalled(client, 51); !hasRemovedLabel(removed, "stage:Review:in_progress") {
		t.Errorf("issue #51: expected stage:Review:in_progress removed by the terminal-sibling pass; got: %v", removed)
	}
	if removed := removeLabelsCalled(client, 52); !hasRemovedLabel(removed, "stage:Validate:in_progress") {
		t.Errorf("issue #52: expected stage:Validate:in_progress removed by the bare-in_progress pass; got: %v", removed)
	}
}

// TestWorkerStaleTimeoutDefault verifies that workerStaleTimeout() returns 5 minutes
// when Config.WorkerStaleTimeout is zero, and the configured value otherwise.
func TestWorkerStaleTimeoutDefault(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	if got := e.workerStaleTimeout(); got != 5*time.Minute {
		t.Errorf("default workerStaleTimeout = %v, want 5m", got)
	}

	e.cfg.WorkerStaleTimeout = 10 * time.Minute
	if got := e.workerStaleTimeout(); got != 10*time.Minute {
		t.Errorf("configured workerStaleTimeout = %v, want 10m", got)
	}
}

// withSentinelProbe overrides claudeNameFlagSupported and sentinelProbeFn for
// the duration of a test, restoring both on cleanup — mirroring the existing
// save/restore convention for claudeNameFlagSupported elsewhere in this
// package (engine_test.go, invoke_claude_test.go).
func withSentinelProbe(t *testing.T, supported bool, fn func(sentinel string) sentinelProbeResult) {
	t.Helper()
	origSupported := claudeNameFlagSupported
	origFn := sentinelProbeFn
	claudeNameFlagSupported = supported
	sentinelProbeFn = fn
	t.Cleanup(func() {
		claudeNameFlagSupported = origSupported
		sentinelProbeFn = origFn
	})
}

// withProcessListProbe overrides claudeNameFlagSupported and listProcessArgvFn
// for the duration of a test, restoring both on cleanup — the batched-fetch
// analog of withSentinelProbe, for dispatchCandidates' R5 check (poll.go),
// which fetches the process table once per call via listProcessArgvFn and
// matches it against each candidate's sentinel in-process, rather than
// probing per sentinel (#1779 review finding: avoids one `ps` subprocess
// spawn per dispatch-eligible item per poll).
func withProcessListProbe(t *testing.T, supported bool, procs []procArgvEntry, err error) {
	t.Helper()
	origSupported := claudeNameFlagSupported
	origFn := listProcessArgvFn
	claudeNameFlagSupported = supported
	listProcessArgvFn = func() ([]procArgvEntry, error) { return procs, err }
	t.Cleanup(func() {
		claudeNameFlagSupported = origSupported
		listProcessArgvFn = origFn
	})
}

// TestDetectorSentinelLive_KeepsWorkerAndAdoptsPID is the R1/R2/Acceptance-1,3
// case: a PID<=0 worker past workerStaleTimeout whose sentinel IS found live
// must NOT be cleared, and the discovered PID must be adopted into the
// worker handle so ordinary signal-0 liveness governs from then on.
func TestDetectorSentinelLive_KeepsWorkerAndAdoptsPID(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	const adoptedPID = 424242
	var probedSentinel string
	withSentinelProbe(t, true, func(sentinel string) sentinelProbeResult {
		probedSentinel = sentinel
		return sentinelProbeResult{Live: true, PID: adoptedPID}
	})

	bootstrapItem(t, e, 60, []string{"fabrik:locked:testuser", "stage:Implement:in_progress"})
	staleStart := time.Now().Add(-10 * time.Minute)
	e.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     60,
		User:       e.cfg.User,
		AcquiredAt: staleStart,
		Worker: &itemstate.WorkerHandle{
			PID:        0,
			StageName:  "Implement",
			StartedAt:  staleStart,
			LastSignAt: staleStart,
		},
	})

	e.runWorkerDetectorScan()

	wantSentinel := sessionNameSentinel("owner/repo", 60, "Implement")
	if probedSentinel != wantSentinel {
		t.Errorf("probed sentinel = %q, want %q", probedSentinel, wantSentinel)
	}

	w := getWorker(t, e, 60)
	if w == nil {
		t.Fatal("expected Worker to remain non-nil when sentinel is found live")
	}
	if w.PID != adoptedPID {
		t.Errorf("Worker.PID = %d, want %d (adopted from probe)", w.PID, adoptedPID)
	}

	removed := removeLabelsCalled(client, 60)
	if len(removed) > 0 {
		t.Errorf("expected no labels removed when sentinel is found live; got: %v", removed)
	}

	// The item must not be re-dispatchable while its Worker is still present —
	// R5's dispatch guard is asserted independently in poll_helpers_test.go,
	// but this confirms the store-level precondition it depends on.
	snap, err := e.store.Get("owner/repo", 60)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.Worker() == nil {
		t.Fatal("expected snap.Worker() != nil after sentinel-live keep (dispatch guard precondition)")
	}
}

// TestDetectorSentinelNotFound_ClearsWorker is R3/Acceptance-2's sibling with
// the sentinel probe actually engaged (claudeNameFlagSupported=true): an
// affirmative "not found" result must clear exactly like the plain #1303
// timeout path. TestDetectorClearsPIDNeverSetAfterStartedAtTimeout covers the
// claudeNameFlagSupported=false (probe-skipped) variant of this same
// regression guard.
func TestDetectorSentinelNotFound_ClearsWorker(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	var probeCalled bool
	withSentinelProbe(t, true, func(sentinel string) sentinelProbeResult {
		probeCalled = true
		return sentinelProbeResult{Live: false}
	})

	bootstrapItem(t, e, 61, []string{"fabrik:locked:testuser", "stage:Implement:in_progress"})
	staleStart := time.Now().Add(-10 * time.Minute)
	e.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     61,
		User:       e.cfg.User,
		AcquiredAt: staleStart,
		Worker: &itemstate.WorkerHandle{
			PID:        0,
			StageName:  "Implement",
			StartedAt:  staleStart,
			LastSignAt: staleStart,
		},
	})

	e.runWorkerDetectorScan()

	if !probeCalled {
		t.Fatal("expected sentinel probe to be invoked when claudeNameFlagSupported is true")
	}
	if w := getWorker(t, e, 61); w != nil {
		t.Errorf("expected Worker == nil after sentinel-not-found cleanup, got PID=%d", w.PID)
	}
	removed := removeLabelsCalled(client, 61)
	if !hasRemovedLabel(removed, "fabrik:locked:testuser") {
		t.Errorf("expected lock label to be removed; got: %v", removed)
	}
	if !hasRemovedLabel(removed, "stage:Implement:in_progress") {
		t.Errorf("expected in_progress label to be removed; got: %v", removed)
	}
}

// TestDetectorSentinelUnsupportedCLI_SkipsProbe verifies that when
// claudeNameFlagSupported is false (an older installed claude binary), the
// scan never invokes the sentinel probe at all — it degrades straight to the
// plain #1303 clear, since no worker in this process could ever carry a
// sentinel and probing would only ever waste a subprocess spawn.
func TestDetectorSentinelUnsupportedCLI_SkipsProbe(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	var probeCalled bool
	withSentinelProbe(t, false, func(sentinel string) sentinelProbeResult {
		probeCalled = true
		return sentinelProbeResult{Live: true, PID: 1}
	})

	bootstrapItem(t, e, 62, []string{"fabrik:locked:testuser", "stage:Implement:in_progress"})
	staleStart := time.Now().Add(-10 * time.Minute)
	e.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     62,
		User:       e.cfg.User,
		AcquiredAt: staleStart,
		Worker: &itemstate.WorkerHandle{
			PID:        0,
			StageName:  "Implement",
			StartedAt:  staleStart,
			LastSignAt: staleStart,
		},
	})

	e.runWorkerDetectorScan()

	if probeCalled {
		t.Error("expected sentinel probe NOT to be invoked when claudeNameFlagSupported is false")
	}
	if w := getWorker(t, e, 62); w != nil {
		t.Errorf("expected Worker == nil after plain timeout cleanup, got PID=%d", w.PID)
	}
}

// TestDetectorSentinelProbeError_DefersThenClearsAfterBound is the R4/
// Acceptance-4 case: a probe that itself fails (ps error, unsupported
// platform, timeout) must not clear immediately, but must not defer forever
// either — after sentinelProbeUnverifiableCycleLimit consecutive scan-cycle
// failures, the worker is cleared, distinctly logged as unverified.
func TestDetectorSentinelProbeError_DefersThenClearsAfterBound(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	probeErr := errSentinelProbeUnsupported
	withSentinelProbe(t, true, func(sentinel string) sentinelProbeResult {
		return sentinelProbeResult{Err: probeErr}
	})

	bootstrapItem(t, e, 63, []string{"fabrik:locked:testuser", "stage:Implement:in_progress"})
	staleStart := time.Now().Add(-10 * time.Minute)
	e.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     63,
		User:       e.cfg.User,
		AcquiredAt: staleStart,
		Worker: &itemstate.WorkerHandle{
			PID:        0,
			StageName:  "Implement",
			StartedAt:  staleStart,
			LastSignAt: staleStart,
		},
	})

	for i := 1; i < sentinelProbeUnverifiableCycleLimit; i++ {
		e.runWorkerDetectorScan()
		if w := getWorker(t, e, 63); w == nil {
			t.Fatalf("expected Worker to remain non-nil before the R4 bound is reached (cycle %d/%d)", i, sentinelProbeUnverifiableCycleLimit)
		}
		removed := removeLabelsCalled(client, 63)
		if len(removed) > 0 {
			t.Fatalf("expected no labels removed before the R4 bound is reached (cycle %d/%d); got: %v", i, sentinelProbeUnverifiableCycleLimit, removed)
		}
	}

	// The Nth cycle reaches the bound and clears.
	e.runWorkerDetectorScan()
	if w := getWorker(t, e, 63); w != nil {
		t.Errorf("expected Worker == nil once the R4 bound is reached, got PID=%d", w.PID)
	}
	removed := removeLabelsCalled(client, 63)
	if !hasRemovedLabel(removed, "fabrik:locked:testuser") {
		t.Errorf("expected lock label to be removed once the R4 bound is reached; got: %v", removed)
	}
	if !hasRemovedLabel(removed, "stage:Implement:in_progress") {
		t.Errorf("expected in_progress label to be removed once the R4 bound is reached; got: %v", removed)
	}
}

// TestDetectorSentinelProbeError_RecoveryResetsFailureCount verifies that a
// probe which fails and then succeeds (finding the sentinel live) resets the
// R4 consecutive-failure count — a later run of failures must again get the
// full sentinelProbeUnverifiableCycleLimit grace period, not an
// already-partially-consumed one.
func TestDetectorSentinelProbeError_RecoveryResetsFailureCount(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})

	var live bool
	withSentinelProbe(t, true, func(sentinel string) sentinelProbeResult {
		if live {
			return sentinelProbeResult{Live: true, PID: 777}
		}
		return sentinelProbeResult{Err: errSentinelProbeUnsupported}
	})

	bootstrapItem(t, e, 64, []string{"fabrik:locked:testuser", "stage:Implement:in_progress"})
	staleStart := time.Now().Add(-10 * time.Minute)
	e.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     64,
		User:       e.cfg.User,
		AcquiredAt: staleStart,
		Worker: &itemstate.WorkerHandle{
			PID:        0,
			StageName:  "Implement",
			StartedAt:  staleStart,
			LastSignAt: staleStart,
		},
	})

	// Accumulate failures right up to (but not reaching) the bound.
	for i := 1; i < sentinelProbeUnverifiableCycleLimit; i++ {
		e.runWorkerDetectorScan()
	}
	if w := getWorker(t, e, 64); w == nil {
		t.Fatal("expected Worker to remain non-nil before the bound is reached")
	}

	// Now the probe recovers (sentinel found live) — resets the counter, adopts the PID.
	live = true
	e.runWorkerDetectorScan()
	w := getWorker(t, e, 64)
	if w == nil {
		t.Fatal("expected Worker to remain non-nil once the sentinel is found live")
	}
	if w.PID != 777 {
		t.Errorf("Worker.PID = %d, want 777 (adopted on recovery)", w.PID)
	}

	// Probe fails again. If the counter had NOT been reset, this single
	// failure would already be at/over the bound and clear immediately.
	// Since PID is now 777 (>0), the scan takes the ordinary signal-0 path,
	// not the sentinel-probe path at all — so instead directly verify the
	// failure map was cleared via a fresh unverifiable episode from scratch.
	live = false
	e.store.Apply(itemstate.WorkerPIDSet{Repo: "owner/repo", Number: 64, PID: 0})
	for i := 1; i < sentinelProbeUnverifiableCycleLimit; i++ {
		e.runWorkerDetectorScan()
		if w := getWorker(t, e, 64); w == nil {
			t.Fatalf("expected Worker to remain non-nil on fresh episode cycle %d/%d (counter should have reset on recovery)", i, sentinelProbeUnverifiableCycleLimit)
		}
	}
	e.runWorkerDetectorScan()
	if w := getWorker(t, e, 64); w != nil {
		t.Errorf("expected Worker == nil once the fresh episode reaches the bound, got PID=%d", w.PID)
	}
}
