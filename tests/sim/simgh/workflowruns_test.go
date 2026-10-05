package simgh

import (
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func TestWorkflowRunsAreKeyedBySHAWithDefaults(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedWorkflowRun("acme/widgets", "sha-a", gh.WorkflowRun{Name: "CI", Conclusion: "startup_failure"})
	got, err := s.FetchWorkflowRuns("acme", "widgets", "sha-a")
	if err != nil || len(got) != 1 {
		t.Fatalf("FetchWorkflowRuns = %+v, %v; want one run", got, err)
	}
	if got[0].ID == 0 || got[0].Status != "completed" || got[0].CreatedAt.IsZero() {
		t.Errorf("defaults not applied: %+v", got[0])
	}
	if other, _ := s.FetchWorkflowRuns("acme", "widgets", "sha-b"); len(other) != 0 {
		t.Errorf("sha-b leaked runs: %+v", other)
	}
}

// Reopening a closed PR is allowed (the issues endpoint accepts a PR number)
// and consumes one scripted step per reopen; an unscripted reopen starts
// nothing.
func TestReopenPRConsumesScriptedStepsInOrder(t *testing.T) {
	s, clk, sha := seedPRForScheduling(t)
	s.SeedReopenSteps("acme/widgets", 8,
		ReopenStep{SHA: sha, Delay: time.Minute, WorkflowRuns: []gh.WorkflowRun{{ID: 900, Name: "CI", Status: "in_progress"}}},
	)
	for round := 0; round < 2; round++ {
		if err := s.CloseIssue("acme", "widgets", 8); err != nil {
			t.Fatalf("CloseIssue: %v", err)
		}
		if err := s.ReopenIssue("acme", "widgets", 8); err != nil {
			t.Fatalf("ReopenIssue (PR fallback): %v", err)
		}
		pr, err := s.FetchPRDetails("acme", "widgets", 8)
		if err != nil || pr.State != "open" {
			t.Fatalf("PR after reopen = %+v, %v; want open", pr, err)
		}
	}
	if got, _ := s.FetchWorkflowRuns("acme", "widgets", sha); len(got) != 0 {
		t.Fatalf("run visible before its Delay: %+v", got)
	}
	clk.Advance(2 * time.Minute)
	got, _ := s.FetchWorkflowRuns("acme", "widgets", sha)
	if len(got) != 1 || got[0].ID != 900 {
		t.Fatalf("after Delay got %+v; want exactly the one scripted run (second reopen had no step)", got)
	}
}

func TestRerunFailedJobsRefusesWhatGitHubRefuses(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedWorkflowRun("acme/widgets", "sha", gh.WorkflowRun{ID: 1, Status: "completed", Conclusion: "startup_failure"}).
		SeedWorkflowRun("acme/widgets", "sha", gh.WorkflowRun{ID: 2, Status: "in_progress"}).
		SeedWorkflowRun("acme/widgets", "sha", gh.WorkflowRun{ID: 3, Status: "completed", Conclusion: "failure", JobCount: 2})
	for _, id := range []int64{1, 2} {
		if err := s.RerunFailedJobs("acme", "widgets", id); !errors.Is(err, gh.ErrForbidden) {
			t.Errorf("run %d: err = %v, want ErrForbidden", id, err)
		}
	}
	if err := s.RerunFailedJobs("acme", "widgets", 99); !errors.Is(err, gh.ErrNotFound) {
		t.Errorf("unknown run: err = %v, want ErrNotFound", err)
	}
	if err := s.RerunFailedJobs("acme", "widgets", 3); err != nil {
		t.Fatalf("re-running a finished failed run: %v", err)
	}
	got, _ := s.FetchWorkflowRuns("acme", "widgets", "sha")
	for _, r := range got {
		if r.ID == 3 && r.Status != "in_progress" {
			t.Errorf("accepted re-run should go in_progress, got %+v", r)
		}
	}
}

// A scripted re-run supersedes the failed check run: the new run has a higher
// ID under the same name once its Delay has passed.
func TestRerunStepSupersedesFailedCheckRun(t *testing.T) {
	s, clk := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedWorkflowRun("acme/widgets", "sha", gh.WorkflowRun{ID: 3, Status: "completed", Conclusion: "failure", JobCount: 1}).
		SeedCheckRun("acme/widgets", "sha", gh.CheckRun{Name: "build", Conclusion: "failure"}).
		SeedRerunStep("acme/widgets", 3, RerunStep{Delay: time.Minute, SHA: "sha", CheckRuns: []gh.CheckRun{{Name: "build", Conclusion: "success"}}})
	if err := s.RerunFailedJobs("acme", "widgets", 3); err != nil {
		t.Fatal(err)
	}
	before, _ := s.FetchCheckRuns("acme", "widgets", "sha")
	if len(before) != 1 {
		t.Fatalf("before Delay: %+v", before)
	}
	clk.Advance(2 * time.Minute)
	after, _ := s.FetchCheckRuns("acme", "widgets", "sha")
	if len(after) != 2 || after[1].ID <= after[0].ID || after[1].Conclusion != "success" {
		t.Fatalf("after Delay: %+v", after)
	}
	runs, _ := s.FetchWorkflowRuns("acme", "widgets", "sha")
	if runs[0].Status != "completed" || runs[0].Conclusion != "success" {
		t.Errorf("run after re-run = %+v", runs[0])
	}
}
