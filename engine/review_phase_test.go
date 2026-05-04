package engine

import (
	"context"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestCatchUpLoop_NonYolo_ReviewReinvoke_Fires verifies that Phase 1 of the
// catch-up loop dispatches dispatchReviewReinvoke even when the item has no
// fabrik:yolo or fabrik:cruise label. This is the core behavior added by
// issue #392: inline PR review thread comments must be addressed on all issues,
// not just yolo/cruise ones.
func TestCatchUpLoop_NonYolo_ReviewReinvoke_Fires(t *testing.T) {
	threadComment := gh.Comment{
		ID:             "PRRC_nonyolo_1",
		DatabaseID:     301,
		Author:         "copilot",
		Body:           "Please fix the error handling.",
		ReviewThreadID: "RT_nonyolo_1",
	}

	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 55,
						ItemID: "PVTI_55",
						Status: "Implement",
						Repo:   "owner/repo",
						// No yolo or cruise label — this is a normal issue
						Labels: []string{"stage:Implement:complete"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRReviewThreadComments = []gh.Comment{threadComment}
			return nil
		},
	}

	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(client, stgs)
	eng.cfg.MaxReviewCycles = 5

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	// ReviewCycles should be 1 — dispatchReviewReinvoke was dispatched.
	snap55, _ := eng.store.Get("owner/repo", 55)
	if snap55.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1 (reinvoke must fire for non-yolo items with unresolved review threads)", snap55.ReviewCycles("Implement"))
	}

	// No stage advancement should have occurred (Phase 2 is gated on yolo/cruise).
	client.mu.Lock()
	statusCalls := len(client.updateStatusCalls)
	client.mu.Unlock()
	if statusCalls != 0 {
		t.Errorf("updateStatusCalls = %d; want 0 (non-yolo items must not auto-advance)", statusCalls)
	}
}

// TestCatchUpLoop_NonYolo_NoThreads_NoAdvance verifies that a non-yolo item
// with stage:X:complete but no unresolved review thread comments does NOT
// auto-advance. Phase 2 advancement remains gated on yolo/cruise/auto_advance.
func TestCatchUpLoop_NonYolo_NoThreads_NoAdvance(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 56,
						ItemID: "PVTI_56",
						Status: "Implement",
						Repo:   "owner/repo",
						// No yolo or cruise label, no review threads
						Labels: []string{"stage:Implement:complete"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// No review thread comments — nothing to reinvoke or advance.
			item.LinkedPRReviewThreadComments = nil
			return nil
		},
	}

	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(client, stgs)

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	// No advancement and no reinvoke cycle.
	client.mu.Lock()
	statusCalls := len(client.updateStatusCalls)
	client.mu.Unlock()
	if statusCalls != 0 {
		t.Errorf("updateStatusCalls = %d; want 0 (non-yolo items must not auto-advance)", statusCalls)
	}
	snap56, _ := eng.store.Get("owner/repo", 56)
	if snap56.ReviewCycles("Implement") != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0 (no review threads, no reinvoke)", snap56.ReviewCycles("Implement"))
	}
}

// TestProcessComments_MergesReviewThreadComments verifies that processComments
// automatically merges unresolved PR review thread comments from
// item.LinkedPRReviewThreadComments into the working slice. This closes the
// race where a user nudge arrives before the catch-up loop Phase 1 fires —
// the review thread comments are addressed in the same invocation, receive 🚀
// reactions, and have their threads resolved.
func TestProcessComments_MergesReviewThreadComments(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "Addressed the review feedback.", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Implement", Order: 1}

	// Item has a linked PR review thread comment that is unresolved.
	reviewThreadComment := gh.Comment{
		ID:             "PRRC_merge_1",
		DatabaseID:     401,
		Author:         "copilot",
		Body:           "Please add error handling here.",
		ReviewThreadID: "RT_merge_1",
	}
	item := gh.ProjectItem{
		Number:                       20,
		Repo:                         "owner/repo",
		Body:                         "spec",
		LinkedPRReviewThreadComments: []gh.Comment{reviewThreadComment},
	}

	// User nudge — just a conversation comment, NOT the review thread comment.
	userComment := gh.Comment{
		ID:         "IC_nudge_1",
		DatabaseID: 402,
		Author:     "user",
		Body:       "Please address the Copilot feedback.",
	}

	err := eng.processComments(context.Background(), board, item, stage, []gh.Comment{userComment})
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// ResolveReviewThread must have been called for the review thread.
	client.mu.Lock()
	resolvedThreads := make([]string, len(client.resolveReviewThreadCalls))
	copy(resolvedThreads, client.resolveReviewThreadCalls)
	client.mu.Unlock()

	found := false
	for _, tid := range resolvedThreads {
		if tid == "RT_merge_1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ResolveReviewThread to be called for RT_merge_1; got calls: %v", resolvedThreads)
	}

	// The ROCKET reaction must have been added to the review thread comment.
	client.mu.Lock()
	rocketCalls := make([]prReviewCommentReactionCall, len(client.addPRReviewCommentReactionCalls))
	copy(rocketCalls, client.addPRReviewCommentReactionCalls)
	client.mu.Unlock()

	rocketFound := false
	for _, rc := range rocketCalls {
		if rc.commentID == 401 && rc.content == "rocket" {
			rocketFound = true
			break
		}
	}
	if !rocketFound {
		t.Errorf("expected ROCKET reaction to be added to review comment 401; reaction calls: %v", rocketCalls)
	}
}

// TestCatchUpLoop_YoloIssue_ReviewReinvoke_StillFires verifies that Phase 1
// review reinvoke continues to work correctly for yolo-labeled items
// (regression guard for the existing TestCatchUpLoop_InFlightGuard behavior).
func TestCatchUpLoop_YoloIssue_ReviewReinvoke_StillFires(t *testing.T) {
	threadComment := gh.Comment{
		ID:             "PRRC_yolo_1",
		DatabaseID:     501,
		Author:         "copilot",
		Body:           "Fix this.",
		ReviewThreadID: "RT_yolo_1",
	}

	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 57,
						ItemID: "PVTI_57",
						Status: "Implement",
						Repo:   "owner/repo",
						Labels: []string{"stage:Implement:complete", "fabrik:yolo"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRReviewThreadComments = []gh.Comment{threadComment}
			return nil
		},
	}

	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(client, stgs)
	eng.cfg.MaxReviewCycles = 5

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	snap57, _ := eng.store.Get("owner/repo", 57)
	if snap57.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1 (yolo items must still trigger review reinvoke)", snap57.ReviewCycles("Implement"))
	}
}
