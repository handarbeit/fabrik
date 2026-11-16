package github

import (
	"encoding/json"
	"errors"
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

func TestCreateCommitStatus_PostsStateContextDescription(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/statuses/deadbeef" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.CreateCommitStatus("owner", "repo", "deadbeef", "pending", ReviewStatusContext, "waiting"); err != nil {
		t.Fatalf("CreateCommitStatus: %v", err)
	}
	if got["state"] != "pending" || got["context"] != "fabrik/review" || got["description"] != "waiting" {
		t.Errorf("body = %v", got)
	}
}

func TestCreateCommitStatus_TruncatesDescriptionTo140(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	long := ""
	for i := 0; i < 200; i++ {
		long += "é"
	}
	if err := c.CreateCommitStatus("o", "r", "abc", "success", "fabrik/review", long); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got["description"])); n != 140 {
		t.Errorf("description length = %d runes, want 140", n)
	}
}

func TestCreateCommitStatus_ClassifiesRefusals(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{{http.StatusForbidden, ErrForbidden}, {http.StatusNotFound, ErrNotFound}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		}))
		c := NewClientWithBaseURL("token", srv.URL)
		err := c.CreateCommitStatus("o", "r", "abc", "pending", "fabrik/review", "x")
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.code, err, tc.want)
		}
	}
}
