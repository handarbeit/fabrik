package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// TestRegression504_LastAttemptAtNotRefreshedByDeepFetch verifies that
// ItemDeepFetched (board refresh) does not reset LastAttemptAt. In the old
// processedSet design the key was shared, so a board reconcile could silently
// reset the invocation gate and cause immediate re-dispatch.
func TestRegression504_LastAttemptAtNotRefreshedByDeepFetch(t *testing.T) {
	store := itemstate.NewStore(nil)
	t1 := time.Now().Add(-5 * time.Minute)

	store.Apply(itemstate.StageAttempted{Repo: "owner/repo", Number: 1, StageName: "Implement", At: t1})
	store.Apply(itemstate.ItemDeepFetched{
		Repo:       "owner/repo",
		Number:     1,
		FreshState: gh.ProjectItem{Repo: "owner/repo", Number: 1, Status: "Implement"},
	})

	snap, err := store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !snap.LastAttemptAt("Implement").Equal(t1) {
		t.Errorf("LastAttemptAt was wiped by ItemDeepFetched: got %v, want %v", snap.LastAttemptAt("Implement"), t1)
	}
}

// TestRegression488_TerminalItemCooldownPreserved verifies that CooldownAt
// survives a BoardReconciled (shallow board refresh). The old processedSet was
// keyed in-memory and inconsistently populated; shallow refreshes could reset it.
func TestRegression488_TerminalItemCooldownPreserved(t *testing.T) {
	store := itemstate.NewStore(nil)
	until := time.Now().Add(2 * time.Minute)

	store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 7, Reason: "periodic-re-eval", Until: until})
	store.Apply(itemstate.BoardReconciled{
		Items: []gh.ProjectItem{{Repo: "owner/repo", Number: 7, Status: "Validate"}},
	})

	snap, err := store.Get("owner/repo", 7)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !snap.CooldownAt("periodic-re-eval").Equal(until) {
		t.Errorf("CooldownAt wiped by BoardReconciled: got %v, want %v", snap.CooldownAt("periodic-re-eval"), until)
	}
}

// TestDualCooldownIndependence verifies that LastAttemptAt (per-stage) and
// CooldownAt (per-reason) are fully independent. Setting or clearing one must
// not affect the other.
func TestDualCooldownIndependence(t *testing.T) {
	store := itemstate.NewStore(nil)
	t1 := time.Now().Add(-3 * time.Minute)
	t2 := time.Now().Add(10 * time.Minute)

	store.Apply(itemstate.StageAttempted{Repo: "owner/repo", Number: 2, StageName: "Review", At: t1})
	store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 2, Reason: "periodic-re-eval", Until: t2})

	snap, _ := store.Get("owner/repo", 2)
	if !snap.LastAttemptAt("Review").Equal(t1) {
		t.Errorf("LastAttemptAt = %v, want %v", snap.LastAttemptAt("Review"), t1)
	}
	if !snap.CooldownAt("periodic-re-eval").Equal(t2) {
		t.Errorf("CooldownAt = %v, want %v", snap.CooldownAt("periodic-re-eval"), t2)
	}

	// Clearing LastAttemptAt must not touch CooldownAt.
	store.Apply(itemstate.StageLastAttemptCleared{Repo: "owner/repo", Number: 2, StageName: "Review"})
	snap, _ = store.Get("owner/repo", 2)
	if !snap.LastAttemptAt("Review").IsZero() {
		t.Errorf("LastAttemptAt after clear = %v, want zero", snap.LastAttemptAt("Review"))
	}
	if !snap.CooldownAt("periodic-re-eval").Equal(t2) {
		t.Errorf("CooldownAt affected by LastAttemptAt clear: got %v, want %v", snap.CooldownAt("periodic-re-eval"), t2)
	}
}

// TestStageAttemptsLifecycle verifies the Attempts counter semantics:
// StageAttempted does NOT increment Attempts; StageRetryIncremented does;
// StageRetryCleared resets to zero.
func TestStageAttemptsLifecycle(t *testing.T) {
	store := itemstate.NewStore(nil)
	repo, number := "owner/repo", 3

	snap, _ := store.Get(repo, number)
	if snap.Attempts("Research") != 0 {
		t.Errorf("initial Attempts = %d, want 0", snap.Attempts("Research"))
	}

	// Invocation recorded — must NOT bump Attempts.
	store.Apply(itemstate.StageAttempted{Repo: repo, Number: number, StageName: "Research", At: time.Now()})
	snap, _ = store.Get(repo, number)
	if snap.Attempts("Research") != 0 {
		t.Errorf("Attempts after StageAttempted = %d, want 0 (only StageRetryIncremented increments)", snap.Attempts("Research"))
	}
	if snap.LastAttemptAt("Research").IsZero() {
		t.Error("LastAttemptAt must be set by StageAttempted")
	}

	store.Apply(itemstate.StageRetryIncremented{Repo: repo, Number: number, StageName: "Research"})
	store.Apply(itemstate.StageRetryIncremented{Repo: repo, Number: number, StageName: "Research"})
	snap, _ = store.Get(repo, number)
	if snap.Attempts("Research") != 2 {
		t.Errorf("Attempts after 2× RetryIncremented = %d, want 2", snap.Attempts("Research"))
	}

	store.Apply(itemstate.StageRetryCleared{Repo: repo, Number: number, StageName: "Research"})
	snap, _ = store.Get(repo, number)
	if snap.Attempts("Research") != 0 {
		t.Errorf("Attempts after RetryCleared = %d, want 0", snap.Attempts("Research"))
	}
}

// TestCycleCountLifecycle verifies that review/ci-fix/rebase cycle counts are
// per-stage and independent, and that EngineCyclesCleared resets only the
// targeted stage.
func TestCycleCountLifecycle(t *testing.T) {
	store := itemstate.NewStore(nil)
	repo, number := "owner/repo", 4

	for i := 0; i < 3; i++ {
		store.Apply(itemstate.ReviewCycleIncremented{Repo: repo, Number: number, StageName: "Review"})
	}
	store.Apply(itemstate.CIFixCycleIncremented{Repo: repo, Number: number, StageName: "Validate"})
	store.Apply(itemstate.RebaseCycleIncremented{Repo: repo, Number: number, StageName: "Validate"})
	store.Apply(itemstate.RebaseCycleIncremented{Repo: repo, Number: number, StageName: "Validate"})

	snap, _ := store.Get(repo, number)
	if snap.ReviewCycles("Review") != 3 {
		t.Errorf("ReviewCycles(Review) = %d, want 3", snap.ReviewCycles("Review"))
	}
	if snap.CIFixCycles("Validate") != 1 {
		t.Errorf("CIFixCycles(Validate) = %d, want 1", snap.CIFixCycles("Validate"))
	}
	if snap.RebaseCycles("Validate") != 2 {
		t.Errorf("RebaseCycles(Validate) = %d, want 2", snap.RebaseCycles("Validate"))
	}
	// Cross-stage counts must be zero.
	if snap.ReviewCycles("Validate") != 0 {
		t.Errorf("ReviewCycles(Validate) = %d, want 0 (must be per-stage)", snap.ReviewCycles("Validate"))
	}

	// EngineCyclesCleared for Review must not affect Validate.
	store.Apply(itemstate.EngineCyclesCleared{Repo: repo, Number: number, StageName: "Review"})
	snap, _ = store.Get(repo, number)
	if snap.ReviewCycles("Review") != 0 {
		t.Errorf("ReviewCycles(Review) after clear = %d, want 0", snap.ReviewCycles("Review"))
	}
	if snap.RebaseCycles("Validate") != 2 {
		t.Errorf("RebaseCycles(Validate) after unrelated clear = %d, want 2", snap.RebaseCycles("Validate"))
	}
}

// TestPausedByEngineVsUserPause verifies that PausedByEngine is tracked per-stage
// and is independent of the issue-level fabrik:paused label.
func TestPausedByEngineVsUserPause(t *testing.T) {
	store := itemstate.NewStore(nil)
	repo, number := "owner/repo", 5

	store.Apply(itemstate.EnginePaused{Repo: repo, Number: number, StageName: "Implement"})

	snap, _ := store.Get(repo, number)
	if !snap.PausedByEngine("Implement") {
		t.Error("PausedByEngine(Implement) = false, want true")
	}
	if snap.PausedByEngine("Review") {
		t.Error("PausedByEngine(Review) = true, want false (must be per-stage)")
	}

	store.Apply(itemstate.EngineUnpaused{Repo: repo, Number: number, StageName: "Implement"})
	snap, _ = store.Get(repo, number)
	if snap.PausedByEngine("Implement") {
		t.Error("PausedByEngine(Implement) = true after Unpaused, want false")
	}
}

// TestProcessedCommentsRocketDetection verifies that CommentProcessed mutations
// are stored and retrieved correctly, providing the durable "already seen"
// gate that rocket reactions provide at the GitHub API level.
func TestProcessedCommentsRocketDetection(t *testing.T) {
	store := itemstate.NewStore(nil)
	repo, number := "owner/repo", 6
	at := time.Now()

	store.Apply(itemstate.CommentProcessed{Repo: repo, Number: number, CommentID: "c1", At: at})

	snap, _ := store.Get(repo, number)
	if snap.CommentProcessed("c1").IsZero() {
		t.Error("CommentProcessed(c1) = zero, want non-zero")
	}
	if !snap.CommentProcessed("c1").Equal(at) {
		t.Errorf("CommentProcessed(c1) = %v, want %v", snap.CommentProcessed("c1"), at)
	}
	if !snap.CommentProcessed("c2").IsZero() {
		t.Error("CommentProcessed(c2) should be zero (never processed)")
	}
}
