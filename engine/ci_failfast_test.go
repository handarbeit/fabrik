package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// #2614: a cancelled run with a failed step is red.

// jobsClient layers the optional workflowJobFetcher capability on the mock.
type jobsClient struct {
	*mockGitHubClient
	mu    sync.Mutex
	jobs  map[int64][]gh.WorkflowJob
	err   error
	calls []int64
}

func (j *jobsClient) FetchWorkflowRunJobs(owner, repo string, runID int64) ([]gh.WorkflowJob, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, runID)
	if j.err != nil {
		return nil, j.err
	}
	return j.jobs[runID], nil
}

func failFastEngine(t *testing.T, jc *jobsClient) *Engine {
	t.Helper()
	eng := testEngineForMerge(t, jc.mockGitHubClient)
	eng.client = jc
	return eng
}

func failedStepJobs(jobID int64) []gh.WorkflowJob {
	return []gh.WorkflowJob{{
		ID: jobID, Name: "test", Status: "completed", Conclusion: "cancelled",
		Steps: []gh.WorkflowStep{
			{Name: "Set up job", Number: 1, Conclusion: "success"},
			{Name: "Run tests", Number: 2, Conclusion: "failure"},
			{Name: "Cancel run", Number: 3, Conclusion: "success"},
		},
	}}
}

func TestPromoteFailFast_FailedStepPromotes(t *testing.T) {
	jc := &jobsClient{mockGitHubClient: &mockGitHubClient{}, jobs: map[int64][]gh.WorkflowJob{900: failedStepJobs(2)}}
	eng := failFastEngine(t, jc)
	in := []gh.CheckRun{
		{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
		cancelledCheck(2),
	}
	out := eng.promoteFailFast("owner", "repo", "sha1", in)
	if out[1].Conclusion != "failure" || out[1].ObservedConclusion != "cancelled" ||
		out[1].FailedJob != "test" || out[1].FailedStep != "Run tests" {
		t.Fatalf("promoted run = %+v", out[1])
	}
	if out[0].Conclusion != "success" {
		t.Errorf("unrelated run changed: %+v", out[0])
	}
	if in[1].Conclusion != "cancelled" {
		t.Error("input slice was mutated")
	}
	if got := failedStepText(out[1]); !strings.Contains(got, "Run tests") || !strings.Contains(got, "test") {
		t.Errorf("failedStepText = %q", got)
	}
	if failedStepText(in[1]) != "" {
		t.Error("an unpromoted run must render no failed-step text")
	}
}

func TestPromoteFailFast_NoFailedStepStaysCancelled(t *testing.T) {
	jobs := []gh.WorkflowJob{{ID: 2, Name: "test", Status: "completed", Conclusion: "cancelled",
		Steps: []gh.WorkflowStep{{Name: "Run tests", Conclusion: "cancelled"}, {Name: "x", Conclusion: "skipped"}}}}
	jc := &jobsClient{mockGitHubClient: &mockGitHubClient{}, jobs: map[int64][]gh.WorkflowJob{900: jobs}}
	eng := failFastEngine(t, jc)
	in := []gh.CheckRun{cancelledCheck(2)}
	out := eng.promoteFailFast("owner", "repo", "sha1", in)
	if out[0].Conclusion != "cancelled" || out[0].ObservedConclusion != "" {
		t.Fatalf("run = %+v, want unchanged", out[0])
	}
}

func TestPromoteFailFast_SiblingJobFailurePromotesCancelledRun(t *testing.T) {
	// Run-level (R1): the failed step is in another job of the same run.
	jobs := []gh.WorkflowJob{
		{ID: 5, Name: "unit", Status: "completed", Conclusion: "failure", Steps: []gh.WorkflowStep{{Name: "go test", Conclusion: "failure"}}},
		{ID: 2, Name: "test", Status: "completed", Conclusion: "cancelled", Steps: []gh.WorkflowStep{{Name: "Run tests", Conclusion: "cancelled"}}},
	}
	jc := &jobsClient{mockGitHubClient: &mockGitHubClient{}, jobs: map[int64][]gh.WorkflowJob{900: jobs}}
	eng := failFastEngine(t, jc)
	out := eng.promoteFailFast("owner", "repo", "sha1", []gh.CheckRun{cancelledCheck(2)})
	if out[0].Conclusion != "failure" || out[0].FailedJob != "unit" || out[0].FailedStep != "go test" {
		t.Fatalf("run = %+v", out[0])
	}
}

func TestFailedStepOf_PrefersOwnJob(t *testing.T) {
	jobs := []gh.WorkflowJob{
		{ID: 1, Name: "a", Steps: []gh.WorkflowStep{{Name: "sa", Conclusion: "failure"}}},
		{ID: 2, Name: "b", Steps: []gh.WorkflowStep{{Name: "sb", Conclusion: "failure"}}},
	}
	if j, s, ok := failedStepOf(jobs, gh.CheckRun{ID: 2, Name: "zzz"}); !ok || j != "b" || s != "sb" {
		t.Errorf("by ID: %q %q %v", j, s, ok)
	}
	if j, _, ok := failedStepOf(jobs, gh.CheckRun{ID: 99, Name: "b"}); !ok || j != "b" {
		t.Errorf("by name: %q %v", j, ok)
	}
	if j, _, ok := failedStepOf(jobs, gh.CheckRun{ID: 99, Name: "zzz"}); !ok || j != "a" {
		t.Errorf("fallback first failed job: %q %v", j, ok)
	}
	if _, _, ok := failedStepOf(nil, gh.CheckRun{}); ok {
		t.Error("no jobs must not report a failed step")
	}
}

func TestPromoteFailFast_ReadFailureKeepsClassificationAndLogsOncePerHead(t *testing.T) {
	for _, rerr := range []error{errors.New("boom"), fmt.Errorf("x: %w", gh.ErrForbidden)} {
		jc := &jobsClient{mockGitHubClient: &mockGitHubClient{}, err: rerr}
		eng := failFastEngine(t, jc)
		var lines []string
		var mu sync.Mutex
		eng.SetLogSinkForTest(func(_ int, _, msg string) { mu.Lock(); lines = append(lines, msg); mu.Unlock() })
		in := []gh.CheckRun{cancelledCheck(2)}
		for i := 0; i < 3; i++ {
			out := eng.promoteFailFast("owner", "repo", "sha1", in)
			if out[0].Conclusion != "cancelled" {
				t.Fatalf("err=%v: run = %+v, want today's classification", rerr, out[0])
			}
		}
		n := 0
		for _, l := range lines {
			if strings.Contains(l, "could not read the jobs") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("err=%v: logged %d times for one head, want 1 (%v)", rerr, n, lines)
		}
		// A new head logs again.
		eng.promoteFailFast("owner", "repo", "sha2", in)
		n = 0
		for _, l := range lines {
			if strings.Contains(l, "could not read the jobs") {
				n++
			}
		}
		if n != 2 {
			t.Errorf("err=%v: new head logged total %d, want 2", rerr, n)
		}
		// A failed read is never cached as "no failed step".
		jc.mu.Lock()
		jc.err = nil
		jc.jobs = map[int64][]gh.WorkflowJob{900: failedStepJobs(2)}
		jc.mu.Unlock()
		if out := eng.promoteFailFast("owner", "repo", "sha1", in); out[0].Conclusion != "failure" {
			t.Errorf("err=%v: after recovery run = %+v, want promoted", rerr, out[0])
		}
	}
}

func TestPromoteFailFast_UnsupportedClientAndThirdPartyCheck(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{}) // no workflowJobFetcher
	in := []gh.CheckRun{cancelledCheck(2)}
	if out := eng.promoteFailFast("owner", "repo", "sha1", in); out[0].Conclusion != "cancelled" {
		t.Errorf("unsupported client: %+v", out[0])
	}
	jc := &jobsClient{mockGitHubClient: &mockGitHubClient{}, jobs: map[int64][]gh.WorkflowJob{900: failedStepJobs(2)}}
	eng = failFastEngine(t, jc)
	third := gh.CheckRun{ID: 3, Name: "ext", Status: "completed", Conclusion: "cancelled", DetailsURL: "https://ci.example.com/build/3"}
	if out := eng.promoteFailFast("owner", "repo", "sha1", []gh.CheckRun{third}); out[0].Conclusion != "cancelled" || len(jc.calls) != 0 {
		t.Errorf("third-party check: %+v, calls %v", out[0], jc.calls)
	}
}

func TestPromoteFailFast_OnlyCompletedCancelledLatestRead(t *testing.T) {
	jc := &jobsClient{mockGitHubClient: &mockGitHubClient{}, jobs: map[int64][]gh.WorkflowJob{900: failedStepJobs(5)}}
	eng := failFastEngine(t, jc)
	in := []gh.CheckRun{
		{ID: 1, Name: "a", Status: "completed", Conclusion: "success", DetailsURL: "https://github.com/o/r/actions/runs/900/job/1"},
		{ID: 2, Name: "b", Status: "completed", Conclusion: "stale", DetailsURL: "https://github.com/o/r/actions/runs/900/job/2"},
		{ID: 3, Name: "c", Status: "in_progress", Conclusion: "", DetailsURL: "https://github.com/o/r/actions/runs/900/job/3"},
		// superseded cancelled run (older ID, same name as a later green one)
		{ID: 4, Name: "d", Status: "completed", Conclusion: "cancelled", DetailsURL: "https://github.com/o/r/actions/runs/900/job/4"},
		{ID: 6, Name: "d", Status: "completed", Conclusion: "success", DetailsURL: "https://github.com/o/r/actions/runs/901/job/6"},
	}
	out := eng.promoteFailFast("o", "r", "sha", in)
	if len(jc.calls) != 0 {
		t.Errorf("jobs read %v, want none", jc.calls)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("run %d changed: %+v", i, out[i])
		}
	}
}

func TestPromoteFailFast_MemoByCheckRunIDAndRerunReads(t *testing.T) {
	jc := &jobsClient{mockGitHubClient: &mockGitHubClient{}, jobs: map[int64][]gh.WorkflowJob{900: failedStepJobs(2)}}
	eng := failFastEngine(t, jc)
	in := []gh.CheckRun{cancelledCheck(2)}
	eng.promoteFailFast("owner", "repo", "sha1", in)
	eng.promoteFailFast("owner", "repo", "sha1", in)
	if len(jc.calls) != 1 {
		t.Errorf("reads = %d, want 1 (memo hit)", len(jc.calls))
	}
	// A re-run's new check-run ID is read afresh.
	rerun := cancelledCheck(20)
	jc.jobs[900] = []gh.WorkflowJob{{ID: 20, Name: "test", Status: "completed", Steps: []gh.WorkflowStep{{Name: "Run tests", Conclusion: "cancelled"}}}}
	if out := eng.promoteFailFast("owner", "repo", "sha1", []gh.CheckRun{rerun}); out[0].Conclusion != "cancelled" {
		t.Errorf("re-run attempt served a stale promotion: %+v", out[0])
	}
	if len(jc.calls) != 2 {
		t.Errorf("reads = %d, want 2", len(jc.calls))
	}
}

func TestPromoteFailFast_NegativeNotMemoisedWhileJobsRunning(t *testing.T) {
	running := []gh.WorkflowJob{
		{ID: 2, Name: "test", Status: "completed", Steps: []gh.WorkflowStep{{Name: "Run tests", Conclusion: "cancelled"}}},
		{ID: 3, Name: "other", Status: "in_progress"},
	}
	jc := &jobsClient{mockGitHubClient: &mockGitHubClient{}, jobs: map[int64][]gh.WorkflowJob{900: running}}
	eng := failFastEngine(t, jc)
	in := []gh.CheckRun{cancelledCheck(2)}
	if out := eng.promoteFailFast("owner", "repo", "sha1", in); out[0].Conclusion != "cancelled" {
		t.Fatalf("run = %+v", out[0])
	}
	jc.jobs[900] = failedStepJobs(2)
	if out := eng.promoteFailFast("owner", "repo", "sha1", in); out[0].Conclusion != "failure" {
		t.Errorf("a still-running run's negative answer was cached: %+v", out[0])
	}
}

func TestPromoteFailFast_NeutralisedIsInert(t *testing.T) {
	jc := &jobsClient{mockGitHubClient: &mockGitHubClient{}, jobs: map[int64][]gh.WorkflowJob{900: failedStepJobs(2)}}
	eng := failFastEngine(t, jc)
	restore := eng.SetFailFastPromotionDisabledForTest()
	defer restore()
	if out := eng.promoteFailFast("owner", "repo", "sha1", []gh.CheckRun{cancelledCheck(2)}); out[0].Conclusion != "cancelled" {
		t.Errorf("neutralised promotion still promoted: %+v", out[0])
	}
}

// Every non-test FetchCheckRuns call in engine/ must feed promoteFailFast or
// carry a `// failfast-audited: <reason>` marker, so a new CI consumer cannot
// silently skip the promotion (#2614).
func TestFetchCheckRunsCallersPromoteOrAreAudited(t *testing.T) {
	bad := scanFailFastCallers(t, ".")
	if len(bad) > 0 {
		t.Errorf("FetchCheckRuns call sites neither promoted nor audited:\n%s", strings.Join(bad, "\n"))
	}
}

func TestFetchCheckRunsScan_DeliberateViolation(t *testing.T) {
	dir := t.TempDir()
	src := "package x\nfunc f() {\n\truns, _ := e.client.FetchCheckRuns(a, b, c)\n\t_ = runs\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if bad := scanFailFastCallers(t, dir); len(bad) != 1 {
		t.Fatalf("scan found %v, want exactly the one violation", bad)
	}
	ok := "package x\nfunc f() {\n\t// failfast-audited: test\n\truns, _ := e.client.FetchCheckRuns(a, b, c)\n\t_ = runs\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(ok), 0o644); err != nil {
		t.Fatal(err)
	}
	if bad := scanFailFastCallers(t, dir); len(bad) != 0 {
		t.Fatalf("audited call flagged: %v", bad)
	}
}

var failFastCallRe = regexp.MustCompile(`\.FetchCheckRuns\(`)

func scanFailFastCallers(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if !failFastCallRe.MatchString(l) || strings.HasPrefix(strings.TrimSpace(l), "//") {
				continue
			}
			if strings.Contains(l, "interface") || strings.HasPrefix(strings.TrimSpace(l), "FetchCheckRuns(") {
				continue
			}
			// The marker or the promotion must sit on the call's line or within
			// the ten lines that follow it (the statement that consumes the result)
			// or the three lines above it.
			ok := false
			for j := max(0, i-3); j < len(lines) && j <= i+10; j++ {
				if strings.Contains(lines[j], "promoteFailFast(") || strings.Contains(lines[j], "failfast-audited:") {
					ok = true
					break
				}
			}
			if !ok {
				bad = append(bad, fmt.Sprintf("%s:%d: %s", filepath.Base(f), i+1, strings.TrimSpace(l)))
			}
		}
	}
	return bad
}
