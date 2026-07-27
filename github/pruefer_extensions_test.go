package github

// Tests for the github/ extensions added for Pruefer (issue #1113): token
// refresh, diff fetch, review submission, open-PR listing, and issue-comment
// fetch. Named separately from the feature files they cover since they group
// by "what Pruefer needed", not by pre-existing file boundaries.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestSetToken_UpdatesSubsequentRequests(t *testing.T) {
	var mu sync.Mutex
	var seenTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenTokens = append(seenTokens, r.Header.Get("Authorization"))
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"allow_auto_merge": true})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token-v1", srv.URL)
	if _, err := c.FetchAllowAutoMerge("owner", "repo"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	c.SetToken("token-v2")
	if _, err := c.FetchAllowAutoMerge("owner", "repo"); err != nil {
		t.Fatalf("second call: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seenTokens) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seenTokens))
	}
	if seenTokens[0] != "Bearer token-v1" {
		t.Errorf("first request token = %q", seenTokens[0])
	}
	if seenTokens[1] != "Bearer token-v2" {
		t.Errorf("second request token = %q, want refreshed token", seenTokens[1])
	}
}

func TestToken_ReturnsCurrentValue(t *testing.T) {
	c := NewClient("initial")
	if got := c.Token(); got != "initial" {
		t.Errorf("Token() = %q, want initial", got)
	}
	c.SetToken("updated")
	if got := c.Token(); got != "updated" {
		t.Errorf("Token() = %q, want updated", got)
	}
}

func TestFetchPRDiff(t *testing.T) {
	const diffText = "diff --git a/foo.go b/foo.go\n+added line\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls/42" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github.v3.diff" {
			t.Errorf("Accept header = %q, want application/vnd.github.v3.diff", got)
		}
		w.Write([]byte(diffText))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	diff, err := c.FetchPRDiff("owner", "repo", 42)
	if err != nil {
		t.Fatalf("FetchPRDiff: %v", err)
	}
	if diff != diffText {
		t.Errorf("diff = %q, want %q", diff, diffText)
	}
}

func TestSubmitPRReview_HardcodesCommentEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/pulls/42/reviews" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body["event"] != "COMMENT" {
			t.Errorf("event = %v, want COMMENT (must never be caller-controlled)", body["event"])
		}
		if body["commit_id"] != "abc123" {
			t.Errorf("commit_id = %v, want abc123", body["commit_id"])
		}
		if body["body"] != "review text" {
			t.Errorf("body = %v, want %q", body["body"], "review text")
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 555})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	id, err := c.SubmitPRReview("owner", "repo", 42, "abc123", "review text")
	if err != nil {
		t.Fatalf("SubmitPRReview: %v", err)
	}
	if id != 555 {
		t.Errorf("id = %d, want 555", id)
	}
}

// SubmitPRReview's signature has no event parameter at all — verified at
// compile time by the call above passing exactly (owner, repo, prNumber,
// commitSHA, body). This test additionally guards the wire format so a
// future refactor can't reintroduce a caller-supplied event without failing
// TestSubmitPRReview_HardcodesCommentEvent above.

func TestListOpenPRs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Errorf("state query param = %q, want open", got)
		}
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"number": 1, "title": "Fix bug", "state": "open", "draft": false,
				"body": "body text",
				"user": map[string]string{"login": "alice"},
				"labels": []map[string]string{
					{"name": "bug"}, {"name": "priority:high"},
				},
				"head": map[string]string{"sha": "sha1", "ref": "fix-branch"},
			},
			{
				"number": 2, "title": "Draft work", "state": "open", "draft": true,
				"user": map[string]string{"login": "dependabot[bot]"},
				"head": map[string]string{"sha": "sha2", "ref": "draft-branch"},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	prs, err := c.ListOpenPRs("owner", "repo")
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("expected 2 PRs, got %d", len(prs))
	}
	if prs[0].Author != "alice" {
		t.Errorf("prs[0].Author = %q, want alice", prs[0].Author)
	}
	if len(prs[0].Labels) != 2 || prs[0].Labels[0] != "bug" || prs[0].Labels[1] != "priority:high" {
		t.Errorf("prs[0].Labels = %v", prs[0].Labels)
	}
	if !prs[1].Draft {
		t.Error("prs[1].Draft = false, want true")
	}
	if prs[1].Author != "dependabot[bot]" {
		t.Errorf("prs[1].Author = %q", prs[1].Author)
	}
}

func TestListOpenPRs_WarnsAtCap(t *testing.T) {
	var warned bool
	Logf = func(issueNumber int, tag, format string, args ...any) {
		if tag == "prs" {
			warned = true
		}
	}
	defer func() { Logf = nil }()

	entries := make([]map[string]interface{}, 100)
	for i := range entries {
		entries[i] = map[string]interface{}{
			"number": i + 1, "state": "open",
			"user": map[string]string{"login": "someone"},
			"head": map[string]string{"sha": "sha", "ref": "ref"},
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(entries)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	prs, err := c.ListOpenPRs("owner", "repo")
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(prs) != 100 {
		t.Fatalf("expected 100 PRs, got %d", len(prs))
	}
	if !warned {
		t.Error("expected a warning to be logged when exactly 100 PRs are returned")
	}
}

func TestListPRs_PopulatesAuthorAndLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"number": 7, "state": "open",
				"user":   map[string]string{"login": "bob"},
				"labels": []map[string]string{{"name": "needs-review"}},
				"head":   map[string]string{"sha": "sha7", "ref": "branch7"},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	prs, err := c.ListPRs("owner", "repo")
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(prs))
	}
	if prs[0].Author != "bob" {
		t.Errorf("Author = %q, want bob", prs[0].Author)
	}
	if len(prs[0].Labels) != 1 || prs[0].Labels[0] != "needs-review" {
		t.Errorf("Labels = %v", prs[0].Labels)
	}
}

func TestFetchPRReviews_PopulatesCommitID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": 1, "user": map[string]string{"login": "pruefer-bot[bot]"}, "state": "COMMENTED", "commit_id": "deadbeef"},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	reviews, err := c.FetchPRReviews("owner", "repo", 42)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("expected 1 review, got %d", len(reviews))
	}
	if reviews[0].CommitID != "deadbeef" {
		t.Errorf("CommitID = %q, want deadbeef", reviews[0].CommitID)
	}
}

func TestFetchIssueComments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/issues/9/comments" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"id": 100, "body": "/pruefer review", "created_at": "2026-01-01T00:00:00Z",
				"user":      map[string]string{"login": "maintainer"},
				"reactions": map[string]interface{}{"+1": 0, "eyes": 1, "rocket": 0},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	comments, err := c.FetchIssueComments("owner", "repo", 9)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].Body != "/pruefer review" {
		t.Errorf("Body = %q", comments[0].Body)
	}
	if comments[0].Author != "maintainer" {
		t.Errorf("Author = %q", comments[0].Author)
	}
	if !comments[0].HasReaction("EYES") {
		t.Error("expected HasReaction(EYES) = true")
	}
	if comments[0].HasReaction("ROCKET") {
		t.Error("expected HasReaction(ROCKET) = false (count 0)")
	}
}

func TestFetchIssueComments_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	comments, err := c.FetchIssueComments("owner", "repo", 999)
	if err != nil {
		t.Fatalf("expected nil error on 404, got %v", err)
	}
	if comments != nil {
		t.Errorf("expected nil comments on 404, got %+v", comments)
	}
}
