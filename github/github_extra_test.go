package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── comments.go ──────────────────────────────────────────────────────────────

func TestAddCommentReaction_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/issues/comments/77/reactions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["content"] != "eyes" {
			t.Errorf("content = %v, want eyes", body["content"])
		}
		w.WriteHeader(201)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.AddCommentReaction("owner", "repo", 77, "eyes"); err != nil {
		t.Fatalf("AddCommentReaction: %v", err)
	}
}

func TestAddCommentReaction_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.AddCommentReaction("owner", "repo", 77, "eyes"); err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestUpdateIssueBody_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/owner/repo/issues/5") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["body"] != "new body" {
			t.Errorf("body = %v", body["body"])
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.UpdateIssueBody("owner", "repo", 5, "new body"); err != nil {
		t.Fatalf("UpdateIssueBody: %v", err)
	}
}

func TestGetIssueBody_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/owner/repo/issues/3") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"body": "the issue body",
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	body, err := c.GetIssueBody("owner", "repo", 3)
	if err != nil {
		t.Fatalf("GetIssueBody: %v", err)
	}
	if body != "the issue body" {
		t.Errorf("body = %q, want %q", body, "the issue body")
	}
}

func TestGetIssueBody_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.GetIssueBody("owner", "repo", 3)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

// ── labels.go ────────────────────────────────────────────────────────────────

func TestFetchLabels_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/owner/repo/issues/10/labels") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"name": "fabrik:yolo"},
			{"name": "stage:Research:complete"},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	labels, err := c.FetchLabels("owner", "repo", 10)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("labels count = %d, want 2", len(labels))
	}
	if labels[0] != "fabrik:yolo" {
		t.Errorf("labels[0] = %q", labels[0])
	}
}

func TestFetchLabels_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.FetchLabels("owner", "repo", 10)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestRemoveLabelFromIssue_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		// Path should be .../issues/7/labels/fabrik%3Ayolo
		if !strings.Contains(r.URL.Path, "/issues/7/labels/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.RemoveLabelFromIssue("owner", "repo", 7, "fabrik:yolo"); err != nil {
		t.Fatalf("RemoveLabelFromIssue: %v", err)
	}
}

func TestRemoveLabelFromIssue_NotFound_ReturnsErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Label does not exist"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.RemoveLabelFromIssue("owner", "repo", 7, "fabrik:yolo")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

// ── prs.go ───────────────────────────────────────────────────────────────────

func TestCreateDraftPR_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/owner/repo/pulls") {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["draft"] != true {
			t.Errorf("draft = %v, want true", body["draft"])
		}
		if !strings.Contains(body["body"].(string), "Closes #42") {
			t.Errorf("body missing Closes #42: %v", body["body"])
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]interface{}{"number": 99})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	prNum, err := c.CreateDraftPR("owner", "repo", "My PR", "fabrik/issue-42", "main", 42)
	if err != nil {
		t.Fatalf("CreateDraftPR: %v", err)
	}
	if prNum != 99 {
		t.Errorf("prNum = %d, want 99", prNum)
	}
}

func TestCreateDraftPR_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.CreateDraftPR("owner", "repo", "My PR", "head", "main", 1)
	if err == nil {
		t.Fatal("expected error for 422 response")
	}
}

func TestFindPRForIssue_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if !strings.Contains(r.URL.RawQuery, "fabrik") {
			t.Errorf("query missing fabrik: %s", r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"items": []map[string]interface{}{{"number": 55}},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	prNum, err := c.FindPRForIssue("owner", "repo", 10)
	if err != nil {
		t.Fatalf("FindPRForIssue: %v", err)
	}
	if prNum != 55 {
		t.Errorf("prNum = %d, want 55", prNum)
	}
}

func TestFindPRForIssue_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{}})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	prNum, err := c.FindPRForIssue("owner", "repo", 10)
	if err != nil {
		t.Fatalf("FindPRForIssue: %v", err)
	}
	if prNum != 0 {
		t.Errorf("expected 0 when no PR found, got %d", prNum)
	}
}

func TestFindPRForIssue_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.FindPRForIssue("owner", "repo", 10)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// ── rest.go ───────────────────────────────────────────────────────────────────

func TestRestDelete_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.restDelete(srv.URL + "/test"); err != nil {
		t.Fatalf("restDelete: %v", err)
	}
}

func TestRestDelete_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.restDelete(srv.URL + "/test")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestRestPostWithResponse_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]interface{}{"number": 7})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	var result struct {
		Number int `json:"number"`
	}
	if err := c.restPostWithResponse(srv.URL+"/test", map[string]interface{}{"key": "val"}, &result); err != nil {
		t.Fatalf("restPostWithResponse: %v", err)
	}
	if result.Number != 7 {
		t.Errorf("number = %d, want 7", result.Number)
	}
}

func TestRestGet_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"items": []map[string]interface{}{{"number": 3}},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	result, err := c.restGet(srv.URL + "/search")
	if err != nil {
		t.Fatalf("restGet: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Number != 3 {
		t.Errorf("unexpected result: %v", result)
	}
}

// ── types.go ──────────────────────────────────────────────────────────────────

func TestHasReaction(t *testing.T) {
	c := Comment{
		Reactions: []ReactionGroup{
			{Content: "EYES", Count: 2},
			{Content: "ROCKET", Count: 0},
		},
	}
	if !c.HasReaction("EYES") {
		t.Error("expected HasReaction(EYES) = true")
	}
	if c.HasReaction("ROCKET") {
		t.Error("expected HasReaction(ROCKET) = false (count 0)")
	}
	if c.HasReaction("THUMBS_UP") {
		t.Error("expected HasReaction(THUMBS_UP) = false (absent)")
	}
}

// ── project.go org/user fallback ─────────────────────────────────────────────

func TestFetchProjectBoard_OrgFailsFallsBackToUser(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)
		query, _ := req["query"].(string)

		if strings.Contains(query, "organization") {
			// First call — org query fails
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"errors": []map[string]interface{}{
					{"message": "Could not resolve to an Organization"},
				},
			})
			return
		}
		// Second call — user query succeeds
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"user": map[string]interface{}{
					"projectV2": map[string]interface{}{
						"id": "PVT_user",
						"items": map[string]interface{}{
							"pageInfo": map[string]interface{}{
								"hasNextPage": false,
								"endCursor":   "",
							},
							"nodes": []interface{}{},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("userorg", "repo", 1)
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if board.ProjectID != "PVT_user" {
		t.Errorf("ProjectID = %q, want PVT_user", board.ProjectID)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 calls (org + user), got %d", callCount)
	}
}

func TestFetchProjectBoard_RepoFieldParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"organization": map[string]interface{}{
					"projectV2": map[string]interface{}{
						"id": "PVT_org",
						"items": map[string]interface{}{
							"pageInfo": map[string]interface{}{
								"hasNextPage": false,
								"endCursor":   "",
							},
							"nodes": []interface{}{
								map[string]interface{}{
									"id": "PVTI_1",
									"fieldValueByName": map[string]interface{}{
										"name": "Research",
									},
									"content": map[string]interface{}{
										"__typename": "Issue",
										"id":         "I_1",
										"number":     float64(1),
										"title":      "Test Issue",
										"body":       "",
										"url":        "https://example.com",
										"updatedAt":  "2024-01-01T00:00:00Z",
										"repository": map[string]interface{}{
											"nameWithOwner": "acme/myrepo",
										},
										"labels": map[string]interface{}{
											"nodes":    []interface{}{},
											"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
										},
										"assignees": map[string]interface{}{
											"nodes": []interface{}{},
										},
									},
								},
							},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("acme", "myrepo", 1)
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 1 {
		t.Fatalf("items count = %d, want 1", len(board.Items))
	}
	if board.Items[0].Repo != "acme/myrepo" {
		t.Errorf("Repo = %q, want %q", board.Items[0].Repo, "acme/myrepo")
	}
}
