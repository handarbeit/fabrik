package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
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

// labelEventsServer serves /issues/N/events pages from the given pages (1-based);
// any page past the end is empty.
func labelEventsServer(t *testing.T, pages []string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		body := "[]"
		if page >= 1 && page <= len(pages) {
			body = pages[page-1]
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchLabelRemovedAt_LatestUnlabeledAcrossPages(t *testing.T) {
	srv := labelEventsServer(t, []string{
		`[{"event":"labeled","created_at":"2026-10-05T10:00:00Z","label":{"name":"fabrik:paused"}},
		  {"event":"unlabeled","created_at":"2026-10-05T11:00:00Z","label":{"name":"fabrik:paused"}},
		  {"event":"unlabeled","created_at":"2026-10-05T12:00:00Z","label":{"name":"other"}}]`,
		`[{"event":"labeled","created_at":"2026-10-06T08:00:00Z","label":{"name":"fabrik:paused"}},
		  {"event":"unlabeled","created_at":"2026-10-06T10:02:56Z","label":{"name":"fabrik:paused"}},
		  {"event":"closed","created_at":"2026-10-07T00:00:00Z"}]`,
	}, nil)
	c := NewClientWithBaseURL("token", srv.URL)

	got, err := c.FetchLabelRemovedAt("o", "r", 1, "fabrik:paused")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 10, 6, 10, 2, 56, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("FetchLabelRemovedAt = %v, want %v", got, want)
	}

	// FetchLabelAppliedAt is unchanged: it reads the labeled events.
	applied, err := c.FetchLabelAppliedAt("o", "r", 1, "fabrik:paused")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantApplied := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	if !applied.Equal(wantApplied) {
		t.Errorf("FetchLabelAppliedAt = %v, want %v", applied, wantApplied)
	}
}

func TestFetchLabelRemovedAt_AbsentIsZeroNoError(t *testing.T) {
	srv := labelEventsServer(t, []string{
		`[{"event":"labeled","created_at":"2026-10-05T10:00:00Z","label":{"name":"fabrik:paused"}}]`,
	}, nil)
	c := NewClientWithBaseURL("token", srv.URL)

	got, err := c.FetchLabelRemovedAt("o", "r", 1, "fabrik:paused")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("expected zero time, got %v", got)
	}
}

func TestFetchLabelRemovedAt_HTTPErrorPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)

	if _, err := c.FetchLabelRemovedAt("o", "r", 1, "fabrik:paused"); err == nil {
		t.Fatal("expected an error for an HTTP 500, got nil")
	}
}

func TestFetchLabelRemovedAt_PageCapBounded(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// Never an empty page: the loop must stop at restMaxPages.
		w.Write([]byte(`[{"event":"unlabeled","created_at":"2026-10-06T10:00:00Z","label":{"name":"fabrik:paused"}}]`))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)

	got, err := c.FetchLabelRemovedAt("o", "r", 1, "fabrik:paused")
	if err != nil {
		t.Fatalf("unexpected error (cap is fail-soft): %v", err)
	}
	if got.IsZero() {
		t.Error("expected the newest removal seen before the cap, got zero")
	}
	if n := atomic.LoadInt32(&hits); n != restMaxPages {
		t.Errorf("expected %d page requests, got %d", restMaxPages, n)
	}
}
