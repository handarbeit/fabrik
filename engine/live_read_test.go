package engine

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Unit tests for the live-read helper family (#2350): a live read is applied to the
// Store, the next decision sees it with no second GitHub call (SC-002), and a read
// that failed applies nothing (FR-010).

func liveReadEngine(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	eng := testEngineForMerge(t, client)
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Repo: "owner/repo", Number: 1, Status: "Validate", Labels: []string{"stage:Validate:complete"},
		LinkedPRNumber: 10,
	}})
	eng.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: 1, PRNumber: 10, Title: "T", State: "open", Draft: true})
	return eng
}

func TestLiveLabels_AppliesTheReadAndReturnsTheRawRead(t *testing.T) {
	var calls atomic.Int32
	client := &mockGitHubClient{fetchLabelsFn: func(string, string, int) ([]string, error) {
		calls.Add(1)
		return []string{"stage:Validate:complete", "fabrik:paused", "fabrik:awaiting-input"}, nil
	}}
	eng := liveReadEngine(t, client)

	got, err := eng.liveLabels("owner", "repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLabel(got, "fabrik:paused") {
		t.Fatalf("labels = %v", got)
	}
	// The next decision reads the Store and needs no second call.
	snap, _ := eng.store.Peek("owner/repo", 1)
	if !hasLabel(snap.Labels(), "fabrik:paused") {
		t.Fatalf("the live read was not applied to the Store: %v", snap.Labels())
	}
	if calls.Load() != 1 {
		t.Fatalf("FetchLabels called %d times, want 1", calls.Load())
	}
}

// The guard's regression: the engine removed fabrik:paused (an unpause) and a human
// re-paused on GitHub inside the engine-write grace. The Store keeps the engine's
// removal, but the live read returned to a fail-closed guard must still show the
// pause, so refuseIfPausedLive refuses.
func TestLiveLabels_ReturnsGitHubsAnswerNotTheEngineProtectedView(t *testing.T) {
	client := &mockGitHubClient{fetchLabelsFn: func(string, string, int) ([]string, error) {
		return []string{"stage:Validate:complete", "fabrik:paused", "fabrik:awaiting-input"}, nil
	}}
	eng := liveReadEngine(t, client)
	eng.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:paused"})
	eng.store.Apply(itemstate.LocalLabelRemoved{Repo: "owner/repo", Number: 1, Label: "fabrik:paused"})

	got, err := eng.liveLabels("owner", "repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLabel(got, "fabrik:paused") {
		t.Fatalf("the guard must see GitHub's pause, got %v", got)
	}
	if !eng.refuseIfPausedLive(gh.ProjectItem{Repo: "owner/repo", Number: 1}, "test") {
		t.Fatal("refuseIfPausedLive must refuse when the live read shows fabrik:paused")
	}
}

func TestLiveLabels_AFailedReadIsNeverApplied(t *testing.T) {
	client := &mockGitHubClient{fetchLabelsFn: func(string, string, int) ([]string, error) { return nil, errors.New("boom") }}
	eng := liveReadEngine(t, client)
	if _, err := eng.liveLabels("owner", "repo", 1); err == nil {
		t.Fatal("expected the read error")
	}
	snap, _ := eng.store.Peek("owner/repo", 1)
	if !hasLabel(snap.Labels(), "stage:Validate:complete") {
		t.Fatalf("a failed read wiped the Store labels: %v", snap.Labels())
	}
}

func TestLiveLabels_DisabledApplyReproducesReadAndDiscard(t *testing.T) {
	client := &mockGitHubClient{fetchLabelsFn: func(string, string, int) ([]string, error) {
		return []string{"fabrik:paused"}, nil
	}}
	eng := liveReadEngine(t, client)
	eng.SetLiveReadApplyDisabledForTest(true)
	if _, err := eng.liveLabels("owner", "repo", 1); err != nil {
		t.Fatal(err)
	}
	snap, _ := eng.store.Peek("owner/repo", 1)
	if hasLabel(snap.Labels(), "fabrik:paused") {
		t.Fatal("the seam did not disable the apply")
	}
}

func TestLivePRMerged_AppliesAConfirmedMergeAtOnce(t *testing.T) {
	var calls atomic.Int32
	client := &mockGitHubClient{fetchPRMergedFn: func(string, string, int) (bool, error) {
		calls.Add(1)
		return true, nil
	}}
	eng := liveReadEngine(t, client)

	merged, err := eng.livePRMerged("owner", "repo", 1, 10)
	if err != nil || !merged {
		t.Fatalf("merged=%v err=%v", merged, err)
	}
	snap, _ := eng.store.Peek("owner/repo", 1)
	lpr := snap.LinkedPR()
	if lpr == nil || !lpr.Merged || lpr.State != "closed" {
		t.Fatalf("the merged state was not applied: %+v", lpr)
	}
	if lpr.Title != "T" || !lpr.Draft {
		t.Fatalf("Title/Draft were blanked by the merged-state apply: %+v", lpr)
	}
	if calls.Load() != 1 {
		t.Fatalf("FetchPRMerged called %d times", calls.Load())
	}
}

func TestLivePRMerged_NotMergedAndOtherPRsApplyNothing(t *testing.T) {
	merged := false
	client := &mockGitHubClient{fetchPRMergedFn: func(string, string, int) (bool, error) { return merged, nil }}
	eng := liveReadEngine(t, client)
	if _, err := eng.livePRMerged("owner", "repo", 1, 10); err != nil {
		t.Fatal(err)
	}
	if snap, _ := eng.store.Peek("owner/repo", 1); snap.LinkedPR().Merged {
		t.Fatal("a not-merged read must never be applied")
	}
	// A merged PR that is not the item's linked PR (a merge-train integration PR)
	// is reported but not written onto the item.
	merged = true
	if got, _ := eng.livePRMerged("owner", "repo", 1, 99); !got {
		t.Fatal("the merged answer was not returned")
	}
	if snap, _ := eng.store.Peek("owner/repo", 1); snap.LinkedPR().Merged {
		t.Fatal("a merge of a different PR was written onto the linked PR")
	}
}

func TestLiveReviews_AppliesBothHalvesAndAPartialReadAppliesNothing(t *testing.T) {
	failRequests := false
	client := &mockGitHubClient{
		fetchPRReviewsFn: func(string, string, int) ([]gh.PRReview, error) {
			return []gh.PRReview{{Author: "bob", State: "APPROVED", DatabaseID: 1}}, nil
		},
		fetchPRReviewRequestsFn: func(string, string, int) ([]gh.ReviewRequest, error) {
			if failRequests {
				return nil, errors.New("boom")
			}
			return []gh.ReviewRequest{{Login: "carol"}}, nil
		},
	}
	eng := liveReadEngine(t, client)

	failRequests = true
	if _, _, err := eng.liveReviews("owner", "repo", 1, 10); err == nil {
		t.Fatal("expected the partial-read error")
	}
	if snap, _ := eng.store.Peek("owner/repo", 1); len(snap.LinkedPR().Reviews) != 0 {
		t.Fatal("a partial read was applied")
	}

	failRequests = false
	eng.SetLiveFreshForTest(time.Minute)
	reviews, requests, err := eng.liveReviews("owner", "repo", 1, 10)
	if err != nil || len(reviews) != 1 || len(requests) != 1 {
		t.Fatalf("reviews=%v requests=%v err=%v", reviews, requests, err)
	}
	snap, _ := eng.store.Peek("owner/repo", 1)
	if lpr := snap.LinkedPR(); len(lpr.Reviews) != 1 || len(lpr.ReviewRequests) != 1 || lpr.ReviewRequests[0].Login != "carol" {
		t.Fatalf("the live read was not applied: %+v", lpr)
	}
	if !eng.liveReviewsFresh("owner/repo", 1) {
		t.Fatal("a just-made live read must count as fresh")
	}
}

// FR-005 / SC-002: an issue comment seen only by the feedback gate's live read is
// counted as feedback and applied to the Store, so the next decision sees it
// without another read. With the apply disabled (the pre-#2350 read-and-discard)
// the Store stays without it.
func TestFeedbackGate_LiveReadAppliesAnIssueCommentToTheStore(t *testing.T) {
	run := func(applyOff bool) (held bool, stored int) {
		client := landingClient()
		client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
			item.Comments = []gh.Comment{humanComment}
			return nil
		}
		eng := testEngineForMerge(t, client)
		eng.SetLiveReadApplyDisabledForTest(applyOff)
		eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Repo: "owner/repo", Number: 1, Status: "Validate"}})
		item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo"}
		held = eng.feedbackGateBlocks(item, false, "advance")
		snap, _ := eng.store.Peek("owner/repo", 1)
		return held, len(snap.State().Comments)
	}
	if held, stored := run(false); !held || stored != 1 {
		t.Fatalf("held=%v stored=%d, want the comment held on and applied to the Store", held, stored)
	}
	if held, stored := run(true); !held || stored != 0 {
		t.Fatalf("non-vacuity: held=%v stored=%d, want held but not applied", held, stored)
	}
}

// liveFreshFor is one poll interval floored at 30 s (and zero with no interval
// configured, so every call confirms live) — not the ten-interval API-cost knob.
func TestLiveFreshFor_IsOnePollIntervalWithAFloor(t *testing.T) {
	eng := liveReadEngine(t, &mockGitHubClient{})
	for _, c := range []struct {
		poll int
		want time.Duration
	}{{0, 0}, {10, liveFreshFloor}, {45, 45 * time.Second}} {
		eng.cfg.PollSeconds = c.poll
		if got := eng.liveFreshFor(); got != c.want {
			t.Errorf("PollSeconds=%d: liveFreshFor=%v, want %v", c.poll, got, c.want)
		}
	}
}
