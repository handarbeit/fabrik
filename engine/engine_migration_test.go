package engine

// engine_migration_test.go — Phase 3-E integration tests
//
// Verifies that per-item engine state (formerly stored in Engine struct maps) now
// lives correctly in itemstate.Store and is accessible via store.Get/store.Apply.
// These tests correspond to the acceptance criteria in issue #517.

import (
	"sync"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
)

// TestMigration_LockAcquireReleaseRoundtrip verifies the lock acquire → read → release
// cycle via the store: acquire sets HeldByThis=true, release clears it (Lock==nil).
func TestMigration_LockAcquireReleaseRoundtrip(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	// Before acquire: item may not exist yet; if it does, no lock.
	if snap0, err := eng.store.Get("owner/repo", 42); err == nil {
		if snap0.Lock() != nil {
			t.Fatal("expected no lock before acquire")
		}
	}

	// Acquire.
	eng.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     42,
		User:       "testuser",
		AcquiredAt: time.Now(),
	})

	// After acquire: lock present, HeldByThis=true.
	snap1, _ := eng.store.Get("owner/repo", 42)
	lock1 := snap1.Lock()
	if lock1 == nil {
		t.Fatal("expected lock to be set after acquire")
	}
	if !lock1.HeldByThis {
		t.Error("expected HeldByThis=true after LocalLockAcquired")
	}

	// Second Get returns same state.
	snap2, _ := eng.store.Get("owner/repo", 42)
	if lock2 := snap2.Lock(); lock2 == nil || !lock2.HeldByThis {
		t.Error("second Get should still show held lock")
	}

	// Release.
	eng.store.Apply(itemstate.LocalLockReleased{Repo: "owner/repo", Number: 42})

	// After release: Lock==nil, HeldByThis effectively false.
	snap3, _ := eng.store.Get("owner/repo", 42)
	if snap3.Lock() != nil {
		t.Error("expected Lock==nil after LocalLockReleased")
	}
}

// TestMigration_LockDerivedField verifies HeldByThis transitions:
// nil → set by self (HeldByThis=true) → released (Lock==nil).
// (Other-user locks arrive via webhook IssueLabeled and are not emitted by LocalLockAcquired.)
func TestMigration_LockDerivedField(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	// nil state.
	snap, _ := eng.store.Get("owner/repo", 5)
	if snap.Lock() != nil {
		t.Fatal("initial lock should be nil")
	}

	// Set by self.
	eng.store.Apply(itemstate.LocalLockAcquired{Repo: "owner/repo", Number: 5, User: "testuser", AcquiredAt: time.Now()})
	snap, _ = eng.store.Get("owner/repo", 5)
	if lock := snap.Lock(); lock == nil || !lock.HeldByThis {
		t.Error("after self-acquire: Lock must be non-nil and HeldByThis=true")
	}

	// Release → nil.
	eng.store.Apply(itemstate.LocalLockReleased{Repo: "owner/repo", Number: 5})
	snap, _ = eng.store.Get("owner/repo", 5)
	if snap.Lock() != nil {
		t.Error("after release: Lock must be nil")
	}
}

// TestMigration_TokenUsageUpdate verifies that InvocationRecorded stores
// LastTokenUsage, LastInvocationCompleted, and LastInvocationBlocked atomically.
func TestMigration_TokenUsageUpdate(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	usage := itemstate.TokenUsage{InputTokens: 100, OutputTokens: 200, CacheReadTokens: 50}
	eng.store.Apply(itemstate.InvocationRecorded{
		Repo:      "owner/repo",
		Number:    10,
		Usage:     usage,
		Completed: true,
		Blocked:   false,
	})

	snap, _ := eng.store.Get("owner/repo", 10)
	st := snap.State()
	if st.LastTokenUsage.InputTokens != 100 || st.LastTokenUsage.OutputTokens != 200 || st.LastTokenUsage.CacheReadTokens != 50 {
		t.Errorf("unexpected LastTokenUsage: %+v", st.LastTokenUsage)
	}
	if !st.LastInvocationCompleted {
		t.Error("expected LastInvocationCompleted=true")
	}
	if st.LastInvocationBlocked {
		t.Error("expected LastInvocationBlocked=false")
	}
}

// TestMigration_CompletionFlags verifies completed=true, blocked=false → correct snapshot.
func TestMigration_CompletionFlags(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	eng.store.Apply(itemstate.InvocationRecorded{
		Repo:      "owner/repo",
		Number:    11,
		Completed: true,
		Blocked:   false,
	})

	snap, _ := eng.store.Get("owner/repo", 11)
	st := snap.State()
	if !st.LastInvocationCompleted {
		t.Error("expected LastInvocationCompleted=true")
	}
	if st.LastInvocationBlocked {
		t.Error("expected LastInvocationBlocked=false")
	}

	// Overwrite with blocked invocation.
	eng.store.Apply(itemstate.InvocationRecorded{
		Repo:      "owner/repo",
		Number:    11,
		Completed: false,
		Blocked:   true,
	})
	snap2, _ := eng.store.Get("owner/repo", 11)
	st2 := snap2.State()
	if st2.LastInvocationCompleted {
		t.Error("expected LastInvocationCompleted=false after blocked invocation")
	}
	if !st2.LastInvocationBlocked {
		t.Error("expected LastInvocationBlocked=true after blocked invocation")
	}
}

// TestMigration_DeepFetchCooldown verifies the 10×PollSeconds cooldown semantics:
// recent failure → itemMayNeedWork returns false; expired failure → returns true.
func TestMigration_DeepFetchCooldown(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1 // 10-second cooldown

	item := gh.ProjectItem{Number: 51, Status: "Research", ItemID: "PVTI_51"}

	// Recent failure: within cooldown.
	eng.store.Apply(itemstate.DeepFetchFailed{Repo: "owner/repo", Number: 51, At: time.Now()})
	if eng.itemMayNeedWork(item) {
		t.Error("item with recent deep-fetch failure should be skipped (within cooldown)")
	}

	// Expired failure: past 10×PollSeconds.
	eng.store.Apply(itemstate.DeepFetchFailed{Repo: "owner/repo", Number: 51, At: time.Now().Add(-20 * time.Second)})
	if !eng.itemMayNeedWork(item) {
		t.Error("item with expired deep-fetch failure cooldown should be retried")
	}
}

// TestMigration_DeepFetchSuccessClears verifies that ItemDeepFetched zeros LastDeepFetchFailureAt.
func TestMigration_DeepFetchSuccessClears(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	eng.store.Apply(itemstate.DeepFetchFailed{Repo: "owner/repo", Number: 20, At: time.Now().Add(-time.Minute)})

	snap1, _ := eng.store.Get("owner/repo", 20)
	if snap1.State().LastDeepFetchFailureAt.IsZero() {
		t.Fatal("expected LastDeepFetchFailureAt to be set after DeepFetchFailed")
	}

	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:       "owner/repo",
		Number:     20,
		FreshState: gh.ProjectItem{Number: 20},
	})

	snap2, _ := eng.store.Get("owner/repo", 20)
	if !snap2.State().LastDeepFetchFailureAt.IsZero() {
		t.Error("expected LastDeepFetchFailureAt zeroed after ItemDeepFetched")
	}
}

// TestMigration_PRChecksObservedMonotonic verifies HasHadChecks transitions false→true
// when PRChecksObserved is applied and remains true on subsequent applications.
func TestMigration_PRChecksObservedMonotonic(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	// Initially false.
	snap0, _ := eng.store.Get("owner/repo", 30)
	if lpr := snap0.LinkedPR(); lpr != nil && lpr.HasHadChecks {
		t.Fatal("expected HasHadChecks=false initially")
	}

	// First PRChecksObserved: false→true.
	eng.store.Apply(itemstate.PRChecksObserved{Repo: "owner/repo", Number: 30})
	snap1, _ := eng.store.Get("owner/repo", 30)
	lpr1 := snap1.LinkedPR()
	if lpr1 == nil || !lpr1.HasHadChecks {
		t.Fatal("expected HasHadChecks=true after PRChecksObserved")
	}

	// Second PRChecksObserved: stays true (monotonic).
	eng.store.Apply(itemstate.PRChecksObserved{Repo: "owner/repo", Number: 30})
	snap2, _ := eng.store.Get("owner/repo", 30)
	lpr2 := snap2.LinkedPR()
	if lpr2 == nil || !lpr2.HasHadChecks {
		t.Error("expected HasHadChecks to remain true after second PRChecksObserved")
	}
}

// TestMigration_CIMergePendingSetAndClear verifies CIMergePendingSince set/clear transitions.
func TestMigration_CIMergePendingSetAndClear(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	// Initially zero.
	snap0, _ := eng.store.Get("owner/repo", 40)
	if lpr := snap0.LinkedPR(); lpr != nil && !lpr.CIMergePendingSince.IsZero() {
		t.Fatal("expected CIMergePendingSince zero initially")
	}

	// Set via CIMergePendingStarted.
	pendingAt := time.Now().Add(-5 * time.Minute)
	eng.store.Apply(itemstate.CIMergePendingStarted{Repo: "owner/repo", Number: 40, At: pendingAt})
	snap1, _ := eng.store.Get("owner/repo", 40)
	lpr1 := snap1.LinkedPR()
	if lpr1 == nil {
		t.Fatal("expected LinkedPR to be non-nil after CIMergePendingStarted")
	}
	if lpr1.CIMergePendingSince.IsZero() {
		t.Error("expected CIMergePendingSince to be set after CIMergePendingStarted")
	}
	if !lpr1.CIMergePendingSince.Equal(pendingAt) {
		t.Errorf("CIMergePendingSince = %v, want %v", lpr1.CIMergePendingSince, pendingAt)
	}

	// Clear via CIMergePendingCleared.
	eng.store.Apply(itemstate.CIMergePendingCleared{Repo: "owner/repo", Number: 40})
	snap2, _ := eng.store.Get("owner/repo", 40)
	if lpr2 := snap2.LinkedPR(); lpr2 != nil && !lpr2.CIMergePendingSince.IsZero() {
		t.Error("expected CIMergePendingSince zeroed after CIMergePendingCleared")
	}
}

// TestMigration_SnapshotImmutability verifies that a snapshot taken before an Apply
// is not affected by the subsequent mutation.
func TestMigration_SnapshotImmutability(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	eng.store.Apply(itemstate.PRChecksObserved{Repo: "owner/repo", Number: 50})
	snapBefore, _ := eng.store.Get("owner/repo", 50)

	// Apply another mutation.
	eng.store.Apply(itemstate.CIMergePendingStarted{Repo: "owner/repo", Number: 50, At: time.Now()})

	// snapBefore should not see the new CIMergePendingSince.
	if lpr := snapBefore.LinkedPR(); lpr != nil && !lpr.CIMergePendingSince.IsZero() {
		t.Error("earlier snapshot should not reflect mutations applied after it was taken")
	}
}

// TestMigration_ConcurrentCompletions verifies that concurrent InvocationRecorded
// applications for different issues are race-detector clean.
func TestMigration_ConcurrentCompletions(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(num int) {
			defer wg.Done()
			eng.store.Apply(itemstate.InvocationRecorded{
				Repo:      "owner/repo",
				Number:    num,
				Completed: true,
				Usage:     itemstate.TokenUsage{InputTokens: num * 10},
			})
		}(i)
	}
	wg.Wait()

	// All goroutines wrote their state; verify each is readable.
	for i := 0; i < n; i++ {
		snap, _ := eng.store.Get("owner/repo", i)
		if !snap.State().LastInvocationCompleted {
			t.Errorf("issue #%d: expected LastInvocationCompleted=true", i)
		}
	}
}
