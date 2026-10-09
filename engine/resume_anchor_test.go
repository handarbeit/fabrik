package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Tests for ADR-1813: a paused / awaiting-input issue is resumed only by a
// human comment created at or after the latest fabrik:paused labeled event.

var anchorPausedAt = time.Date(2026, 9, 16, 13, 0, 0, 0, time.UTC)

func TestCommentsPredatePause(t *testing.T) {
	before := gh.Comment{ID: "b", CreatedAt: anchorPausedAt.Add(-time.Hour)}
	equal := gh.Comment{ID: "e", CreatedAt: anchorPausedAt}
	after := gh.Comment{ID: "a", CreatedAt: anchorPausedAt.Add(time.Second)}
	zero := gh.Comment{ID: "z"}

	cases := []struct {
		name     string
		comments []gh.Comment
		anchor   time.Time
		want     bool
	}{
		{"strictly older refuses", []gh.Comment{before}, anchorPausedAt, true},
		{"all older refuses", []gh.Comment{before, before}, anchorPausedAt, true},
		{"newer resumes", []gh.Comment{after}, anchorPausedAt, false},
		{"equal resumes (R6)", []gh.Comment{equal}, anchorPausedAt, false},
		{"zero anchor resumes (R6)", []gh.Comment{before}, time.Time{}, false},
		{"zero CreatedAt resumes (R6)", []gh.Comment{zero}, anchorPausedAt, false},
		{"old plus new resumes (R2)", []gh.Comment{before, after}, anchorPausedAt, false},
		{"old plus zero CreatedAt resumes (R6)", []gh.Comment{before, zero}, anchorPausedAt, false},
		{"no comments never refuses", nil, anchorPausedAt, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commentsPredatePause(tc.comments, tc.anchor); got != tc.want {
				t.Errorf("commentsPredatePause = %v, want %v", got, tc.want)
			}
		})
	}
}

func anchoredClient(at time.Time) *mockGitHubClient {
	return &mockGitHubClient{
		fetchLabelAppliedAtFn: func(owner, repo string, n int, label string) (time.Time, error) {
			if label != "fabrik:paused" {
				return time.Time{}, errors.New("unexpected label " + label)
			}
			return at, nil
		},
	}
}

func TestResumeAuthorised(t *testing.T) {
	oldHuman := gh.Comment{ID: "h_old", Author: "alice", Body: "old", CreatedAt: anchorPausedAt.Add(-time.Hour)}
	newHuman := gh.Comment{ID: "h_new", Author: "alice", Body: "new", CreatedAt: anchorPausedAt.Add(time.Minute)}
	newBot := gh.Comment{ID: "b_new", Author: "some-bot[bot]", Body: "chatter", CreatedAt: anchorPausedAt.Add(time.Minute)}

	t.Run("pre-pause human comment refused", func(t *testing.T) {
		eng := testEngine(t, anchoredClient(anchorPausedAt), &mockClaudeInvoker{})
		ok, raw, refused := eng.resumeAuthorised(gh.ProjectItem{Number: 1, Comments: []gh.Comment{oldHuman}})
		if ok || refused != 1 || len(raw) != 1 {
			t.Errorf("got ok=%v refused=%d raw=%d, want false/1/1", ok, refused, len(raw))
		}
	})
	t.Run("post-pause human comment resumes and returns full raw set (R5)", func(t *testing.T) {
		eng := testEngine(t, anchoredClient(anchorPausedAt), &mockClaudeInvoker{})
		ok, raw, refused := eng.resumeAuthorised(gh.ProjectItem{Number: 1, Comments: []gh.Comment{oldHuman, newBot, newHuman}})
		if !ok || refused != 0 || len(raw) != 3 {
			t.Errorf("got ok=%v refused=%d raw=%d, want true/0/3", ok, refused, len(raw))
		}
	})
	t.Run("bot-only chatter refused without refused count", func(t *testing.T) {
		eng := testEngine(t, anchoredClient(anchorPausedAt), &mockClaudeInvoker{})
		ok, raw, refused := eng.resumeAuthorised(gh.ProjectItem{Number: 1, Comments: []gh.Comment{newBot}})
		if ok || refused != 0 || len(raw) != 1 {
			t.Errorf("got ok=%v refused=%d raw=%d, want false/0/1", ok, refused, len(raw))
		}
	})
	t.Run("anchor fetch error resumes (R6)", func(t *testing.T) {
		client := &mockGitHubClient{fetchLabelAppliedAtFn: func(string, string, int, string) (time.Time, error) {
			return time.Time{}, errors.New("boom")
		}}
		eng := testEngine(t, client, &mockClaudeInvoker{})
		ok, _, _ := eng.resumeAuthorised(gh.ProjectItem{Number: 1, Comments: []gh.Comment{oldHuman}})
		if !ok {
			t.Error("expected resume when anchor fetch errors")
		}
	})
	t.Run("zero anchor resumes (R6)", func(t *testing.T) {
		eng := testEngine(t, anchoredClient(time.Time{}), &mockClaudeInvoker{})
		ok, _, _ := eng.resumeAuthorised(gh.ProjectItem{Number: 1, Comments: []gh.Comment{oldHuman}})
		if !ok {
			t.Error("expected resume when no labeled event is found")
		}
	})
	t.Run("stale cache entry is bypassed", func(t *testing.T) {
		// A human removing the label in the UI leaves an old cached timestamp;
		// the predicate must read the live anchor, not the cache.
		eng := testEngine(t, anchoredClient(anchorPausedAt), &mockClaudeInvoker{})
		eng.store.Apply(itemstate.LabelAppliedAtRecorded{
			Repo: "owner/repo", Number: 1, Label: "fabrik:paused", At: anchorPausedAt.Add(-24 * time.Hour),
		})
		ok, _, refused := eng.resumeAuthorised(gh.ProjectItem{Number: 1, Comments: []gh.Comment{oldHuman}})
		if ok || refused != 1 {
			t.Errorf("got ok=%v refused=%d, want false/1 (live anchor, not stale cache)", ok, refused)
		}
	})
}

// pausedItem builds a paused item (optionally also awaiting-input) carrying
// the given comments.
func pausedItem(awaitingInput bool, comments ...gh.Comment) gh.ProjectItem {
	labels := []string{"fabrik:paused"}
	if awaitingInput {
		labels = append(labels, "fabrik:awaiting-input")
	}
	return gh.ProjectItem{Number: 1, Title: "Test", Status: "Research", Labels: labels, Comments: comments}
}

func removedLabel(client *mockGitHubClient, label string) bool {
	for _, c := range client.removeLabelCalls {
		if c.labelName == label {
			return true
		}
	}
	return false
}

// TestResumeGates_RepeatComment_PauseHolds covers the #1752 loop: one
// unprocessable human comment predating the pause must not resume the issue on
// any number of consecutive polls, at admission (itemNeedsWork, R8) or in
// processItem, for both labels (R4), and must not touch breaker counters (R7).
func TestResumeGates_RepeatComment_PauseHolds(t *testing.T) {
	for _, awaiting := range []bool{false, true} {
		name := "paused"
		if awaiting {
			name = "paused+awaiting-input"
		}
		t.Run(name, func(t *testing.T) {
			client := anchoredClient(anchorPausedAt)
			eng := testEngine(t, client, &mockClaudeInvoker{})
			board := &gh.ProjectBoard{ProjectID: "PVT_1"}
			item := pausedItem(awaiting, gh.Comment{
				ID: "C1", Author: "arbeithand", Body: "## D2's blast radius is wider",
				CreatedAt: anchorPausedAt.Add(-time.Hour),
			})
			stage := eng.cfg.Stages[0]

			eng.recordCommentBreakerInvocation(item, "arbeithand")
			eng.store.Apply(itemstate.NoOpCommentCycleIncremented{Repo: "owner/repo", Number: 1, StageName: stage.Name})

			for i := 0; i < 5; i++ {
				if eng.itemNeedsWork(item) {
					t.Fatalf("poll %d: itemNeedsWork admitted a paused item whose only human comment predates the pause", i)
				}
				if err := eng.processItem(context.Background(), board, item); err != nil {
					t.Fatalf("poll %d: processItem: %v", i, err)
				}
			}
			if len(client.removeLabelCalls) != 0 {
				t.Errorf("pause lifted: removeLabelCalls=%v", client.removeLabelCalls)
			}
			if len(client.addLabelCalls) != 0 {
				t.Errorf("processComments entered: addLabelCalls=%v", client.addLabelCalls)
			}
			if got := eng.commentBreakerCount(item); got != 1 {
				t.Errorf("commentBreakerCount = %d, want 1 (untouched, R7)", got)
			}
			snap, _ := eng.store.Get("owner/repo", 1)
			if got := snap.NoOpCommentCycles(stage.Name); got != 1 {
				t.Errorf("NoOpCommentCycles = %d, want 1 (untouched, R7)", got)
			}
		})
	}
}

// TestResumeGates_PostPauseComment_Resumes covers R2/R5: a comment arriving
// after the pause resumes it, even alongside an older unprocessable one, and
// the full raw set (incl. bot chatter) reaches processComments.
func TestResumeGates_PostPauseComment_Resumes(t *testing.T) {
	skipIfNoGit(t)
	for _, awaiting := range []bool{false, true} {
		name := "paused"
		if awaiting {
			name = "paused+awaiting-input"
		}
		t.Run(name, func(t *testing.T) {
			client := anchoredClient(anchorPausedAt)
			eng := testEngineWithRepo(t, client, &mockClaudeInvoker{})
			board := &gh.ProjectBoard{ProjectID: "PVT_1"}
			item := pausedItem(awaiting,
				gh.Comment{ID: "C_old", Author: "arbeithand", Body: "old", CreatedAt: anchorPausedAt.Add(-time.Hour)},
				gh.Comment{ID: "C_bot", Author: "some-bot[bot]", Body: "chatter", CreatedAt: anchorPausedAt.Add(30 * time.Second)},
				gh.Comment{ID: "C_new", Author: "arbeithand", Body: "please continue", CreatedAt: anchorPausedAt.Add(time.Minute)},
			)
			if !eng.itemNeedsWork(item) {
				t.Fatal("itemNeedsWork refused a post-pause human comment")
			}
			// Errors from the real Claude invocation are irrelevant; only routing matters.
			_ = eng.processItem(context.Background(), board, item)

			if !removedLabel(client, "fabrik:paused") {
				t.Error("expected fabrik:paused to be removed")
			}
			if awaiting && !removedLabel(client, "fabrik:awaiting-input") {
				t.Error("expected fabrik:awaiting-input to be removed")
			}
			var editing bool
			for _, c := range client.addLabelCalls {
				if c.labelName == "fabrik:editing" {
					editing = true
				}
			}
			if !editing {
				t.Error("expected fabrik:editing, confirming processComments was entered")
			}
		})
	}
}

// TestResumeGates_NoInMemoryState covers R3: a brand-new engine (empty store)
// makes the same decision from the same GitHub-resident anchor.
func TestResumeGates_NoInMemoryState(t *testing.T) {
	item := pausedItem(true, gh.Comment{
		ID: "C1", Author: "arbeithand", Body: "old", CreatedAt: anchorPausedAt.Add(-time.Hour),
	})
	for i := 0; i < 2; i++ { // "before" and "after" a restart
		eng := testEngine(t, anchoredClient(anchorPausedAt), &mockClaudeInvoker{})
		if eng.itemNeedsWork(item) {
			t.Fatalf("engine #%d admitted a refused resume", i)
		}
	}
}

// TestResumeGates_IndeterminateAnchor_Resumes covers R6 end to end.
func TestResumeGates_IndeterminateAnchor_Resumes(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{fetchLabelAppliedAtFn: func(string, string, int, string) (time.Time, error) {
		return time.Time{}, errors.New("events API unavailable")
	}}
	eng := testEngineWithRepo(t, client, &mockClaudeInvoker{})
	item := pausedItem(true, gh.Comment{
		ID: "C1", Author: "arbeithand", Body: "old", CreatedAt: anchorPausedAt.Add(-time.Hour),
	})
	_ = eng.processItem(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item)
	if !removedLabel(client, "fabrik:paused") {
		t.Error("indeterminate anchor must resume, not leave the pause unliftable")
	}
}
