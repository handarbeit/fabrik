package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// runCIStatusWith writes ci-status.md for stage using client (which may layer the
// optional annotation capability) and a stage list of the caller's choosing.
func runCIStatusWith(t *testing.T, base *mockGitHubClient, client GitHubClient, all []*stages.Stage, stage *stages.Stage) string {
	t.Helper()
	eng := testEngine(t, base, &mockClaudeInvoker{})
	eng.client = client
	if all != nil {
		eng.cfg.Stages = all
	}
	workDir := t.TempDir()
	eng.writeContextFiles(gh.ProjectItem{Number: 7, Body: "body"}, stage, workDir, false)
	data, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", "ci-status.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func failRun(name string, id int64) gh.CheckRun {
	return gh.CheckRun{ID: id, Name: name, Status: "completed", Conclusion: "failure",
		DetailsURL: fmt.Sprintf("https://ci.example/%s", name)}
}

var ciGatedValidate = &stages.Stage{Name: "Validate", Order: 5, WaitForCI: ciBoolPtr(true)}

func TestCIStatus_RedListsFailingCheckDetails(t *testing.T) {
	cr := failRun("worker-shaped", 2)
	cr.OutputTitle = "1 test failed"
	cr.OutputSummary = "TestMultiBed broke"
	base := ciStatusClient([]gh.CheckRun{passRun("unit", 1), cr})
	ac := &annotatingClient{mockGitHubClient: base, annotations: map[int64][]gh.CheckRunAnnotation{
		2: {{Title: "FAIL", Message: "TestMultiBedRunEndToEnd: expected 1 got 2"}},
	}}
	content := runCIStatusWith(t, base, ac, nil, ciGatedValidate)
	wantVerdict(t, content, "red")
	for _, want := range []string{
		"## Failing checks", "### worker-shaped", "- conclusion: failure",
		"- details: https://ci.example/worker-shaped", "title: 1 test failed",
		"summary: TestMultiBed broke", "FAIL: TestMultiBedRunEndToEnd: expected 1 got 2",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in:\n%s", want, content)
		}
	}
	if strings.Contains(content, "### unit") {
		t.Errorf("passing check listed as failing:\n%s", content)
	}
	if len(ac.calls) != 1 || ac.calls[0] != 2 {
		t.Errorf("annotation calls = %v, want only the failing run", ac.calls)
	}
}

func TestCIStatus_RerunThatPassedIsNotListed(t *testing.T) {
	base := ciStatusClient([]gh.CheckRun{failRun("a", 1), passRun("a", 2), failRun("b", 3)})
	content := runCIStatusWith(t, base, base, nil, ciGatedValidate)
	wantVerdict(t, content, "red")
	if strings.Contains(content, "### a\n") || !strings.Contains(content, "### b\n") {
		t.Errorf("want only b failing:\n%s", content)
	}
}

func TestCIStatus_FailingCapAndAnnotationCap(t *testing.T) {
	var runs []gh.CheckRun
	for i := 0; i < 13; i++ {
		runs = append(runs, failRun(fmt.Sprintf("job%02d", i), int64(i+1)))
	}
	base := ciStatusClient(runs)
	ac := &annotatingClient{mockGitHubClient: base}
	content := runCIStatusWith(t, base, ac, nil, ciGatedValidate)
	if n := strings.Count(content, "\n### "); n != ciStatusMaxFailing {
		t.Errorf("listed %d failing checks, want %d", n, ciStatusMaxFailing)
	}
	if !strings.Contains(content, "3 more failing check(s) omitted") {
		t.Errorf("missing omitted line:\n%s", content)
	}
	if len(ac.calls) != ciStatusMaxAnnotated {
		t.Errorf("annotation calls = %d, want %d", len(ac.calls), ciStatusMaxAnnotated)
	}
}

func TestCIStatus_AnnotationErrorKeepsCheckLevelDetails(t *testing.T) {
	cr := failRun("lint", 4)
	cr.OutputSummary = "3 issues"
	base := ciStatusClient([]gh.CheckRun{cr})
	ac := &annotatingClient{mockGitHubClient: base, err: errors.New("403")}
	content := runCIStatusWith(t, base, ac, nil, ciGatedValidate)
	wantVerdict(t, content, "red")
	for _, want := range []string{"### lint", "- conclusion: failure", "- details: https://ci.example/lint", "summary: 3 issues"} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in:\n%s", want, content)
		}
	}
}

func TestCIStatus_NoAnnotationCapabilityBehavesTheSame(t *testing.T) {
	cr := failRun("lint", 4)
	cr.OutputSummary = "3 issues"
	base := ciStatusClient([]gh.CheckRun{cr})
	content := runCIStatusWith(t, base, base, nil, ciGatedValidate)
	for _, want := range []string{"### lint", "summary: 3 issues"} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in:\n%s", want, content)
		}
	}
}

func TestCIStatus_NonRedHasNoFailingSectionAndNoAnnotationReads(t *testing.T) {
	cases := map[string][]gh.CheckRun{
		"green":   {passRun("t", 1)},
		"pending": {{ID: 1, Name: "t", Status: "in_progress"}},
		"none":    nil,
	}
	for want, runs := range cases {
		t.Run(want, func(t *testing.T) {
			base := ciStatusClient(runs)
			ac := &annotatingClient{mockGitHubClient: base}
			content := runCIStatusWith(t, base, ac, nil, ciGatedValidate)
			wantVerdict(t, content, want)
			if strings.Contains(content, "## Failing checks") {
				t.Errorf("unexpected failing section:\n%s", content)
			}
			if len(ac.calls) != 0 {
				t.Errorf("annotation calls = %v, want none", ac.calls)
			}
		})
	}
	t.Run("unknown", func(t *testing.T) {
		base := ciStatusClient(nil)
		base.fetchCheckRunsFn = func(owner, repo, sha string) ([]gh.CheckRun, error) { return nil, errors.New("boom") }
		ac := &annotatingClient{mockGitHubClient: base}
		content := runCIStatusWith(t, base, ac, nil, ciGatedValidate)
		wantVerdict(t, content, "unknown")
		if strings.Contains(content, "## Failing checks") || len(ac.calls) != 0 {
			t.Errorf("unexpected failing section/reads:\n%s", content)
		}
	})
}

func TestCIStatus_RedWithoutIdentifiableRun(t *testing.T) {
	// A red verdict can come from a required status context, with no failing run.
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	extras := &ciStatusExtras{}
	eng.collectFailingChecks(gh.ProjectItem{Number: 7}, "o", "r", []gh.CheckRun{passRun("t", 1)}, extras)
	if !extras.redWithoutRun {
		t.Fatal("redWithoutRun should be set")
	}
	body := renderCIStatus(50, ciTestSHA, ciVerdictRed, true, eng.now(), nil, extras)
	if !strings.Contains(body, "## Failing checks") || !strings.Contains(body, "no failing check run identified") {
		t.Errorf("want the explanatory note:\n%s", body)
	}
}

func TestCIStatus_GateAhead(t *testing.T) {
	impl := &stages.Stage{Name: "Implement", Order: 3}
	review := &stages.Stage{Name: "Review", Order: 4, WaitForCI: ciBoolPtr(false)}
	done := &stages.Stage{Name: "Done", Order: 9, CleanupWorktree: true, WaitForCI: ciBoolPtr(true)}
	queued := &stages.Stage{Name: "Queued", Order: 6, HoldingStage: true, WaitForCI: ciBoolPtr(true)}
	pipeline := []*stages.Stage{impl, review, ciGatedValidate, queued, done}
	noGate := []*stages.Stage{impl, review, queued, done}
	for _, tc := range []struct {
		name  string
		all   []*stages.Stage
		stage *stages.Stage
		want  string
	}{
		{"implement sees validate gate", pipeline, impl, "ci_gate_ahead: true\n"},
		{"review sees validate gate", pipeline, review, "ci_gate_ahead: true\n"},
		{"validate is the last gate", pipeline, ciGatedValidate, "ci_gate_ahead: false\n"},
		{"no gate anywhere", noGate, impl, "ci_gate_ahead: false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := ciStatusClient([]gh.CheckRun{passRun("t", 1)})
			content := runCIStatusWith(t, base, base, tc.all, tc.stage)
			if !strings.Contains(content, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, content)
			}
		})
	}
}

// A CI-fix, review or rebase reinvoke runs with the current (gated) stage, so it
// sees ci_gated: true; the comment path passes the same stage through.
func TestCIStatus_ReinvokePathsSeeGatedStage(t *testing.T) {
	base := ciStatusClient([]gh.CheckRun{passRun("t", 1)})
	eng := testEngine(t, base, &mockClaudeInvoker{})
	workDir := t.TempDir()
	eng.writeContextFiles(gh.ProjectItem{Number: 7, Body: "body"}, ciGatedValidate, workDir, true)
	data, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", "ci-status.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ci_gated: true\n") {
		t.Errorf("comment-path invocation in a gated stage should be ci_gated:\n%s", data)
	}
}
