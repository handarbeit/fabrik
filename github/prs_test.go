package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
