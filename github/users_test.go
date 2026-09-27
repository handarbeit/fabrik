package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchUserID_Success_EscapesBotLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is the decoded form; EscapedPath keeps the wire form.
		if r.URL.Path != "/users/my-app[bot]" {
			t.Errorf("path = %s, want /users/my-app[bot]", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 123456, "login": "my-app[bot]"})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	id, err := c.FetchUserID("my-app[bot]")
	if err != nil {
		t.Fatalf("FetchUserID: %v", err)
	}
	if id != 123456 {
		t.Errorf("id = %d, want 123456", id)
	}
}

func TestFetchUserID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.FetchUserID("nobody[bot]"); err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestFetchUserID_EmptyLoginAndMissingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"login":"x"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.FetchUserID(""); err == nil {
		t.Error("expected error for empty login")
	}
	if _, err := c.FetchUserID("x[bot]"); err == nil {
		t.Error("expected error when the response carries no id")
	}
}

func TestFetchProjectItem_FillsAuthor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"number":7,"title":"t","state":"open","node_id":"I_7","user":{"login":"my-app[bot]"},"assignees":[{"login":"alice"}]}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	pi, err := c.FetchProjectItem("o", "r", 7)
	if err != nil {
		t.Fatalf("FetchProjectItem: %v", err)
	}
	if pi.Author != "my-app[bot]" {
		t.Errorf("Author = %q", pi.Author)
	}
	if len(pi.Assignees) != 1 || pi.Assignees[0] != "alice" {
		t.Errorf("Assignees = %v", pi.Assignees)
	}
}
