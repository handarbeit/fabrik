package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// ---- isTerminalPredicate unit tests (Task 11) ----

func TestIsTerminalPredicate_CleanupCompleteNoTransient(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	labels := []string{"stage:Done:complete"}
	if !isTerminalPredicate(labels, "Done", stagesCfg) {
		t.Error("expected terminal=true for cleanup stage + complete label + no transient")
	}
}

func TestIsTerminalPredicate_HasAwaitingCI(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	labels := []string{"stage:Done:complete", "fabrik:awaiting-ci"}
	if isTerminalPredicate(labels, "Done", stagesCfg) {
		t.Error("expected terminal=false when fabrik:awaiting-ci is present")
	}
}

func TestIsTerminalPredicate_HasLockLabel(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	labels := []string{"stage:Done:complete", "fabrik:locked:bob"}
	if isTerminalPredicate(labels, "Done", stagesCfg) {
		t.Error("expected terminal=false when fabrik:locked:* is present")
	}
}

func TestIsTerminalPredicate_NoCompleteLabel(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	labels := []string{"bug", "enhancement"}
	if isTerminalPredicate(labels, "Done", stagesCfg) {
		t.Error("expected terminal=false without stage:Done:complete label")
	}
}

func TestIsTerminalPredicate_NonCleanupStage(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	labels := []string{"stage:Implement:complete"}
	if isTerminalPredicate(labels, "Implement", stagesCfg) {
		t.Error("expected terminal=false for non-cleanup stage")
	}
}

func TestIsTerminalPredicate_UnknownStatus(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	labels := []string{"stage:Unknown:complete"}
	if isTerminalPredicate(labels, "Unknown", stagesCfg) {
		t.Error("expected terminal=false for unknown status")
	}
}

func TestIsTerminalPredicate_AllTransientLabels(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	for _, tl := range transientLifecycleLabels {
		labels := []string{"stage:Done:complete", tl}
		if isTerminalPredicate(labels, "Done", stagesCfg) {
			t.Errorf("expected terminal=false when transient label %q is present", tl)
		}
	}
}

// ---- runStartupTerminalScan tests (Task 12) ----

func testEngineWithCleanup(t *testing.T, client *mockGitHubClient, claude *mockClaudeInvoker) *Engine {
	t.Helper()
	return NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithCleanup(),
		},
		client,
		claude,
		NewWorktreeManager(t.TempDir()),
	)
}

func TestRunStartupTerminalScan_MarksTerminalItems(t *testing.T) {
	eng := testEngineWithCleanup(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	// Item 1: terminal — Done + stage:Done:complete + no transient labels.
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Repo:     "owner/repo",
		Number:   1,
		Status:   "Done",
		IsClosed: true,
		Labels:   []string{"stage:Done:complete"},
	}})

	// Item 2: not terminal — Done + stage:Done:complete but has transient label.
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Repo:     "owner/repo",
		Number:   2,
		Status:   "Done",
		IsClosed: true,
		Labels:   []string{"stage:Done:complete", "fabrik:awaiting-ci"},
	}})

	// Item 3: not terminal — non-Done stage.
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Repo:     "owner/repo",
		Number:   3,
		Status:   "Implement",
		IsClosed: false,
		Labels:   []string{"stage:Implement:complete"},
	}})

	eng.runStartupTerminalScan()

	snap1, _ := eng.store.Get("owner/repo", 1)
	if !snap1.IsTerminal() {
		t.Error("item 1 (terminal): expected IsTerminal()=true after runStartupTerminalScan")
	}

	snap2, _ := eng.store.Get("owner/repo", 2)
	if snap2.IsTerminal() {
		t.Error("item 2 (has transient label): expected IsTerminal()=false")
	}

	snap3, _ := eng.store.Get("owner/repo", 3)
	if snap3.IsTerminal() {
		t.Error("item 3 (non-Done stage): expected IsTerminal()=false")
	}
}

func TestRunStartupTerminalScan_IdempotentOnAlreadyTerminal(t *testing.T) {
	eng := testEngineWithCleanup(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Repo:     "owner/repo",
		Number:   1,
		Status:   "Done",
		IsClosed: true,
		Labels:   []string{"stage:Done:complete"},
	}})
	eng.store.Apply(itemstate.TerminalFlagSet{Repo: "owner/repo", Number: 1, Terminal: true})

	// Calling again should be a no-op — no panic, no duplicate logging side-effects.
	eng.runStartupTerminalScan()

	snap, _ := eng.store.Get("owner/repo", 1)
	if !snap.IsTerminal() {
		t.Error("item should still be terminal after second scan")
	}
}

// ---- probe-loop terminal skip tests (Task 13) ----

// testEngineWithCleanupCache creates an Engine with testStagesWithCleanup and a
// live CacheImpl seeded with one item already in "Done" with stage:Done:complete.
func testEngineWithCleanupCache(t *testing.T, client *mockGitHubClient, claude *mockClaudeInvoker) (*Engine, *boardcache.CacheImpl) {
	t.Helper()
	eng := testEngineWithCleanup(t, client, claude)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{
				ID:        "I_001",
				ItemID:    "PVTI_001",
				Number:    1,
				Repo:      "owner/repo",
				Status:    "Done",
				IsClosed:  true,
				Labels:    []string{"stage:Done:complete"},
				UpdatedAt: time.Now().Add(-time.Hour),
			},
		},
	})
	eng.readClient = cache
	return eng, cache
}

func TestRunProbeAndDeepFetch_TerminalItem_SkipsDeepFetch(t *testing.T) {
	var deepFetchCalls int

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// EffectiveUpdatedAt is newer than T1 (would normally trigger deep-fetch).
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
					Status: "Done", EffectiveUpdatedAt: time.Now()},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCleanupCache(t, client, &mockClaudeInvoker{})

	// Mark item 1 as terminal (simulate prior deep-fetch that found it terminal).
	eng.store.Apply(itemstate.TerminalFlagSet{Repo: "owner/repo", Number: 1, Terminal: true})

	eng.runProbeAndDeepFetch(cache)

	if deepFetchCalls != 0 {
		t.Errorf("terminal item in Done: expected 0 FetchItemDetails calls; got %d", deepFetchCalls)
	}

	// Terminal flag must still be set after the skip.
	snap, _ := eng.store.Get("owner/repo", 1)
	if !snap.IsTerminal() {
		t.Error("terminal flag should remain set after probe-loop skip")
	}
}

func TestRunProbeAndDeepFetch_TerminalItem_StatusDrift_ClearsFlagAndDeepFetches(t *testing.T) {
	var deepFetchCalls int

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// Item now shows "Implement" — user dragged it out of Done.
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
					Status: "Implement", EffectiveUpdatedAt: time.Now()},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			item.Labels = []string{"stage:Implement:in_progress"} // populate labels
			return nil
		},
	}
	eng, cache := testEngineWithCleanupCache(t, client, &mockClaudeInvoker{})

	// Pre-set terminal flag.
	eng.store.Apply(itemstate.TerminalFlagSet{Repo: "owner/repo", Number: 1, Terminal: true})

	eng.runProbeAndDeepFetch(cache)

	// Status drifted out of Done → terminal flag should be cleared.
	snap, _ := eng.store.Get("owner/repo", 1)
	if snap.IsTerminal() {
		t.Error("terminal flag should have been cleared when status left Done")
	}

	// Deep-fetch must have fired (item is no longer terminal).
	if deepFetchCalls == 0 {
		t.Error("expected FetchItemDetails called after terminal flag cleared; got 0 calls")
	}
}

// TestIsTerminalPredicate_LockLabelPrefix verifies that any fabrik:locked:*
// label (not just the engine user's own) blocks the predicate.
func TestIsTerminalPredicate_LockLabelPrefix(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	lockVariants := []string{
		"fabrik:locked:alice",
		"fabrik:locked:bob",
		"fabrik:locked:someone-else",
	}
	for _, lk := range lockVariants {
		labels := []string{"stage:Done:complete", lk}
		if isTerminalPredicate(labels, "Done", stagesCfg) {
			t.Errorf("expected terminal=false for lock label %q", lk)
		}
	}
}

// TestRunProbeAndDeepFetch_TerminalItem_MovedBetweenCleanupStages ensures that
// when a terminal item's probe shows a different cleanup stage (not just
// non-cleanup), the terminal flag is cleared and the probe update is applied.
func TestRunProbeAndDeepFetch_TerminalItem_MovedBetweenCleanupStages(t *testing.T) {
	var deepFetchCalls int

	twoCleanupStages := []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "p", Completion: stages.CompletionCriteria{Type: "claude"}},
		{Name: "Done", Order: 90, CleanupWorktree: true},
		{Name: "Archive", Order: 99, CleanupWorktree: true},
	}

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// Item moved from "Done" to "Archive" — both are cleanup stages.
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
					Status: "Archive", EffectiveUpdatedAt: time.Now()},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			// Return labels that do NOT satisfy the terminal predicate for "Archive"
			// (no stage:Archive:complete) so the item is not re-terminalized after fetch.
			item.Labels = []string{"stage:Archive:in_progress"}
			item.Status = "Archive"
			return nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token", MaxConcurrent: 5,
			Stages: twoCleanupStages,
		},
		client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
				Status: "Done", IsClosed: true, Labels: []string{"stage:Done:complete"},
				UpdatedAt: time.Now().Add(-time.Hour)},
		},
	})
	eng.readClient = cache

	// Mark terminal in "Done".
	eng.store.Apply(itemstate.TerminalFlagSet{Repo: "owner/repo", Number: 1, Terminal: true})

	eng.runProbeAndDeepFetch(cache)

	// Terminal flag must have been cleared (status moved to different cleanup stage).
	snap, _ := eng.store.Get("owner/repo", 1)
	if snap.IsTerminal() {
		t.Error("terminal flag should be cleared when item moves to a different cleanup stage")
	}
	// Deep-fetch must have fired.
	if deepFetchCalls == 0 {
		t.Error("expected FetchItemDetails called after terminal cleared on cleanup-stage move; got 0")
	}
}

// TestRunStartupTerminalScan_UsesCleanupStageNotHardcodedDone ensures that
// the terminal scan works for any cleanup stage name, not just "Done".
func TestRunStartupTerminalScan_UsesCleanupStageNotHardcodedDone(t *testing.T) {
	// Create an engine with a cleanup stage named "Archived" instead of "Done".
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages: []*stages.Stage{
				{Name: "Research", Order: 1, Prompt: "p", Completion: stages.CompletionCriteria{Type: "claude"}},
				{Name: "Archived", Order: 99, CleanupWorktree: true},
			},
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)

	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Repo:     "owner/repo",
		Number:   10,
		Status:   "Archived",
		IsClosed: true,
		Labels:   []string{"stage:Archived:complete"},
	}})

	eng.runStartupTerminalScan()

	snap, _ := eng.store.Get("owner/repo", 10)
	if !snap.IsTerminal() {
		t.Error("expected IsTerminal()=true for cleanup stage named 'Archived'")
	}
}

// ---- linkage-drift gate tests (Task 5) ----

// TestRunProbeAndDeepFetch_LinkageDrift_ColdStart_AuthoritativeWrite verifies that
// when an item has never been deep-fetched and the probe finds a different LinkedPRNumber
// than the cache holds, the probe's value is written authoritatively via PRDetailsUpdated
// (not DeepFetchInvalidated). The prToKey reverse index must be updated.
func TestRunProbeAndDeepFetch_LinkageDrift_ColdStart_AuthoritativeWrite(t *testing.T) {
	// Track deep-fetch calls — we allow one (the item has no LastDeepFetchAt,
	// so IsItemCacheFresh returns false and a normal deep-fetch will fire).
	var deepFetchCalls int
	staleTime := time.Now().Add(-2 * time.Hour)
	probeTime := time.Now().Add(-time.Hour)

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
					Status: "Research", EffectiveUpdatedAt: probeTime, LinkedPRNumber: 42},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			item.LinkedPRNumber = 42 // deep-fetch confirms the PR
			return nil
		},
	}
	eng := testEngineWithCleanup(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	// Bootstrap with LinkedPRNumber=0 (old-style bootstrap that did not populate PR number).
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
				Status: "Research", UpdatedAt: staleTime, LinkedPRNumber: 0},
		},
	})
	eng.readClient = cache

	eng.runProbeAndDeepFetch(cache)

	// LinkedPR.Number must be set to 42 (either by PRDetailsUpdated before or by deep-fetch after).
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("item not found: %v", err)
	}
	s := snap.State()
	if s.LinkedPR == nil || s.LinkedPR.Number != 42 {
		t.Errorf("LinkedPR.Number = %d, want 42", func() int {
			if s.LinkedPR != nil {
				return s.LinkedPR.Number
			}
			return 0
		}())
	}

	// prToKey reverse index must be populated so LinkedPRByNumber lookup works.
	_, found := eng.store.GetByPRKey("owner/repo", 42)
	if !found {
		t.Error("prToKey index should have entry for PR #42 after authoritative write")
	}
}

// TestRunProbeAndDeepFetch_LinkageDrift_WarmCache_FiresDeepFetchInvalidated verifies
// that when an item HAS been deep-fetched and the probe finds a different LinkedPRNumber,
// DeepFetchInvalidated fires and the item is re-deep-fetched.
func TestRunProbeAndDeepFetch_LinkageDrift_WarmCache_FiresDeepFetchInvalidated(t *testing.T) {
	var deepFetchCalls int
	warmTime := time.Now().Add(-30 * time.Minute)
	probeTime := warmTime // same time so staleness check does NOT trigger deep-fetch on its own

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// Same timestamp as warm state — staleness alone would NOT trigger a re-fetch.
				// Only the changed LinkedPRNumber (0→99) should cause a re-fetch.
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
					Status: "Research", EffectiveUpdatedAt: probeTime, LinkedPRNumber: 99},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			item.LinkedPRNumber = 99
			return nil
		},
	}
	eng := testEngineWithCleanup(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	// Bootstrap with LinkedPRNumber=0.
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
				Status: "Research", UpdatedAt: warmTime, LinkedPRNumber: 0},
		},
	})
	eng.readClient = cache

	// Simulate a prior deep-fetch so LastDeepFetchAt is non-zero and
	// IsItemCacheFresh would normally return true for warmTime.
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 1,
		FreshState: gh.ProjectItem{
			ID: "I_001", Number: 1, Repo: "owner/repo",
			Status:    "Research",
			UpdatedAt: warmTime,
		},
	})

	eng.runProbeAndDeepFetch(cache)

	// DeepFetchInvalidated should have fired for the warm-cache drift, causing
	// a re-deep-fetch. Without DeepFetchInvalidated, the same-timestamp probe
	// would have been a cache hit (IsItemCacheFresh = true → skip).
	if deepFetchCalls == 0 {
		t.Error("expected FetchItemDetails call after warm-cache linkage drift; got 0 calls")
	}
	// LinkedPR.Number must be updated to 99.
	snap, _ := eng.store.Get("owner/repo", 1)
	s := snap.State()
	if s.LinkedPR == nil || s.LinkedPR.Number != 99 {
		t.Errorf("LinkedPR.Number = %d after warm-cache drift re-fetch, want 99", func() int {
			if s.LinkedPR != nil {
				return s.LinkedPR.Number
			}
			return 0
		}())
	}
}

// TestRunProbeAndDeepFetch_LinkageDrift_ColdStart_ClearsStalePR verifies that
// when a never-deep-fetched item has a cached PR number but the probe reports
// LinkedPRNumber=0 (PR delinked), PRDetailsUpdated{PRNumber:0} is applied to
// clear the stale LinkedPR.Number and remove the stale prToKey reverse index entry.
func TestRunProbeAndDeepFetch_LinkageDrift_ColdStart_ClearsStalePR(t *testing.T) {
	bootstrapTime := time.Now().Add(-2 * time.Hour)
	probeTime := time.Now().Add(-time.Hour)

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// Same item, now probe reports no linked PR (LinkedPRNumber=0).
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
					Status: "Research", EffectiveUpdatedAt: probeTime, LinkedPRNumber: 0},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { return nil },
	}
	eng := testEngineWithCleanup(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	// Bootstrap with LinkedPRNumber=42 so the cache has a stale prToKey entry.
	cache.BootstrapFromProbe([]gh.BoardProbeItem{
		{ContentID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
			Status: "Research", EffectiveUpdatedAt: bootstrapTime, LinkedPRNumber: 42},
	}, "PVT_1")
	eng.readClient = cache

	// Verify prToKey entry for PR 42 exists after bootstrap.
	if _, found := eng.store.GetByPRKey("owner/repo", 42); !found {
		t.Fatal("prToKey should have entry for PR #42 after bootstrap")
	}

	eng.runProbeAndDeepFetch(cache)

	// After probe reports LinkedPRNumber=0, the stale entry must be cleared.
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("item not found: %v", err)
	}
	if s := snap.State(); s.LinkedPR != nil && s.LinkedPR.Number != 0 {
		t.Errorf("LinkedPR.Number = %d after delink, want 0", s.LinkedPR.Number)
	}
	if _, found := eng.store.GetByPRKey("owner/repo", 42); found {
		t.Error("prToKey should NOT have entry for PR #42 after probe reports delink")
	}
}

// ---- isProbeOnlyTerminal unit tests ----

// probeOnlyTerminalEngine returns a minimal *Engine for isProbeOnlyTerminal tests.
// It registers NO WorktreeManager so worktreeExistsForItem uses the fabrikDir
// fallback path, which is set to a fresh temp dir for deterministic path checks.
func probeOnlyTerminalEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	fabrikDir := t.TempDir()
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithCleanup(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		nil, // no WM → worktreeExistsForItem uses the fabrikDir fallback path
	)
	eng.fabrikDir = fabrikDir
	return eng, fabrikDir
}

// worktreeDirForItem returns the conventional worktree path used by worktreeExistsForItem's
// fallback (no WM registered): <fabrikDir>/.fabrik/worktrees/<owner>-<repo>/issue-<N>.
// repo must be "owner/repo" form.
func worktreeDirForItem(fabrikDir, repo string, number int) string {
	parts := strings.SplitN(repo, "/", 2)
	dirName := parts[0] + "-" + parts[1]
	return filepath.Join(fabrikDir, ".fabrik", "worktrees", dirName, fmt.Sprintf("issue-%d", number))
}

// TestIsProbeOnlyTerminal_ClosedCleanup_NoWorktree_True (SC-2): closed item in
// a cleanup stage with no on-disk worktree → predicate returns true.
func TestIsProbeOnlyTerminal_ClosedCleanup_NoWorktree_True(t *testing.T) {
	eng, _ := probeOnlyTerminalEngine(t)
	item := gh.ProjectItem{Number: 1, IsClosed: true, Status: "Done", Repo: "owner/repo"}
	if !eng.isProbeOnlyTerminal(item) {
		t.Error("expected true for closed item in cleanup stage with no worktree")
	}
}

// TestIsProbeOnlyTerminal_ClosedCleanup_WorktreePresent_False (SC-1): closed item
// in a cleanup stage WITH an on-disk worktree → predicate returns false so cleanup runs.
func TestIsProbeOnlyTerminal_ClosedCleanup_WorktreePresent_False(t *testing.T) {
	eng, fabrikDir := probeOnlyTerminalEngine(t)
	wtDir := worktreeDirForItem(fabrikDir, "owner/repo", 1)
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	item := gh.ProjectItem{Number: 1, IsClosed: true, Status: "Done", Repo: "owner/repo"}
	if eng.isProbeOnlyTerminal(item) {
		t.Error("expected false for closed item in cleanup stage when worktree exists on disk")
	}
}

// TestIsProbeOnlyTerminal_OpenCleanup_False (SC-3): open item in a cleanup stage
// → predicate returns false regardless of worktree presence.
func TestIsProbeOnlyTerminal_OpenCleanup_False(t *testing.T) {
	eng, fabrikDir := probeOnlyTerminalEngine(t)
	// Create a worktree to confirm worktree presence doesn't affect the result for open items.
	wtDir := worktreeDirForItem(fabrikDir, "owner/repo", 1)
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	item := gh.ProjectItem{Number: 1, IsClosed: false, Status: "Done", Repo: "owner/repo"}
	if eng.isProbeOnlyTerminal(item) {
		t.Error("expected false for open item in cleanup stage")
	}
}

// TestIsProbeOnlyTerminal_ClosedNonCleanup_False: closed item in a non-cleanup
// stage → predicate returns false.
func TestIsProbeOnlyTerminal_ClosedNonCleanup_False(t *testing.T) {
	eng, _ := probeOnlyTerminalEngine(t)
	item := gh.ProjectItem{Number: 1, IsClosed: true, Status: "Research", Repo: "owner/repo"}
	if eng.isProbeOnlyTerminal(item) {
		t.Error("expected false for closed item in non-cleanup stage")
	}
}

// TestIsProbeOnlyTerminal_OpenNonCleanup_False: open item in a non-cleanup stage
// → predicate returns false.
func TestIsProbeOnlyTerminal_OpenNonCleanup_False(t *testing.T) {
	eng, _ := probeOnlyTerminalEngine(t)
	item := gh.ProjectItem{Number: 1, IsClosed: false, Status: "Research", Repo: "owner/repo"}
	if eng.isProbeOnlyTerminal(item) {
		t.Error("expected false for open item in non-cleanup stage")
	}
}

// TestSeedTerminalFromProbeItems_SC5 covers SC-5: the bootstrap path must NOT
// seed terminal for a closed Done item when its worktree exists on disk. A
// same-call item without a worktree MUST be seeded terminal.
func TestSeedTerminalFromProbeItems_SC5(t *testing.T) {
	eng, fabrikDir := probeOnlyTerminalEngine(t)

	// Issue #1: closed + Done + worktree EXISTS → must NOT be terminal after seed.
	wtDir := worktreeDirForItem(fabrikDir, "owner/repo", 1)
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Issue #2: closed + Done + NO worktree → MUST be terminal after seed.
	// (worktree directory deliberately not created)

	probeItems := []gh.BoardProbeItem{
		{ContentID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Done", IsClosed: true},
		{ContentID: "I_2", ItemID: "PVTI_2", Number: 2, Repo: "owner/repo", Status: "Done", IsClosed: true},
	}

	// Populate the store so Apply calls have a valid target.
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 1, Repo: "owner/repo", Status: "Done", IsClosed: true}})
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 2, Repo: "owner/repo", Status: "Done", IsClosed: true}})

	eng.seedTerminalFromProbeItems(probeItems)

	snap1, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get issue #1: %v", err)
	}
	if snap1.IsTerminal() {
		t.Error("SC-5: issue #1 (worktree present) must NOT be seeded terminal — cleanup would be skipped")
	}

	snap2, err := eng.store.Get("owner/repo", 2)
	if err != nil {
		t.Fatalf("store.Get issue #2: %v", err)
	}
	if !snap2.IsTerminal() {
		t.Error("SC-5: issue #2 (no worktree) MUST be seeded terminal to skip unnecessary deep-fetch")
	}
}

// ---- stage-membership guard tests ----

// TestRunProbeAndDeepFetch_UnconfiguredColumn_ColdCache_SkipsDeepFetch verifies
// that a probe item in an unconfigured board column (e.g. "Backlog") does not
// trigger FetchItemDetails when the item is not yet in the store (cold cache /
// new-item path). The item must still appear in newKeys so it is not tombstoned.
func TestRunProbeAndDeepFetch_UnconfiguredColumn_ColdCache_SkipsDeepFetch(t *testing.T) {
	var deepFetchCalls int

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// Configured stage item (should be deep-fetched normally).
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
					Status: "Research", EffectiveUpdatedAt: time.Now()},
				// Unconfigured column item (must NOT be deep-fetched).
				{ItemID: "PVTI_002", ContentID: "I_002", Number: 2, Repo: "owner/repo",
					Status: "Backlog", EffectiveUpdatedAt: time.Now()},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			item.Labels = []string{"stage:Research:in_progress"}
			return nil
		},
	}

	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	eng.runProbeAndDeepFetch(cache)

	// Only the configured-stage item should have been deep-fetched.
	if deepFetchCalls != 1 {
		t.Errorf("expected exactly 1 FetchItemDetails call (for Research item); got %d", deepFetchCalls)
	}

	// The Backlog item must NOT be in the store (guard fired before IssueOpened).
	if _, err := eng.store.Get("owner/repo", 2); err == nil {
		t.Error("Backlog item should not be seeded into the store (guard must fire before IssueOpened)")
	}
}

// TestRunProbeAndDeepFetch_UnconfiguredColumn_WarmCache_SkipsDeepFetch verifies
// that a probe item in an unconfigured board column does not trigger FetchItemDetails
// when the item is already in the store (warm cache / existing-item path). The item
// must remain in the store after the cycle (guard must not cause tombstoning).
func TestRunProbeAndDeepFetch_UnconfiguredColumn_WarmCache_SkipsDeepFetch(t *testing.T) {
	var deepFetchCalls int

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// Unconfigured column item already in the store — must not be deep-fetched.
				{ItemID: "PVTI_002", ContentID: "I_002", Number: 2, Repo: "owner/repo",
					Status: "Backlog", EffectiveUpdatedAt: time.Now()},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}

	eng := testEngine(t, client, &mockClaudeInvoker{})
	// Seed the Backlog item into the store directly (simulating a prior bootstrap).
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		ID: "I_002", ItemID: "PVTI_002", Number: 2, Repo: "owner/repo", Status: "Backlog",
	}})

	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	eng.runProbeAndDeepFetch(cache)

	if deepFetchCalls != 0 {
		t.Errorf("Backlog item (warm cache): expected 0 FetchItemDetails calls; got %d", deepFetchCalls)
	}

	// Item must still be in the store — the guard must not cause tombstoning.
	if _, err := eng.store.Get("owner/repo", 2); err != nil {
		t.Error("Backlog item should remain in the store after the probe cycle (newKeys guard prevents tombstoning)")
	}
}

// TestRunProbeAndDeepFetch_UnconfiguredColumn_StatusDrift_UpdatesStore verifies
// that when a probe item moves from a configured stage to an unconfigured column
// (e.g. Research → Backlog), ProbeBoardItemUpdated is still applied so the store
// reflects the new status — while deep-fetch is still skipped.
func TestRunProbeAndDeepFetch_UnconfiguredColumn_StatusDrift_UpdatesStore(t *testing.T) {
	var deepFetchCalls int

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// Item that was in Research is now in Backlog (unconfigured column).
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
					Status: "Backlog", EffectiveUpdatedAt: time.Now()},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}

	eng := testEngine(t, client, &mockClaudeInvoker{})
	// Seed the item into the store as if it was previously in a configured stage.
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research",
	}})

	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	eng.runProbeAndDeepFetch(cache)

	if deepFetchCalls != 0 {
		t.Errorf("item in unconfigured column: expected 0 FetchItemDetails calls; got %d", deepFetchCalls)
	}

	// Status must be updated to "Backlog" so TUI and itemMayNeedWork see the correct column.
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("item should remain in store: %v", err)
	}
	if got := snap.Status(); got != "Backlog" {
		t.Errorf("store Status = %q, want %q (ProbeBoardItemUpdated must apply even for unconfigured columns)", got, "Backlog")
	}
}

// ---------------------------------------------------------------------------
// runProbeAndDeepFetch integration tests
// ---------------------------------------------------------------------------

// TestRunProbeAndDeepFetch_StaleItem_TriggersDeepFetch verifies that an item
// with no prior deep-fetch (LastSeenSourceUpdatedAt == zero) triggers
// FetchItemDetails when the probe returns a nonzero EffectiveUpdatedAt.
func TestRunProbeAndDeepFetch_StaleItem_TriggersDeepFetch(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	var deepFetchCalls int
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research", EffectiveUpdatedAt: now},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.runProbeAndDeepFetch(cache)
	if deepFetchCalls == 0 {
		t.Error("expected FetchItemDetails called for stale item (zero LastSeenSourceUpdatedAt); got 0 calls")
	}
}

// TestRunProbeAndDeepFetch_FreshItem_SkipsDeepFetch verifies that an item
// whose LastSeenSourceUpdatedAt matches the probe's EffectiveUpdatedAt does
// not trigger a FetchItemDetails call.
func TestRunProbeAndDeepFetch_FreshItem_SkipsDeepFetch(t *testing.T) {
	T1 := time.Now().Add(-time.Hour).Truncate(time.Second)
	var deepFetchCalls int
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research", EffectiveUpdatedAt: T1},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	// Simulate a prior deep-fetch that set LastSeenSourceUpdatedAt = T1.
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 1,
		FreshState: gh.ProjectItem{
			ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", UpdatedAt: T1,
		},
	})
	eng.runProbeAndDeepFetch(cache)
	if deepFetchCalls != 0 {
		t.Errorf("expected 0 FetchItemDetails calls for fresh item; got %d", deepFetchCalls)
	}
}

// TestRunProbeAndDeepFetch_LinkageDrift_InvalidatesAndDeepFetches verifies
// that when the probe detects a linked PR number different from the cached
// value, the cache is invalidated (DeepFetchInvalidated) and FetchItemDetails
// is triggered even though EffectiveUpdatedAt has not advanced.
func TestRunProbeAndDeepFetch_LinkageDrift_InvalidatesAndDeepFetches(t *testing.T) {
	T1 := time.Now().Add(-time.Hour).Truncate(time.Second)
	var deepFetchCalls int
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// Same EffectiveUpdatedAt as last deep-fetch (would be fresh) but LinkedPRNumber changed.
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research", EffectiveUpdatedAt: T1, LinkedPRNumber: 99},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	// Simulate fresh state at T1 with no linked PR (cached LinkedPRNumber = 0).
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 1,
		FreshState: gh.ProjectItem{
			ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", UpdatedAt: T1,
		},
	})
	eng.runProbeAndDeepFetch(cache)
	if deepFetchCalls == 0 {
		t.Error("expected FetchItemDetails called after linkage drift (PR# 0 → 99); got 0 calls")
	}
}

// TestRunProbeAndDeepFetch_BaseLabeledItem_ZeroProbeSuppressesDrift verifies
// that for a base:<branch>-labeled item, a probe reporting LinkedPRNumber == 0
// against a warm cache holding a nonzero linked PR is NOT treated as linkage
// drift: FetchItemDetails must not be called and the cached LinkedPR.Number
// must survive unchanged. This is the structurally-expected case where
// closedByPullRequestsReferences is empty because the PR targets a non-default
// base branch.
func TestRunProbeAndDeepFetch_BaseLabeledItem_ZeroProbeSuppressesDrift(t *testing.T) {
	T1 := time.Now().Add(-time.Hour).Truncate(time.Second)
	var deepFetchCalls int
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				// Structurally-expected probe value of 0 for a base:<branch> item.
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research", EffectiveUpdatedAt: T1, LinkedPRNumber: 0},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	// Warm cache: prior deep-fetch resolved the real PR #42 via the
	// base-independent linkage path, and the item carries a base: label.
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 1,
		FreshState: gh.ProjectItem{
			ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", UpdatedAt: T1,
			LinkedPRNumber: 42,
			Labels:         []string{"base:release"},
		},
	})
	eng.runProbeAndDeepFetch(cache)
	if deepFetchCalls != 0 {
		t.Errorf("expected 0 FetchItemDetails calls (suppressed base-label drift); got %d", deepFetchCalls)
	}
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get failed: %v", err)
	}
	st := snap.State()
	if st.LinkedPR == nil || st.LinkedPR.Number != 42 {
		t.Errorf("expected cached LinkedPR.Number to remain 42, got %+v", st.LinkedPR)
	}
}

// TestRunProbeAndDeepFetch_BaseLabeledItem_GenuineDriftStillInvalidates
// verifies that for a base:<branch>-labeled item, a probe reporting a
// *different nonzero* linked PR than the cache still triggers real linkage
// drift (invalidate + re-deep-fetch) — only the 0-vs-nonzero case is
// suppressed.
func TestRunProbeAndDeepFetch_BaseLabeledItem_GenuineDriftStillInvalidates(t *testing.T) {
	T1 := time.Now().Add(-time.Hour).Truncate(time.Second)
	var deepFetchCalls int
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research", EffectiveUpdatedAt: T1, LinkedPRNumber: 99},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 1,
		FreshState: gh.ProjectItem{
			ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", UpdatedAt: T1,
			LinkedPRNumber: 42,
			Labels:         []string{"base:release"},
		},
	})
	eng.runProbeAndDeepFetch(cache)
	if deepFetchCalls == 0 {
		t.Error("expected FetchItemDetails called after genuine drift on base-labeled item (PR #42 → #99); got 0 calls")
	}
}

// TestRunProbeAndDeepFetch_ItemGone_RemovedFromStore verifies that an item
// present in the store but absent from probe results is removed from the store.
func TestRunProbeAndDeepFetch_ItemGone_RemovedFromStore(t *testing.T) {
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			// Only item #1; item #2 has left the board.
			return []gh.BoardProbeItem{
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research"},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo"},
			{ID: "I_002", ItemID: "PVTI_002", Number: 2, Repo: "owner/repo"},
		},
	})
	eng.readClient = cache

	eng.runProbeAndDeepFetch(cache)

	if _, err := eng.store.Get("owner/repo", 2); err == nil {
		t.Error("item #2 should be removed from store after probe omits it")
	}
	if _, err := eng.store.Get("owner/repo", 1); err != nil {
		t.Errorf("item #1 should still be in store after probe includes it: %v", err)
	}
}

// TestColdStart_ProbeBootstrap_TerminalItemsSkipDeepFetch verifies the cold-start cost
// reduction: 10 closed Done items are seeded terminal by BootstrapFromProbe and
// are never deep-fetched, while 3 open active items are deep-fetched normally.
// Expected deep-fetch count after the first probe cycle: ≤ 3.
func TestColdStart_ProbeBootstrap_TerminalItemsSkipDeepFetch(t *testing.T) {
	var deepFetchCalls int
	probeTime := time.Now().Add(-time.Minute)

	// Build 10 closed Done items + 3 open Research items for the probe response.
	var probeItems []gh.BoardProbeItem
	for i := 1; i <= 10; i++ {
		probeItems = append(probeItems, gh.BoardProbeItem{
			ContentID: fmt.Sprintf("I_%03d", i), ItemID: fmt.Sprintf("PVTI_%03d", i),
			Number: i, Repo: "owner/repo",
			Status:             "Done",
			IsClosed:           true,
			EffectiveUpdatedAt: probeTime,
		})
	}
	for i := 11; i <= 13; i++ {
		probeItems = append(probeItems, gh.BoardProbeItem{
			ContentID: fmt.Sprintf("I_%03d", i), ItemID: fmt.Sprintf("PVTI_%03d", i),
			Number: i, Repo: "owner/repo",
			Status:             "Research",
			IsClosed:           false,
			EffectiveUpdatedAt: probeTime,
		})
	}

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return probeItems, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token", MaxConcurrent: 5,
			Stages: testStagesWithCleanup(),
		},
		client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	// Simulate the virgin-cache branch: probe bootstrap seeds the store.
	items, projectID, err := client.ProbeProjectBoard("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	cache.BootstrapFromProbe(items, projectID)
	eng.seedTerminalFromProbeItems(items)

	// Now simulate the next poll cycle — probe-driven deep-fetch pass.
	eng.runProbeAndDeepFetch(cache)

	// The 10 closed Done items are seeded terminal and must NOT be deep-fetched.
	// The 3 open Research items have no prior deep-fetch and MUST be deep-fetched.
	if deepFetchCalls > 3 {
		t.Errorf("cold-start deep-fetch count = %d, want ≤ 3 (only active items)", deepFetchCalls)
	}
	if deepFetchCalls == 0 {
		t.Error("expected ≥ 1 deep-fetch for active items; got 0")
	}

	// Terminal flag must be set on all closed Done items.
	for i := 1; i <= 10; i++ {
		snap, snapErr := eng.store.Get("owner/repo", i)
		if snapErr != nil {
			t.Errorf("item #%d not found in store", i)
			continue
		}
		if !snap.IsTerminal() {
			t.Errorf("item #%d (closed Done): expected IsTerminal()=true", i)
		}
	}

	// Active Research items must NOT be terminal.
	for i := 11; i <= 13; i++ {
		snap, snapErr := eng.store.Get("owner/repo", i)
		if snapErr != nil {
			t.Errorf("item #%d not found in store", i)
			continue
		}
		if snap.IsTerminal() {
			t.Errorf("item #%d (open Research): expected IsTerminal()=false", i)
		}
	}
}

// TestWebhookModeStartup_ClosedDoneItemsNotDeepFetched is the regression test
// for issue #751. It verifies that after the fixed webhook-mode startup path
// (BootstrapFromProbe instead of Bootstrap), the first probe cycle does NOT
// call FetchItemDetails for closed Done items. Only active items are fetched.
func TestWebhookModeStartup_ClosedDoneItemsNotDeepFetched(t *testing.T) {
	var deepFetchCalls int
	probeTime := time.Now().Add(-time.Minute)

	// 3 closed Done items + 1 open Research item returned by the probe.
	allProbeItems := []gh.BoardProbeItem{
		{ContentID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
			Status: "Done", IsClosed: true, EffectiveUpdatedAt: probeTime},
		{ContentID: "I_002", ItemID: "PVTI_002", Number: 2, Repo: "owner/repo",
			Status: "Done", IsClosed: true, EffectiveUpdatedAt: probeTime},
		{ContentID: "I_003", ItemID: "PVTI_003", Number: 3, Repo: "owner/repo",
			Status: "Done", IsClosed: true, EffectiveUpdatedAt: probeTime},
		{ContentID: "I_004", ItemID: "PVTI_004", Number: 4, Repo: "owner/repo",
			Status: "Research", IsClosed: false, EffectiveUpdatedAt: probeTime},
	}

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return allProbeItems, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token", MaxConcurrent: 5,
			Stages: testStagesWithCleanup(),
		},
		client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})

	// Simulate the fixed webhook startup path: BootstrapFromProbe seeds Terminal
	// for closed Done items before the first poll cycle runs.
	cache.BootstrapFromProbe(allProbeItems, "PVT_1")
	eng.seedTerminalFromProbeItems(allProbeItems)
	eng.readClient = cache

	// Run one probe cycle — this is what the first poll does after startup.
	eng.runProbeAndDeepFetch(cache)

	// The 3 closed Done items are seeded terminal and must NOT be deep-fetched.
	// Only the 1 open Research item should trigger FetchItemDetails.
	if deepFetchCalls != 1 {
		t.Errorf("webhook startup deep-fetch count = %d, want 1 (only active Research item)", deepFetchCalls)
	}

	// Verify terminal flag is set for closed Done items.
	for i := 1; i <= 3; i++ {
		snap, err := eng.store.Get("owner/repo", i)
		if err != nil {
			t.Errorf("item #%d not in store: %v", i, err)
			continue
		}
		if !snap.IsTerminal() {
			t.Errorf("item #%d (closed Done): expected IsTerminal()=true after webhook startup", i)
		}
	}
}

// TestRunProbeAndDeepFetch_IsClosedPropagates_WithoutDeepFetch verifies that
// IsClosed=true is written to the store via ProbeBoardItemUpdated even when
// the item is cache-fresh (EffectiveUpdatedAt unchanged → no deep-fetch).
func TestRunProbeAndDeepFetch_IsClosedPropagates_WithoutDeepFetch(t *testing.T) {
	T1 := time.Now().Add(-time.Hour).Truncate(time.Second)
	var deepFetchCalls int
	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{
				{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo", Status: "Research", IsClosed: true, EffectiveUpdatedAt: T1},
			}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	// Fresh at T1 — deep-fetch should not be triggered.
	eng.store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 1,
		FreshState: gh.ProjectItem{
			ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", UpdatedAt: T1,
		},
	})
	eng.runProbeAndDeepFetch(cache)
	if deepFetchCalls != 0 {
		t.Errorf("expected 0 deep-fetch calls for fresh item; got %d", deepFetchCalls)
	}
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get after probe: %v", err)
	}
	if !snap.IsClosed() {
		t.Error("expected IsClosed=true after ProbeBoardItemUpdated; got false")
	}
}

// TestProbeNewItem_ClosedDone_SkipsDeepFetch is a regression test for the
// new-item branch of runProbeAndDeepFetch. It verifies that closed Done items
// discovered by the probe (not yet in store, no prior bootstrap) are seeded as
// terminal and never deep-fetched, while open active items are deep-fetched
// normally. This covers the gap where BootstrapFromProbe cannot help: items
// that appear in the probe for the first time during a mid-run cycle.
func TestProbeNewItem_ClosedDone_SkipsDeepFetch(t *testing.T) {
	const numClosed = 3 // closed Done items
	const numOpen = 2   // open Research items
	var deepFetchCalls int
	probeTime := time.Now().Add(-time.Minute)

	var probeItems []gh.BoardProbeItem
	for i := 1; i <= numClosed; i++ {
		probeItems = append(probeItems, gh.BoardProbeItem{
			ContentID: fmt.Sprintf("I_%03d", i), ItemID: fmt.Sprintf("PVTI_%03d", i),
			Number:             i,
			Repo:               "owner/repo",
			Status:             "Done",
			IsClosed:           true,
			EffectiveUpdatedAt: probeTime,
		})
	}
	for i := numClosed + 1; i <= numClosed+numOpen; i++ {
		probeItems = append(probeItems, gh.BoardProbeItem{
			ContentID: fmt.Sprintf("I_%03d", i), ItemID: fmt.Sprintf("PVTI_%03d", i),
			Number:             i,
			Repo:               "owner/repo",
			Status:             "Research",
			IsClosed:           false,
			EffectiveUpdatedAt: probeTime,
		})
	}

	client := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return probeItems, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			deepFetchCalls++
			return nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token", MaxConcurrent: 5,
			Stages: testStagesWithCleanup(),
		},
		client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	// No prior bootstrap — store is empty. All items are new-item discoveries.
	eng.runProbeAndDeepFetch(cache)

	// Only the open Research items should have been deep-fetched.
	if deepFetchCalls != numOpen {
		t.Errorf("deep-fetch count = %d, want %d (open items only)", deepFetchCalls, numOpen)
	}

	// Closed Done items must be terminal in the store.
	for i := 1; i <= numClosed; i++ {
		snap, snapErr := eng.store.Get("owner/repo", i)
		if snapErr != nil {
			t.Errorf("closed Done item #%d not found in store", i)
			continue
		}
		if !snap.IsTerminal() {
			t.Errorf("item #%d (closed Done): expected IsTerminal()=true", i)
		}
	}

	// Open Research items must NOT be terminal.
	for i := numClosed + 1; i <= numClosed+numOpen; i++ {
		snap, snapErr := eng.store.Get("owner/repo", i)
		if snapErr != nil {
			t.Errorf("open Research item #%d not found in store", i)
			continue
		}
		if snap.IsTerminal() {
			t.Errorf("item #%d (open Research): expected IsTerminal()=false", i)
		}
	}
}
