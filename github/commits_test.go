package github

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func commitServer(t *testing.T, status int, body string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/commits/abc123", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClientWithBaseURL("test-token", srv.URL)
}

func TestFetchCommitTime_ParsesCommitterDate(t *testing.T) {
	c := commitServer(t, 200, `{"sha":"abc123","commit":{"author":{"date":"2026-10-24T16:00:00Z"},"committer":{"date":"2026-10-24T16:45:40Z"}}}`)
	got, err := c.FetchCommitTime("o", "r", "abc123")
	if err != nil {
		t.Fatalf("FetchCommitTime: %v", err)
	}
	want := time.Date(2026, 10, 24, 16, 45, 40, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v (must be the committer date, not the author date)", got, want)
	}
}

func TestFetchCommitTime_MissingDateIsAnError(t *testing.T) {
	c := commitServer(t, 200, `{"sha":"abc123","commit":{"committer":{}}}`)
	if got, err := c.FetchCommitTime("o", "r", "abc123"); err == nil {
		t.Fatalf("expected an error for a missing date, got %v", got)
	}
}

func TestFetchCommitTime_MalformedDateIsAnError(t *testing.T) {
	c := commitServer(t, 200, `{"commit":{"committer":{"date":"yesterday"}}}`)
	if _, err := c.FetchCommitTime("o", "r", "abc123"); err == nil {
		t.Fatal("expected an error for a malformed date")
	}
}

func TestFetchCommitTime_NotFoundIsAnError(t *testing.T) {
	c := commitServer(t, 404, `{"message":"Not Found"}`)
	if _, err := c.FetchCommitTime("o", "r", "abc123"); err == nil {
		t.Fatal("expected an error for a 404")
	}
}

func TestFetchCommitTime_EmptySHAIsAnError(t *testing.T) {
	c := commitServer(t, 200, `{}`)
	if _, err := c.FetchCommitTime("o", "r", ""); err == nil {
		t.Fatal("expected an error for an empty sha")
	}
}
