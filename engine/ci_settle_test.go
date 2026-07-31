package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// ciSettleWaitForCIStages returns a minimal Validate/wait_for_ci stage list used
// across settleAwaitingCIScan tests.
func ciSettleWaitForCIStages() []*stages.Stage {
	waitTrue := true
	return []*stages.Stage{
		{Name: "Validate", Order: 1, Prompt: "validate", WaitForCI: &waitTrue},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

// TestSettleAwaitingCIScan_BypassesBoardColumnAdmission is the core #1270
// regression: settleAwaitingCIScan must evaluate the CI gate for a
// fabrik:awaiting-ci item using only board.Items — it never calls
// itemMayNeedWork, selectDeepFetchCandidates, or the main catch-up loop's
// per-item admission gate. This test calls it directly (bypassing poll()
// entirely) and confirms the CI-fix reinvoke still dispatches, demonstrating
// the scan's independence from whatever silently excluded #3915 from that
// shared pipeline in the field.
func TestSettleAwaitingCIScan_BypassesBoardColumnAdmission(t *testing.T) {
	client := ciFailureSettleClient()
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 5

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{
				Number: 20,
				Repo:   "owner/repo",
				Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}
	advancedItems := make(map[string]bool)

	eng.settleAwaitingCIScan(context.Background(), board, advancedItems)
	eng.wg.Wait()

	snap, _ := eng.store.Get("owner/repo", 20)
	if got := snap.CIFixCycles("Validate"); got != 1 {
		t.Errorf("CIFixCycles(Validate) = %d; want 1 — settleAwaitingCIScan must dispatch a CI-fix reinvoke directly from board.Items", got)
	}
	if !advancedItems["owner/repo#20"] {
		t.Error("expected advancedItems[owner/repo#20] set on successful CI-fix dispatch")
	}
}

// TestSettleAwaitingCIScan_NoDoubleDispatch runs a full eng.poll(ctx) for an
// item at a wait_for_ci Validate stage carrying only fabrik:awaiting-ci (no
// stage:Validate:complete) with a failing check run, and confirms the CI-fix
// cycle count is incremented exactly once — proving the main catch-up loop's
// narrowed (hasComplete-only) admission gate and the dedicated
// settleAwaitingCIScan never both act on the same item in the same poll.
func TestSettleAwaitingCIScan_NoDoubleDispatch(t *testing.T) {
	client := ciFailureSettleClient()
	client.fetchProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
		return &gh.ProjectBoard{
			ProjectID: "PVT_1",
			Items: []gh.ProjectItem{
				{
					Number: 21,
					ItemID: "PVTI_21",
					Status: "Validate",
					Repo:   "owner/repo",
					Labels: []string{"fabrik:awaiting-ci"},
				},
			},
		}, nil
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxCiFixCycles = 5

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	snap, _ := eng.store.Get("owner/repo", 21)
	if got := snap.CIFixCycles("Validate"); got != 1 {
		t.Errorf("CIFixCycles(Validate) = %d; want exactly 1 — Phase 1's narrowed admission gate and settleAwaitingCIScan must never both dispatch for the same item in the same poll", got)
	}
}

// TestSettleAwaitingCIScan_OrphanColumn_Escalates covers the "gate genuinely
// cannot be evaluated" case: an item carries fabrik:awaiting-ci but sits at a
// column with no wait_for_ci stage (here, a HoldingStage). Repeated settle
// passes must eventually pause the issue, remove the marker, and post an
// explanatory comment naming the stray column — never leave it silent forever.
func TestSettleAwaitingCIScan_OrphanColumn_Escalates(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
	stgs := []*stages.Stage{
		{Name: "Queued", Order: 1, HoldingStage: true},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxRetries = 2

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 22,
				Repo:   "owner/repo",
				Status: "Queued",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	pausedAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			markerRemoved = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added after MaxRetries settle failures on an orphan column")
	}
	if !markerRemoved {
		t.Error("expected fabrik:awaiting-ci to be removed on escalation")
	}
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected an explanatory escalation comment to be posted")
	}
	found := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 22 && strings.Contains(c.body, "Queued") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected escalation comment to name the stray column, got: %v", client.addCommentCalls)
	}
}

// TestSettleAwaitingCIScan_DeepFetchFailure_Escalates covers the other "gate
// genuinely cannot be evaluated" case: the item resolves to a real wait_for_ci
// stage, but FetchItemDetails fails on every settle pass (e.g. permissions,
// a deleted issue node, sustained API errors), so the scan can never reach
// checkCIGate for it. This must retry-then-escalate exactly like the
// orphan-column case, not retry silently forever — and the escalation comment
// must describe the fetch failure, not falsely claim a stray board column.
func TestSettleAwaitingCIScan_DeepFetchFailure_Escalates(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			return errors.New("simulated persistent GraphQL failure")
		},
	}
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())
	eng.cfg.MaxRetries = 2

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 25,
				Repo:   "owner/repo",
				Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	pausedAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			markerRemoved = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added after MaxRetries settle failures on a persistent deep-fetch failure")
	}
	if !markerRemoved {
		t.Error("expected fabrik:awaiting-ci to be removed on escalation")
	}
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected an explanatory escalation comment to be posted")
	}
	found := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 25 && strings.Contains(c.body, "fetched from GitHub") && !strings.Contains(c.body, "wait_for_ci`") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected escalation comment to describe the fetch failure (not a stray-column claim), got: %v", client.addCommentCalls)
	}
}

// TestSettleAwaitingCIScan_SkipsPausedAndClosedItems mirrors the other settle
// scans' guards: paused items must not be fought by this scan, and closed
// items are the exclusive responsibility of runValidatePRTerminalAdvance
// (ADR-056 D2) — duplicating that recovery here would create a second owner.
func TestSettleAwaitingCIScan_SkipsPausedAndClosedItems(t *testing.T) {
	client := ciFailureSettleClient()
	eng := testEngineWithStages(t, client, ciSettleWaitForCIStages())

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 23, Repo: "owner/repo", Status: "Validate",
				Labels: []string{"fabrik:awaiting-ci", "fabrik:paused"},
			},
			{
				Number: 24, Repo: "owner/repo", Status: "Validate", IsClosed: true,
				Labels: []string{"fabrik:awaiting-ci"},
			},
		},
	}

	eng.settleAwaitingCIScan(context.Background(), board, make(map[string]bool))
	eng.wg.Wait()

	for _, n := range []int{23, 24} {
		snap, _ := eng.store.Get("owner/repo", n)
		if snap.CIFixCycles("Validate") != 0 {
			t.Errorf("#%d: expected no CI-fix dispatch for a paused/closed item, got CIFixCycles=%d", n, snap.CIFixCycles("Validate"))
		}
	}
}
