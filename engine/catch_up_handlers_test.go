package engine

import (
	"context"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// TestCatchUpPhase1HandlersOrder asserts the handler precedence order by name.
// A future reorder of catchUpPhase1Handlers is a test failure, not a silent
// behavioral change — this directly implements the ADR-056 D3 "ordering as data"
// invariant. ADR-028 requires merge gate before CI gate; both are inside
// handleMergeAndCIGates (position 3), which follows handleAutoMergeConvergence
// (position 2) so that auto-merge items bypass settlePRMergeState entirely.
func TestCatchUpPhase1HandlersOrder(t *testing.T) {
	want := []string{
		"dependencies",
		"reviewGate",
		"autoMergeConvergence",
		"mergeAndCIGates",
	}
	if len(catchUpPhase1Handlers) != len(want) {
		t.Fatalf("catchUpPhase1Handlers has %d entries; want %d", len(catchUpPhase1Handlers), len(want))
	}
	for i, h := range catchUpPhase1Handlers {
		if h.name != want[i] {
			t.Errorf("catchUpPhase1Handlers[%d].name = %q; want %q", i, h.name, want[i])
		}
	}
}

// ---- handleReviewGate dispatch tests ----

// makeReviewGatePctx returns a phase1Ctx configured for review gate handler tests.
// The item has stage:Implement:complete and a single unresolved review thread comment.
func makeReviewGatePctx(board *gh.ProjectBoard, advancedItems map[string]bool) *phase1Ctx {
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"stage:Implement:complete"},
		LinkedPRReviewThreadComments: []gh.Comment{
			{
				ID:             "PRRC_handler_1",
				DatabaseID:     100,
				Author:         "copilot",
				Body:           "Please fix this.",
				ReviewThreadID: "RT_handler_1",
			},
		},
	}
	// Stage without WaitForReviews so checkReviewGate returns (false, false)
	// immediately — the handler then proceeds to buildReviewThreadComments which
	// finds the unresolved thread comment and dispatches a review reinvoke.
	stage := &stages.Stage{Name: "Implement", Order: 1, Prompt: "implement"}
	return &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stage,
		hasComplete:   true,
		advancedItems: advancedItems,
	}
}

// TestHandleReviewGate_WorkerInFlight_SkipsDispatch verifies that when a goroutine
// from a previous poll cycle is still running for an item, handleReviewGate
// claims the item (returns true) but does NOT increment ReviewCycles or dispatch
// a new reinvoke — preventing double-dispatch and spurious cycle limit advances.
func TestHandleReviewGate_WorkerInFlight_SkipsDispatch(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeReviewGatePctx(board, advancedItems)

	// Simulate an in-flight worker from a previous poll cycle.
	eng.store.Apply(itemstate.WorkerEntered{
		Repo: "owner/repo", Number: 10, StageName: "Implement", StartedAt: time.Now(),
	})

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	snap, _ := eng.store.Get("owner/repo", 10)
	if snap.ReviewCycles("Implement") != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0 (guard must prevent dispatch)", snap.ReviewCycles("Implement"))
	}
	if advancedItems["owner/repo#10"] {
		t.Error("advancedItems must not be set when worker-in-flight guard fires")
	}
}

// TestHandleReviewGate_CycleLimit_Pauses verifies that when ReviewCycles reaches
// MaxReviewCycles, handleReviewGate pauses the issue (adds fabrik:paused +
// fabrik:awaiting-input) instead of dispatching another reinvoke.
func TestHandleReviewGate_CycleLimit_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeReviewGatePctx(board, advancedItems)

	// Pre-fill ReviewCycles to the limit.
	for i := 0; i < eng.cfg.MaxReviewCycles; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{
			Repo: "owner/repo", Number: 10, StageName: "Implement",
		})
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	// Pause labels must have been added.
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused to be added on cycle limit; labels added: %v", labelNames)
	}
	if advancedItems["owner/repo#10"] {
		t.Error("advancedItems must not be set on cycle limit pause")
	}
}

// TestHandleReviewGate_HappyPath_Dispatches verifies the happy path: no worker
// in-flight, cycle count below limit, unresolved review thread comments present →
// ReviewCycles incremented, advancedItems set, handler returns true.
func TestHandleReviewGate_HappyPath_Dispatches(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeReviewGatePctx(board, advancedItems)

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	// advancedItems is set synchronously before the goroutine runs.
	if !advancedItems["owner/repo#10"] {
		t.Error("advancedItems[owner/repo#10] must be set on successful dispatch")
	}
	// ReviewCycles incremented synchronously.
	snap, _ := eng.store.Get("owner/repo", 10)
	if snap.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1", snap.ReviewCycles("Implement"))
	}
	eng.wg.Wait()
}

// ---- handleReviewGate #1207 guard-2 disable tests ----

// makeAutoMergeReviewGatePctx returns a phase1Ctx for an item already in the
// GitHub auto-merge convergence flow (fabrik:auto-merge-enabled present) with
// a single unresolved review thread comment on the current head — the shape
// of the #1183/PR#1202 incident this guard closes.
func makeAutoMergeReviewGatePctx(board *gh.ProjectBoard, advancedItems map[string]bool, extraLabels []string, mergeQueueEnabled bool, outdated bool) *phase1Ctx {
	item := gh.ProjectItem{
		Number:                      10,
		Repo:                        "owner/repo",
		Labels:                      append([]string{"stage:Implement:complete", "fabrik:auto-merge-enabled"}, extraLabels...),
		LinkedPRNumber:              55,
		LinkedPRHeadSHA:             "sha1",
		LinkedPRIsMergeQueueEnabled: mergeQueueEnabled,
		LinkedPRReviewThreadComments: []gh.Comment{
			{
				ID:             "PRRC_handler_1",
				DatabaseID:     100,
				Author:         "pruefer",
				Body:           "late finding",
				ReviewThreadID: "RT_handler_1",
				IsOutdated:     outdated,
			},
		},
	}
	stage := &stages.Stage{Name: "Implement", Order: 1, Prompt: "implement"}
	return &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stage,
		hasComplete:   true,
		advancedItems: advancedItems,
	}
}

// TestHandleReviewGate_DisablesNativeAutoMerge_OnUnresolvedCurrentHeadThread
// verifies #1207 guard 2: when an item already carrying
// fabrik:auto-merge-enabled has a fresh unresolved thread on its current
// head, DisablePullRequestAutoMerge fires (not DequeuePullRequest) and
// fabrik:auto-merge-enabled is removed, on the SAME poll that dispatches the
// review reinvoke — not deferred to a separate checkAutoMergeConvergence pass.
func TestHandleReviewGate_DisablesNativeAutoMerge_OnUnresolvedCurrentHeadThread(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeAutoMergeReviewGatePctx(board, advancedItems, nil, false, false)

	got := eng.handleReviewGate(pctx)
	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()

	if len(client.disablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected DisablePullRequestAutoMerge called once, got %d", len(client.disablePullRequestAutoMergeCalls))
	}
	if client.disablePullRequestAutoMergeCalls[0].prNumber != 55 {
		t.Errorf("DisablePullRequestAutoMerge called with PR %d, want 55", client.disablePullRequestAutoMergeCalls[0].prNumber)
	}
	if len(client.dequeuePullRequestCalls) != 0 {
		t.Errorf("DequeuePullRequest must not be called on a non-queue repo, got %d call(s)", len(client.dequeuePullRequestCalls))
	}
	foundRemoval := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundRemoval = true
		}
	}
	if !foundRemoval {
		t.Error("expected fabrik:auto-merge-enabled to be removed after successful disable")
	}
}

// TestHandleReviewGate_DequeuesOnMergeQueueEnabledRepo verifies the
// queue-aware branch of guard 2 (#1207): when the linked PR is merge-queue
// enabled, DequeuePullRequest fires instead of DisablePullRequestAutoMerge —
// mirroring reenableAutoMergeAfterRebase's existing queue-aware branch.
func TestHandleReviewGate_DequeuesOnMergeQueueEnabledRepo(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeAutoMergeReviewGatePctx(board, advancedItems, nil, true, false)

	got := eng.handleReviewGate(pctx)
	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()

	if len(client.dequeuePullRequestCalls) != 1 {
		t.Fatalf("expected DequeuePullRequest called once, got %d", len(client.dequeuePullRequestCalls))
	}
	if client.dequeuePullRequestCalls[0].prNumber != 55 {
		t.Errorf("DequeuePullRequest called with PR %d, want 55", client.dequeuePullRequestCalls[0].prNumber)
	}
	if len(client.disablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("DisablePullRequestAutoMerge must not be called on a queue-enabled repo, got %d call(s)", len(client.disablePullRequestAutoMergeCalls))
	}
	foundRemoval := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundRemoval = true
		}
	}
	if !foundRemoval {
		t.Error("expected fabrik:auto-merge-enabled to be removed after successful dequeue")
	}
}

// TestHandleReviewGate_OutdatedThread_DoesNotDisableAutoMerge verifies the
// stale-SHA scoping half of guard 2 (#1207): a thread GitHub has marked
// isOutdated (superseded by a later push) must not trigger a disable — the
// review-reinvoke dispatch still fires (buildReviewThreadComments is
// unscoped), but no GitHub mutation to disable merge machinery happens.
func TestHandleReviewGate_OutdatedThread_DoesNotDisableAutoMerge(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeAutoMergeReviewGatePctx(board, advancedItems, nil, false, true)

	got := eng.handleReviewGate(pctx)
	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()

	if len(client.disablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("DisablePullRequestAutoMerge must not be called for an outdated-only thread, got %d call(s)", len(client.disablePullRequestAutoMergeCalls))
	}
	if len(client.dequeuePullRequestCalls) != 0 {
		t.Errorf("DequeuePullRequest must not be called for an outdated-only thread, got %d call(s)", len(client.dequeuePullRequestCalls))
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			t.Error("fabrik:auto-merge-enabled must not be removed for an outdated-only thread")
		}
	}
}

// TestHandleReviewGate_DisablesAutoMerge_EvenWhenCycleLimitReached verifies
// that guard 2's disable fires even on the same poll that trips
// MaxReviewCycles and pauses the issue — the disable must not be
// conditioned on the reinvoke actually dispatching, since a paused item is
// exactly the case where GitHub must not be left free to merge while nobody
// is addressing the thread. Also confirms MaxReviewCycles still bounds and
// escalates (#1207's "no new never-advancing state" requirement).
func TestHandleReviewGate_DisablesAutoMerge_EvenWhenCycleLimitReached(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 1, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeAutoMergeReviewGatePctx(board, advancedItems, nil, false, false)

	for i := 0; i < eng.cfg.MaxReviewCycles; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{
			Repo: "owner/repo", Number: 10, StageName: "Implement",
		})
	}

	got := eng.handleReviewGate(pctx)
	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
	}
	eng.wg.Wait()

	if len(client.disablePullRequestAutoMergeCalls) != 1 {
		t.Errorf("expected DisablePullRequestAutoMerge called once even at cycle limit, got %d", len(client.disablePullRequestAutoMergeCalls))
	}
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused to be added on cycle limit (MaxReviewCycles must still escalate); labels added: %v", labelNames)
	}
}

// ---- handleMergeAndCIGates rebase reinvoke dispatch tests ----

// makeMergeGatePctx returns a phase1Ctx for merge-gate/CI-gate handler tests.
// The stage has WaitForCI enabled.
func makeMergeGatePctx(board *gh.ProjectBoard, advancedItems map[string]bool) *phase1Ctx {
	waitTrue := true
	item := gh.ProjectItem{
		Number: 20,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-ci"},
	}
	stage := &stages.Stage{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue}
	return &phase1Ctx{
		ctx:           context.Background(),
		board:         board,
		item:          item,
		stage:         stage,
		hasComplete:   false,
		advancedItems: advancedItems,
	}
}

// conflictingSettleClient returns a mockGitHubClient whose FetchLinkedPR +
// FetchPRMergeableFields produce a PRMergeConflicting result from settlePRMergeState.
func conflictingSettleClient() *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "deadbeef", State: "open", Merged: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			f := false
			return &f, "dirty", nil
		},
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
}

// TestHandleRebaseReinvoke_WorkerInFlight_SkipsDispatch verifies the worker-in-flight
// guard for the rebase reinvoke path inside handleMergeAndCIGates.
func TestHandleRebaseReinvoke_WorkerInFlight_SkipsDispatch(t *testing.T) {
	client := conflictingSettleClient()
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRebaseCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	// Simulate an in-flight worker from a previous poll cycle.
	eng.store.Apply(itemstate.WorkerEntered{
		Repo: "owner/repo", Number: 20, StageName: "Implement", StartedAt: time.Now(),
	})

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.RebaseCycles("Implement") != 0 {
		t.Errorf("RebaseCycles(Implement) = %d; want 0 (guard must prevent dispatch)", snap.RebaseCycles("Implement"))
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set when worker-in-flight guard fires")
	}
}

// TestHandleRebaseReinvoke_CycleLimit_Pauses verifies that when RebaseCycles
// reaches MaxRebaseCycles, handleMergeAndCIGates pauses the issue.
func TestHandleRebaseReinvoke_CycleLimit_Pauses(t *testing.T) {
	client := conflictingSettleClient()
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRebaseCycles = 2

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	// Pre-fill RebaseCycles to the limit.
	for i := 0; i < eng.cfg.MaxRebaseCycles; i++ {
		eng.store.Apply(itemstate.RebaseCycleIncremented{
			Repo: "owner/repo", Number: 20, StageName: "Implement",
		})
	}

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused on rebase cycle limit; labels added: %v", labelNames)
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set on cycle limit pause")
	}
}

// TestHandleRebaseReinvoke_HappyPath_Dispatches verifies the happy path for rebase
// reinvoke: PRMergeConflicting, no worker in-flight, cycle below limit →
// RebaseCycles incremented, advancedItems set, handler returns true.
func TestHandleRebaseReinvoke_HappyPath_Dispatches(t *testing.T) {
	client := conflictingSettleClient()
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRebaseCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	if !advancedItems["owner/repo#20"] {
		t.Error("advancedItems[owner/repo#20] must be set on successful rebase dispatch")
	}
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.RebaseCycles("Implement") != 1 {
		t.Errorf("RebaseCycles(Implement) = %d; want 1", snap.RebaseCycles("Implement"))
	}
	eng.wg.Wait()
}

// ---- handleMergeAndCIGates CI-fix reinvoke dispatch tests ----

// ciFailureSettleClient returns a mockGitHubClient whose settle sequence produces
// PRMergeBlocked (a completed CI failure check run, mergeable PR).
func ciFailureSettleClient() *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 43, HeadSHA: "cafebabe", State: "open", Merged: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "test", Status: "completed", Conclusion: "failure"},
			}, nil
		},
		fetchLabelAppliedAtFn: func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
			// Return the current time so elapsed ≈ 0 — well within the default
			// 30-minute CIWaitTimeout, preventing the timeout path from firing.
			return time.Now(), nil
		},
		addLabelToIssueFn:    func(_, _ string, _ int, _ string) error { return nil },
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
}

// TestHandleCIFixReinvoke_WorkerInFlight_SkipsDispatch verifies the worker-in-flight
// guard for the CI-fix reinvoke path inside handleMergeAndCIGates.
func TestHandleCIFixReinvoke_WorkerInFlight_SkipsDispatch(t *testing.T) {
	client := ciFailureSettleClient()
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxCiFixCycles = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	// Simulate an in-flight worker.
	eng.store.Apply(itemstate.WorkerEntered{
		Repo: "owner/repo", Number: 20, StageName: "Implement", StartedAt: time.Now(),
	})

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.CIFixCycles("Implement") != 0 {
		t.Errorf("CIFixCycles(Implement) = %d; want 0 (guard must prevent dispatch)", snap.CIFixCycles("Implement"))
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set when worker-in-flight guard fires")
	}
}

// TestHandleCIFixReinvoke_CycleLimit_Pauses verifies that when CIFixCycles
// reaches MaxCiFixCycles, handleMergeAndCIGates pauses the issue.
func TestHandleCIFixReinvoke_CycleLimit_Pauses(t *testing.T) {
	client := ciFailureSettleClient()
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxCiFixCycles = 2

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	// Pre-fill CIFixCycles to the limit.
	for i := 0; i < eng.cfg.MaxCiFixCycles; i++ {
		eng.store.Apply(itemstate.CIFixCycleIncremented{
			Repo: "owner/repo", Number: 20, StageName: "Implement",
		})
	}

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	client.mu.Lock()
	labelNames := make([]string, len(client.addLabelCalls))
	for i, c := range client.addLabelCalls {
		labelNames[i] = c.labelName
	}
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labelNames {
		if l == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Errorf("expected fabrik:paused on CI-fix cycle limit; labels added: %v", labelNames)
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set on cycle limit pause")
	}
}

// TestHandleCIFixReinvoke_NoOpDebounce_SkipsDispatch is the #958 leg 2
// regression: when a prior CI-fix reinvoke already recorded a no-op (no new
// commit pushed) for the exact current head SHA, handleMergeAndCIGates must
// not dispatch another reinvoke or increment CIFixCycles — repeating the
// same no-op burns cycle budget for nothing.
func TestHandleCIFixReinvoke_NoOpDebounce_SkipsDispatch(t *testing.T) {
	client := ciFailureSettleClient()
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxCiFixCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	// A prior reinvoke already observed no new commit for this exact head SHA
	// (ciFailureSettleClient's PR HeadSHA is "cafebabe").
	eng.store.Apply(itemstate.CIFixNoOpRecorded{Repo: "owner/repo", Number: 20, SHA: "cafebabe"})

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	eng.wg.Wait()
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.CIFixCycles("Implement") != 0 {
		t.Errorf("CIFixCycles(Implement) = %d; want 0 (no-op debounce must prevent dispatch)", snap.CIFixCycles("Implement"))
	}
	if advancedItems["owner/repo#20"] {
		t.Error("advancedItems must not be set when the no-op debounce guard fires")
	}
}

// TestHandleCIFixReinvoke_HappyPath_Dispatches verifies the happy path for CI-fix
// reinvoke: PRMergeBlocked (CI failed), no worker in-flight, cycle below limit →
// CIFixCycles incremented, advancedItems set, handler returns true.
func TestHandleCIFixReinvoke_HappyPath_Dispatches(t *testing.T) {
	client := ciFailureSettleClient()
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxCiFixCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeMergeGatePctx(board, advancedItems)

	got := eng.handleMergeAndCIGates(pctx)

	if !got {
		t.Error("handleMergeAndCIGates: expected true (item claimed), got false")
	}
	if !advancedItems["owner/repo#20"] {
		t.Error("advancedItems[owner/repo#20] must be set on successful CI-fix dispatch")
	}
	snap, _ := eng.store.Get("owner/repo", 20)
	if snap.CIFixCycles("Implement") != 1 {
		t.Errorf("CIFixCycles(Implement) = %d; want 1", snap.CIFixCycles("Implement"))
	}
	eng.wg.Wait()
}

// TestDispatchCIFixReinvoke_BotNoticeInReviewThread_ExcludedFromInvocation
// verifies the #1221 chokepoint end to end via dispatchCIFixReinvoke: a bot
// service notice merged in from item.LinkedPRReviewThreadComments is excluded
// by processComments's chokepoint filter, while the synthetic
// "ci-fix-synthetic" comment (author "fabrik", never bot-classified) survives
// and reaches Claude.
func TestDispatchCIFixReinvoke_BotNoticeInReviewThread_ExcludedFromInvocation(t *testing.T) {
	skipIfNoGit(t)

	var seenComments []gh.Comment
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			seenComments = comments
			return "fixed the CI failure", false, TokenUsage{}, nil
		},
	}
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 60,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-ci"},
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "RT_notice", DatabaseID: 950, ReviewThreadID: "thread1", Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."},
		},
	}
	stage := stgs[0]
	settle := PRSettleResult{
		Status:    PRMergeBlocked,
		CheckRuns: []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}},
	}

	eng.dispatchCIFixReinvoke(context.Background(), board, item, stage, settle)
	eng.wg.Wait()

	if len(claude.forCommentsCalls) != 1 {
		t.Fatalf("expected exactly 1 InvokeForComments call, got %d", len(claude.forCommentsCalls))
	}
	if len(seenComments) != 1 {
		t.Fatalf("expected exactly 1 comment reaching InvokeForComments (synthetic only), got %d: %v", len(seenComments), seenComments)
	}
	if seenComments[0].ID != "ci-fix-synthetic" {
		t.Errorf("expected synthetic ci-fix comment to survive, got %q", seenComments[0].ID)
	}
}

// TestDispatchRebaseReinvoke_BotNoticeInReviewThread_ExcludedFromInvocation
// mirrors the CI-fix test above for dispatchRebaseReinvoke: a bot service
// notice merged in from item.LinkedPRReviewThreadComments is excluded, while
// the synthetic "rebase-synthetic" comment survives and reaches Claude.
func TestDispatchRebaseReinvoke_BotNoticeInReviewThread_ExcludedFromInvocation(t *testing.T) {
	skipIfNoGit(t)

	var seenComments []gh.Comment
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			seenComments = comments
			return "rebased", false, TokenUsage{}, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 61,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "RT_notice", DatabaseID: 951, ReviewThreadID: "thread1", Author: "coderabbitai[bot]", Body: "auto-generated reply by CodeRabbit"},
		},
	}
	stage := stgs[0]

	eng.dispatchRebaseReinvoke(context.Background(), board, item, stage)
	eng.wg.Wait()

	if len(claude.forCommentsCalls) != 1 {
		t.Fatalf("expected exactly 1 InvokeForComments call, got %d", len(claude.forCommentsCalls))
	}
	if len(seenComments) != 1 {
		t.Fatalf("expected exactly 1 comment reaching InvokeForComments (synthetic only), got %d: %v", len(seenComments), seenComments)
	}
	if seenComments[0].ID != "rebase-synthetic" {
		t.Errorf("expected synthetic rebase comment to survive, got %q", seenComments[0].ID)
	}
}
