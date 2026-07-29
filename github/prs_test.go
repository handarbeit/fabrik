package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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

// TestMergePR_ConfiguredStrategyFirst asserts that MergePR attempts the
// configured strategy (via SetMergeStrategy) as its very first call, not a
// hardcoded rebase.
func TestMergePR_ConfiguredStrategyFirst(t *testing.T) {
	srv, c := fakePRServer(t, 42,
		func(w http.ResponseWriter, r *http.Request) {
			mergeable := true
			json.NewEncoder(w).Encode(map[string]interface{}{"mergeable": mergeable, "mergeable_state": "clean"})
		},
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["merge_method"] != "squash" {
				t.Errorf("merge_method = %v, want squash", body["merge_method"])
			}
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]interface{}{"merged": true, "message": "Pull Request successfully merged"})
		},
	)
	defer srv.Close()
	c.SetMergeStrategy("SQUASH")

	if err := c.MergePR("owner", "repo", 42); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
}

// TestMergePR_DefaultAttemptsMergeFirst is a regression test guarding against
// reintroducing rebase-first behavior: a Client with no configured strategy
// (the zero value) must attempt "merge" first, not "rebase".
func TestMergePR_DefaultAttemptsMergeFirst(t *testing.T) {
	putCalls := 0
	srv, c := fakePRServer(t, 42,
		func(w http.ResponseWriter, r *http.Request) {
			mergeable := true
			json.NewEncoder(w).Encode(map[string]interface{}{"mergeable": mergeable, "mergeable_state": "clean"})
		},
		func(w http.ResponseWriter, r *http.Request) {
			putCalls++
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if body["merge_method"] != "merge" {
				t.Errorf("merge_method = %v, want merge", body["merge_method"])
			}
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]interface{}{"merged": true, "message": "Pull Request successfully merged"})
		},
	)
	defer srv.Close()

	if err := c.MergePR("owner", "repo", 42); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if putCalls != 1 {
		t.Errorf("expected exactly 1 PUT call, got %d", putCalls)
	}
}

func TestMergePR_405FallbackToMergeCommit(t *testing.T) {
	putCalls := 0
	srv, c := fakePRServer(t, 42,
		func(w http.ResponseWriter, r *http.Request) {
			mergeable := true
			json.NewEncoder(w).Encode(map[string]interface{}{"mergeable": mergeable, "mergeable_state": "clean"})
		},
		func(w http.ResponseWriter, r *http.Request) {
			putCalls++
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if putCalls == 1 {
				// First call: configured default (merge) — return 405
				if body["merge_method"] != "merge" {
					t.Errorf("first call merge_method = %v, want merge", body["merge_method"])
				}
				w.WriteHeader(405)
				w.Write([]byte(`{"message":"Merge commits are not allowed"}`))
				return
			}
			// Second call: next in fallback order (squash)
			if body["merge_method"] != "squash" {
				t.Errorf("second call merge_method = %v, want squash", body["merge_method"])
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

// TestMergePR_NonMethodNotAllowedErrorPropagatesImmediately asserts that a
// non-405 error from the merge endpoint bails out immediately without
// attempting any fallback method.
func TestMergePR_NonMethodNotAllowedErrorPropagatesImmediately(t *testing.T) {
	putCalls := 0
	srv, c := fakePRServer(t, 42,
		func(w http.ResponseWriter, r *http.Request) {
			mergeable := true
			json.NewEncoder(w).Encode(map[string]interface{}{"mergeable": mergeable, "mergeable_state": "clean"})
		},
		func(w http.ResponseWriter, r *http.Request) {
			putCalls++
			w.WriteHeader(500)
			w.Write([]byte(`{"message":"Internal Server Error"}`))
		},
	)
	defer srv.Close()

	err := c.MergePR("owner", "repo", 42)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrMethodNotAllowed) {
		t.Fatalf("expected non-405 error, got ErrMethodNotAllowed: %v", err)
	}
	if putCalls != 1 {
		t.Errorf("expected exactly 1 PUT call (no fallback on non-405 error), got %d", putCalls)
	}
}

// TestMergeMethodAttemptOrder asserts mergeMethodAttemptOrder's exact output
// against literal expected slices, independent of MergePR — this is what
// guards the fixed fallback order (merge -> squash -> rebase) itself, since
// TestMergePR_StrategyFallbackTable derives its own expectations from this
// same function and can't catch a bug inside it.
func TestMergeMethodAttemptOrder(t *testing.T) {
	tests := []struct {
		strategy string
		want     []string
	}{
		{"MERGE", []string{"merge", "squash", "rebase"}},
		{"SQUASH", []string{"squash", "merge", "rebase"}},
		{"REBASE", []string{"rebase", "merge", "squash"}},
		{"merge", []string{"merge", "squash", "rebase"}},
		{"Squash", []string{"squash", "merge", "rebase"}},
		{"", []string{"merge", "squash", "rebase"}},
		{"bogus", []string{"merge", "squash", "rebase"}},
		{"SQUASH ", []string{"merge", "squash", "rebase"}}, // untrimmed input is not a recognized method
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("strategy=%q", tt.strategy), func(t *testing.T) {
			got := mergeMethodAttemptOrder(tt.strategy)
			if !slices.Equal(got, tt.want) {
				t.Errorf("mergeMethodAttemptOrder(%q) = %v, want %v", tt.strategy, got, tt.want)
			}
		})
	}
}

// TestMergePR_StrategyFallbackTable exercises every configured strategy
// against every combination of repo-allowed merge methods, asserting the
// first method attempted, the fallback order, the total call count, and that
// the merge ultimately succeeds via whichever allowed method comes first in
// the attempt order.
func TestMergePR_StrategyFallbackTable(t *testing.T) {
	allMethods := []string{"merge", "squash", "rebase"}

	tests := []struct {
		name     string
		strategy string
		allowed  map[string]bool // methods the fake repo accepts (others 405)
	}{
		{"MERGE configured, all allowed", "MERGE", map[string]bool{"merge": true, "squash": true, "rebase": true}},
		{"SQUASH configured, all allowed", "SQUASH", map[string]bool{"merge": true, "squash": true, "rebase": true}},
		{"REBASE configured, all allowed", "REBASE", map[string]bool{"merge": true, "squash": true, "rebase": true}},
		{"REBASE configured, only merge allowed", "REBASE", map[string]bool{"merge": true, "squash": false, "rebase": false}},
		{"SQUASH configured, only rebase allowed", "SQUASH", map[string]bool{"merge": false, "squash": false, "rebase": true}},
		{"default (unset) configured, only squash allowed", "", map[string]bool{"merge": false, "squash": true, "rebase": false}},
		{"REBASE configured, nothing allowed", "REBASE", map[string]bool{"merge": false, "squash": false, "rebase": false}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantOrder := mergeMethodAttemptOrder(tt.strategy)
			var gotOrder []string

			srv, c := fakePRServer(t, 42,
				func(w http.ResponseWriter, r *http.Request) {
					mergeable := true
					json.NewEncoder(w).Encode(map[string]interface{}{"mergeable": mergeable, "mergeable_state": "clean"})
				},
				func(w http.ResponseWriter, r *http.Request) {
					var body map[string]interface{}
					json.NewDecoder(r.Body).Decode(&body)
					method, _ := body["merge_method"].(string)
					gotOrder = append(gotOrder, method)
					if !tt.allowed[method] {
						w.WriteHeader(405)
						w.Write([]byte(`{"message":"Method not allowed"}`))
						return
					}
					w.WriteHeader(200)
					json.NewEncoder(w).Encode(map[string]interface{}{"merged": true, "message": "Pull Request successfully merged"})
				},
			)
			defer srv.Close()
			c.SetMergeStrategy(tt.strategy)

			err := c.MergePR("owner", "repo", 42)

			// Determine the expected stopping point: the first method in
			// wantOrder that is allowed.
			var wantCalls []string
			for _, m := range wantOrder {
				wantCalls = append(wantCalls, m)
				if tt.allowed[m] {
					break
				}
			}
			// If nothing is allowed, every method in wantOrder is attempted.
			anyAllowed := false
			for _, m := range allMethods {
				if tt.allowed[m] {
					anyAllowed = true
				}
			}

			if !anyAllowed {
				if err == nil {
					t.Fatalf("expected error when no method is allowed, got nil")
				}
				if len(gotOrder) != len(wantOrder) {
					t.Errorf("call count = %d, want %d (order=%v)", len(gotOrder), len(wantOrder), gotOrder)
				}
			} else {
				if err != nil {
					t.Fatalf("MergePR: %v", err)
				}
				if len(gotOrder) != len(wantCalls) {
					t.Errorf("call count = %d, want %d (got=%v want=%v)", len(gotOrder), len(wantCalls), gotOrder, wantCalls)
				}
			}
			for i := range gotOrder {
				if i >= len(wantOrder) {
					break
				}
				if gotOrder[i] != wantOrder[i] {
					t.Errorf("call %d method = %q, want %q (full order got=%v want=%v)", i, gotOrder[i], wantOrder[i], gotOrder, wantOrder)
				}
			}
		})
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

// TestMergePR_MergeableStateTable exercises MergePR's mergeable x
// mergeable_state precondition: a merge conflict (mergeable=false) must
// still return ErrNotMergeable (the pre-existing behavior, preserving the
// rebase-needed path), while an accepted mergeable=true PR must additionally
// be refused with ErrNotMergeableCI — and never reach the merge endpoint —
// unless mergeable_state is in the MergeableStateAccepted allowlist
// ({clean, unstable}).
func TestMergePR_MergeableStateTable(t *testing.T) {
	tests := []struct {
		name           string
		mergeable      interface{} // bool or nil
		mergeableState string
		wantMerge      bool
		wantErr        error // nil means "any error is fine as long as it's not the other sentinel"
	}{
		{"clean merges", true, "clean", true, nil},
		{"unstable merges", true, "unstable", true, nil},
		{"blocked refused CI", true, "blocked", false, ErrNotMergeableCI},
		{"behind refused CI", true, "behind", false, ErrNotMergeableCI},
		{"draft refused CI", true, "draft", false, ErrNotMergeableCI},
		{"has_hooks refused CI", true, "has_hooks", false, ErrNotMergeableCI},
		{"unknown refused CI", true, "unknown", false, ErrNotMergeableCI},
		{"empty state refused CI", true, "", false, ErrNotMergeableCI},
		{"dirty refused conflict", false, "dirty", false, ErrNotMergeable},
		{"still computing refused conflict", nil, "", false, ErrNotMergeable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			putCalls := 0
			srv, c := fakePRServer(t, 42,
				func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode(map[string]interface{}{
						"mergeable":       tt.mergeable,
						"mergeable_state": tt.mergeableState,
					})
				},
				func(w http.ResponseWriter, r *http.Request) {
					putCalls++
					w.WriteHeader(200)
					json.NewEncoder(w).Encode(map[string]interface{}{"merged": true, "message": "Pull Request successfully merged"})
				},
			)
			defer srv.Close()

			err := c.MergePR("owner", "repo", 42)

			if tt.wantMerge {
				if err != nil {
					t.Fatalf("MergePR: unexpected error: %v", err)
				}
				if putCalls != 1 {
					t.Errorf("expected exactly 1 PUT call, got %d", putCalls)
				}
				return
			}

			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want errors.Is match for %v", err, tt.wantErr)
			}
			if putCalls != 0 {
				t.Errorf("expected no PUT call when refusing merge, got %d", putCalls)
			}
		})
	}
}

// TestMergePR_UnstableAcceptedExplicit is a dedicated regression test for the
// operator-confirmed acceptance criterion that mergeable_state=unstable must
// merge (not just an entry in the combinatorial table above) — this is the
// {clean, unstable} decision this issue exists to make deliberate and tested.
func TestMergePR_UnstableAcceptedExplicit(t *testing.T) {
	putCalls := 0
	srv, c := fakePRServer(t, 42,
		func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]interface{}{"mergeable": true, "mergeable_state": "unstable"})
		},
		func(w http.ResponseWriter, r *http.Request) {
			putCalls++
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]interface{}{"merged": true, "message": "Pull Request successfully merged"})
		},
	)
	defer srv.Close()

	if err := c.MergePR("owner", "repo", 42); err != nil {
		t.Fatalf("MergePR with mergeable_state=unstable: unexpected error: %v", err)
	}
	if putCalls != 1 {
		t.Errorf("expected exactly 1 PUT call for accepted unstable state, got %d", putCalls)
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

func TestFetchPRReviewDecision(t *testing.T) {
	tests := []struct {
		name   string
		decide string // GraphQL response value, "" for null
		want   string
	}{
		{"approved", "APPROVED", "APPROVED"},
		{"changes requested", "CHANGES_REQUESTED", "CHANGES_REQUESTED"},
		{"review required", "REVIEW_REQUIRED", "REVIEW_REQUIRED"},
		{"null (no branch protection review requirement)", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var reviewDecision interface{}
				if tt.decide != "" {
					reviewDecision = tt.decide
				}
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"reviewDecision": reviewDecision,
							},
						},
					},
				})
			}))
			defer srv.Close()

			c := NewClientWithBaseURL("test-token", srv.URL)
			got, err := c.FetchPRReviewDecision("owner", "repo", 42)
			if err != nil {
				t.Fatalf("FetchPRReviewDecision: %v", err)
			}
			if got != tt.want {
				t.Errorf("FetchPRReviewDecision = %q, want %q", got, tt.want)
			}
		})
	}
}

// A null pullRequest object (bad prNumber, or an app-token permission gap)
// must return an error, not "" — folding it into the same "" the
// no-branch-protection fallback treats as legitimate data would silently
// satisfy the authoritative gate on unknown state.
func TestFetchPRReviewDecision_NullPullRequest_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"repository": map[string]interface{}{
					"pullRequest": nil,
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	got, err := c.FetchPRReviewDecision("owner", "repo", 42)
	if err == nil {
		t.Fatal("expected an error for a null pullRequest object, got nil")
	}
	if !strings.Contains(err.Error(), "PR #42 not found") {
		t.Errorf("expected 'PR #42 not found' error, got %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string alongside the error, got %q", got)
	}
}

func TestFetchPRReviewDecision_ErrorWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	_, err := c.FetchPRReviewDecision("owner", "repo", 42)
	if err == nil || !strings.Contains(err.Error(), "fetching PR #42 review decision") {
		t.Errorf("expected wrapped 'fetching PR #42 review decision' error, got %v", err)
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

func TestDisablePullRequestAutoMerge(t *testing.T) {
	const prNodeID = "PR_disableautomergenode"

	_, c := makeTwoStepGraphQLServer(t, prNodeID, func(t *testing.T, vars map[string]interface{}) {
		t.Helper()
		if got, ok := vars["prId"].(string); !ok || got != prNodeID {
			t.Errorf("mutation vars[prId] = %v, want %q", vars["prId"], prNodeID)
		}
	})

	if err := c.DisablePullRequestAutoMerge("owner", "repo", 42); err != nil {
		t.Fatalf("DisablePullRequestAutoMerge: %v", err)
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
