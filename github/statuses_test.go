package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchCombinedStatus_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/commits/deadbeef/status" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"statuses": []map[string]string{
				{"context": "fantasy/local-test", "state": "success"},
				{"context": "other/status", "state": "pending"},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	statuses, err := c.FetchCombinedStatus("owner", "repo", "deadbeef")
	if err != nil {
		t.Fatalf("FetchCombinedStatus: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("len(statuses) = %d, want 2", len(statuses))
	}
	if statuses[0].Context != "fantasy/local-test" || statuses[0].State != "success" {
		t.Errorf("statuses[0] = %+v", statuses[0])
	}
	if statuses[1].Context != "other/status" || statuses[1].State != "pending" {
		t.Errorf("statuses[1] = %+v", statuses[1])
	}
}

func TestFetchCombinedStatus_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"statuses": []interface{}{}})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	statuses, err := c.FetchCombinedStatus("owner", "repo", "abc")
	if err != nil {
		t.Fatalf("FetchCombinedStatus: %v", err)
	}
	if len(statuses) != 0 {
		t.Errorf("expected 0 statuses, got %d", len(statuses))
	}
}

func TestFetchCombinedStatus_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.FetchCombinedStatus("owner", "repo", "abc"); err == nil {
		t.Fatal("expected error for 500 response")
	}
}
