package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// reviewTestStages returns a two-stage pipeline for review gate tests.
func reviewTestStages() []*stages.Stage {
	waitTrue := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
}

func reviewTestEngine(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	return testEngineWithStages(t, client, reviewTestStages())
}

// (a) No requested reviewers AND no reviews submitted → gate STAYS BLOCKED,
// waiting for self-assigning bot reviewers (Copilot, Gemini) to post. This
// is the common yolo case: the pipeline marks the PR ready and immediately
// evaluates the gate; bots are still processing and haven't submitted yet,
// so we wait.
func TestCheckReviewGate_NoReviewersNoReviews_Blocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRReviewRequests: nil, // no requested reviewers (bots don't use this)
		LinkedPRReviews:        nil, // no reviews submitted yet
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected blocked when no reviews submitted yet (bots may still be processing)")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
	if len(client.addLabelCalls) != 1 {
		t.Errorf("expected 1 label add (fabrik:awaiting-review), got %d", len(client.addLabelCalls))
	}
}

// (a2) No requested reviewers but at least one review submitted → gate clears.
// This covers the case where a bot like Copilot or Gemini has self-submitted
// without ever appearing in reviewRequests.
func TestCheckReviewGate_NoReviewersWithReview_Clears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRReviewRequests: nil, // no requested reviewers
		LinkedPRReviews: []gh.PRReview{
			{Author: "copilot-pull-request-reviewer", State: "COMMENTED", Body: "## Pull request overview\n\nLGTM."},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked once a review has been submitted")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label adds, got %d", len(client.addLabelCalls))
	}
}

// (a2) Gate disabled (nil WaitForReviews) → always returns false.
func TestCheckReviewGate_GateDisabled_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot"},
		},
	}
	// WaitForReviews is nil (not set)
	stage := &stages.Stage{Name: "Implement", WaitForReviews: nil}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked when WaitForReviews is nil")
	}
	if timedOut {
		t.Error("expected not timedOut when WaitForReviews is nil")
	}
}

// (b) Reviewer requested but no review submitted → block and apply label.
func TestCheckReviewGate_ReviewerRequested_NoReview_Blocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		// copilot is in reviewRequests → outstanding
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot"},
		},
		// No reviews submitted yet
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected blocked when reviewer has not submitted")
	}
	if timedOut {
		t.Error("expected not timedOut when reviewer has not submitted")
	}
	// Label should be applied on first block
	if len(client.addLabelCalls) != 1 {
		t.Fatalf("expected 1 add label call, got %d", len(client.addLabelCalls))
	}
	if client.addLabelCalls[0].labelName != "fabrik:awaiting-review" {
		t.Errorf("expected fabrik:awaiting-review label, got %q", client.addLabelCalls[0].labelName)
	}
}

// (b2) Already has awaiting-review label → still blocked but no duplicate label add.
func TestCheckReviewGate_AlreadyWaiting_NoLabelAdd(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	// FetchLabelAppliedAt returns recent time (no timeout)
	recentTime := time.Now().Add(-1 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return recentTime, nil
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked")
	}
	if timedOut {
		t.Error("expected not timedOut when recently applied label")
	}
	// No new label add (already present)
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label add when already waiting, got %d", len(client.addLabelCalls))
	}
}

// (c) All requested reviewers have submitted → advance (no reviewers in reviewRequests).
func TestCheckReviewGate_AllReviewersSubmitted_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// copilot no longer in reviewRequests (they submitted) and awaiting-review label present
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
		// Empty reviewRequests = all reviewers submitted
		LinkedPRReviewRequests: nil,
		LinkedPRReviews: []gh.PRReview{
			{Author: "copilot", State: "APPROVED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked when all reviewers submitted")
	}
	if timedOut {
		t.Error("expected not timedOut when all reviewers submitted")
	}
	// Label should be removed
	if len(client.removeLabelCalls) != 1 {
		t.Fatalf("expected 1 remove label call, got %d", len(client.removeLabelCalls))
	}
	if client.removeLabelCalls[0].labelName != "fabrik:awaiting-review" {
		t.Errorf("expected removal of fabrik:awaiting-review, got %q", client.removeLabelCalls[0].labelName)
	}
}

// (d) Timeout elapsed → advance with warning, label removed.
func TestCheckReviewGate_TimeoutElapsed_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	// Override timeout to 5 minutes; label was applied 10 minutes ago → timed out
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute
	appliedAt := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return appliedAt, nil
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
		// Reviewer still outstanding (would block if not for timeout)
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked when timeout elapsed")
	}
	if !timedOut {
		t.Error("expected timedOut == true when timeout elapsed")
	}
	// Label should be removed after timeout
	if len(client.removeLabelCalls) != 1 {
		t.Fatalf("expected 1 remove label call, got %d", len(client.removeLabelCalls))
	}
	if client.removeLabelCalls[0].labelName != "fabrik:awaiting-review" {
		t.Errorf("expected removal of fabrik:awaiting-review, got %q", client.removeLabelCalls[0].labelName)
	}
	// FetchLabelAppliedAt should have been called for timeout check
	if len(client.fetchLabelAppliedAtCalls) != 1 {
		t.Errorf("expected FetchLabelAppliedAt to be called, got %d calls", len(client.fetchLabelAppliedAtCalls))
	}
}

// (e) Dismissed reviewer re-blocks gate: reviewer re-appears in reviewRequests.
func TestCheckReviewGate_DismissedReviewer_Reblocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	// Label was applied recently — not timed out
	recentTime := time.Now().Add(-1 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return recentTime, nil
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// Reviewer submitted once (appears in latestReviews) but review was dismissed
	// and they were re-added to reviewRequests — so they're outstanding again.
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
		// copilot re-appears in reviewRequests after dismissal
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot"},
		},
		// Prior review (now dismissed)
		LinkedPRReviews: []gh.PRReview{
			{Author: "copilot", State: "APPROVED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected re-blocked after reviewer dismissal and re-request")
	}
	if timedOut {
		t.Error("expected not timedOut on dismissed reviewer re-block")
	}
	// No new label (already present)
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no new label add, got %d", len(client.addLabelCalls))
	}
}

func TestCheckReviewGate_DismissedReviewDoesNotClear(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	recentTime := time.Now().Add(-1 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return recentTime, nil
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// No outstanding review requests, but the only review is DISMISSED.
	// hasReviews must be false — the gate should stay blocked.
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		Labels:                 []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: nil,
		LinkedPRReviews:        []gh.PRReview{{Author: "alice", State: "DISMISSED"}},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to stay blocked when only review is DISMISSED")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}
}

// (f) buildReviewThreadComments returns inline thread comments with real DatabaseIDs.
func TestBuildReviewThreadComments(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_1", DatabaseID: 101, Author: "copilot", Body: "Please fix the error handling.", ReviewThreadID: "RT_1"},
			{ID: "PRRC_2", DatabaseID: 102, Author: "human", Body: "Consider edge case.", ReviewThreadID: "RT_2"},
		},
	}

	comments := eng.buildReviewThreadComments(item)

	if len(comments) != 2 {
		t.Fatalf("expected 2 thread comments, got %d", len(comments))
	}
	if comments[0].DatabaseID == 0 {
		t.Error("thread comments must carry real DatabaseIDs so reactions work")
	}
	if comments[0].ReviewThreadID == "" {
		t.Error("thread comments must carry ReviewThreadID so threads can be resolved later")
	}
}

// (f2) buildReviewThreadComments skips comments already present in ProcessedComments.
func TestBuildReviewThreadComments_ProcessedCommentsSkip(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_1", DatabaseID: 101, Author: "copilot", Body: "Already handled.", ReviewThreadID: "RT_1"},
			{ID: "PRRC_2", DatabaseID: 102, Author: "human", Body: "Not yet handled.", ReviewThreadID: "RT_2"},
		},
	}

	// Pre-populate ProcessedComments for comment PRRC_1 (simulates markCommentsProcessed).
	eng.store.Apply(itemstate.CommentProcessed{Repo: "owner/repo", Number: 10, CommentID: "PRRC_1", At: time.Now()})

	comments := eng.buildReviewThreadComments(item)

	if len(comments) != 1 {
		t.Fatalf("expected 1 comment (PRRC_1 should be skipped), got %d", len(comments))
	}
	if comments[0].ID != "PRRC_2" {
		t.Errorf("expected remaining comment to be PRRC_2, got %q", comments[0].ID)
	}
}

// (f3) catch-up loop skips dispatchReviewReinvoke when a goroutine is already
// in-flight for the item, and does NOT increment ReviewCycles.
func TestCatchUpLoop_InFlightGuard(t *testing.T) {
	threadComment := gh.Comment{
		ID:             "PRRC_guard_1",
		DatabaseID:     201,
		Author:         "copilot",
		Body:           "Please fix this.",
		ReviewThreadID: "RT_guard_1",
	}

	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 42,
						ItemID: "PVTI_42",
						Status: "Implement",
						Repo:   "owner/repo",
						Labels: []string{"stage:Implement:complete", "fabrik:yolo"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// Simulate FetchItemDetails populating review thread comments.
			item.LinkedPRReviewThreadComments = []gh.Comment{threadComment}
			return nil
		},
	}

	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	// Pre-populate the store Worker to simulate a goroutine already running.
	// The dispatch guard uses snap.Worker() != nil (Store-backed) so only the
	// Store mutation is needed; no separate inFlight map entry is required.
	eng.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     42,
		User:       "testuser",
		Worker:     &itemstate.WorkerHandle{StageName: "Implement", StartedAt: time.Now()},
		AcquiredAt: time.Now(),
	})

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Drain any goroutines (none should have been launched, but be safe).
	eng.wg.Wait()

	// ReviewCycles must remain 0 — the inFlight guard must prevent the dispatch.
	snap42, _ := eng.store.Get("owner/repo", 42)
	if snap42.ReviewCycles("Implement") != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0 (dispatch must be suppressed when in-flight)", snap42.ReviewCycles("Implement"))
	}
}

// (g) pauseForReviewTimeout applies labels and posts a comment.
func TestPauseForReviewTimeout(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}

	eng.pauseForReviewTimeout(board, item, stage)

	// Should have added fabrik:paused and fabrik:awaiting-input
	labelNames := make(map[string]bool)
	for _, call := range client.addLabelCalls {
		labelNames[call.labelName] = true
	}
	if !labelNames["fabrik:paused"] {
		t.Error("expected fabrik:paused label to be added")
	}
	if !labelNames["fabrik:awaiting-input"] {
		t.Error("expected fabrik:awaiting-input label to be added")
	}

	// Should have posted a comment
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
}

// (h) pauseForReviewCycleLimit applies labels, posts a comment with cycle count.
func TestPauseForReviewCycleLimit(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}

	eng.pauseForReviewCycleLimit(board, item, stage, 5, 5)

	// Should have added fabrik:paused and fabrik:awaiting-input
	labelNames := make(map[string]bool)
	for _, call := range client.addLabelCalls {
		labelNames[call.labelName] = true
	}
	if !labelNames["fabrik:paused"] {
		t.Error("expected fabrik:paused label to be added")
	}
	if !labelNames["fabrik:awaiting-input"] {
		t.Error("expected fabrik:awaiting-input label to be added")
	}

	// Should have posted a comment mentioning the cycle count
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
}

// (i1) ReviewCycles is per-stage: cycles consumed by one stage do not
// reduce the budget for a different stage on the same issue.
func TestReviewCycleCount_PerStageNotPerIssue(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{
		{Name: "Review", Order: 1, Prompt: "review"},
		{Name: "Validate", Order: 2, Prompt: "validate"},
	}
	eng := testEngineWithStages(t, client, stgs)

	// Simulate Review consuming 3 cycles out of 5.
	for i := 0; i < 3; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 10, StageName: "Review"})
	}

	// Before Validate "runs", its counter must be 0 — proving Review's counter
	// did not bleed into Validate's key.
	snapBefore, _ := eng.store.Get("owner/repo", 10)
	if snapBefore.ReviewCycles("Validate") != 0 {
		t.Fatalf("Validate ReviewCycles = %d before Validate runs; want 0 (must be independent)", snapBefore.ReviewCycles("Validate"))
	}

	// Simulate Validate consuming one cycle and verify it uses its own counter
	// without disturbing Review's existing count.
	eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 10, StageName: "Validate"})
	snapAfter, _ := eng.store.Get("owner/repo", 10)
	reviewCount := snapAfter.ReviewCycles("Review")
	validateCount := snapAfter.ReviewCycles("Validate")

	if reviewCount != 3 {
		t.Errorf("Review ReviewCycles = %d after Validate increment; want 3", reviewCount)
	}
	if validateCount != 1 {
		t.Errorf("Validate ReviewCycles = %d; want 1 (must be independent of Review cycles)", validateCount)
	}
}

// (i2) clearFailedStage resets only the paused stage's ReviewCycles; a
// different stage's counter on the same issue is unaffected.
func TestClearFailedStage_ReviewCycleCount_ResetsOnlyCurrentStage(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{
		{Name: "Review", Order: 1, Prompt: "review", WaitForReviews: boolPtr(true)},
		{Name: "Validate", Order: 2, Prompt: "validate", WaitForReviews: boolPtr(true)},
	}
	eng := testEngineWithStages(t, client, stgs)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"stage:Review:failed", "fabrik:paused"},
	}

	// Simulate both stages having consumed cycles (Review hit limit; Validate consumed 2).
	for i := 0; i < 5; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 10, StageName: "Review"})
	}
	for i := 0; i < 2; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 10, StageName: "Validate"})
	}

	// User manually unpauses Review.
	reviewStage := &stages.Stage{Name: "Review", Order: 1}
	eng.clearFailedStage(item, reviewStage)

	// Review's counter must be reset to 0.
	snapAfter, _ := eng.store.Get("owner/repo", 10)
	afterReview := snapAfter.ReviewCycles("Review")
	afterValidate := snapAfter.ReviewCycles("Validate")

	if afterReview != 0 {
		t.Errorf("Review ReviewCycles = %d after clearFailedStage; want 0", afterReview)
	}
	// Validate's counter must be untouched — it has an independent budget.
	if afterValidate != 2 {
		t.Errorf("Validate ReviewCycles = %d after clearing Review; want 2 (independent)", afterValidate)
	}
}

// --- Bot-reviewer escalation ladder tests ---

// Phase 1: pure-bot outstanding, awaiting-review timeout elapsed, no reprompted label.
// Verifies DELETE+POST review requests, @mention PR comment, and label are applied.
// Gate returns (true, false) — still blocked.
func TestCheckReviewGate_BotPhase1_Reprompts(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	// awaiting-review label was applied 10 minutes ago — 1× timeout elapsed.
	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked after Phase 1 re-prompt")
	}
	if timedOut {
		t.Error("expected not timedOut after Phase 1 (still blocked)")
	}

	// DeleteReviewRequest + AddReviewRequest should each have been called once.
	if len(client.deleteReviewRequestCalls) != 1 {
		t.Errorf("expected 1 DeleteReviewRequest call, got %d", len(client.deleteReviewRequestCalls))
	}
	if len(client.addReviewRequestCalls) != 1 {
		t.Errorf("expected 1 AddReviewRequest call, got %d", len(client.addReviewRequestCalls))
	}

	// @mention comment should have been posted on the PR.
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 PR @mention comment, got %d", len(client.addCommentCalls))
	}
	if client.addCommentCalls[0].issueNumber != 42 {
		t.Errorf("expected comment on PR #42, got #%d", client.addCommentCalls[0].issueNumber)
	}
	// Copilot login must be mentioned as @copilot, not @copilot-pull-request-reviewer.
	if !strings.Contains(client.addCommentCalls[0].body, "@copilot") {
		t.Errorf("expected @copilot in reprompt comment body, got: %q", client.addCommentCalls[0].body)
	}
	if strings.Contains(client.addCommentCalls[0].body, "@copilot-pull-request-reviewer") {
		t.Errorf("reprompt comment must not contain @copilot-pull-request-reviewer, got: %q", client.addCommentCalls[0].body)
	}

	// fabrik:bot-reprompted label should have been applied once (not per-login).
	var foundReprompted bool
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:bot-reprompted" {
			foundReprompted = true
		}
	}
	if !foundReprompted {
		t.Error("expected fabrik:bot-reprompted label to be added")
	}
}

// Phase 1 idempotency: if fabrik:bot-reprompted already present, Phase 1 does not re-fire.
// When the reprompted label is present but not yet timed out, the gate stays blocked silently.
func TestCheckReviewGate_BotPhase1_Idempotent_StillBlocked(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	repromptedApplied := time.Now().Add(-2 * time.Minute) // 2 min ago — not yet timed out for Phase 2
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		if labelName == "fabrik:bot-reprompted" {
			return repromptedApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review", "fabrik:bot-reprompted"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked between Phase 1 and Phase 2")
	}
	if timedOut {
		t.Error("expected not timedOut between Phase 1 and Phase 2")
	}
	// No new review requests or comments — Phase 1 does not re-fire.
	if len(client.deleteReviewRequestCalls) != 0 {
		t.Errorf("expected no DeleteReviewRequest (Phase 1 idempotency), got %d", len(client.deleteReviewRequestCalls))
	}
	if len(client.addReviewRequestCalls) != 0 {
		t.Errorf("expected no AddReviewRequest (Phase 1 idempotency), got %d", len(client.addReviewRequestCalls))
	}
}

// Phase 2: bot-reprompted label timed out — gate fires (false, true) and cleans up labels.
// pauseForReviewTimeout then detects Phase 2 context and posts a contextual message.
func TestCheckReviewGate_BotPhase2_PausesForHuman(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-15 * time.Minute)
	repromptedApplied := time.Now().Add(-10 * time.Minute) // 10 min > 5 min timeout → Phase 2
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		if labelName == "fabrik:bot-reprompted" {
			return repromptedApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review", "fabrik:bot-reprompted"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked on Phase 2 (should return false, true)")
	}
	if !timedOut {
		t.Error("expected timedOut == true on Phase 2")
	}

	// fabrik:bot-reprompted and fabrik:awaiting-review should both be removed.
	removedLabels := make(map[string]bool)
	for _, call := range client.removeLabelCalls {
		removedLabels[call.labelName] = true
	}
	if !removedLabels["fabrik:bot-reprompted"] {
		t.Error("expected fabrik:bot-reprompted to be removed in Phase 2")
	}
	if !removedLabels["fabrik:awaiting-review"] {
		t.Error("expected fabrik:awaiting-review to be removed in Phase 2")
	}

	// Verify pauseForReviewTimeout posts a Phase 2 contextual message.
	eng2 := reviewTestEngine(t, client)
	eng2.pauseForReviewTimeout(board, item, stage) // item still has pre-cleanup labels

	var foundPhase2Comment bool
	for _, call := range client.addCommentCalls {
		if len(call.body) > 0 && containsAll(call.body, "after bot re-prompt", "copilot-pull-request-reviewer") {
			foundPhase2Comment = true
		}
	}
	if !foundPhase2Comment {
		t.Error("expected pauseForReviewTimeout to post a Phase 2 contextual message mentioning the bot and re-prompt")
	}
}

// Pure-bot stuck then bot responds before Phase 2 — gate clears naturally.
func TestCheckReviewGate_BotRespondsBeforePhase2_Clears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		// Bot submitted a review and no longer in reviewRequests.
		LinkedPRReviewRequests: nil,
		LinkedPRReviews: []gh.PRReview{
			{Author: "copilot-pull-request-reviewer", State: "COMMENTED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked when bot submitted a review")
	}
	if timedOut {
		t.Error("expected not timedOut when bot submitted a review")
	}
	// Gate clears naturally — no re-prompt calls.
	if len(client.deleteReviewRequestCalls) != 0 {
		t.Errorf("expected no DeleteReviewRequest when gate clears, got %d", len(client.deleteReviewRequestCalls))
	}
}

// Mixed bot+human outstanding at 1× timeout — existing pause path fires, no re-prompt.
func TestCheckReviewGate_MixedBotHuman_PausesWithoutReprompt(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute) // 1× elapsed
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
			{Login: "alice", IsBot: false}, // human reviewer present
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked (timeout elapsed → timedOut)")
	}
	if !timedOut {
		t.Error("expected timedOut == true when mixed outstanding at 1× timeout")
	}
	// Phase 1 must NOT have fired for mixed outstanding.
	if len(client.deleteReviewRequestCalls) != 0 {
		t.Errorf("expected no DeleteReviewRequest for mixed outstanding, got %d", len(client.deleteReviewRequestCalls))
	}
	if len(client.addReviewRequestCalls) != 0 {
		t.Errorf("expected no AddReviewRequest for mixed outstanding, got %d", len(client.addReviewRequestCalls))
	}
	// No bot-reprompted label should have been applied.
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:bot-reprompted" {
			t.Errorf("unexpected bot-reprompted label added for mixed outstanding: %q", call.labelName)
		}
	}
}

// Pure-human outstanding at 1× timeout — existing pause path fires.
func TestCheckReviewGate_PureHuman_PausesWithoutReprompt(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "alice", IsBot: false},
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked (timeout elapsed → timedOut)")
	}
	if !timedOut {
		t.Error("expected timedOut == true for pure-human at 1× timeout")
	}
	if len(client.deleteReviewRequestCalls) != 0 {
		t.Errorf("expected no DeleteReviewRequest for pure-human, got %d", len(client.deleteReviewRequestCalls))
	}
}

// pauseForReviewTimeout enhanced comment lists reviewers with bot/human tags.
func TestPauseForReviewTimeout_ListsReviewerTypes(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
			{Login: "alice", IsBot: false},
		},
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}

	eng.pauseForReviewTimeout(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !containsAll(body, "copilot-pull-request-reviewer", "bot", "alice", "human") {
		t.Errorf("pause comment should list reviewers with bot/human tags; got:\n%s", body)
	}
}

// removeAwaitingReviewLabel also removes the fabrik:bot-reprompted label.
func TestRemoveAwaitingReviewLabel_CleansRepromptedLabels(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review", "fabrik:bot-reprompted"},
	}

	eng.removeAwaitingReviewLabel("owner", "repo", item)

	removedLabels := make(map[string]bool)
	for _, call := range client.removeLabelCalls {
		removedLabels[call.labelName] = true
	}
	if !removedLabels["fabrik:awaiting-review"] {
		t.Error("expected fabrik:awaiting-review to be removed")
	}
	if !removedLabels["fabrik:bot-reprompted"] {
		t.Error("expected fabrik:bot-reprompted to be removed")
	}
}

// TestRemoveAwaitingReviewLabel_ErrNotFound verifies fix #4 (issue #1024): a 404
// from RemoveLabelFromIssue must be treated as success (label already absent on
// GitHub) and the cache synced accordingly — mirroring removeAwaitingCILabel's
// ErrNotFound branch exactly. Before the fix, an ErrNotFound just logged a
// warning and left the cache believing the label was still present.
//
// The item is constructed with an empty Repo (relying on the defaultRepo()
// fallback to "owner/repo") so the item.Repo-vs-resolved-owner/repo cache key
// mismatch is also observable here — a test using an explicit non-empty
// item.Repo would not catch that bug class.
func TestRemoveAwaitingReviewLabel_ErrNotFound(t *testing.T) {
	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:awaiting-review" {
				calls++
				return fmt.Errorf("GitHub API returned 404: label not found: %w", gh.ErrNotFound)
			}
			return nil
		},
	}
	eng := reviewTestEngine(t, client)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	cache.BootstrapFromProbe([]gh.BoardProbeItem{
		{ContentID: "I_010", ItemID: "PVTI_010", Number: 10, Repo: "owner/repo", Status: "Review"},
	}, "PVT_1")
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 10), "fabrik:awaiting-review")
	eng.readClient = cache

	item := gh.ProjectItem{
		Number: 10,
		Labels: []string{"fabrik:awaiting-review"},
	}

	eng.removeAwaitingReviewLabel("owner", "repo", item)

	if calls != 1 {
		t.Errorf("expected exactly 1 RemoveLabelFromIssue call for ErrNotFound, got %d", calls)
	}

	labels, err := cache.FetchLabels("owner", "repo", 10)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	for _, l := range labels {
		if l == "fabrik:awaiting-review" {
			t.Error("expected fabrik:awaiting-review to be removed from cache after ErrNotFound — cache is stuck believing the label is still present")
		}
	}
}

// TestBotRepromptedLabelLength guards against the botRepromptedLabel constant
// exceeding GitHub's 50-character REST API limit for label names.
func TestBotRepromptedLabelLength(t *testing.T) {
	if len(botRepromptedLabel) > 50 {
		t.Errorf("botRepromptedLabel is %d chars (max 50): %q", len(botRepromptedLabel), botRepromptedLabel)
	}
}

// containsAll checks that all substrings appear in s (case-sensitive).
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// boolPtr is a helper to create a *bool from a bool literal.
func boolPtr(b bool) *bool {
	return &b
}

func TestBotMentionHandle(t *testing.T) {
	cases := []struct {
		login string
		want  string
	}{
		{"copilot-pull-request-reviewer", "copilot"},
		{"copilot", "copilot"},
		{"Copilot-pull-request-reviewer", "copilot"},
		{"dependabot[bot]", "dependabot[bot]"},
		{"someuser", "someuser"},
	}
	for _, tc := range cases {
		got := botMentionHandle(tc.login)
		if got != tc.want {
			t.Errorf("botMentionHandle(%q) = %q, want %q", tc.login, got, tc.want)
		}
	}
}

// Phase 1: non-Copilot bot reviewer — reprompt comment must mention @<login> directly.
func TestCheckReviewGate_BotPhase1_NonCopilot_MentionsLogin(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "dependabot[bot]", IsBot: true},
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	eng.checkReviewGate(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 PR @mention comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "@dependabot[bot]") {
		t.Errorf("expected @dependabot[bot] in reprompt comment body, got: %q", body)
	}
}

// Broken-linkage guard: PR exists on branch but LinkedPRNumber == 0 → pause instead of loop.
func TestCheckReviewGate_BrokenLinkage_PRFound_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 0, // no linkage via closingIssuesReferences
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	// Gate returns (false, false) — not blocked-for-reviews, not timed out.
	if blocked {
		t.Error("gate should not report blocked when broken linkage detected")
	}
	if timedOut {
		t.Error("gate should not report timedOut for broken linkage")
	}

	// Issue MUST be paused.
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Error("issue should have fabrik:paused label applied")
	}

	// fabrik:awaiting-review must NOT be applied.
	for _, l := range labels {
		if l.labelName == "fabrik:awaiting-review" {
			t.Error("fabrik:awaiting-review must not be applied for broken linkage")
		}
	}

	// Comment should mention the closing keyword and PR number.
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected a broken-linkage comment to be posted")
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "Closes #10") {
		t.Errorf("comment should contain recovery closing keyword, got: %q", body)
	}
}

// Broken-linkage guard: LinkedPRNumber == 0 and no PR found on branch → falls through to normal logic.
func TestCheckReviewGate_BrokenLinkage_NoPRFound_FallsThrough(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no PR on branch
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, _ := eng.checkReviewGate(board, item, stage)

	// No PR found → falls through to normal reviewer logic. With no reviews, gate blocks.
	if !blocked {
		t.Error("gate should still block when no PR is found (normal logic applies)")
	}

	// Issue must NOT be paused for broken linkage.
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause when no PR exists on branch")
		}
	}
}

// Broken-linkage guard on a base:<branch> repo: closedByPullRequestsReferences is
// structurally empty there, so LinkedPRNumber == 0 alone doesn't mean linkage is
// broken. When the PR body already contains the closing keyword (confirmed via
// FetchPRClosingIssues), the gate must fall through to normal reviewer logic instead
// of pausing. See #1046/#1047.
func TestCheckReviewGate_BrokenLinkage_NonDefaultBase_ConfirmedViaBody_ClearsNormally(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil // PR body already contains "Closes #10"
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	// Falls through to normal reviewer logic: no reviewers requested, no reviews yet → blocks.
	if !blocked {
		t.Error("gate should fall through to normal blocking logic once linkage is confirmed via body")
	}
	if timedOut {
		t.Error("gate should not report timedOut")
	}

	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause when linkage is confirmed via PR body on a base:<branch> repo")
		}
	}
}

// Broken-linkage guard on a base:<branch> repo: PR exists but its body still lacks the
// closing keyword — this is genuinely broken linkage (base-independent), so the gate
// must pause exactly as it does on the default-branch path.
func TestCheckReviewGate_BrokenLinkage_NonDefaultBase_BodyMissing_StillPauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return nil, nil // no closing keyword found in the PR body
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("gate should not report blocked when broken linkage detected")
	}
	if timedOut {
		t.Error("gate should not report timedOut for broken linkage")
	}

	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			hasPaused = true
		}
		if l.labelName == "fabrik:awaiting-review" {
			t.Error("fabrik:awaiting-review must not be applied for broken linkage")
		}
	}
	if !hasPaused {
		t.Error("issue should have fabrik:paused label applied")
	}

	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected a broken-linkage comment to be posted")
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "Closes #10") {
		t.Errorf("comment should contain recovery closing keyword, got: %q", body)
	}
}

// On a base:<branch> repo, closedByPullRequestsReferences (and everything nested
// inside it — reviewRequests, latestReviews) is structurally empty, so
// item.LinkedPRReviewRequests/LinkedPRReviews are always empty regardless of the
// PR's actual review state. checkReviewGate must fetch reviews/requests via REST,
// keyed on the PR number handleBrokenReviewLinkage resolves, and clear the gate
// naturally once a review is in and no requests remain outstanding. See #1046/#1047/#1050.
func TestCheckReviewGate_NonDefaultBase_RESTReviewSubmitted_ClearsNaturally(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil // PR body already contains "Closes #10"
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			if prNumber != 77 {
				t.Errorf("expected FetchPRReviews to be called with resolved PR #77, got #%d", prNumber)
			}
			return []gh.PRReview{{Author: "reviewer1", State: "APPROVED"}}, nil
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			if prNumber != 77 {
				t.Errorf("expected FetchPRReviewRequests to be called with resolved PR #77, got #%d", prNumber)
			}
			return nil, nil // no outstanding requests
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected gate to clear naturally once a REST-sourced review is submitted with no outstanding requests")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}

	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause when the REST-sourced review data clears the gate")
		}
		if l.labelName == "fabrik:awaiting-review" {
			t.Error("fabrik:awaiting-review should not be applied when the gate clears immediately")
		}
	}
}

// Outstanding-reviewer detection must also work on base:<branch> repos: a requested
// reviewer who hasn't submitted keeps the gate blocked, mirroring default-branch behavior.
func TestCheckReviewGate_NonDefaultBase_RESTOutstandingReviewer_Blocks(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, nil // no reviews submitted yet
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return []gh.ReviewRequest{{Login: "alice", IsBot: false}}, nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to stay blocked while a REST-sourced outstanding reviewer hasn't submitted")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}

	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	if len(labels) != 1 || labels[0].labelName != "fabrik:awaiting-review" {
		t.Errorf("expected exactly 1 fabrik:awaiting-review label add, got %v", labels)
	}
}

// A transient REST error on either the reviews or requested-reviewers endpoint must
// not falsely clear the gate — treat the poll as no-data-available and stay blocked,
// retrying on the next poll.
func TestCheckReviewGate_NonDefaultBase_RESTFetchError_StaysBlocked(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, fmt.Errorf("transient GitHub API error")
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			// Succeeds with no outstanding requests — but the reviews call failed, so
			// the gate must NOT trust this partial success and false-clear.
			return nil, nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to stay blocked on a partial REST fetch failure (conservative no-data fallback)")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
}

// Default-branch items must never invoke the REST review-fetch helpers — the gate
// continues to use the GraphQL-sourced item.LinkedPRReviewRequests/LinkedPRReviews
// exclusively, exactly as before this change.
func TestCheckReviewGate_DefaultBranch_DoesNotCallRESTReviewFetch(t *testing.T) {
	var restReviewCalls, restRequestCalls int
	client := &mockGitHubClient{
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			restReviewCalls++
			return nil, fmt.Errorf("should not be called on default-branch items")
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			restRequestCalls++
			return nil, fmt.Errorf("should not be called on default-branch items")
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         77, // linkage already resolved via GraphQL
		LinkedPRReviewRequests: nil,
		LinkedPRReviews: []gh.PRReview{
			{Author: "reviewer1", State: "APPROVED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected gate to clear using the existing GraphQL-sourced data")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}
	if restReviewCalls != 0 || restRequestCalls != 0 {
		t.Errorf("expected no REST review-fetch calls on a default-branch item, got reviews=%d requests=%d", restReviewCalls, restRequestCalls)
	}
}
