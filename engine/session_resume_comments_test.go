package engine

import (
	"context"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Comment review decides once per pass and hands it to the invoker as
// InvokeOptions.NoResume (ADR 2300); the extension loop resumes from its second
// call on, and the decision is logged once.
func TestProcessComments_SessionDecisionDrivesNoResume(t *testing.T) {
	skipIfNoGit(t)
	cases := []struct {
		name         string
		mode         string
		tokens       int
		wantNoResume bool
	}{
		{"default resumes a large session", "", 900000, false},
		{"fresh", "fresh", 10, true},
		{"auto above threshold", "auto", 900000, true},
		{"auto below threshold", "auto", 1000, false},
		{"auto unknown", "auto", -1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var callCount int
			client := &mockGitHubClient{
				addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) { return 0, nil },
				fetchItemDetailsFn: func(item *gh.ProjectItem) error {
					item.Comments = append(item.Comments, gh.Comment{Body: "new comment"})
					return nil
				},
			}
			claude := &mockClaudeInvoker{
				invokeForCommentsFn: func(s *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
					callCount++
					if callCount == 1 {
						return "partial", false, TokenUsage{TurnsUsed: opts.MaxTurnsOverride}, nil
					}
					return "final", true, TokenUsage{TurnsUsed: 1}, nil
				},
			}
			eng := testEngineWithRepo(t, client, claude)
			eng.SetSessionDirForTest(t.TempDir())
			var logged []string
			eng.SetLogSinkForTest(func(_ int, tag, msg string) {
				if tag == "session" {
					logged = append(logged, msg)
				}
			})
			stage := &stages.Stage{Name: "Validate", CommentMaxTurns: 5, ResumeSession: c.mode, ResumeThresholdTokens: 250000}
			item := gh.ProjectItem{Number: 32, Body: "spec", Labels: []string{"fabrik:extend-turns"}}
			eng.RecordSessionForTest(item, "Validate", "S1", c.tokens, true)

			board := &gh.ProjectBoard{ProjectID: "PVT_1"}
			userComments := []gh.Comment{{ID: "C_3", DatabaseID: 802, Author: "testuser", Body: "please validate"}}
			if err := eng.processComments(context.Background(), board, item, stage, userComments); err != nil {
				t.Fatalf("processComments: %v", err)
			}
			calls := claude.forCommentsCalls
			if len(calls) != 2 {
				t.Fatalf("expected 2 InvokeForComments calls (extension), got %d", len(calls))
			}
			if calls[0].opts.NoResume != c.wantNoResume {
				t.Errorf("first call NoResume = %v, want %v", calls[0].opts.NoResume, c.wantNoResume)
			}
			if calls[1].opts.NoResume {
				t.Error("extension iteration must resume the session the first call saved")
			}
			if len(logged) != 1 {
				t.Errorf("decision logged %d times, want once: %q", len(logged), logged)
			}
		})
	}
}

// Without a saved session nothing changes: NoResume stays false and nothing is logged.
func TestProcessComments_NoSavedSession_Unchanged(t *testing.T) {
	skipIfNoGit(t)
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(s *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "ok", true, TokenUsage{}, nil
		},
	}
	eng := testEngineWithRepo(t, &mockGitHubClient{}, claude)
	eng.SetSessionDirForTest(t.TempDir())
	stage := &stages.Stage{Name: "Validate", ResumeSession: "fresh"}
	item := gh.ProjectItem{Number: 33, Body: "spec"}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	if err := eng.processComments(context.Background(), board, item, stage, []gh.Comment{{ID: "C_4", DatabaseID: 803, Author: "testuser", Body: "hi"}}); err != nil {
		t.Fatalf("processComments: %v", err)
	}
	if calls := claude.forCommentsCalls; len(calls) != 1 || calls[0].opts.NoResume {
		t.Fatalf("calls = %+v, want one call with NoResume=false", calls)
	}
}
