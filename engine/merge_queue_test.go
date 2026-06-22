package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestPRInMergeQueue covers the FR-1 per-PR guard signal: it fires on
// LinkedPRIsInMergeQueue alone and is false-by-default (FR-3).
func TestPRInMergeQueue(t *testing.T) {
	tests := []struct {
		name string
		item gh.ProjectItem
		want bool
	}{
		{"default (no flags)", gh.ProjectItem{}, false},
		{"queue enabled but not in queue", gh.ProjectItem{LinkedPRIsMergeQueueEnabled: true}, false},
		{"in queue", gh.ProjectItem{LinkedPRIsInMergeQueue: true}, true},
		{"in queue and enabled", gh.ProjectItem{LinkedPRIsInMergeQueue: true, LinkedPRIsMergeQueueEnabled: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prInMergeQueue(tt.item); got != tt.want {
				t.Errorf("prInMergeQueue() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSuppressPreemptiveRebase covers the FR-2 per-repo guard signal across the
// full decision matrix: queue-disabled / enabled+off / enabled+auto, asserting
// false-by-default (FR-3) and that the "off" kill-switch restores legacy behavior.
func TestSuppressPreemptiveRebase(t *testing.T) {
	tests := []struct {
		name       string
		mergeQueue string
		item       gh.ProjectItem
		want       bool
	}{
		{"queue disabled, cfg auto", "auto", gh.ProjectItem{}, false},
		{"queue disabled, cfg off", "off", gh.ProjectItem{}, false},
		{"queue disabled, cfg empty", "", gh.ProjectItem{}, false},
		{"queue enabled, cfg auto", "auto", gh.ProjectItem{LinkedPRIsMergeQueueEnabled: true}, true},
		{"queue enabled, cfg empty (default != off)", "", gh.ProjectItem{LinkedPRIsMergeQueueEnabled: true}, true},
		{"queue enabled, cfg off (kill-switch)", "off", gh.ProjectItem{LinkedPRIsMergeQueueEnabled: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{cfg: Config{MergeQueue: tt.mergeQueue}}
			if got := e.suppressPreemptiveRebase(tt.item); got != tt.want {
				t.Errorf("suppressPreemptiveRebase() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPushBranchUnlessQueued_SkipsWhenQueued verifies, against a real git repo
// with a bare origin, that pushBranchUnlessQueued does NOT advance the origin
// branch when the PR is in the merge queue, and DOES when it is not.
func TestPushBranchUnlessQueued_SkipsWhenQueued(t *testing.T) {
	skipIfNoGit(t)

	sourceDir := initRepoWithRemote(t)
	wm := NewWorktreeManager(sourceDir)
	e := NewWithDeps(Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()},
		&mockGitHubClient{}, &mockClaudeInvoker{}, wm)

	// Helper: query the origin for the issue branch ref (empty when absent).
	remoteRef := func(wtDir, branch string) string {
		cmd := exec.Command("git", "ls-remote", "origin", branch)
		cmd.Dir = wtDir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git ls-remote: %v", err)
		}
		return strings.TrimSpace(string(out))
	}

	// Helper: create a fresh commit in the worktree so there is something to push.
	commit := func(wtDir, msg string) {
		cmd := exec.Command("git", "commit", "--allow-empty", "-m", msg)
		cmd.Dir = wtDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit: %s: %v", out, err)
		}
	}

	branch := "fabrik/issue-7"

	// ── Queued PR: push must be skipped, origin branch must not appear. ──
	wtDir, err := wm.EnsureWorktree(7, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	commit(wtDir, "queued change")

	queued := gh.ProjectItem{Number: 7, LinkedPRIsInMergeQueue: true}
	if err := e.pushBranchUnlessQueued(queued, wm); err != nil {
		t.Fatalf("pushBranchUnlessQueued (queued): %v", err)
	}
	if ref := remoteRef(wtDir, branch); ref != "" {
		t.Errorf("origin should not have %s after skipped push, got %q", branch, ref)
	}

	// ── Not-queued PR: push must proceed, origin branch must appear. ──
	notQueued := gh.ProjectItem{Number: 7, LinkedPRIsInMergeQueue: false}
	if err := e.pushBranchUnlessQueued(notQueued, wm); err != nil {
		t.Fatalf("pushBranchUnlessQueued (not queued): %v", err)
	}
	if ref := remoteRef(wtDir, branch); ref == "" {
		t.Errorf("origin should have %s after push, got empty", branch)
	}
}

// ── FR-1: syncPRBase (UpdatePRBase) in-queue guard (Task 5) ──────────────────

// TestSyncPRBase_InMergeQueue_SkipsUpdate verifies the FR-1 guard at syncPRBase:
// when the linked PR is in the merge queue, the base sync is skipped entirely
// (no FindPRForIssue/UpdatePRBase calls), because a base change ejects the PR.
// The not-in-queue case (FR-3 boundary) still updates when the base differs.
func TestSyncPRBase_InMergeQueue_SkipsUpdate(t *testing.T) {
	newClient := func() *mockGitHubClient {
		return &mockGitHubClient{
			findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) { return 42, nil },
			getPRBaseFn:      func(owner, repo string, prNumber int) (string, error) { return "main", nil },
		}
	}

	// In queue: must not call UpdatePRBase.
	queuedClient := newClient()
	engQueued := testEngine(t, queuedClient, &mockClaudeInvoker{})
	engQueued.syncPRBase(gh.ProjectItem{Number: 1, LinkedPRIsInMergeQueue: true}, "feature/foo")
	if len(queuedClient.updatePRBaseCalls) != 0 {
		t.Errorf("queued PR: expected 0 UpdatePRBase calls, got %d", len(queuedClient.updatePRBaseCalls))
	}

	// Not in queue, base differs: must call UpdatePRBase (FR-3 unchanged).
	openClient := newClient()
	engOpen := testEngine(t, openClient, &mockClaudeInvoker{})
	engOpen.syncPRBase(gh.ProjectItem{Number: 1, LinkedPRIsInMergeQueue: false}, "feature/foo")
	if len(openClient.updatePRBaseCalls) != 1 {
		t.Errorf("non-queued PR: expected 1 UpdatePRBase call, got %d", len(openClient.updatePRBaseCalls))
	}
}

// ── FR-1/FR-2 boundary: dispatchRebaseReinvoke dispatch guards (Tasks 6, 7) ───

// TestCheckAutoMergeConvergence_InMergeQueue_SkipsRebaseDispatch drives the
// merge_gate.go dispatch site (checkAutoMergeConvergence). A PRMergeConflicting
// PR that is in the queue must NOT dispatch a rebase reinvoke (FR-1: it would
// eject the PR); the same PR not in the queue still dispatches (FR-2 boundary —
// genuine conflict resolution is preserved when the PR is not queued).
func TestCheckAutoMergeConvergence_InMergeQueue_SkipsRebaseDispatch(t *testing.T) {
	run := func(inQueue bool) int {
		client := &mockGitHubClient{
			fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "dirty"}, nil
			},
		}
		eng := testEngineForMerge(t, client)
		eng.cfg.MaxRebaseCycles = 3
		item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
		stage := &stages.Stage{Name: "Validate"}
		settle := PRSettleResult{Status: PRMergeConflicting, PR: &gh.PRDetails{Number: 10, IsInMergeQueue: inQueue}}

		eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle)
		eng.wg.Wait()
		snap, err := eng.store.Get("owner/repo", 42)
		if err != nil {
			// No store entry means no dispatch occurred (the increment is what
			// creates it) — treat as zero cycles.
			return 0
		}
		return snap.RebaseCycles("Validate")
	}

	if got := run(true); got != 0 {
		t.Errorf("in-queue conflict: expected RebaseCycles=0 (dispatch skipped), got %d", got)
	}
	if got := run(false); got != 1 {
		t.Errorf("not-in-queue conflict: expected RebaseCycles=1 (dispatch fired), got %d", got)
	}
}

// TestHandleMergeGate_InMergeQueue_SkipsRebaseDispatch drives the
// catch_up_handlers.go dispatch site (handleMergeAndCIGates). settle.PR is built
// from the mock FetchLinkedPR; when it reports IsInMergeQueue the conflict
// dispatch is skipped and advancedItems is not set (FR-1). The not-in-queue case
// still dispatches (FR-2 boundary).
func TestHandleMergeGate_InMergeQueue_SkipsRebaseDispatch(t *testing.T) {
	run := func(inQueue bool) (int, bool) {
		client := &mockGitHubClient{
			fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 42, HeadSHA: "deadbeef", State: "open", Merged: false, IsInMergeQueue: inQueue}, nil
			},
			fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
				f := false
				return &f, "dirty", nil
			},
			addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		}
		stgs := []*stages.Stage{
			{Name: "Implement", Order: 1, Prompt: "implement"},
			{Name: "Review", Order: 2, Prompt: "review"},
		}
		eng := testEngineWithStages(t, client, stgs)
		eng.cfg.MaxRebaseCycles = 3
		board := &gh.ProjectBoard{ProjectID: "PVT_1"}
		advancedItems := make(map[string]bool)
		pctx := makeMergeGatePctx(board, advancedItems)

		eng.handleMergeAndCIGates(pctx)
		eng.wg.Wait()
		snap, _ := eng.store.Get("owner/repo", 20)
		return snap.RebaseCycles("Implement"), advancedItems["owner/repo#20"]
	}

	if cycles, advanced := run(true); cycles != 0 || advanced {
		t.Errorf("in-queue conflict: expected RebaseCycles=0 & not advanced, got cycles=%d advanced=%v", cycles, advanced)
	}
	if cycles, advanced := run(false); cycles != 1 || !advanced {
		t.Errorf("not-in-queue conflict: expected RebaseCycles=1 & advanced, got cycles=%d advanced=%v", cycles, advanced)
	}
}

// TestRebaseDispatchGuards_PollNativeSignal_CacheMiss verifies the dispatch guards
// fire from the poll-native ProjectItem field (LinkedPRIsInMergeQueue) even when
// settle.PR does NOT carry the flag. This simulates a boardcache miss: readClient
// falls back to REST, whose FetchLinkedPR never decodes isInMergeQueue (GraphQL-only),
// so settle.PR.IsInMergeQueue is false. The ProjectItem field — always GraphQL-
// populated each poll cycle — must still suppress the dispatch (FR-1 completeness:
// a guard that fails on cache miss is partial coverage).
func TestRebaseDispatchGuards_PollNativeSignal_CacheMiss(t *testing.T) {
	// catch_up_handlers.go: handleMergeAndCIGates. settle.PR reports not-in-queue
	// (cache-miss REST fallback), but the item carries the poll-native flag.
	t.Run("handleMergeAndCIGates", func(t *testing.T) {
		client := &mockGitHubClient{
			fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 42, HeadSHA: "deadbeef", State: "open", Merged: false, IsInMergeQueue: false}, nil
			},
			fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
				f := false
				return &f, "dirty", nil
			},
			addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		}
		stgs := []*stages.Stage{
			{Name: "Implement", Order: 1, Prompt: "implement"},
			{Name: "Review", Order: 2, Prompt: "review"},
		}
		eng := testEngineWithStages(t, client, stgs)
		eng.cfg.MaxRebaseCycles = 3
		board := &gh.ProjectBoard{ProjectID: "PVT_1"}
		advancedItems := make(map[string]bool)
		pctx := makeMergeGatePctx(board, advancedItems)
		pctx.item.LinkedPRIsInMergeQueue = true // poll-native signal set; settle.PR is not

		eng.handleMergeAndCIGates(pctx)
		eng.wg.Wait()
		snap, _ := eng.store.Get("owner/repo", 20)
		if snap.RebaseCycles("Implement") != 0 || advancedItems["owner/repo#20"] {
			t.Errorf("cache-miss in-queue: expected no dispatch from poll-native signal, got cycles=%d advanced=%v",
				snap.RebaseCycles("Implement"), advancedItems["owner/repo#20"])
		}
	})

	// merge_gate.go: checkAutoMergeConvergence. Same cache-miss shape.
	t.Run("checkAutoMergeConvergence", func(t *testing.T) {
		client := &mockGitHubClient{
			fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "dirty"}, nil
			},
		}
		eng := testEngineForMerge(t, client)
		eng.cfg.MaxRebaseCycles = 3
		item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}, LinkedPRIsInMergeQueue: true}
		stage := &stages.Stage{Name: "Validate"}
		settle := PRSettleResult{Status: PRMergeConflicting, PR: &gh.PRDetails{Number: 10, IsInMergeQueue: false}}

		eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle)
		eng.wg.Wait()
		if snap, err := eng.store.Get("owner/repo", 42); err == nil && snap.RebaseCycles("Validate") != 0 {
			t.Errorf("cache-miss in-queue: expected no dispatch from poll-native signal, got cycles=%d", snap.RebaseCycles("Validate"))
		}
	})
}
