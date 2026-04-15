package engine

import (
	"context"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// reviewTestStages returns a two-stage pipeline for review gate tests.
func reviewTestStages() []*stages.Stage {
	waitTrue := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
}

func reviewTestEngine(client *mockGitHubClient) *Engine {
	return testEngineWithStages(client, reviewTestStages())
}

// (a) No requested reviewers → gate returns false, advance proceeds.
func TestCheckReviewGate_NoRequestedReviewers_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRReviewRequests: nil, // no pending reviewers
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked when no requested reviewers")
	}
	if timedOut {
		t.Error("expected not timedOut when no requested reviewers")
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label adds, got %d", len(client.addLabelCalls))
	}
}

// (a2) Gate disabled (nil WaitForReviews) → always returns false.
func TestCheckReviewGate_GateDisabled_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(client)
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
	eng := reviewTestEngine(client)
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
	eng := reviewTestEngine(client)
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
	eng := reviewTestEngine(client)
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
	eng := reviewTestEngine(client)
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
	eng := reviewTestEngine(client)
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

// (f) buildReviewThreadComments returns inline thread comments with real DatabaseIDs.
func TestBuildReviewThreadComments(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(client)
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

// (f2) buildReviewThreadComments skips comments already present in processedSet.
func TestBuildReviewThreadComments_ProcessedSetSkip(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_1", DatabaseID: 101, Author: "copilot", Body: "Already handled.", ReviewThreadID: "RT_1"},
			{ID: "PRRC_2", DatabaseID: 102, Author: "human", Body: "Not yet handled.", ReviewThreadID: "RT_2"},
		},
	}

	// Pre-populate processedSet for comment PRRC_1 (simulates markCommentsProcessed).
	iKey := issueKey(item, eng.defaultRepo())
	eng.mu.Lock()
	eng.processedSet[iKey+"-comment-PRRC_1"] = time.Now()
	eng.mu.Unlock()

	comments := eng.buildReviewThreadComments(item)

	if len(comments) != 1 {
		t.Fatalf("expected 1 comment (PRRC_1 should be skipped), got %d", len(comments))
	}
	if comments[0].ID != "PRRC_2" {
		t.Errorf("expected remaining comment to be PRRC_2, got %q", comments[0].ID)
	}
}

// (f3) catch-up loop skips dispatchReviewReinvoke when a goroutine is already
// in-flight for the item, and does NOT increment reviewCycleCount.
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
	eng := testEngineWithStages(client, stgs)
	eng.cfg.MaxReviewCycles = 5

	// Pre-store inFlight for this item to simulate a goroutine already running.
	iKey := "owner/repo#42"
	eng.inFlight.Store(iKey, false)

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Drain any goroutines (none should have been launched, but be safe).
	eng.wg.Wait()

	// reviewCycleCount must remain 0 — the inFlight guard must prevent the dispatch.
	stageKey := iKey + "-Implement" // item.Status == "Implement"
	eng.mu.Lock()
	count := eng.reviewCycleCount[stageKey]
	eng.mu.Unlock()
	if count != 0 {
		t.Errorf("reviewCycleCount = %d; want 0 (dispatch must be suppressed when in-flight)", count)
	}
}

// (g) pauseForReviewTimeout applies labels and posts a comment.
func TestPauseForReviewTimeout(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(client)
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
	eng := reviewTestEngine(client)
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

// (i1) reviewCycleCount is per-stage: cycles consumed by one stage do not
// reduce the budget for a different stage on the same issue.
func TestReviewCycleCount_PerStageNotPerIssue(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{
		{Name: "Review", Order: 1, Prompt: "review"},
		{Name: "Validate", Order: 2, Prompt: "validate"},
	}
	eng := testEngineWithStages(client, stgs)
	iKey := "owner/repo#10"

	// Simulate Review consuming 3 cycles out of 5.
	eng.mu.Lock()
	eng.reviewCycleCount[iKey+"-Review"] = 3
	eng.mu.Unlock()

	// Validate stage must have an independent budget: its counter is still 0.
	eng.mu.Lock()
	validateCount := eng.reviewCycleCount[iKey+"-Validate"]
	eng.mu.Unlock()

	if validateCount != 0 {
		t.Errorf("Validate reviewCycleCount = %d; want 0 (must be independent of Review cycles)", validateCount)
	}
}

// (i2) clearFailedStage resets only the paused stage's reviewCycleCount; a
// different stage's counter on the same issue is unaffected.
func TestClearFailedStage_ReviewCycleCount_ResetsOnlyCurrentStage(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{
		{Name: "Review", Order: 1, Prompt: "review", WaitForReviews: boolPtr(true)},
		{Name: "Validate", Order: 2, Prompt: "validate", WaitForReviews: boolPtr(true)},
	}
	eng := testEngineWithStages(client, stgs)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"stage:Review:failed", "fabrik:paused"},
	}
	iKey := "owner/repo#10"
	reviewStageKey := iKey + "-Review"
	validateStageKey := iKey + "-Validate"

	// Simulate both stages having consumed cycles (Review hit limit; Validate consumed 2).
	eng.mu.Lock()
	eng.reviewCycleCount[reviewStageKey] = 5
	eng.reviewCycleCount[validateStageKey] = 2
	eng.mu.Unlock()

	// User manually unpauses Review.
	reviewStage := &stages.Stage{Name: "Review", Order: 1}
	eng.clearFailedStage(item, reviewStage)

	// Review's counter must be reset to 0.
	eng.mu.Lock()
	afterReview := eng.reviewCycleCount[reviewStageKey]
	afterValidate := eng.reviewCycleCount[validateStageKey]
	eng.mu.Unlock()

	if afterReview != 0 {
		t.Errorf("Review reviewCycleCount = %d after clearFailedStage; want 0", afterReview)
	}
	// Validate's counter must be untouched — it has an independent budget.
	if afterValidate != 2 {
		t.Errorf("Validate reviewCycleCount = %d after clearing Review; want 2 (independent)", afterValidate)
	}
}

// boolPtr is a helper to create a *bool from a bool literal.
func boolPtr(b bool) *bool {
	return &b
}
