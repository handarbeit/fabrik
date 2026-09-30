package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

var (
	// The #616 shape: a body-only COMMENTED review from a self-submitting bot.
	bodyOnlyReview = gh.PRReview{
		Author: "handarbeit-pruefer", State: "COMMENTED", Body: "lookup_key masking in resolve_entities_by_name_in_kinds",
		DatabaseID: 5356334497, NodeID: "PRR_kwDOx",
	}
	openThread = gh.Comment{ID: "PRRC_t", DatabaseID: 31, ReviewThreadID: "RT_1", Author: "copilot", Body: "fix this"}
)

func feedbackItem(mods ...func(*gh.ProjectItem)) gh.ProjectItem {
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo"}
	for _, m := range mods {
		m(&item)
	}
	return item
}

// Each source of unprocessed feedback holds the gate on its own (R2), and a
// clean item does not.
func TestFeedbackGate_EachSourceHolds(t *testing.T) {
	cases := map[string]struct {
		mod  func(*gh.ProjectItem)
		want bool
	}{
		"clean item":    {func(*gh.ProjectItem) {}, false},
		"issue comment": {func(i *gh.ProjectItem) { i.Comments = []gh.Comment{humanComment} }, true},
		"review thread": {func(i *gh.ProjectItem) { i.LinkedPRReviewThreadComments = []gh.Comment{openThread} }, true},
		"review body":   {func(i *gh.ProjectItem) { i.LinkedPRReviews = []gh.PRReview{bodyOnlyReview} }, true},
		"approved no body": {func(i *gh.ProjectItem) {
			i.LinkedPRReviews = []gh.PRReview{{Author: "a", State: "APPROVED", DatabaseID: 9}}
		}, false},
		"dismissed body": {func(i *gh.ProjectItem) {
			r := bodyOnlyReview
			r.State = "DISMISSED"
			i.LinkedPRReviews = []gh.PRReview{r}
		}, false},
		"outdated thread never holds (#1207)": {func(i *gh.ProjectItem) {
			th := openThread
			th.IsOutdated = true
			i.LinkedPRReviewThreadComments = []gh.Comment{th}
		}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			eng := testEngineForMerge(t, landingClient())
			if got := eng.feedbackGateBlocks(feedbackItem(tc.mod), true, "advance"); got != tc.want {
				t.Errorf("feedbackGateBlocks = %v, want %v", got, tc.want)
			}
		})
	}
}

// #1555: a review body already recorded as processed (store watermark) — or in
// the durable review-ids-addressed marker — never holds, so an addressed review
// cannot hold an advance forever.
func TestFeedbackGate_AddressedReviewBodyDoesNotHold(t *testing.T) {
	eng := testEngineForMerge(t, landingClient())
	eng.store.Apply(itemstate.CommentProcessed{Repo: "owner/repo", Number: 1, CommentID: "review-body:5356334497", At: time.Now()})
	item := feedbackItem(func(i *gh.ProjectItem) { i.LinkedPRReviews = []gh.PRReview{bodyOnlyReview} })
	if eng.feedbackGateBlocks(item, true, "advance") {
		t.Error("an already-addressed review body must not hold the gate")
	}
}

// R5: a failed live read fails closed and records the re-evaluation cooldown.
func TestFeedbackGate_LiveReadFailureHolds(t *testing.T) {
	client := landingClient()
	client.fetchItemDetailsFn = func(*gh.ProjectItem) error { return errors.New("rate limited") }
	eng := testEngineForMerge(t, client)
	if !eng.feedbackGateBlocks(feedbackItem(), false, "advance") {
		t.Fatal("a failed live re-read must hold")
	}
	snap, _ := eng.store.Get("owner/repo", 1)
	if snap.CooldownAt("feedback-pending").IsZero() {
		t.Error("expected a feedback-pending cooldown on hold")
	}
}

// R5, base:<branch>: the review read is a live REST call; its error also holds.
func TestFeedbackGate_BaseBranchReviewReadErrorHolds(t *testing.T) {
	client := landingClient()
	client.fetchLinkedPRFn = func(string, string, int) (*gh.PRDetails, error) {
		return &gh.PRDetails{Number: 10, State: "open"}, nil
	}
	client.fetchPRReviewsFn = func(string, string, int) ([]gh.PRReview, error) { return nil, errors.New("boom") }
	eng := testEngineForMerge(t, client)
	item := feedbackItem(func(i *gh.ProjectItem) { i.Labels = []string{"base:dev"} })
	if !eng.feedbackGateBlocks(item, true, "advance") {
		t.Error("a base:<branch> review read error must hold")
	}
}

// R2: on a base:<branch> item GraphQL reviews are structurally empty, so a body
// only visible through REST must still hold.
func TestFeedbackGate_BaseBranchBodyViaRESTHolds(t *testing.T) {
	client := landingClient()
	client.fetchLinkedPRFn = func(string, string, int) (*gh.PRDetails, error) {
		return &gh.PRDetails{Number: 10, State: "open"}, nil
	}
	client.fetchPRReviewsFn = func(string, string, int) ([]gh.PRReview, error) {
		return []gh.PRReview{bodyOnlyReview}, nil
	}
	eng := testEngineForMerge(t, client)
	item := feedbackItem(func(i *gh.ProjectItem) { i.Labels = []string{"base:dev"} })
	if !eng.feedbackGateBlocks(item, true, "advance") {
		t.Error("a review body visible only via REST must hold")
	}
}

// The predicate reads live when the caller's item is a snapshot: the body that
// arrived during the run is visible only in the FetchItemDetails result.
func TestFeedbackGate_ReadsLiveWhenNotAlreadyLive(t *testing.T) {
	client := landingClient()
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		item.LinkedPRReviews = []gh.PRReview{bodyOnlyReview}
		return nil
	}
	eng := testEngineForMerge(t, client)
	if !eng.feedbackGateBlocks(feedbackItem(), false, "advance") {
		t.Error("feedback present only in the live read must hold")
	}
}

// R6: the hold log names the feedback IDs, review bodies without their prefix.
func TestDescribePendingFeedback_LogShape(t *testing.T) {
	got := describePendingFeedback(pendingFeedback{
		Comments: []gh.Comment{{ID: "IC_1", DatabaseID: 11}, {ID: "IC_2", DatabaseID: 12}},
		Bodies:   []gh.Comment{{ID: "review-body:5356334497"}},
	})
	want := "2 unprocessed comment(s) (11, 12), 1 unprocessed review body (5356334497)"
	if got != want {
		t.Errorf("describePendingFeedback = %q, want %q", got, want)
	}
	if strings.Contains(got, reviewBodyIDPrefix) {
		t.Error("review-body: prefix must be stripped from the log IDs")
	}
}

// #616: a body-only COMMENTED review that landed during the run holds the
// Validate landing decision, where before the gate consulted reviewers and
// authority only and a COMMENTED review cleared it.
func TestAttemptMergeOnValidate_HeldByUnprocessedReviewBody(t *testing.T) {
	client := landingClient()
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		item.LinkedPRReviews = []gh.PRReview{bodyOnlyReview}
		return nil
	}
	eng := testEngineForMerge(t, client)
	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{},
		gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}, validateStage())
	if err != nil || enabled || !deferred {
		t.Fatalf("want held (deferred, no error); got enabled=%v deferred=%v err=%v", enabled, deferred, err)
	}
	assertNoLanding(t, client)
}

// R2/R3: a review body that landed while the stage was running is invisible to
// handleStageComplete's pre-run snapshot but visible to the live read — the
// advance is held, and the stage keeps its completion label.
func TestHandleStageComplete_HoldsAdvanceForMidRunReviewBody(t *testing.T) {
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRReviews = []gh.PRReview{bodyOnlyReview}
			return nil
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	eng.cfg.Yolo = true
	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"},
		gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo"}, &stages.Stage{Name: "Research"})

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("advanced with an unprocessed review body pending: %+v", client.updateStatusCalls)
	}
	if !hasAddLabelCall(client, "stage:Research:complete") {
		t.Error("the completion label must still be recorded — a hold defers the advance, not the completion")
	}
	snap, _ := eng.store.Get("owner/repo", 1)
	if snap.CooldownAt("feedback-pending").IsZero() {
		t.Error("expected a feedback-pending cooldown so the item is re-evaluated")
	}
}

// The gate is inert on a clean item: yolo still advances.
func TestHandleStageComplete_CleanItemStillAdvances(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	eng.cfg.Yolo = true
	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"},
		gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}, &stages.Stage{Name: "Research"})
	if len(client.updateStatusCalls) != 1 {
		t.Errorf("expected one advance, got %d", len(client.updateStatusCalls))
	}
}

// Fail closed on the advance too: a failed live read holds.
func TestHandleStageComplete_LiveReadFailureHoldsAdvance(t *testing.T) {
	client := &mockGitHubClient{fetchItemDetailsFn: func(*gh.ProjectItem) error { return errors.New("boom") }}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	eng.cfg.Yolo = true
	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"},
		gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}, &stages.Stage{Name: "Research"})
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("advanced despite an unreadable feedback state: %+v", client.updateStatusCalls)
	}
}

// The catch-up loop's non-Validate advance consults the same predicate.
func TestRunCatchUpPhase2_HoldsAdvanceForUnprocessedReviewBody(t *testing.T) {
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRReviews = []gh.PRReview{bodyOnlyReview}
			return nil
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	eng.cfg.Yolo = true
	advanced := map[string]bool{}
	eng.runCatchUpPhase2(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"},
		gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Research"}, &stages.Stage{Name: "Research"}, advanced)
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("Phase 2 advanced with an unprocessed review body pending: %+v", client.updateStatusCalls)
	}
	if advanced["owner/repo#1"] {
		t.Error("a held item must not be marked advanced")
	}
}
