package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

const ciTestSHA = "abc123def456"

func ciBoolPtr(b bool) *bool { return &b }

func passRun(name string, id int64) gh.CheckRun {
	return gh.CheckRun{ID: id, Name: name, Status: "completed", Conclusion: "success"}
}

// ciStatusClient builds a mock with an open PR #50 at ciTestSHA and the given runs.
func ciStatusClient(runs []gh.CheckRun) *mockGitHubClient {
	return &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, n int) (int, error) { return 50, nil },
		fetchPRDetailsFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: n, State: "open", HeadSHA: ciTestSHA, MergeableState: "clean"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) { return runs, nil },
	}
}

// runCIStatus writes the file and returns its content ("" when absent).
func runCIStatus(t *testing.T, client *mockGitHubClient, stage *stages.Stage, comment bool) (string, string) {
	t.Helper()
	eng := testEngine(t, client, &mockClaudeInvoker{})
	workDir := t.TempDir()
	item := gh.ProjectItem{Number: 7, Body: "body"}
	eng.writeContextFiles(item, stage, workDir, comment)
	data, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", "ci-status.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", workDir
		}
		t.Fatal(err)
	}
	return string(data), workDir
}

func wantVerdict(t *testing.T, content, verdict string) {
	t.Helper()
	if !strings.Contains(content, "\nverdict: "+verdict+"\n") {
		t.Errorf("want verdict %s, got:\n%s", verdict, content)
	}
}

func TestCIStatus_VerdictMapping(t *testing.T) {
	stage := &stages.Stage{Name: "Validate", Order: 5}
	tests := []struct {
		name    string
		runs    []gh.CheckRun
		suites  []gh.CheckSuite
		suiteEr error
		want    string
	}{
		{"green", []gh.CheckRun{passRun("test", 1)}, nil, nil, "green"},
		{"red", []gh.CheckRun{{ID: 1, Name: "test", Status: "completed", Conclusion: "failure"}}, nil, nil, "red"},
		{"pending run", []gh.CheckRun{{ID: 1, Name: "test", Status: "in_progress"}}, nil, nil, "pending"},
		{"zero runs is none", nil, nil, nil, "none"},
		{"suite hold is pending", []gh.CheckRun{passRun("test", 1)},
			[]gh.CheckSuite{{ID: 9, Status: "in_progress", LatestCheckRunsCount: 1}}, nil, "pending"},
		{"suite read error is not green", []gh.CheckRun{passRun("test", 1)}, nil, errors.New("boom"), "pending"},
		{"cancelled demoted", []gh.CheckRun{passRun("a", 1), {ID: 2, Name: "b", Status: "completed", Conclusion: "cancelled"}}, nil, nil, "pending"},
		{"unknown conclusion demoted", []gh.CheckRun{{ID: 1, Name: "a", Status: "completed", Conclusion: "stale"}}, nil, nil, "pending"},
		{"superseded failure with newer success", []gh.CheckRun{
			{ID: 1, Name: "test", Status: "completed", Conclusion: "failure"}, passRun("test", 2)}, nil, nil, "green"},
		{"neutral is passing", []gh.CheckRun{
			passRun("a", 1),
			{ID: 2, Name: "b", Status: "completed", Conclusion: "neutral"}}, nil, nil, "green"},
		{"skipped demoted (a skipped test job proves nothing ran)", []gh.CheckRun{
			passRun("lint", 1),
			{ID: 2, Name: "test", Status: "completed", Conclusion: "skipped"}}, nil, nil, "pending"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := ciStatusClient(tc.runs)
			client.fetchCheckSuitesFn = func(owner, repo, sha string) ([]gh.CheckSuite, error) {
				return tc.suites, tc.suiteEr
			}
			content, _ := runCIStatus(t, client, stage, false)
			wantVerdict(t, content, tc.want)
			if !strings.Contains(content, "head_sha: "+ciTestSHA+"\n") || !strings.Contains(content, "pr: 50\n") {
				t.Errorf("missing pr/head_sha:\n%s", content)
			}
		})
	}
}

// Zero runs must be none even when mergeable_state would classify green via
// the ADR-933 path inside classifyLandingCI.
func TestCIStatus_ZeroRunsNeverGreenDespiteMergeableState(t *testing.T) {
	client := ciStatusClient(nil)
	client.fetchPRDetailsFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
		return &gh.PRDetails{Number: n, State: "open", HeadSHA: ciTestSHA, MergeableState: "clean"}, nil
	}
	content, _ := runCIStatus(t, client, &stages.Stage{Name: "Validate", Order: 5}, false)
	wantVerdict(t, content, "none")
}

func TestCIStatus_ReadErrorsAreUnknown(t *testing.T) {
	stage := &stages.Stage{Name: "Validate", Order: 5}

	t.Run("check runs error", func(t *testing.T) {
		client := ciStatusClient(nil)
		client.fetchCheckRunsFn = func(owner, repo, sha string) ([]gh.CheckRun, error) { return nil, errors.New("boom") }
		content, _ := runCIStatus(t, client, stage, false)
		wantVerdict(t, content, "unknown")
	})
	t.Run("PR details error", func(t *testing.T) {
		client := ciStatusClient(nil)
		client.fetchPRDetailsFn = func(owner, repo string, n int) (*gh.PRDetails, error) { return nil, errors.New("boom") }
		content, _ := runCIStatus(t, client, stage, false)
		wantVerdict(t, content, "unknown")
	})
	t.Run("nil PR details", func(t *testing.T) {
		client := ciStatusClient(nil)
		client.fetchPRDetailsFn = func(owner, repo string, n int) (*gh.PRDetails, error) { return nil, nil }
		content, _ := runCIStatus(t, client, stage, false)
		wantVerdict(t, content, "unknown")
	})
	t.Run("empty head SHA", func(t *testing.T) {
		client := ciStatusClient(nil)
		client.fetchPRDetailsFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: n, State: "open"}, nil
		}
		content, _ := runCIStatus(t, client, stage, false)
		wantVerdict(t, content, "unknown")
	})
}

func TestCIStatus_CIGatedFollowsWaitForCI(t *testing.T) {
	for _, tc := range []struct {
		name string
		wfc  *bool
		want string
	}{
		{"true", ciBoolPtr(true), "ci_gated: true\n"},
		{"false", ciBoolPtr(false), "ci_gated: false\n"},
		{"nil", nil, "ci_gated: false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := &stages.Stage{Name: "Validate", Order: 5, WaitForCI: tc.wfc}
			content, _ := runCIStatus(t, ciStatusClient([]gh.CheckRun{passRun("t", 1)}), stage, false)
			if !strings.Contains(content, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, content)
			}
		})
	}
}

func TestCIStatus_NoPRNoFileAndStaleRemoved(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, n int) (int, error) { return 0, nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	workDir := t.TempDir()
	ctxDir := filepath.Join(workDir, ".fabrik-context")
	if err := os.MkdirAll(ctxDir, 0755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(ctxDir, "ci-status.md")
	if err := os.WriteFile(stale, []byte("verdict: green\n"), 0644); err != nil {
		t.Fatal(err)
	}
	eng.writeContextFiles(gh.ProjectItem{Number: 7}, &stages.Stage{Name: "Implement", Order: 3}, workDir, false)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale ci-status.md should be removed, stat err = %v", err)
	}
}

func TestCIStatus_FindPRErrorRemovesStaleFile(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, n int) (int, error) { return 0, errors.New("boom") },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	workDir := t.TempDir()
	ctxDir := filepath.Join(workDir, ".fabrik-context")
	if err := os.MkdirAll(ctxDir, 0755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(ctxDir, "ci-status.md")
	if err := os.WriteFile(stale, []byte("verdict: green\n"), 0644); err != nil {
		t.Fatal(err)
	}
	eng.writeContextFiles(gh.ProjectItem{Number: 7}, &stages.Stage{Name: "Implement", Order: 3}, workDir, false)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale ci-status.md should be removed on lookup error, stat err = %v", err)
	}
}

func TestCIStatus_ClosedOrMergedPRNoFile(t *testing.T) {
	for _, d := range []*gh.PRDetails{
		{Number: 50, State: "closed", HeadSHA: ciTestSHA},
		{Number: 50, State: "closed", Merged: true, HeadSHA: ciTestSHA},
	} {
		client := ciStatusClient([]gh.CheckRun{passRun("t", 1)})
		d := d
		client.fetchPRDetailsFn = func(owner, repo string, n int) (*gh.PRDetails, error) { return d, nil }
		content, _ := runCIStatus(t, client, &stages.Stage{Name: "Validate", Order: 5}, false)
		if content != "" {
			t.Errorf("no file expected for state=%s merged=%t, got:\n%s", d.State, d.Merged, content)
		}
	}
}

func TestCIStatus_ReadsUseHeadSHA(t *testing.T) {
	var runsSHA, suitesSHA string
	client := ciStatusClient([]gh.CheckRun{passRun("t", 1)})
	client.fetchCheckRunsFn = func(owner, repo, sha string) ([]gh.CheckRun, error) {
		runsSHA = sha
		return []gh.CheckRun{passRun("t", 1)}, nil
	}
	client.fetchCheckSuitesFn = func(owner, repo, sha string) ([]gh.CheckSuite, error) {
		suitesSHA = sha
		return nil, nil
	}
	runCIStatus(t, client, &stages.Stage{Name: "Validate", Order: 5}, false)
	if runsSHA != ciTestSHA {
		t.Errorf("FetchCheckRuns sha = %q, want %q", runsSHA, ciTestSHA)
	}
	if suitesSHA != ciTestSHA {
		t.Errorf("classifier's suite hold sha = %q, want %q", suitesSHA, ciTestSHA)
	}
}

func TestCIStatus_WrittenInBothInvocationPaths(t *testing.T) {
	for _, comment := range []bool{false, true} {
		content, _ := runCIStatus(t, ciStatusClient([]gh.CheckRun{passRun("t", 1)}),
			&stages.Stage{Name: "Validate", Order: 5}, comment)
		wantVerdict(t, content, "green")
	}
}

// The pr-description block ends in bare returns; ci-status.md must be written
// ahead of it so a failed GetIssueBody cannot skip it.
func TestCIStatus_WrittenEvenWhenPRBodyFetchFails(t *testing.T) {
	client := ciStatusClient([]gh.CheckRun{passRun("t", 1)})
	client.getIssueBodyFn = func(owner, repo string, n int) (string, error) { return "", errors.New("boom") }
	content, _ := runCIStatus(t, client, &stages.Stage{Name: "Review", Order: 4, PostToPR: true}, false)
	wantVerdict(t, content, "green")
}

func TestCIStatus_ListsEveryRun(t *testing.T) {
	client := ciStatusClient([]gh.CheckRun{
		passRun("build", 1),
		{ID: 2, Name: "test", Status: "completed", Conclusion: "failure"},
	})
	content, _ := runCIStatus(t, client, &stages.Stage{Name: "Validate", Order: 5}, false)
	for _, want := range []string{"| build | completed | success |", "| test | completed | failure |", "written_at: "} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in:\n%s", want, content)
		}
	}
}
