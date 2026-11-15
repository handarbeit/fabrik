package pruefer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestAlreadyReviewedAtHead_AfterDismissedReview pins the intent of the #2588
// collapse change (ADR 2582, R4) for Pruefer: when an earlier review by the bot
// was dismissed and the bot later reviewed the current head, the collapsed
// entry that feeds alreadyReviewedAtHead is the real latest review (head
// commit), so the head reads as already reviewed. Before the change the
// collapse stayed on the DISMISSED entry at the old commit and the head looked
// unreviewed. Uses the real client collapse over an httptest REST response.
func TestAlreadyReviewedAtHead_AfterDismissedReview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": 1, "user": map[string]string{"login": "pruefer-bot"}, "state": "DISMISSED", "commit_id": "aaa"},
			{"id": 2, "user": map[string]string{"login": "pruefer-bot"}, "state": "COMMENTED", "commit_id": "bbb"},
		})
	}))
	defer srv.Close()

	c := gh.NewClientWithBaseURL("token", srv.URL)
	reviews, err := c.FetchPRReviews("owner", "repo", 42)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	matched, found := alreadyReviewedAtHead(reviews, "pruefer-bot", "bbb")
	if !matched || found != "bbb" {
		t.Fatalf("alreadyReviewedAtHead = %v (found %q), want true at bbb: a dismissal must not hide the bot's later review", matched, found)
	}
	if !alreadyReviewedAtAll(reviews, "pruefer-bot") {
		t.Fatal("alreadyReviewedAtAll = false, want true")
	}
}
