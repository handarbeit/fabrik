package engine

import (
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

func testEngineWithCleanup(client *mockGitHubClient, claude *mockClaudeInvoker) *Engine {
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
		NewWorktreeManager("/tmp/test-repo"),
	)
}

func TestRunStartupTerminalScan_MarksTerminalItems(t *testing.T) {
	eng := testEngineWithCleanup(&mockGitHubClient{}, &mockClaudeInvoker{})

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
	eng := testEngineWithCleanup(&mockGitHubClient{}, &mockClaudeInvoker{})

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
func testEngineWithCleanupCache(client *mockGitHubClient, claude *mockClaudeInvoker) (*Engine, *boardcache.CacheImpl) {
	eng := testEngineWithCleanup(client, claude)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	cache.Bootstrap(&gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{
				ID:       "I_001",
				ItemID:   "PVTI_001",
				Number:   1,
				Repo:     "owner/repo",
				Status:   "Done",
				IsClosed: true,
				Labels:   []string{"stage:Done:complete"},
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
	eng, cache := testEngineWithCleanupCache(client, &mockClaudeInvoker{})

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
	eng, cache := testEngineWithCleanupCache(client, &mockClaudeInvoker{})

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
		client, &mockClaudeInvoker{}, NewWorktreeManager("/tmp/test-repo"),
	)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	cache.Bootstrap(&gh.ProjectBoard{
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
		NewWorktreeManager("/tmp/test-repo"),
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
	eng := testEngineWithCleanup(client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	// Bootstrap with LinkedPRNumber=0 (old-style bootstrap that did not populate PR number).
	cache.Bootstrap(&gh.ProjectBoard{
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
	eng := testEngineWithCleanup(client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	// Bootstrap with LinkedPRNumber=0.
	cache.Bootstrap(&gh.ProjectBoard{
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
	eng := testEngineWithCleanup(client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	// Bootstrap with LinkedPRNumber=42 so the cache has a stale prToKey entry.
	cache.BootstrapFromProbe([]gh.BoardProbeItem{
		{ContentID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
			Status: "Research", EffectiveUpdatedAt: bootstrapTime, LinkedPRNumber: 42},
	}, "PVT_1", testStagesWithCleanup())
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

func TestIsProbeOnlyTerminal_ClosedCleanup_True(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	if !isProbeOnlyTerminal(true, "Done", stagesCfg) {
		t.Error("expected true for closed item in cleanup stage")
	}
}

func TestIsProbeOnlyTerminal_OpenCleanup_False(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	if isProbeOnlyTerminal(false, "Done", stagesCfg) {
		t.Error("expected false for open item in cleanup stage")
	}
}

func TestIsProbeOnlyTerminal_ClosedNonCleanup_False(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	if isProbeOnlyTerminal(true, "Research", stagesCfg) {
		t.Error("expected false for closed item in non-cleanup stage")
	}
}

func TestIsProbeOnlyTerminal_OpenNonCleanup_False(t *testing.T) {
	stagesCfg := testStagesWithCleanup()
	if isProbeOnlyTerminal(false, "Research", stagesCfg) {
		t.Error("expected false for open item in non-cleanup stage")
	}
}
