package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchItemDetails_CommitAttribution verifies that the head commit a review and a
// review-thread comment were made against (#2044) is decoded into PRReview.CommitID and
// Comment.CommitOID, and that a null commit degrades to the empty string rather than
// failing the fetch (the recognition code treats "" as "not attributable" = actionable).
func TestFetchItemDetails_CommitAttribution(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotQuery = body.Query
		author := map[string]interface{}{"__typename": "Bot", "login": "handarbeit-pruefer"}
		resp := map[string]interface{}{"data": map[string]interface{}{"node": map[string]interface{}{
			"comments": map[string]interface{}{"nodes": []interface{}{}, "pageInfo": map[string]interface{}{"hasNextPage": false}},
			"closedByPullRequestsReferences": map[string]interface{}{"nodes": []interface{}{map[string]interface{}{
				"id": "PR_10", "number": 10,
				"comments":       map[string]interface{}{"nodes": []interface{}{}, "pageInfo": map[string]interface{}{"hasNextPage": false}},
				"reviewRequests": map[string]interface{}{"nodes": []interface{}{}},
				"latestReviews": map[string]interface{}{"nodes": []interface{}{
					map[string]interface{}{"id": "R1", "databaseId": 1, "author": author, "state": "COMMENTED", "body": "with commit", "submittedAt": "2026-01-15T10:30:00Z", "commit": map[string]interface{}{"oid": "abc123"}},
					map[string]interface{}{"id": "R2", "databaseId": 2, "author": author, "state": "COMMENTED", "body": "null commit", "submittedAt": "2026-01-15T10:31:00Z", "commit": nil},
				}},
				"reviewThreads": map[string]interface{}{"nodes": []interface{}{map[string]interface{}{
					"id": "RT_1", "isResolved": false,
					"comments": map[string]interface{}{"nodes": []interface{}{
						map[string]interface{}{"id": "C1", "databaseId": 11, "author": author, "body": "finding", "createdAt": "2026-01-15T10:30:00Z", "originalCommit": map[string]interface{}{"oid": "def456"}},
						map[string]interface{}{"id": "C2", "databaseId": 12, "author": author, "body": "no commit", "createdAt": "2026-01-15T10:30:00Z", "originalCommit": nil},
					}},
				}}},
			}}},
		}}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	item := &ProjectItem{ID: "I_1", Number: 1}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}

	for _, want := range []string{"commit { oid }", "originalCommit { oid }"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("deep-fetch query does not select %q", want)
		}
	}
	if len(item.LinkedPRReviews) != 2 {
		t.Fatalf("LinkedPRReviews = %d, want 2", len(item.LinkedPRReviews))
	}
	if got := item.LinkedPRReviews[0].CommitID; got != "abc123" {
		t.Errorf("review[0].CommitID = %q, want abc123", got)
	}
	if got := item.LinkedPRReviews[1].CommitID; got != "" {
		t.Errorf("review[1].CommitID = %q, want empty for a null commit", got)
	}
	if len(item.LinkedPRReviewThreadComments) != 2 {
		t.Fatalf("thread comments = %d, want 2", len(item.LinkedPRReviewThreadComments))
	}
	if got := item.LinkedPRReviewThreadComments[0].CommitOID; got != "def456" {
		t.Errorf("thread comment[0].CommitOID = %q, want def456", got)
	}
	if got := item.LinkedPRReviewThreadComments[1].CommitOID; got != "" {
		t.Errorf("thread comment[1].CommitOID = %q, want empty for a null originalCommit", got)
	}
}
