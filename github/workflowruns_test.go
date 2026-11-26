package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestFetchWorkflowRuns_Recorded serves the fixture shapes for the run list
// and the per-run job count and asserts the exact paths and the decoded
// startup-failure / healthy distinction (#2052 R1, R7).
func TestFetchWorkflowRuns_Recorded(t *testing.T) {
	runs := loadRecording(t, "fetch_workflow_runs")
	jobs := loadRecording(t, "fetch_workflow_run_jobs")
	var jobCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head_sha") != "sha1" {
			t.Errorf("head_sha = %q", r.URL.Query().Get("head_sha"))
		}
		w.Write(runs)
	})
	mux.HandleFunc("/repos/o/r/actions/runs/37244372904/jobs", func(w http.ResponseWriter, r *http.Request) {
		jobCalls.Add(1)
		w.Write(jobs)
	})
	mux.HandleFunc("/repos/o/r/actions/runs/37244372903/jobs", func(w http.ResponseWriter, r *http.Request) {
		t.Error("jobs must not be fetched for a startup_failure run")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClientWithBaseURL("tok", srv.URL)
	got, err := c.FetchWorkflowRuns("o", "r", "sha1")
	if err != nil {
		t.Fatalf("FetchWorkflowRuns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d runs", len(got))
	}
	if got[0].ID != 37244372903 || got[0].Conclusion != "startup_failure" || got[0].JobCount != 0 || got[0].Name != "CI" {
		t.Errorf("run[0] = %+v", got[0])
	}
	if got[0].HTMLURL == "" || got[0].CreatedAt.IsZero() {
		t.Errorf("run[0] missing url/created_at: %+v", got[0])
	}
	if got[1].JobCount != 3 || got[1].Conclusion != "success" {
		t.Errorf("run[1] = %+v", got[1])
	}
	if jobCalls.Load() != 1 {
		t.Errorf("job-count calls = %d, want 1", jobCalls.Load())
	}
}

func TestFetchWorkflowRuns_IncompleteIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":3,"workflow_runs":[{"id":1,"status":"in_progress"}]}`))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("tok", srv.URL)
	if _, err := c.FetchWorkflowRuns("o", "r", "sha1"); err == nil {
		t.Fatal("expected error for a truncated run list")
	}
}

func TestFetchWorkflowRuns_PermissionRefusedIsClassifiable(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{403, ErrForbidden}, {404, ErrNotFound}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		}))
		c := NewClientWithBaseURL("tok", srv.URL)
		_, err := c.FetchWorkflowRuns("o", "r", "sha1")
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want errors.Is %v", tc.status, err, tc.want)
		}
	}
}

func TestRerunFailedJobs_Recorded(t *testing.T) {
	body := loadRecording(t, "rerun_failed_jobs")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/repos/o/r/actions/runs/42/rerun-failed-jobs" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write(body)
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("tok", srv.URL)
	if err := c.RerunFailedJobs("o", "r", 42); err != nil {
		t.Fatalf("RerunFailedJobs: %v", err)
	}
}

func TestRerunFailedJobs_ForbiddenIsClassifiable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"This workflow run cannot be retried"}`))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("tok", srv.URL)
	err := c.RerunFailedJobs("o", "r", 42)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

// A rate-limited 403 must stay a *RateLimitError, never ErrForbidden — the
// permission-degrade path must not swallow rate limiting.
func TestForbiddenSentinel_DoesNotClaimRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("tok", srv.URL)
	err := c.RerunFailedJobs("o", "r", 42)
	if errors.Is(err, ErrForbidden) {
		t.Fatalf("rate-limited 403 classified as ErrForbidden: %v", err)
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
}

func TestActionsRunIDFromDetailsURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		id  int64
		ok  bool
	}{
		{"https://github.com/o/r/actions/runs/12345/job/678", 12345, true},
		{"https://github.com/o/r/actions/runs/12345/job/678?pr=9", 12345, true},
		{"https://ci.example.com/build/9", 0, false},
		{"https://github.com/o/r/runs/678", 0, false},
		{"", 0, false},
	} {
		id, ok := ActionsRunIDFromDetailsURL(tc.url)
		if id != tc.id || ok != tc.ok {
			t.Errorf("%q = (%d,%v), want (%d,%v)", tc.url, id, ok, tc.id, tc.ok)
		}
	}
}

// TestFetchWorkflowRunJobs_DecodesSteps pins the request shape and the step
// decoding the fail-fast promotion reads (#2614).
func TestFetchWorkflowRunJobs_DecodesSteps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/actions/runs/42/jobs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("filter") != "latest" {
			t.Errorf("filter = %q, want latest", r.URL.Query().Get("filter"))
		}
		w.Write([]byte(`{"total_count":2,"jobs":[
			{"id":7,"name":"test","status":"completed","conclusion":"cancelled","steps":[
				{"name":"Set up job","number":1,"status":"completed","conclusion":"success"},
				{"name":"Run tests","number":2,"status":"completed","conclusion":"failure"}]},
			{"id":8,"name":"lint","status":"completed","conclusion":"cancelled","steps":[]}]}`))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("tok", srv.URL)
	jobs, err := c.FetchWorkflowRunJobs("o", "r", 42)
	if err != nil {
		t.Fatalf("FetchWorkflowRunJobs: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID != 7 || jobs[0].Name != "test" || jobs[0].Conclusion != "cancelled" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if len(jobs[0].Steps) != 2 || jobs[0].Steps[1].Name != "Run tests" || jobs[0].Steps[1].Conclusion != "failure" || jobs[0].Steps[1].Number != 2 {
		t.Errorf("steps = %+v", jobs[0].Steps)
	}
}

func TestFetchWorkflowRunJobs_IncompleteIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":3,"jobs":[{"id":1,"name":"a","status":"completed"}]}`))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("tok", srv.URL)
	if _, err := c.FetchWorkflowRunJobs("o", "r", 42); err == nil {
		t.Fatal("expected error for an incomplete job list")
	}
}

func TestFetchWorkflowRunJobs_PaginatesAndVerifies(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		var b strings.Builder
		n := 100
		if r.URL.Query().Get("page") == "2" {
			n = 5
		}
		b.WriteString(`{"total_count":105,"jobs":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"id":1,"name":"j","status":"completed"}`)
		}
		b.WriteString(`]}`)
		w.Write([]byte(b.String()))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("tok", srv.URL)
	jobs, err := c.FetchWorkflowRunJobs("o", "r", 42)
	if err != nil {
		t.Fatalf("FetchWorkflowRunJobs: %v", err)
	}
	if len(jobs) != 105 || pages.Load() != 2 {
		t.Errorf("got %d jobs over %d pages, want 105 over 2", len(jobs), pages.Load())
	}
}

func TestFetchWorkflowRunJobs_PermissionRefusedIsClassifiable(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{403, ErrForbidden}, {404, ErrNotFound}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		}))
		c := NewClientWithBaseURL("tok", srv.URL)
		_, err := c.FetchWorkflowRunJobs("o", "r", 42)
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want errors.Is %v", tc.status, err, tc.want)
		}
	}
}
