package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// completionItem is a Validate item as the worker's stale snapshot sees it.
func completionItem(labels ...string) gh.ProjectItem {
	return gh.ProjectItem{Number: 42, ItemID: "PVTI_42", Status: "Validate", Repo: "owner/repo", Labels: labels}
}

func completeValidate(t *testing.T, eng *Engine, item gh.ProjectItem) {
	t.Helper()
	stage := eng.cfg.Stages[2] // Validate (wait_for_ci)
	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage)
}

func TestHandleStageComplete_WaitForCI_MergedPR_TakesTerminalPath(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	completeValidate(t, eng, completionItem("stage:Implement:complete", "fabrik:yolo"))

	added := addedLabelNames(client.addLabelCalls)
	if containsLabel(added, "fabrik:awaiting-ci") {
		t.Errorf("fabrik:awaiting-ci must not be armed for a merged PR; added %v", added)
	}
	if !containsLabel(added, "stage:Validate:complete") {
		t.Errorf("terminal path must add stage:Validate:complete; added %v", added)
	}
	if len(client.updateStatusCalls) == 0 {
		t.Error("expected the item to be advanced to Done")
	}
}

func TestHandleStageComplete_WaitForCI_MergedPR_NonVacuity(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	eng.SetCompletionMergedCheckDisabledForTest(true)
	completeValidate(t, eng, completionItem("stage:Implement:complete", "fabrik:yolo"))
	if !containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:awaiting-ci") {
		t.Error("with the merged check neutralised awaiting-ci must be armed (proves the test bites)")
	}
}

func TestHandleStageComplete_WaitForCI_MergedViaConfirmation(t *testing.T) {
	// The list endpoint says closed/merged=false; the single-PR endpoint confirms.
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "closed"}, nil
		},
		fetchPRMergedFn: func(owner, repo string, n int) (bool, error) { return true, nil },
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	completeValidate(t, eng, completionItem("stage:Implement:complete"))
	if containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:awaiting-ci") {
		t.Error("awaiting-ci must not be armed when FetchPRMerged confirms the merge")
	}
	if len(client.updateStatusCalls) == 0 {
		t.Error("expected advance to Done")
	}
}

func TestHandleStageComplete_WaitForCI_ClosedUnmergedPR_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "closed"}, nil
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	completeValidate(t, eng, completionItem("stage:Implement:complete"))

	added := addedLabelNames(client.addLabelCalls)
	if containsLabel(added, "fabrik:awaiting-ci") {
		t.Errorf("awaiting-ci must not be armed for a closed-unmerged PR; added %v", added)
	}
	if !containsLabel(added, "fabrik:paused") || len(client.addCommentCalls) == 0 {
		t.Errorf("expected the closed-not-merged pause (label + comment); added %v", added)
	}
	if len(client.updateStatusCalls) != 0 {
		t.Error("a closed-unmerged PR must not advance the item")
	}
}

func TestHandleStageComplete_WaitForCI_LiveReadError_ArmsAsBefore(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return nil, errors.New("boom")
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	completeValidate(t, eng, completionItem("stage:Implement:complete"))
	if !containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:awaiting-ci") {
		t.Error("a failed live read must fall back to arming awaiting-ci")
	}
}

func TestHandleStageComplete_WaitForCI_OpenPR_ArmsAsBefore(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open"}, nil
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	completeValidate(t, eng, completionItem("stage:Implement:complete"))
	if !containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:awaiting-ci") {
		t.Error("an open PR must still arm awaiting-ci")
	}
}

// --- R4: in-flight merge deferral -------------------------------------------

func registerValidateWorker(eng *Engine, number int) {
	eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: number, StageName: "Validate", StartedAt: time.Now()})
}

func phase2Client() *mockGitHubClient {
	return &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, n int) ([]string, error) {
			return []string{"fabrik:yolo", "stage:Validate:complete"}, nil
		},
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "abc123", State: "open"}, nil
		},
	}
}

func TestRunCatchUpPhase2_WorkerInFlight_DefersMerge(t *testing.T) {
	client := phase2Client()
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	registerValidateWorker(eng, 42)

	eng.runCatchUpPhase2(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"},
		completionItem("fabrik:yolo", "stage:Validate:complete"), eng.cfg.Stages[2], map[string]bool{})

	if len(client.mergePRCalls) != 0 || len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("no merge/auto-merge while a worker is in flight; merge=%d auto=%d",
			len(client.mergePRCalls), len(client.enablePullRequestAutoMergeCalls))
	}
	snap, err := eng.store.Get("owner/repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	if snap.CooldownAt(mergeUnsettledCooldownReason).IsZero() {
		t.Errorf("deferral must record a %s admission cooldown (ADR 2096)", mergeUnsettledCooldownReason)
	}
}

func TestRunCatchUpPhase2_NoWorker_MergesAsBefore(t *testing.T) {
	client := phase2Client()
	eng := testEngineWithStages(t, client, terminalAdvanceStages())

	eng.runCatchUpPhase2(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"},
		completionItem("fabrik:yolo", "stage:Validate:complete"), eng.cfg.Stages[2], map[string]bool{})

	if len(client.enablePullRequestAutoMergeCalls)+len(client.mergePRCalls) == 0 {
		t.Error("without a worker the merge decision must proceed as today")
	}
}

func TestRunCatchUpPhase2_WorkerInFlight_NonVacuity(t *testing.T) {
	client := phase2Client()
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	eng.SetMergeInFlightGuardDisabledForTest(true)
	registerValidateWorker(eng, 42)

	eng.runCatchUpPhase2(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"},
		completionItem("fabrik:yolo", "stage:Validate:complete"), eng.cfg.Stages[2], map[string]bool{})

	if len(client.enablePullRequestAutoMergeCalls)+len(client.mergePRCalls) == 0 {
		t.Error("with the guard neutralised the merge must happen (proves the test bites)")
	}
}

func TestRunCatchUpPhase2_OpenValidateWithMergedPR_TakesTerminalPath(t *testing.T) {
	client := phase2Client()
	client.fetchLinkedPRFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
		return &gh.PRDetails{Number: 99, Merged: true, State: "closed"}, nil
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	advanced := map[string]bool{}

	eng.runCatchUpPhase2(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"},
		completionItem("fabrik:yolo", "stage:Validate:complete"), eng.cfg.Stages[2], advanced)

	if len(client.mergePRCalls)+len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Error("a merged PR must not get a merge attempt")
	}
	if len(client.updateStatusCalls) == 0 {
		t.Error("expected the terminal advance to Done")
	}
}

// --- R6: awaiting-ci leftovers on a landed item -------------------------------

func TestSettleAwaitingCIScan_DoneColumnMergedPR_NoOrphanPause(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	eng.cfg.MaxRetries = 2
	board := &gh.ProjectBoard{Items: []gh.ProjectItem{{
		Number: 22, Repo: "owner/repo", Status: "Done", Labels: []string{"fabrik:awaiting-ci"},
	}}}

	for i := 0; i < 4; i++ {
		eng.settleAwaitingCIScan(context.Background(), board, map[string]bool{})
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:paused") {
		t.Error("a landed item must never be paused as stranded")
	}
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "move the issue back") {
			t.Errorf("the 'move back to Validate' advice must not be posted for a merged PR: %s", c.body)
		}
	}
	if !containsLabel(removedLabelNames(client.removeLabelCalls), "fabrik:awaiting-ci") {
		t.Error("the leftover awaiting-ci label must be cleared")
	}
	// The withheld completion label is back-filled so the item is not hidden from
	// merged-but-open sweeps (FR-010).
	if !containsLabel(addedLabelNames(client.addLabelCalls), "stage:Validate:complete") {
		t.Errorf("stage:Validate:complete must be back-filled; added %v", addedLabelNames(client.addLabelCalls))
	}
}

// A closed issue is skipped by the settle scan (ADR-1387); its leftover
// fabrik:awaiting-ci is cleared by the closed-issue sweep instead, and the scan
// never pauses it. Pins US5 scenario 2 so neither half regresses.
func TestClosedDoneItemWithMergedPR_AwaitingCICleared_NoPause(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	eng.cfg.MaxRetries = 2
	board := &gh.ProjectBoard{Items: []gh.ProjectItem{{
		Number: 23, Repo: "owner/repo", Status: "Done", IsClosed: true, Labels: []string{"fabrik:awaiting-ci"},
	}}}

	for i := 0; i < 4; i++ {
		eng.settleAwaitingCIScan(context.Background(), board, map[string]bool{})
	}
	eng.cleanupClosedIssueTransientLabels(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:paused") {
		t.Error("a closed landed item must never be paused")
	}
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "move the issue back") {
			t.Errorf("unexpected 'move back to Validate' advice: %s", c.body)
		}
	}
	if !containsLabel(removedLabelNames(client.removeLabelCalls), "fabrik:awaiting-ci") {
		t.Error("the leftover awaiting-ci label on a closed item must be cleared")
	}
}

// markPRReady must not touch a merged PR when the push is skipped (#2299 R5):
// no MarkPRReady call and no warning-shaped failure path.
func TestMarkPRReady_MergedPR_SkipsPushAndReady(t *testing.T) {
	g := newPushGuardEnv(t, 7, &gh.PRDetails{Number: 5, State: "closed", Merged: true}, nil)
	if refused := g.e.markPRReady(gh.ProjectItem{Number: 7}, 5); refused {
		t.Error("a merged-PR skip is not a zero-ahead refusal")
	}
	g.client.mu.Lock()
	defer g.client.mu.Unlock()
	if len(g.client.markPRReadyCalls) != 0 {
		t.Errorf("MarkPRReady must not be called on a merged PR: %v", g.client.markPRReadyCalls)
	}
	if g.remoteRef("fabrik/issue-7") != "" {
		t.Error("the remote branch of a merged PR must not be created")
	}
}

func TestEscalateAwaitingCIOrphanFailure_MergedPR_DoesNotPause(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	eng.escalateAwaitingCIOrphanFailure(gh.ProjectItem{
		Number: 22, Repo: "owner/repo", Status: "Done", Labels: []string{"fabrik:awaiting-ci"},
	})
	if containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:paused") || len(client.addCommentCalls) != 0 {
		t.Error("escalation must not pause or comment on an item whose PR merged")
	}
}

func TestSettleAwaitingCIScan_DoneColumnOpenPR_StillEscalates(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open"}, nil
		},
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	eng.cfg.MaxRetries = 2
	board := &gh.ProjectBoard{Items: []gh.ProjectItem{{
		Number: 22, Repo: "owner/repo", Status: "Done", Labels: []string{"fabrik:awaiting-ci"},
	}}}
	for i := 0; i < 2; i++ {
		eng.settleAwaitingCIScan(context.Background(), board, map[string]bool{})
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if !containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:paused") {
		t.Error("an unmerged orphan must still escalate as before")
	}
}

// --- R5: no push to a merged PR's branch -------------------------------------

func TestMergedPRPushRefused(t *testing.T) {
	merged := true
	var readErr error
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			if readErr != nil {
				return nil, readErr
			}
			return &gh.PRDetails{Number: 10, Merged: merged, State: "open"}, nil
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	item := completionItem()

	if !eng.mergedPRPushRefused(item, "fabrik/issue-42") {
		t.Error("merged PR: push must be refused")
	}
	eng.SetMergedPushGuardDisabledForTest(true)
	if eng.mergedPRPushRefused(item, "fabrik/issue-42") {
		t.Error("neutralised guard must not refuse (non-vacuity)")
	}
	eng.SetMergedPushGuardDisabledForTest(false)
	merged = false
	if eng.mergedPRPushRefused(item, "fabrik/issue-42") {
		t.Error("open PR: push must proceed")
	}
	merged = true
	readErr = errors.New("boom")
	if eng.mergedPRPushRefused(item, "fabrik/issue-42") {
		t.Error("read error must fail open")
	}
}

func TestPushBranchUnlessQueued_MergedPR_NoPush(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineWithStages(t, client, terminalAdvanceStages())
	// A nil worktree manager would panic if the push were attempted.
	err := eng.pushBranchUnlessQueued(completionItem(), nil)
	if !errors.Is(err, ErrPushSkippedPRMerged) {
		t.Fatalf("want ErrPushSkippedPRMerged, got %v", err)
	}
}
