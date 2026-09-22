package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func checkSuitesServer(t *testing.T, suites []map[string]any, reportedTotal int) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/commits/sha1/check-suites", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if per < 1 {
			per = 30
		}
		start := min((page-1)*per, len(suites))
		end := min(start+per, len(suites))
		json.NewEncoder(w).Encode(map[string]any{
			"total_count":  reportedTotal,
			"check_suites": suites[start:end],
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClientWithBaseURL("test-token", srv.URL)
}

func TestFetchCheckSuites_PagesBeyondDefaultPageSize(t *testing.T) {
	const total = restPageSize + 7
	suites := make([]map[string]any, 0, total)
	for i := 0; i < total; i++ {
		suites = append(suites, map[string]any{
			"id": i + 1, "status": "completed", "conclusion": "success",
			"latest_check_runs_count": 1, "created_at": "2026-09-20T17:09:04Z",
			"app": map[string]any{"slug": fmt.Sprintf("app-%d", i)},
		})
	}
	c := checkSuitesServer(t, suites, total)
	got, err := c.FetchCheckSuites("o", "r", "sha1")
	if err != nil {
		t.Fatalf("FetchCheckSuites: %v", err)
	}
	if len(got) != total {
		t.Fatalf("got %d suites, want %d", len(got), total)
	}
	if got[total-1].AppSlug != fmt.Sprintf("app-%d", total-1) {
		t.Errorf("last suite app = %q", got[total-1].AppSlug)
	}
}

func TestFetchCheckSuites_TotalCountMismatchIsAnError(t *testing.T) {
	c := checkSuitesServer(t, []map[string]any{{"id": 1, "status": "completed"}}, 4)
	_, err := c.FetchCheckSuites("o", "r", "sha1")
	if err == nil || !strings.Contains(err.Error(), "collected 1 of 4") {
		t.Fatalf("want collected-1-of-4 error, got %v", err)
	}
}

func TestFetchCheckSuites_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("test-token", srv.URL)
	if _, err := c.FetchCheckSuites("o", "r", "sha1"); err == nil {
		t.Fatal("expected an error on HTTP 500 — a failed read must never look like an empty suite set")
	}
}

func TestFetchCheckSuites_MalformedCreatedAtIsZero(t *testing.T) {
	c := checkSuitesServer(t, []map[string]any{
		{"id": 1, "status": "queued", "created_at": "not-a-time", "app": map[string]any{"slug": "x"}},
	}, 1)
	got, err := c.FetchCheckSuites("o", "r", "sha1")
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].CreatedAt.IsZero() {
		t.Errorf("CreatedAt = %v, want zero", got[0].CreatedAt)
	}
}

// TestFetchCheckSuites_RecordedResponse replays a real captured response: several
// github-actions suites on one SHA plus inert queued/0-run App suites.
func TestFetchCheckSuites_RecordedResponse(t *testing.T) {
	body := loadRecording(t, "fetch_check_suites")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("test-token", srv.URL)
	got, err := c.FetchCheckSuites("o", "r", "sha1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatalf("got %d suites, want 8", len(got))
	}
	actions, inert := 0, 0
	for _, s := range got {
		if s.AppSlug == "github-actions" {
			actions++
		}
		if s.Status == "queued" && s.LatestCheckRunsCount == 0 {
			inert++
		}
		if s.CreatedAt.IsZero() {
			t.Errorf("suite %d: CreatedAt not parsed", s.ID)
		}
	}
	if actions < 2 {
		t.Errorf("recorded %d github-actions suites, want several (one per workflow run)", actions)
	}
	if inert != 3 {
		t.Errorf("recorded %d inert suites, want 3", inert)
	}
	// The recording is fully settled long ago: nothing may be outstanding, even
	// with the inert queued suites present — the no-deadlock property.
	after := got[0].CreatedAt.Add(time.Hour)
	if out := OutstandingCheckSuites(got, after, 90*time.Second); len(out) != 0 {
		t.Errorf("recorded settled SHA reports %d outstanding suites: %+v", len(out), out)
	}
}

func TestOutstandingCheckSuites(t *testing.T) {
	now := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)
	dwell := 90 * time.Second
	old := now.Add(-4 * time.Hour)
	young := now.Add(-10 * time.Second)
	tests := []struct {
		name   string
		suites []CheckSuite
		want   int
	}{
		{"none", nil, 0},
		{"completed", []CheckSuite{{Status: "completed", LatestCheckRunsCount: 5, CreatedAt: old}}, 0},
		{"real suite in progress with runs", []CheckSuite{{AppSlug: "github-actions", Status: "in_progress", LatestCheckRunsCount: 5, CreatedAt: old}}, 1},
		{"real suite queued with runs", []CheckSuite{{Status: "queued", LatestCheckRunsCount: 1, CreatedAt: old}}, 1},
		{"young zero-run suite (post-push window)", []CheckSuite{{Status: "queued", CreatedAt: young}}, 1},
		{"old inert zero-run suite", []CheckSuite{{AppSlug: "cursor", Status: "queued", CreatedAt: old}}, 0},
		{"old zero-run github-actions suite is never inert (#1829)", []CheckSuite{{AppSlug: "github-actions", Status: "queued", CreatedAt: old}}, 1},
		{"old zero-run github-actions suite that has settled clears", []CheckSuite{{AppSlug: "github-actions", Status: "completed", Conclusion: "failure", CreatedAt: old}}, 0},
		{"zero CreatedAt on zero-run suite holds", []CheckSuite{{Status: "queued"}}, 1},
		{"exactly at dwell boundary is settled", []CheckSuite{{Status: "queued", CreatedAt: now.Add(-dwell)}}, 0},
		{"mixed: inert + completed + real", []CheckSuite{
			{AppSlug: "cursor", Status: "queued", CreatedAt: old},
			{Status: "completed", LatestCheckRunsCount: 2, CreatedAt: old},
			{Status: "in_progress", LatestCheckRunsCount: 3, CreatedAt: old},
		}, 1},
		{"multiple github-actions suites, two outstanding", []CheckSuite{
			{AppSlug: "github-actions", Status: "in_progress", LatestCheckRunsCount: 1, CreatedAt: old},
			{AppSlug: "github-actions", Status: "queued", LatestCheckRunsCount: 1, CreatedAt: old},
			{AppSlug: "github-actions", Status: "completed", LatestCheckRunsCount: 1, CreatedAt: old},
		}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := OutstandingCheckSuites(tc.suites, now, dwell); len(got) != tc.want {
				t.Errorf("got %d outstanding, want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}
