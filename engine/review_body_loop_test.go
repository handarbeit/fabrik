package engine

import (
	"context"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// This file confirms issue #1205's loop-safety requirement: a bot's PR review
// body — now worker-triggering input, the same surface that produced #1083 —
// cannot produce an unbounded reinvocation cycle. Review-body comments flow
// through the same processComments/dispatchReviewReinvoke machinery as
// existing sources, so the pre-existing protections (#1089 circuit breaker,
// MaxReviewCycles) apply without new wiring; these tests exercise that claim
// specifically for a review-body-sourced batch, since nothing previously did.

// TestCommentBreaker_TripsAfterThreshold_ReviewBodySourced mirrors
// TestCommentBreaker_TripsAfterThreshold (engine/comment_breaker_test.go) but
// sources the non-advancing comment batch from a PR review body (ReviewID
// set) instead of a plain issue comment — confirming the #1089 circuit
// breaker is source-agnostic and trips on a stuck review-body-driven cycle
// exactly as it does for a stuck conversation-comment cycle.
func TestCommentBreaker_TripsAfterThreshold_ReviewBodySourced(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := nonAdvancingClaude()
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxCommentCyclesPerWindow = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 31, Body: "spec"}
	comments := []gh.Comment{
		{
			ID: "PRR_1", ReviewID: "PRR_1", ReviewState: "COMMENTED",
			Author: "some-review-bot", Body: "Additional feedback — could not anchor to diff: still stuck?",
		},
	}

	for i := 0; i < eng.cfg.MaxCommentCyclesPerWindow; i++ {
		if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
			t.Fatalf("processComments call %d: %v", i, err)
		}
	}

	var paused, awaitingInput bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			paused = true
		}
		if c.labelName == "fabrik:awaiting-input" {
			awaitingInput = true
		}
	}
	if !paused || !awaitingInput {
		t.Fatalf("expected fabrik:paused + fabrik:awaiting-input after trip; addLabelCalls=%v", client.addLabelCalls)
	}

	var tripComments int
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "circuit breaker") {
			tripComments++
		}
	}
	if tripComments != 1 {
		t.Fatalf("expected exactly 1 trip comment, got %d", tripComments)
	}
}

// makeReviewBodyGatePctx mirrors makeReviewGatePctx (catch_up_handlers_test.go)
// but sources the outstanding feedback from item.LinkedPRReviews (a review
// body) rather than LinkedPRReviewThreadComments — proving handleReviewGate's
// combined thread+body emptiness check (#1205) dispatches from a body-only
// source, and that the dispatch is still bounded by MaxReviewCycles.
func makeReviewBodyGatePctx(board *gh.ProjectBoard, advancedItems map[string]bool) *phase1Ctx {
	item := gh.ProjectItem{
		Number: 11,
		Repo:   "owner/repo",
		Labels: []string{"stage:Implement:complete"},
		LinkedPRReviews: []gh.PRReview{
			{
				ID: "PRR_gate_1", DatabaseID: 200, Author: "some-review-bot", State: "COMMENTED",
				Body: "Additional feedback — could not anchor to diff: please fix this.",
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

// TestHandleReviewGate_ReviewBodyOnly_Dispatches confirms a review carrying
// only a body (no inline comments) is enough to trigger a review reinvoke —
// the gap issue #1205 exists to close (previously such a review produced zero
// dispatchReviewReinvoke cycles and its finding was silently dropped).
func TestHandleReviewGate_ReviewBodyOnly_Dispatches(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
		addReviewReactionFn:  func(_, _ string) error { return nil },
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	advancedItems := make(map[string]bool)
	pctx := makeReviewBodyGatePctx(board, advancedItems)

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed) for a review-body-only source, got false")
	}
	if !advancedItems["owner/repo#11"] {
		t.Error("advancedItems[owner/repo#11] must be set on successful body-only dispatch")
	}
	snap, _ := eng.store.Get("owner/repo", 11)
	if snap.ReviewCycles("Implement") != 1 {
		t.Errorf("ReviewCycles(Implement) = %d; want 1", snap.ReviewCycles("Implement"))
	}
	eng.wg.Wait()
}

// TestHandleReviewGate_CycleLimit_Pauses_ReviewBodyOnly mirrors
// TestHandleReviewGate_CycleLimit_Pauses but sourced purely from a review
// body — confirming MaxReviewCycles bounds repeated dispatchReviewReinvoke
// calls even when nothing ever produces an inline thread comment, i.e. a
// non-progressing bot review body cannot drive an unbounded reinvocation
// cycle.
func TestHandleReviewGate_CycleLimit_Pauses_ReviewBodyOnly(t *testing.T) {
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
	pctx := makeReviewBodyGatePctx(board, advancedItems)

	// Pre-fill ReviewCycles to the limit, as if MaxReviewCycles worth of
	// body-only reinvokes had already run.
	for i := 0; i < eng.cfg.MaxReviewCycles; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{
			Repo: "owner/repo", Number: 11, StageName: "Implement",
		})
	}

	got := eng.handleReviewGate(pctx)

	if !got {
		t.Error("handleReviewGate: expected true (item claimed), got false")
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
		t.Errorf("expected fabrik:paused to be added on cycle limit for a body-only source; labels added: %v", labelNames)
	}
	if advancedItems["owner/repo#11"] {
		t.Error("advancedItems must not be set on cycle limit pause")
	}
}
