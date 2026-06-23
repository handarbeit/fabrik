package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

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
// merge_gate.go dispatch site (checkAutoMergeConvergence). FR-1 consolidates the
// in-queue suppression into the settle status: an in-queue PR settles as
// PRMergeQueued, which hands off at step ② (no rebase dispatch — a rebase+force-push
// would eject it). The same PR once ejected settles as PRMergeConflicting and
// dispatches the rebase reinvoke (FR-2 ejection→resolve, genuine conflict).
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
		// In-queue → PRMergeQueued (hand-off); ejected/not-queued conflict → PRMergeConflicting (dispatch).
		status := PRMergeConflicting
		if inQueue {
			status = PRMergeQueued
		}
		settle := PRSettleResult{Status: status, PR: &gh.PRDetails{Number: 10, HeadSHA: "abc12345", IsInMergeQueue: inQueue}}

		eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle, false)
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
		t.Errorf("in-queue (PRMergeQueued): expected RebaseCycles=0 (hand-off, dispatch skipped), got %d", got)
	}
	if got := run(false); got != 1 {
		t.Errorf("ejected conflict (PRMergeConflicting): expected RebaseCycles=1 (dispatch fired), got %d", got)
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

	// merge_gate.go: checkAutoMergeConvergence. FR-1 consolidates the poll-native
	// in-queue derivation into settlePRMergeState (covered by
	// TestSettle_InMergeQueue_PollNativeSignal_CacheMiss): an item carrying the
	// poll-native flag settles as PRMergeQueued even on a boardcache miss. This
	// subtest confirms the convergence owner then hands off on PRMergeQueued with
	// no rebase dispatch — even when settle.PR does NOT carry the in-queue flag.
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
		// Poll-native in-queue on a cache miss settles as PRMergeQueued (settle.PR
		// does not carry the flag — REST fallback — but the ProjectItem field does).
		settle := PRSettleResult{Status: PRMergeQueued, PR: &gh.PRDetails{Number: 10, HeadSHA: "abc12345", IsInMergeQueue: false}}

		eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle, false)
		eng.wg.Wait()
		if snap, err := eng.store.Get("owner/repo", 42); err == nil && snap.RebaseCycles("Validate") != 0 {
			t.Errorf("cache-miss in-queue: expected no dispatch (PRMergeQueued hand-off), got cycles=%d", snap.RebaseCycles("Validate"))
		}
	})
}

// ── ADR-058 D5: merge-group stall detect-and-warn ────────────────────────────

// TestCheckAutoMergeConvergence_MergeGroupStall_DwellElapsed verifies that when
// a yolo PR has been in the merge queue past CIWaitTimeout, checkAutoMergeConvergence
// posts the stall comment, applies fabrik:paused + fabrik:awaiting-input, and
// removes fabrik:auto-merge-enabled (FR-1, FR-2, FR-3, FR-4).
func TestCheckAutoMergeConvergence_MergeGroupStall_DwellElapsed(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: false, IsMergeQueueEnabled: true}, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			// Label applied 31 minutes ago — past the 30-minute CIWaitTimeout.
			return time.Now().Add(-31 * time.Minute), nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 999, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = 30 * time.Minute

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{
		Status: PRMergeQueued,
		PR:     &gh.PRDetails{Number: 10, HeadSHA: "abc12345", IsMergeQueueEnabled: true},
	}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle, false)

	// Comment posted once with the spec-required body (FR-1).
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected 1 AddComment call (stall comment), got %d", len(client.addCommentCalls))
	}
	if len(client.addCommentCalls) > 0 {
		body := client.addCommentCalls[0].body
		if !strings.Contains(body, "merge queue stall detected") {
			t.Errorf("stall comment missing 'merge queue stall detected': %q", body)
		}
		if !strings.Contains(body, "on: merge_group") {
			t.Errorf("stall comment missing 'on: merge_group': %q", body)
		}
		if !strings.Contains(body, "fabrik:paused") {
			t.Errorf("stall comment missing 'fabrik:paused' resume instruction: %q", body)
		}
	}
	// 🚀 reaction added.
	if len(client.addCommentReactionCalls) != 1 {
		t.Errorf("expected 1 AddCommentReaction call, got %d", len(client.addCommentReactionCalls))
	}
	if len(client.addCommentReactionCalls) > 0 && client.addCommentReactionCalls[0].content != "rocket" {
		t.Errorf("expected rocket reaction, got %q", client.addCommentReactionCalls[0].content)
	}
	// fabrik:paused applied.
	wantLabels := map[string]bool{"fabrik:paused": false, "fabrik:awaiting-input": false}
	for _, c := range client.addLabelCalls {
		if _, ok := wantLabels[c.labelName]; ok {
			wantLabels[c.labelName] = true
		}
	}
	for label, found := range wantLabels {
		if !found {
			t.Errorf("expected label %q to be added, but it was not", label)
		}
	}
	// fabrik:auto-merge-enabled removed.
	removedAME := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			removedAME = true
		}
	}
	if !removedAME {
		t.Error("expected fabrik:auto-merge-enabled to be removed, but it was not")
	}
}

// TestCheckAutoMergeConvergence_MergeGroupStall_DwellNotElapsed verifies that
// when the PR is in the queue but the dwell has not yet elapsed, no stall comment
// is posted and no pause labels are applied.
func TestCheckAutoMergeConvergence_MergeGroupStall_DwellNotElapsed(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: false, IsMergeQueueEnabled: true}, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			// Label applied only 5 minutes ago — well under the 30-minute timeout.
			return time.Now().Add(-5 * time.Minute), nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = 30 * time.Minute

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	stage := &stages.Stage{Name: "Validate"}
	settle := PRSettleResult{
		Status: PRMergeQueued,
		PR:     &gh.PRDetails{Number: 10, HeadSHA: "abc12345", IsMergeQueueEnabled: true},
	}

	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, settle, false)

	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected 0 AddComment calls (dwell not elapsed), got %d", len(client.addCommentCalls))
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Errorf("expected no fabrik:paused label (dwell not elapsed), but it was added")
		}
	}
}

// TestCheckAutoMergeConvergence_MergeGroupStall_Idempotent verifies that after
// a stall pause, the idempotency is guaranteed by removedfabrik:auto-merge-enabled:
// handleAutoMergeConvergence returns false immediately when the label is absent,
// so checkAutoMergeConvergence (and therefore the stall comment) is never reached
// again on subsequent polls.
func TestCheckAutoMergeConvergence_MergeGroupStall_Idempotent(t *testing.T) {
	commentCount := 0
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: false, IsMergeQueueEnabled: true}, nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			commentCount++
			return 1, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = 30 * time.Minute

	// Simulate state after pauseForMergeGroupStall ran: fabrik:auto-merge-enabled
	// was removed, so the item no longer claims the convergence handler.
	// An item with fabrik:paused but without fabrik:auto-merge-enabled.
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:paused", "fabrik:awaiting-input"}}
	stage := &stages.Stage{Name: "Validate"}

	pctx := &phase1Ctx{
		ctx:   context.Background(),
		board: &gh.ProjectBoard{ProjectID: "PVT_1"},
		item:  item,
		stage: stage,
	}
	// handleAutoMergeConvergence returns false when fabrik:auto-merge-enabled is absent.
	if claimed := eng.handleAutoMergeConvergence(pctx); claimed {
		t.Error("expected handleAutoMergeConvergence to return false (no fabrik:auto-merge-enabled), got true")
	}
	if commentCount != 0 {
		t.Errorf("expected no AddComment calls after stall pause, got %d", commentCount)
	}
}
