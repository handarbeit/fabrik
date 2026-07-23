package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakePRServer sets up a test server with the given PR state.
// getHandler is called for GET /repos/.../pulls/{prNumber}
// putHandler is called for PUT /repos/.../pulls/{prNumber}/merge
func fakePRServer(t *testing.T, prNumber int, getHandler, putHandler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	prPath := fmt.Sprintf("/repos/owner/repo/pulls/%d", prNumber)
	mergePath := fmt.Sprintf("/repos/owner/repo/pulls/%d/merge", prNumber)
	if getHandler != nil {
		mux.HandleFunc(prPath, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Errorf("unexpected method %s on GET path", r.Method)
			}
			getHandler(w, r)
		})
	}
	if putHandler != nil {
		mux.HandleFunc(mergePath, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "PUT" {
				t.Errorf("unexpected method %s on PUT path", r.Method)
			}
			putHandler(w, r)
		})
	}
	srv := httptest.NewServer(mux)
	c := NewClientWithBaseURL("test-token", srv.URL)
	return srv, c
}

func TestMergePR_RebaseSuccess(t *testing.T) {
	srv, c := fakePRServer(t, 42,
		func(w http.ResponseWriter, r *http.Request) {
			mergeable := true
			json.NewEncoder(w).Encode(map[string]interface{}{"mergeable": mergeable})
		},
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["merge_method"] != "rebase" {
				t.Errorf("merge_method = %v, want rebase", body["merge_method"])
			}
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]interface{}{"merged": true, "message": "Pull Request successfully merged"})
		},
	)
	defer srv.Close()

	if err := c.MergePR("owner", "repo", 42); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
}

func TestMergePR_405FallbackToMergeCommit(t *testing.T) {
	putCalls := 0
	srv, c := fakePRServer(t, 42,
		func(w http.ResponseWriter, r *http.Request) {
			mergeable := true
			json.NewEncoder(w).Encode(map[string]interface{}{"mergeable": mergeable})
		},
		func(w http.ResponseWriter, r *http.Request) {
			putCalls++
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if putCalls == 1 {
				// First call: rebase — return 405
				if body["merge_method"] != "rebase" {
					t.Errorf("first call merge_method = %v, want rebase", body["merge_method"])
				}
				w.WriteHeader(405)
				w.Write([]byte(`{"message":"Rebase merge not allowed"}`))
				return
			}
			// Second call: merge commit fallback
			if body["merge_method"] != "merge" {
				t.Errorf("second call merge_method = %v, want merge", body["merge_method"])
			}
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]interface{}{"merged": true, "message": "Pull Request successfully merged"})
		},
	)
	defer srv.Close()

	if err := c.MergePR("owner", "repo", 42); err != nil {
		t.Fatalf("MergePR with 405 fallback: %v", err)
	}
	if putCalls != 2 {
		t.Errorf("expected 2 PUT calls, got %d", putCalls)
	}
}

func TestMergePR_NullMergeable(t *testing.T) {
	srv, c := fakePRServer(t, 42,
		func(w http.ResponseWriter, r *http.Request) {
			// mergeable is null — GitHub hasn't computed it yet
			json.NewEncoder(w).Encode(map[string]interface{}{"mergeable": nil})
		},
		nil,
	)
	defer srv.Close()

	err := c.MergePR("owner", "repo", 42)
	if !errors.Is(err, ErrNotMergeable) {
		t.Fatalf("expected ErrNotMergeable for null mergeable, got: %v", err)
	}
}

func TestMergePR_FalseMergeable(t *testing.T) {
	srv, c := fakePRServer(t, 42,
		func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]interface{}{"mergeable": false})
		},
		nil,
	)
	defer srv.Close()

	err := c.MergePR("owner", "repo", 42)
	if !errors.Is(err, ErrNotMergeable) {
		t.Fatalf("expected ErrNotMergeable for mergeable=false, got: %v", err)
	}
}

func TestFetchPRDetails_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/pulls/7" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"number": 7,
			"title":  "My PR",
			"state":  "open",
			"merged": false,
			"draft":  true,
			"head":   map[string]string{"sha": "abc123def"},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	pr, err := c.FetchPRDetails("owner", "repo", 7)
	if err != nil {
		t.Fatalf("FetchPRDetails: %v", err)
	}
	if pr.Number != 7 {
		t.Errorf("Number = %d, want 7", pr.Number)
	}
	if pr.Title != "My PR" {
		t.Errorf("Title = %q, want 'My PR'", pr.Title)
	}
	if pr.State != "open" {
		t.Errorf("State = %q, want 'open'", pr.State)
	}
	if pr.Merged {
		t.Error("Merged should be false")
	}
	if !pr.Draft {
		t.Error("Draft should be true")
	}
	if pr.HeadSHA != "abc123def" {
		t.Errorf("HeadSHA = %q, want 'abc123def'", pr.HeadSHA)
	}
}

func TestFetchPRDetails_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.FetchPRDetails("owner", "repo", 999)
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestFetchCheckRuns_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/commits/deadbeef/check-runs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"check_runs": []map[string]string{
				{"name": "ci/test", "status": "completed", "conclusion": "success"},
				{"name": "ci/lint", "status": "in_progress", "conclusion": ""},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	runs, err := c.FetchCheckRuns("owner", "repo", "deadbeef")
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	if runs[0].Name != "ci/test" || runs[0].Status != "completed" || runs[0].Conclusion != "success" {
		t.Errorf("runs[0] = %+v", runs[0])
	}
	if runs[1].Name != "ci/lint" || runs[1].Status != "in_progress" || runs[1].Conclusion != "" {
		t.Errorf("runs[1] = %+v", runs[1])
	}
}

func TestFetchCheckRuns_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"check_runs": []interface{}{}})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	runs, err := c.FetchCheckRuns("owner", "repo", "abc")
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("expected 0 runs, got %d", len(runs))
	}
}

func TestFetchLinkedPR_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		// Verify head filter includes the owner and branch
		if !strings.Contains(r.URL.RawQuery, "head=") {
			t.Errorf("expected head= query param, got %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"number": 42,
				"title":  "Linked PR",
				"state":  "open",
				"merged": false,
				"draft":  false,
				"head":   map[string]string{"sha": "deadbeef"},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	pr, err := c.FetchLinkedPR("owner", "repo", 10)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil {
		t.Fatal("expected PR, got nil")
	}
	if pr.Number != 42 {
		t.Errorf("Number = %d, want 42", pr.Number)
	}
	if pr.HeadSHA != "deadbeef" {
		t.Errorf("HeadSHA = %q, want 'deadbeef'", pr.HeadSHA)
	}
}

func TestFetchLinkedPR_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]interface{}{}) // empty array
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	pr, err := c.FetchLinkedPR("owner", "repo", 99)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr != nil {
		t.Errorf("expected nil PR for empty response, got %+v", pr)
	}
}

func TestGetPRBase_DecodesBaseRef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/pulls/42" {
			t.Errorf("path = %s, want /repos/owner/repo/pulls/42", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"base": map[string]string{"ref": "feature/foo"},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	base, err := c.GetPRBase("owner", "repo", 42)
	if err != nil {
		t.Fatalf("GetPRBase: %v", err)
	}
	if base != "feature/foo" {
		t.Errorf("base = %q, want %q", base, "feature/foo")
	}
}

func TestGetPRBase_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.GetPRBase("owner", "repo", 999)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestUpdatePRBase_SendsPatchWithBase(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{"number": 42})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.UpdatePRBase("owner", "repo", 42, "feature/bar"); err != nil {
		t.Fatalf("UpdatePRBase: %v", err)
	}
	if gotMethod != "PATCH" {
		t.Errorf("method = %s, want PATCH", gotMethod)
	}
	if gotPath != "/repos/owner/repo/pulls/42" {
		t.Errorf("path = %s, want /repos/owner/repo/pulls/42", gotPath)
	}
	if gotBody["base"] != "feature/bar" {
		t.Errorf("body[base] = %v, want %q", gotBody["base"], "feature/bar")
	}
}

func TestUpdatePRBase_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.UpdatePRBase("owner", "repo", 42, "nonexistent-branch")
	if err == nil {
		t.Fatal("expected error for 422 response")
	}
}

func TestDeleteReviewRequest_SendsDeleteWithBody(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.DeleteReviewRequest("owner", "repo", 42, []string{"copilot-pull-request-reviewer"}); err != nil {
		t.Fatalf("DeleteReviewRequest: %v", err)
	}
	if gotMethod != "DELETE" {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != "/repos/owner/repo/pulls/42/requested_reviewers" {
		t.Errorf("path = %s, want /repos/owner/repo/pulls/42/requested_reviewers", gotPath)
	}
	reviewers, ok := gotBody["reviewers"].([]interface{})
	if !ok || len(reviewers) != 1 || reviewers[0] != "copilot-pull-request-reviewer" {
		t.Errorf("body[reviewers] = %v, want [copilot-pull-request-reviewer]", gotBody["reviewers"])
	}
}

func TestDeleteReviewRequest_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.DeleteReviewRequest("owner", "repo", 42, []string{"bot"}); err == nil {
		t.Fatal("expected error for 422 response")
	}
}

func TestAddReviewRequest_SendsPost(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]interface{}{})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.AddReviewRequest("owner", "repo", 42, []string{"copilot-pull-request-reviewer"}); err != nil {
		t.Fatalf("AddReviewRequest: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/repos/owner/repo/pulls/42/requested_reviewers" {
		t.Errorf("path = %s, want /repos/owner/repo/pulls/42/requested_reviewers", gotPath)
	}
	reviewers, ok := gotBody["reviewers"].([]interface{})
	if !ok || len(reviewers) != 1 || reviewers[0] != "copilot-pull-request-reviewer" {
		t.Errorf("body[reviewers] = %v, want [copilot-pull-request-reviewer]", gotBody["reviewers"])
	}
}

func TestAddReviewRequest_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.AddReviewRequest("owner", "repo", 42, []string{"bot"}); err == nil {
		t.Fatal("expected error for 422 response")
	}
}

// ---------------------------------------------------------------------------
// Regex unit tests for reClosingKeyword
// ---------------------------------------------------------------------------

func extractIssueNums(body string) []int {
	matches := reClosingKeyword.FindAllStringSubmatch(body, -1)
	var out []int
	for _, m := range matches {
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

func TestReClosingKeyword_LineEndCloses(t *testing.T) {
	body := "This PR implements the feature.\n\nCloses #42"
	nums := extractIssueNums(body)
	if len(nums) != 1 || nums[0] != 42 {
		t.Errorf("want [42], got %v", nums)
	}
}

func TestReClosingKeyword_ProseRejected(t *testing.T) {
	// Mid-sentence prose references must be rejected; only the line-start Closes wins.
	body := "This PR relates to work before fixes #598 and #599 landed.\n\nCloses #42"
	nums := extractIssueNums(body)
	if len(nums) != 1 || nums[0] != 42 {
		t.Errorf("want [42] only (prose refs rejected), got %v", nums)
	}
}

func TestReClosingKeyword_ListForm(t *testing.T) {
	// List items at line start must still match.
	body := "- closes #1\n- fixes #2\n"
	nums := extractIssueNums(body)
	if len(nums) != 2 || nums[0] != 1 || nums[1] != 2 {
		t.Errorf("want [1 2], got %v", nums)
	}
}

func TestReClosingKeyword_MixedCase(t *testing.T) {
	body := "CLOSES #5"
	nums := extractIssueNums(body)
	if len(nums) != 1 || nums[0] != 5 {
		t.Errorf("want [5], got %v", nums)
	}
}

// makeTwoStepGraphQLServer creates an httptest server that handles two sequential
// POST /graphql calls. The first call returns the PR node ID; the second call
// invokes checkFn to verify the mutation variables and returns a success response.
func makeTwoStepGraphQLServer(t *testing.T, prNodeID string, checkFn func(t *testing.T, vars map[string]interface{})) (*httptest.Server, *Client) {
	t.Helper()
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		callCount++
		w.Header().Set("Content-Type", "application/json")
		switch callCount {
		case 1:
			// First call: return the PR node ID
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"repository": map[string]interface{}{
						"pullRequest": map[string]interface{}{
							"id": prNodeID,
						},
					},
				},
			})
		case 2:
			// Second call: verify mutation variables and return success
			vars := readVars(r)
			checkFn(t, vars)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{},
			})
		default:
			t.Errorf("unexpected call #%d to /graphql", callCount)
			http.Error(w, "unexpected call", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(func() {
		srv.Close()
		if callCount != 2 {
			t.Errorf("expected 2 GraphQL calls, got %d", callCount)
		}
	})
	return srv, NewClientWithBaseURL("test-token", srv.URL)
}

func TestPRNodeID_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"repository": map[string]interface{}{
					"pullRequest": map[string]interface{}{
						"id": "PR_abc123",
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	id, err := c.prNodeID("owner", "repo", 42)
	if err != nil {
		t.Fatalf("prNodeID: %v", err)
	}
	if id != "PR_abc123" {
		t.Errorf("prNodeID = %q, want PR_abc123", id)
	}
}

func TestPRNodeID_EmptyIDReturnsNotFoundError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"repository": map[string]interface{}{
					"pullRequest": map[string]interface{}{
						"id": "",
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	_, err := c.prNodeID("owner", "repo", 42)
	if err == nil {
		t.Fatal("expected error for empty PR node id")
	}
	if !strings.Contains(err.Error(), "PR #42 not found in repository owner/repo") {
		t.Errorf("error = %q, want PR-not-found message", err.Error())
	}
}

func TestPRNodeID_GraphQLErrorWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	_, err := c.prNodeID("owner", "repo", 42)
	if err == nil || !strings.Contains(err.Error(), "fetching PR node ID") {
		t.Errorf("expected wrapped 'fetching PR node ID' error, got %v", err)
	}
}

func TestMarkPRReady(t *testing.T) {
	const prNodeID = "PR_readynode"

	_, c := makeTwoStepGraphQLServer(t, prNodeID, func(t *testing.T, vars map[string]interface{}) {
		t.Helper()
		if got, ok := vars["prId"].(string); !ok || got != prNodeID {
			t.Errorf("mutation vars[prId] = %v, want %q", vars["prId"], prNodeID)
		}
	})

	if err := c.MarkPRReady("owner", "repo", 42); err != nil {
		t.Fatalf("MarkPRReady: %v", err)
	}
}

func TestEnablePullRequestAutoMerge(t *testing.T) {
	const prNodeID = "PR_automergenode"

	_, c := makeTwoStepGraphQLServer(t, prNodeID, func(t *testing.T, vars map[string]interface{}) {
		t.Helper()
		if got, ok := vars["prId"].(string); !ok || got != prNodeID {
			t.Errorf("mutation vars[prId] = %v, want %q", vars["prId"], prNodeID)
		}
		if got, ok := vars["method"].(string); !ok || got != "SQUASH" {
			t.Errorf("mutation vars[method] = %v, want SQUASH", vars["method"])
		}
	})

	if err := c.EnablePullRequestAutoMerge("owner", "repo", 42, "SQUASH"); err != nil {
		t.Fatalf("EnablePullRequestAutoMerge: %v", err)
	}
}

func TestEnqueuePullRequest(t *testing.T) {
	const prNodeID = "PR_testnode"
	const expectedHeadOID = "deadbeef1234"

	_, c := makeTwoStepGraphQLServer(t, prNodeID, func(t *testing.T, vars map[string]interface{}) {
		t.Helper()
		if got, ok := vars["prId"].(string); !ok || got != prNodeID {
			t.Errorf("mutation vars[prId] = %v, want %q", vars["prId"], prNodeID)
		}
		if got, ok := vars["expectedHeadOid"].(string); !ok || got != expectedHeadOID {
			t.Errorf("mutation vars[expectedHeadOid] = %v, want %q", vars["expectedHeadOid"], expectedHeadOID)
		}
	})

	if err := c.EnqueuePullRequest("owner", "repo", 42, expectedHeadOID); err != nil {
		t.Fatalf("EnqueuePullRequest: %v", err)
	}
}

func TestDequeuePullRequest(t *testing.T) {
	const prNodeID = "PR_testnode2"

	_, c := makeTwoStepGraphQLServer(t, prNodeID, func(t *testing.T, vars map[string]interface{}) {
		t.Helper()
		if got, ok := vars["prId"].(string); !ok || got != prNodeID {
			t.Errorf("mutation vars[prId] = %v, want %q", vars["prId"], prNodeID)
		}
		// DequeuePullRequest must NOT send expectedHeadOid.
		if _, present := vars["expectedHeadOid"]; present {
			t.Errorf("DequeuePullRequest sent unexpected expectedHeadOid: %v", vars["expectedHeadOid"])
		}
	})

	if err := c.DequeuePullRequest("owner", "repo", 42); err != nil {
		t.Fatalf("DequeuePullRequest: %v", err)
	}
}

func TestListPRs_CapturesHeadRefAndMergedAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if !strings.Contains(r.URL.RawQuery, "state=all") {
			t.Errorf("expected state=all query param, got %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"number":    200,
				"title":     "trial integration",
				"state":     "open",
				"merged_at": nil,
				"draft":     true,
				"body":      "trial body",
				"head":      map[string]string{"sha": "abc123", "ref": "fabrik/merge-train/merge-train-main-99"},
			},
			{
				"number":    201,
				"title":     "landed batch",
				"state":     "closed",
				"merged_at": "2026-07-03T00:00:00Z",
				"draft":     false,
				"body":      "landing body",
				"head":      map[string]string{"sha": "def456", "ref": "fabrik/merge-train/merge-train-main-42"},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	prs, err := c.ListPRs("owner", "repo")
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("expected 2 PRs, got %d", len(prs))
	}
	if prs[0].HeadRefName != "fabrik/merge-train/merge-train-main-99" {
		t.Errorf("PR[0].HeadRefName = %q, want the trial head ref", prs[0].HeadRefName)
	}
	if prs[0].Merged {
		t.Errorf("PR[0].Merged = true, want false (merged_at null)")
	}
	if prs[1].HeadRefName != "fabrik/merge-train/merge-train-main-42" {
		t.Errorf("PR[1].HeadRefName = %q, want the landing head ref", prs[1].HeadRefName)
	}
	if !prs[1].Merged {
		t.Errorf("PR[1].Merged = false, want true (merged_at set)")
	}
}

// FetchPRReviews must collapse the REST API's full review history down to one
// entry per author (the latest submission), matching GraphQL's latestReviews
// semantics. Otherwise an author's earlier non-DISMISSED review (e.g. a stale
// COMMENTED review) could outlive the dismissal of their actual current review
// and falsely satisfy the review-gate's hasReviews check.
func TestFetchPRReviews_CollapsesToLatestPerAuthor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls/42/reviews" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": 1, "user": map[string]string{"login": "alice"}, "state": "COMMENTED", "body": "early comment"},
			{"id": 2, "user": map[string]string{"login": "alice"}, "state": "APPROVED", "body": "lgtm"},
			{"id": 3, "user": map[string]string{"login": "alice"}, "state": "DISMISSED", "body": "lgtm"},
			{"id": 4, "user": map[string]string{"login": "bob"}, "state": "CHANGES_REQUESTED", "body": "fix this"},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	reviews, err := c.FetchPRReviews("owner", "repo", 42)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("expected 2 reviews (one per author), got %d: %+v", len(reviews), reviews)
	}
	byAuthor := make(map[string]PRReview, len(reviews))
	for _, r := range reviews {
		byAuthor[r.Author] = r
	}
	// alice's latest submission (id 3) dismissed her earlier APPROVED — the
	// stale id-1 COMMENTED entry must not leak through and count as a live review.
	if got := byAuthor["alice"].State; got != "DISMISSED" {
		t.Errorf("alice's collapsed review state = %q, want DISMISSED (her latest submission)", got)
	}
	if got := byAuthor["alice"].DatabaseID; got != 3 {
		t.Errorf("alice's collapsed review DatabaseID = %d, want 3 (her latest submission)", got)
	}
	if got := byAuthor["bob"].State; got != "CHANGES_REQUESTED" {
		t.Errorf("bob's collapsed review state = %q, want CHANGES_REQUESTED", got)
	}
}

func TestFetchPRReviews_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	reviews, err := c.FetchPRReviews("owner", "repo", 999)
	if err != nil {
		t.Fatalf("expected nil error on 404, got %v", err)
	}
	if reviews != nil {
		t.Errorf("expected nil reviews on 404, got %+v", reviews)
	}
}

func TestFetchPRReviewRequests_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls/42/requested_reviewers" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"users": []map[string]string{
				{"login": "alice", "type": "User"},
				{"login": "dependabot[bot]", "type": "Bot"},
			},
			"teams": []map[string]string{
				{"slug": "reviewers-team"},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	requests, err := c.FetchPRReviewRequests("owner", "repo", 42)
	if err != nil {
		t.Fatalf("FetchPRReviewRequests: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("expected 2 requested reviewers (teams ignored), got %d: %+v", len(requests), requests)
	}
	if requests[0].Login != "alice" || requests[0].IsBot {
		t.Errorf("requests[0] = %+v, want alice/non-bot", requests[0])
	}
	if requests[1].Login != "dependabot[bot]" || !requests[1].IsBot {
		t.Errorf("requests[1] = %+v, want dependabot[bot]/bot", requests[1])
	}
}

func TestFetchPRReviewRequests_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	requests, err := c.FetchPRReviewRequests("owner", "repo", 999)
	if err != nil {
		t.Fatalf("expected nil error on 404, got %v", err)
	}
	if requests != nil {
		t.Errorf("expected nil requests on 404, got %+v", requests)
	}
}
