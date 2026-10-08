package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
