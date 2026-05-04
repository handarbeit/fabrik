package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestSeedLabels_EmptyRepo verifies that SeedLabels returns ErrNoRepoConfigured
// when repo is empty, without making any HTTP requests.
func TestSeedLabels_EmptyRepo(t *testing.T) {
	var requestCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.SeedLabels("owner", "", []string{"Research"}, "testuser")
	if !errors.Is(err, ErrNoRepoConfigured) {
		t.Fatalf("expected ErrNoRepoConfigured, got %v", err)
	}
	if n := atomic.LoadInt32(&requestCount); n != 0 {
		t.Errorf("expected zero HTTP requests, got %d", n)
	}
}

// TestSeedLabels_LogAndContinue verifies that a 5xx response on one label does
// not prevent subsequent labels from being attempted.
func TestSeedLabels_LogAndContinue(t *testing.T) {
	var getCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			atomic.AddInt32(&getCount, 1)
		}
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"internal server error"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	// Pass no stage names so the label count is deterministic: staticLabelDefs + 1 locked.
	err := c.SeedLabels("owner", "repo", nil, "testuser")
	if err != nil {
		t.Fatalf("expected nil error (log-and-continue), got %v", err)
	}

	want := int32(len(staticLabelDefs) + 1) // +1 for fabrik:locked:<user>
	got := atomic.LoadInt32(&getCount)
	if got != want {
		t.Errorf("expected %d GET requests (one per label), got %d", want, got)
	}
}
