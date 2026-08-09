package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestResumeFailure_ExemptFromMaxRetries_UntilColdStart mirrors
// slice_retry_test.go's shape for #1414: a consecutive resume-failure exit
// (*claudeResumeFailureError, the sentinel interpretClaudeResult returns —
// see claude_resume_failure_test.go for coverage of that classification
// layer) must record StageAttempted (so the normal dispatch cooldown
// applies) but never StageRetryIncremented, apply no
// stage:<name>:failed/fabrik:paused label, and post no issue comment for the
// abandonment itself — mirroring the fabrik:claude-limit precedent (ADR-1119).
// Once the invoker returns a plain (non-resume-failure) error — simulating
// the guaranteed cold-start attempt after the session was abandoned — Attempts
// DOES increment, confirming the exemption is scoped to resume failures only
// and normal max_retries accounting resumes unchanged.
func TestResumeFailure_ExemptFromMaxRetries_UntilColdStart(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	resumeFailing := true
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			if resumeFailing {
				return "partial output", false, TokenUsage{}, &claudeResumeFailureError{
					Cause:               errors.New("exit status 1"),
					SessionID:           "resumed-sid",
					ConsecutiveFailures: 1,
					Threshold:           2,
					Abandoned:           false,
				}
			}
			return "partial output", false, TokenUsage{}, errors.New("a genuine, unrelated failure")
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxResumeFailures: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 210, Title: "Resume-failure test", Status: "Research", ItemID: "PVTI_210"}

	// Two consecutive resume-failure invocations (mirrors MaxResumeFailures=2:
	// this test exercises the item.go-level exemption only — the counting and
	// abandon-at-threshold logic itself lives in classifyResumeFailure,
	// covered by claude_resume_failure_test.go).
	for i := 0; i < 2; i++ {
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem (resume-failure attempt %d): %v", i+1, err)
		}
	}

	snap, err := eng.store.Get("owner/repo", 210)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 (resume failures must NOT count against max_retries)", got)
	}
	if snap.LastAttemptAt("Research").IsZero() {
		t.Errorf("expected StageAttempted to be recorded (dispatch cooldown) despite the max_retries exemption")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Research:failed" {
			t.Error("stage:Research:failed must NOT be applied for a resume failure — it is not (yet) a genuine failure")
		}
		if c.labelName == "fabrik:paused" {
			t.Error("must not pause on repeated resume failures — the mechanism must guarantee a cold-start attempt first")
		}
	}
	// The routine per-invocation output-posting comment (Claude's own output
	// text) is expected here — that behavior is unrelated to #1414 and
	// unconditional for any non-completing invocation with output. What must
	// NOT appear is a dedicated notice about the resume-failure/abandonment
	// condition itself — that information is log-only (claudeLog, tag
	// "resume"), never surfaced as an issue comment.
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "abandon") || strings.Contains(c.body, "resume") || strings.Contains(c.body, "consecutive") {
			t.Errorf("no issue comment must mention the resume-failure/abandonment condition, got: %q", c.body)
		}
	}

	// Now simulate the guaranteed cold-start attempt failing for a genuine,
	// unrelated reason (plain error, not *claudeResumeFailureError) — normal
	// max_retries accounting must resume unchanged.
	resumeFailing = false
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (cold-start failure): %v", err)
	}
	snap, err = eng.store.Get("owner/repo", 210)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 1 {
		t.Errorf("Attempts(\"Research\") = %d, want 1 (a genuine failure after the exemption must count normally)", got)
	}
}
