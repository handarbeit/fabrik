package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hookJSON builds a repoHook with the given id and config.url.
func hookJSON(id int, configURL string) repoHook {
	h := repoHook{ID: id}
	h.Config.URL = configURL
	return h
}

// TestDeleteForwardingHooks_NoMatchingHooks verifies that when no hooks match
// webhookForwarderURL, zero DELETE requests are made.
func TestDeleteForwardingHooks_NoMatchingHooks(t *testing.T) {
	deleteCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleteCount++
		}
		if r.Method == "GET" {
			hooks := []repoHook{
				hookJSON(1, "https://example.com/other-hook"),
				hookJSON(2, "https://ci.example.com/webhook"),
			}
			json.NewEncoder(w).Encode(hooks)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	if err := c.DeleteForwardingHooks("owner", "repo"); err != nil {
		t.Fatalf("DeleteForwardingHooks: %v", err)
	}
	if deleteCount != 0 {
		t.Errorf("DELETE called %d times, want 0", deleteCount)
	}
}

// TestDeleteForwardingHooks_OneMatchingHook verifies that one matching hook results
// in exactly one DELETE request.
func TestDeleteForwardingHooks_OneMatchingHook(t *testing.T) {
	var deletedIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			hooks := []repoHook{
				hookJSON(1, "https://example.com/other"),
				hookJSON(42, webhookForwarderURL),
			}
			json.NewEncoder(w).Encode(hooks)
		case "DELETE":
			deletedIDs = append(deletedIDs, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	if err := c.DeleteForwardingHooks("owner", "repo"); err != nil {
		t.Fatalf("DeleteForwardingHooks: %v", err)
	}
	if len(deletedIDs) != 1 {
		t.Fatalf("DELETE called %d times, want 1; paths: %v", len(deletedIDs), deletedIDs)
	}
	want := "/repos/owner/repo/hooks/42"
	if deletedIDs[0] != want {
		t.Errorf("DELETE path = %q, want %q", deletedIDs[0], want)
	}
}

// TestDeleteForwardingHooks_MultipleMatchingHooks verifies that all matching hooks
// are deleted when multiple forwarding hooks exist.
func TestDeleteForwardingHooks_MultipleMatchingHooks(t *testing.T) {
	deleteCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			hooks := []repoHook{
				hookJSON(10, webhookForwarderURL),
				hookJSON(20, "https://other.example.com/hook"),
				hookJSON(30, webhookForwarderURL),
			}
			json.NewEncoder(w).Encode(hooks)
		case "DELETE":
			deleteCount++
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	if err := c.DeleteForwardingHooks("owner", "repo"); err != nil {
		t.Fatalf("DeleteForwardingHooks: %v", err)
	}
	if deleteCount != 2 {
		t.Errorf("DELETE called %d times, want 2", deleteCount)
	}
}

// TestDeleteForwardingHooks_GETFails verifies that a GET failure propagates as an error.
func TestDeleteForwardingHooks_GETFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	err := c.DeleteForwardingHooks("owner", "repo")
	if err == nil {
		t.Fatal("expected error when GET fails, got nil")
	}
}

// TestDeleteForwardingHooks_DELETE404TreatedAsSuccess verifies that a 404 on DELETE
// is not returned as an error (hook already gone — idempotent).
func TestDeleteForwardingHooks_DELETE404TreatedAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			hooks := []repoHook{hookJSON(99, webhookForwarderURL)}
			json.NewEncoder(w).Encode(hooks)
		case "DELETE":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	if err := c.DeleteForwardingHooks("owner", "repo"); err != nil {
		t.Fatalf("DeleteForwardingHooks: got error on 404 DELETE, want nil: %v", err)
	}
}
