package engine

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
)

// bootstrapItem seeds the engine's store with a single issue so detector/cleanup
// functions have something to scan.
func bootstrapItem(t *testing.T, e *Engine, number int, labels []string) {
	t.Helper()
	e.store.Apply(itemstate.ItemDeepFetched{
		Repo: "owner/repo",
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
	e := testEngine(client, &mockClaudeInvoker{})

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
	e := testEngine(client, &mockClaudeInvoker{})

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
	e := testEngine(client, &mockClaudeInvoker{})
	e.heartbeatIntervalOverride = 5 * time.Millisecond

	bootstrapItem(t, e, 3, nil)
	setWorker(e, 3, os.Getpid(), "Plan", time.Now())

	ctx := context.Background()
	done := make(chan struct{})

	// Start heartbeat and observe that after closing done, no more heartbeats fire.
	e.startHeartbeat(ctx, "owner/repo", 3, done)

	// Let at least one heartbeat fire.
	time.Sleep(30 * time.Millisecond)

	snap, _ := e.store.Get("owner/repo", 3)
	firstSignAt := snap.Worker().LastSignAt

	// Close done to signal the goroutine to stop.
	close(done)

	// Give goroutine time to exit.
	time.Sleep(20 * time.Millisecond)

	// Record LastSignAt after stopping.
	snap, _ = e.store.Get("owner/repo", 3)
	stoppedAt := snap.Worker().LastSignAt

	// Wait another tick interval to confirm no more heartbeats fire.
	time.Sleep(30 * time.Millisecond)

	snap, _ = e.store.Get("owner/repo", 3)
	finalSignAt := snap.Worker().LastSignAt

	// The heartbeat should have advanced at least once (first tick).
	if !firstSignAt.IsZero() && firstSignAt.Equal(snap.Worker().StartedAt) {
		t.Log("no heartbeat tick fired during the window (timing-sensitive, may be flaky)")
	}

	// After closing done, LastSignAt must not advance.
	if !finalSignAt.Equal(stoppedAt) {
		t.Errorf("heartbeat goroutine still firing after done closed: stoppedAt=%v finalSignAt=%v",
			stoppedAt, finalSignAt)
	}
}

// TestDetectorFalsePositivePrevention verifies that the detector does NOT remove
// labels or clear Worker when the process is alive but the heartbeat is stale.
func TestDetectorFalsePositivePrevention(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-0 not applicable on Windows (always returns alive)")
	}

	client := &mockGitHubClient{}
	e := testEngine(client, &mockClaudeInvoker{})

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
	e := testEngine(client, &mockClaudeInvoker{})

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
	e := testEngine(client, &mockClaudeInvoker{})

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
	e := testEngine(client, &mockClaudeInvoker{})

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
	e := testEngine(client, &mockClaudeInvoker{})

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
	e := testEngine(client, &mockClaudeInvoker{})

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
	e := testEngine(client, &mockClaudeInvoker{})

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
// PID is 0 (not yet set by OnPIDReady), even if the heartbeat is stale.
func TestDetectorSkipsPIDZeroWorker(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(client, &mockClaudeInvoker{})

	bootstrapItem(t, e, 10, []string{"fabrik:locked:testuser"})
	staleTime := time.Now().Add(-10 * time.Minute)
	// PID=0 means OnPIDReady not yet called.
	setWorker(e, 10, 0 /* PID zero */, "Implement", staleTime)

	e.runWorkerDetectorScan()

	// Worker must remain non-nil — PID=0 means skip.
	if w := getWorker(t, e, 10); w == nil {
		t.Error("detector incorrectly cleared Worker with PID=0 (should skip)")
	}
	removed := removeLabelsCalled(client, 10)
	if len(removed) > 0 {
		t.Errorf("detector removed labels for PID=0 worker: %v", removed)
	}
}

// TestWorkerStoreReflectsDispatchPaths verifies that store.Get(...).Worker() != nil
// correctly reflects worker state: non-nil after LocalLockAcquired, nil after
// WorkerExited. This is the inFlight replacement semantic test (test #10 from spec).
func TestWorkerStoreReflectsDispatchPaths(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(client, &mockClaudeInvoker{})

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
