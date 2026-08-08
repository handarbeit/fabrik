package pruefer

import (
	"fmt"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// fakeCommenter implements GitHubCommenter in-memory for tests.
type fakeCommenter struct {
	mu          sync.Mutex
	comments    []gh.Comment
	fetchErr    error
	reactErr    error
	addErr      error
	addedBodies []string
	nextAddID   int
}

func (f *fakeCommenter) FetchIssueComments(owner, repo string, issueNumber int) ([]gh.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	out := make([]gh.Comment, len(f.comments))
	copy(out, f.comments)
	return out, nil
}

func (f *fakeCommenter) AddComment(owner, repo string, issueNumber int, body string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return 0, f.addErr
	}
	f.nextAddID++
	f.addedBodies = append(f.addedBodies, body)
	f.comments = append(f.comments, gh.Comment{DatabaseID: f.nextAddID, Body: body})
	return f.nextAddID, nil
}

func (f *fakeCommenter) addCommentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.addedBodies)
}

func (f *fakeCommenter) AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reactErr != nil {
		return f.reactErr
	}
	contentName := map[string]string{"eyes": "EYES", "rocket": "ROCKET"}[content]
	for i := range f.comments {
		if f.comments[i].DatabaseID == commentDatabaseID {
			f.comments[i].Reactions = append(f.comments[i].Reactions, gh.ReactionGroup{Content: contentName, Count: 1})
		}
	}
	return nil
}

func TestIsReviewCommand(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"/pruefer review", true},
		{"/PRUEFER REVIEW", true},
		{"  /pruefer review please", true},
		{"can someone run /pruefer review on this?", true},
		{"looks good to me", false},
		{"/pruefer reviews", true}, // substring match is intentional (prefix of a longer word is fine here)
		{"", false},
	}
	for _, tc := range cases {
		if got := isReviewCommand(tc.body); got != tc.want {
			t.Errorf("isReviewCommand(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestPendingForceReview_NoComments(t *testing.T) {
	f := &fakeCommenter{}
	pending, err := PendingForceReview(f, "owner", "repo", 1)
	if err != nil {
		t.Fatalf("PendingForceReview: %v", err)
	}
	if pending {
		t.Error("expected no pending force review")
	}
}

func TestPendingForceReview_UnprocessedCommand(t *testing.T) {
	f := &fakeCommenter{comments: []gh.Comment{
		{DatabaseID: 1, Body: "/pruefer review"},
	}}
	pending, err := PendingForceReview(f, "owner", "repo", 1)
	if err != nil {
		t.Fatalf("PendingForceReview: %v", err)
	}
	if !pending {
		t.Error("expected a pending force review")
	}
}

func TestPendingForceReview_AlreadyProcessed(t *testing.T) {
	f := &fakeCommenter{comments: []gh.Comment{
		{DatabaseID: 1, Body: "/pruefer review", Reactions: []gh.ReactionGroup{{Content: "ROCKET", Count: 1}}},
	}}
	pending, err := PendingForceReview(f, "owner", "repo", 1)
	if err != nil {
		t.Fatalf("PendingForceReview: %v", err)
	}
	if pending {
		t.Error("expected no pending force review — comment already has a ROCKET reaction")
	}
}

func TestPendingForceReview_PropagatesFetchError(t *testing.T) {
	f := &fakeCommenter{fetchErr: fmt.Errorf("boom")}
	if _, err := PendingForceReview(f, "owner", "repo", 1); err == nil {
		t.Fatal("expected fetch error to propagate")
	}
}

func TestAcknowledgeForceReview_AddsEyesReaction(t *testing.T) {
	f := &fakeCommenter{comments: []gh.Comment{
		{DatabaseID: 1, Body: "/pruefer review"},
	}}
	if err := AcknowledgeForceReview(f, "owner", "repo", 1); err != nil {
		t.Fatalf("AcknowledgeForceReview: %v", err)
	}
	if !f.comments[0].HasReaction("EYES") {
		t.Error("expected EYES reaction to be recorded")
	}
}

func TestAcknowledgeForceReview_SkipsAlreadyAcknowledged(t *testing.T) {
	f := &fakeCommenter{comments: []gh.Comment{
		{DatabaseID: 1, Body: "/pruefer review", Reactions: []gh.ReactionGroup{{Content: "EYES", Count: 1}}},
	}}
	callCount := 0
	origReact := f.reactErr
	_ = origReact
	// Wrap to count calls via a small shim.
	wrapped := &countingCommenter{fakeCommenter: f, onReact: func() { callCount++ }}
	if err := AcknowledgeForceReview(wrapped, "owner", "repo", 1); err != nil {
		t.Fatalf("AcknowledgeForceReview: %v", err)
	}
	if callCount != 0 {
		t.Errorf("expected no AddCommentReaction calls when already acknowledged, got %d", callCount)
	}
}

// countingCommenter wraps *fakeCommenter and counts AddCommentReaction calls.
type countingCommenter struct {
	*fakeCommenter
	onReact func()
}

func (c *countingCommenter) AddCommentReaction(owner, repo string, commentDatabaseID int, content string) error {
	c.onReact()
	return c.fakeCommenter.AddCommentReaction(owner, repo, commentDatabaseID, content)
}

func TestMarkForceReviewsProcessed_AddsRocketToAllUnprocessed(t *testing.T) {
	f := &fakeCommenter{comments: []gh.Comment{
		{DatabaseID: 1, Body: "/pruefer review"},
		{DatabaseID: 2, Body: "unrelated comment"},
		{DatabaseID: 3, Body: "/pruefer review", Reactions: []gh.ReactionGroup{{Content: "ROCKET", Count: 1}}},
	}}
	if err := MarkForceReviewsProcessed(f, "owner", "repo", 1); err != nil {
		t.Fatalf("MarkForceReviewsProcessed: %v", err)
	}
	if !f.comments[0].HasReaction("ROCKET") {
		t.Error("expected comment 1 (unprocessed /pruefer review) to gain a ROCKET reaction")
	}
	if f.comments[1].HasReaction("ROCKET") {
		t.Error("comment 2 (unrelated) must not gain a ROCKET reaction")
	}

	pending, err := PendingForceReview(f, "owner", "repo", 1)
	if err != nil {
		t.Fatalf("PendingForceReview: %v", err)
	}
	if pending {
		t.Error("expected no pending force review after MarkForceReviewsProcessed")
	}
}

func TestMarkForceReviewsProcessed_PropagatesReactionError(t *testing.T) {
	f := &fakeCommenter{
		comments: []gh.Comment{{DatabaseID: 1, Body: "/pruefer review"}},
		reactErr: fmt.Errorf("rate limited"),
	}
	if err := MarkForceReviewsProcessed(f, "owner", "repo", 1); err == nil {
		t.Fatal("expected reaction error to propagate")
	}
}
