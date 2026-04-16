package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
