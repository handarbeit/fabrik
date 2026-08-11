package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// Tests for #1539: REST collection fetchers must page until exhausted rather
// than treating page one as the whole collection.
//
// Each multi-page test builds a fixture strictly larger than restPageSize and
// asserts the LAST record is present — the first page alone can never satisfy
// that, which is what makes these non-vacuous against the pre-fix code.

// pagedArrayServer serves a bare JSON array endpoint that honours page/per_page.
// items is the full collection; each element is marshalled as-is. It records how
// many requests it served so single-page tests can assert no speculative probe.
func pagedArrayServer(t *testing.T, path string, items []map[string]any, requests *int) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		*requests++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if per < 1 {
			per = 30
		}
		start := (page - 1) * per
		if start > len(items) {
			start = len(items)
		}
		end := start + per
		if end > len(items) {
			end = len(items)
		}
		if err := json.NewEncoder(w).Encode(items[start:end]); err != nil {
			t.Errorf("encoding page %d: %v", page, err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, NewClientWithBaseURL("test-token", srv.URL)
}

// TestFetchPRReviews_PagesBeyondFirstPage is the direct regression test for the
// production failure: on handarbeit/fabrik#1256 the newest reviews sat past the
// first page, so FetchPRReviews collapsed each author to a stale verdict and
// Pruefer re-reviewed an unchanged head 315 times.
func TestFetchPRReviews_PagesBeyondFirstPage(t *testing.T) {
	const total = restPageSize*2 + 7 // 207: forces a third, short page
	items := make([]map[string]any, 0, total)
	for i := 0; i < total; i++ {
		items = append(items, map[string]any{
			"id":           i + 1,
			"user":         map[string]any{"login": "bot"},
			"state":        "COMMENTED",
			"body":         "review",
			"commit_id":    fmt.Sprintf("sha%03d", i),
			"submitted_at": "2026-08-11T00:00:00Z",
		})
	}
	reqs := 0
	_, c := pagedArrayServer(t, "/repos/o/r/pulls/1/reviews", items, &reqs)

	got, err := c.FetchPRReviews("o", "r", 1)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	// All reviews share one author, so the collapse keeps exactly one entry —
	// and it must be the LAST review, not the last one on page 1.
	if len(got) != 1 {
		t.Fatalf("expected 1 collapsed review, got %d", len(got))
	}
	wantSHA := fmt.Sprintf("sha%03d", total-1)
	if got[0].CommitID != wantSHA {
		t.Errorf("collapsed to commit %q, want %q — the newest review was not seen, "+
			"which is exactly the truncation that drove the #1256 re-review loop", got[0].CommitID, wantSHA)
	}
	if reqs != 3 {
		t.Errorf("served %d requests, want 3 (two full pages + one short)", reqs)
	}
}

// TestFetchPRReviews_SinglePageIssuesOneRequest pins R5/A4: a collection that
// fits on one page must not cost a speculative second round trip.
func TestFetchPRReviews_SinglePageIssuesOneRequest(t *testing.T) {
	items := []map[string]any{{
		"id": 1, "user": map[string]any{"login": "alice"},
		"state": "APPROVED", "commit_id": "abc", "submitted_at": "2026-08-11T00:00:00Z",
	}}
	reqs := 0
	_, c := pagedArrayServer(t, "/repos/o/r/pulls/1/reviews", items, &reqs)

	if _, err := c.FetchPRReviews("o", "r", 1); err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if reqs != 1 {
		t.Errorf("served %d requests for a single-page collection, want exactly 1", reqs)
	}
}

// TestFetchPRReviews_ExactlyOnePageFullStillTerminates pins the boundary the
// other tests step over: a collection of exactly restPageSize records is
// indistinguishable from a full page with more behind it, so the loop must
// fetch one more page, see it empty, and stop — returning the complete set
// rather than looping or dropping records.
func TestFetchPRReviews_ExactlyOnePageFullStillTerminates(t *testing.T) {
	items := make([]map[string]any, 0, restPageSize)
	for i := 0; i < restPageSize; i++ {
		items = append(items, map[string]any{
			"id":           i + 1,
			"user":         map[string]any{"login": fmt.Sprintf("author-%d", i)},
			"state":        "COMMENTED",
			"commit_id":    fmt.Sprintf("sha%03d", i),
			"submitted_at": "2026-08-11T00:00:00Z",
		})
	}
	reqs := 0
	_, c := pagedArrayServer(t, "/repos/o/r/pulls/1/reviews", items, &reqs)

	got, err := c.FetchPRReviews("o", "r", 1)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	// Distinct authors, so the collapse keeps one entry each.
	if len(got) != restPageSize {
		t.Fatalf("got %d reviews, want %d", len(got), restPageSize)
	}
	if reqs != 2 {
		t.Errorf("served %d requests, want 2 (one full page + one empty page proving the end)", reqs)
	}
}

// TestFetchIssueComments_PagesBeyondFirstPage covers the comment-processing
// consequence: GitHub returns issue comments oldest-first, so truncation drops
// the newest — the ones Fabrik reacts to.
func TestFetchIssueComments_PagesBeyondFirstPage(t *testing.T) {
	const total = restPageSize + 5
	items := make([]map[string]any, 0, total)
	for i := 0; i < total; i++ {
		items = append(items, map[string]any{
			"id":         i + 1,
			"body":       fmt.Sprintf("comment-%d", i),
			"created_at": "2026-08-11T00:00:00Z",
			"user":       map[string]any{"login": "human"},
		})
	}
	reqs := 0
	_, c := pagedArrayServer(t, "/repos/o/r/issues/7/comments", items, &reqs)

	got, err := c.FetchIssueComments("o", "r", 7)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	if len(got) != total {
		t.Fatalf("got %d comments, want %d", len(got), total)
	}
	want := fmt.Sprintf("comment-%d", total-1)
	if got[len(got)-1].Body != want {
		t.Errorf("last comment body = %q, want %q — newest comments were truncated away", got[len(got)-1].Body, want)
	}
}

// TestListOpenPRs_PagesBeyondFirstPage replaces the previous "warn at exactly
// 100" behavior with genuine completeness.
func TestListOpenPRs_PagesBeyondFirstPage(t *testing.T) {
	const total = restPageSize + 3
	items := make([]map[string]any, 0, total)
	for i := 0; i < total; i++ {
		items = append(items, map[string]any{
			"number": i + 1,
			"title":  fmt.Sprintf("pr-%d", i),
			"state":  "open",
			"user":   map[string]any{"login": "someone"},
			"head":   map[string]any{"sha": "s", "ref": "b"},
			"base":   map[string]any{"ref": "main"},
		})
	}
	reqs := 0
	_, c := pagedArrayServer(t, "/repos/o/r/pulls", items, &reqs)

	got, err := c.ListOpenPRs("o", "r")
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(got) != total {
		t.Fatalf("got %d open PRs, want %d", len(got), total)
	}
	if got[len(got)-1].Number != total {
		t.Errorf("last PR number = %d, want %d", got[len(got)-1].Number, total)
	}
}

// TestFetchPRFiles_BoundedAgainstEndlessPages covers the hang vector closed in
// the #1539 review: FetchPRFiles predates paginateREST and looped
// `for page := 1; ; page++` with no cap, so a server that never returns a short
// page spun forever while the result slice grew without bound. Verified to be
// non-vacuous by removing the bound — without it this test does not fail, it
// hangs, which is precisely the defect.
func TestFetchPRFiles_BoundedAgainstEndlessPages(t *testing.T) {
	full := make([]map[string]any, restPageSize)
	for i := range full {
		full[i] = map[string]any{"filename": fmt.Sprintf("file%d.go", i)}
	}
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		json.NewEncoder(w).Encode(full) // always a full page: never terminates on its own
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("test-token", srv.URL)
	files, err := c.FetchPRFiles("o", "r", 1)
	if err == nil {
		t.Fatalf("expected an error when the endpoint never returns a short page, got %d files and nil error", len(files))
	}
	if files != nil {
		t.Errorf("expected nil results alongside the error, got %d files", len(files))
	}
	if pages != restMaxPages {
		t.Errorf("served %d pages, want the restMaxPages bound of %d", pages, restMaxPages)
	}
}

// checkRunsServer serves the object-shaped check-runs endpoint. reportedTotal is
// what total_count claims, independent of how many runs are actually served, so
// a mismatch can be simulated.
func checkRunsServer(t *testing.T, runs []map[string]any, reportedTotal int) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/commits/sha1/check-runs", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if per < 1 {
			per = 30
		}
		start := (page - 1) * per
		if start > len(runs) {
			start = len(runs)
		}
		end := start + per
		if end > len(runs) {
			end = len(runs)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": reportedTotal,
			"check_runs":  runs[start:end],
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClientWithBaseURL("test-token", srv.URL)
}

// TestFetchCheckRuns_PagesBeyondDefaultPageSize is the CI-gate consequence: the
// pre-fix code sent no per_page at all, so GitHub's default of 30 applied and a
// commit with more checks read as a complete set.
func TestFetchCheckRuns_PagesBeyondDefaultPageSize(t *testing.T) {
	const total = restPageSize + 11
	runs := make([]map[string]any, 0, total)
	for i := 0; i < total; i++ {
		runs = append(runs, map[string]any{
			"id": i + 1, "name": fmt.Sprintf("check-%d", i),
			"status": "completed", "conclusion": "success",
		})
	}
	c := checkRunsServer(t, runs, total)

	got, err := c.FetchCheckRuns("o", "r", "sha1")
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if len(got) != total {
		t.Fatalf("got %d check runs, want %d — a CI verdict computed from this set would be based on an incomplete view", len(got), total)
	}
	if got[len(got)-1].Name != fmt.Sprintf("check-%d", total-1) {
		t.Errorf("last check run = %q, want check-%d", got[len(got)-1].Name, total-1)
	}
}

// TestFetchCheckRuns_TotalCountMismatchIsAnError pins R3/A2: an incomplete set
// must fail loudly rather than produce a verdict the caller cannot distinguish
// from a complete one.
func TestFetchCheckRuns_TotalCountMismatchIsAnError(t *testing.T) {
	runs := []map[string]any{
		{"id": 1, "name": "only-one", "status": "completed", "conclusion": "success"},
	}
	c := checkRunsServer(t, runs, 5) // server claims 5, serves 1

	_, err := c.FetchCheckRuns("o", "r", "sha1")
	if err == nil {
		t.Fatal("expected an error when total_count exceeds the collected set, got nil — " +
			"an incomplete check-run list must never be returned as if complete")
	}
	if !strings.Contains(err.Error(), "collected 1 of 5") {
		t.Errorf("error %q should name the collected and reported counts", err)
	}
}
