package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCloseIssue_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/issues/42" {
			t.Errorf("path = %s, want /repos/owner/repo/issues/42", r.URL.Path)
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["state"] != "closed" {
			t.Errorf("state = %v, want closed", body["state"])
		}
		if body["state_reason"] != "completed" {
			t.Errorf("state_reason = %v, want completed", body["state_reason"])
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.CloseIssue("owner", "repo", 42); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
}

func TestCloseIssue_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.CloseIssue("owner", "repo", 999); err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestFetchIssue_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/issues/42" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"number": 42,
			"title":  "Test Issue",
			"state":  "open",
			"labels": []map[string]string{
				{"name": "stage:Research:complete"},
				{"name": "fabrik:locked:alice"},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	issue, err := c.FetchIssue("owner", "repo", 42)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if issue.Number != 42 {
		t.Errorf("Number = %d, want 42", issue.Number)
	}
	if issue.Title != "Test Issue" {
		t.Errorf("Title = %q, want 'Test Issue'", issue.Title)
	}
	if issue.State != "open" {
		t.Errorf("State = %q, want 'open'", issue.State)
	}
	if len(issue.Labels) != 2 {
		t.Fatalf("Labels len = %d, want 2", len(issue.Labels))
	}
	if issue.Labels[0] != "stage:Research:complete" {
		t.Errorf("Labels[0] = %q", issue.Labels[0])
	}
}

func TestFetchIssue_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.FetchIssue("owner", "repo", 999)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}
