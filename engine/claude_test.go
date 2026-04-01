package engine

import (
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

func TestBuildCommentReviewPrompt_Issue(t *testing.T) {
	stage := &stages.Stage{Name: "Research"}
	item := gh.ProjectItem{
		Number: 42,
		Title:  "Test Issue",
		URL:    "https://github.com/org/repo/issues/42",
		Body:   "Issue body content",
	}
	comments := []gh.Comment{
		{Author: "alice", Body: "looks good", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	prompt := buildCommentReviewPrompt(stage, item, comments)

	if !strings.Contains(prompt, "# Issue #42: Test Issue") {
		t.Error("expected issue header in prompt")
	}
	if !strings.Contains(prompt, "## Current Issue Body") {
		t.Error("expected 'Current Issue Body' section")
	}
	if !strings.Contains(prompt, "updated issue body") {
		t.Error("expected issue-specific marker instructions")
	}
	if strings.Contains(prompt, "PR") {
		t.Error("should not contain PR terminology for issues")
	}
}

func TestBuildCommentReviewPrompt_PR(t *testing.T) {
	stage := &stages.Stage{Name: "Review"}
	item := gh.ProjectItem{
		Number: 7,
		Title:  "Fix bug",
		URL:    "https://github.com/org/repo/pull/7",
		Body:   "PR description",
		IsPR:   true,
	}
	comments := []gh.Comment{
		{Author: "bot", Body: "suggestion: use const", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	prompt := buildCommentReviewPrompt(stage, item, comments)

	if !strings.Contains(prompt, "# PR #7: Fix bug") {
		t.Error("expected PR header in prompt")
	}
	if !strings.Contains(prompt, "## Current PR Description") {
		t.Error("expected 'Current PR Description' section")
	}
	if !strings.Contains(prompt, "updated PR description") {
		t.Error("expected PR-specific marker instructions")
	}
	if !strings.Contains(prompt, "FABRIK_ISSUE_UPDATE_BEGIN") {
		t.Error("expected consistent FABRIK_ISSUE_UPDATE markers for PRs")
	}
}

func TestBuildCommentReviewPrompt_CustomCommentPrompt(t *testing.T) {
	stage := &stages.Stage{Name: "Review", CommentPrompt: "Custom prompt text"}
	item := gh.ProjectItem{
		Number: 7,
		Title:  "Fix bug",
		IsPR:   true,
	}
	comments := []gh.Comment{
		{Author: "user", Body: "hello", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	prompt := buildCommentReviewPrompt(stage, item, comments)

	if !strings.Contains(prompt, "Custom prompt text") {
		t.Error("expected custom comment prompt to be used")
	}
	if strings.Contains(prompt, "PR comment review agent") {
		t.Error("should use custom prompt, not default PR prompt")
	}
}

func TestDefaultPRCommentPrompt(t *testing.T) {
	prompt := defaultPRCommentPrompt()

	if !strings.Contains(prompt, "PR comment review agent") {
		t.Error("expected PR-specific agent description")
	}
	if !strings.Contains(prompt, "code changes") {
		t.Error("expected mention of code changes")
	}
	if !strings.Contains(prompt, "review feedback") {
		t.Error("expected mention of review feedback")
	}
}

func TestExtractUpdatedBody(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{
			name:   "normal extraction",
			input:  "Some preamble\nFABRIK_ISSUE_UPDATE_BEGIN\nUpdated body here\nFABRIK_ISSUE_UPDATE_END\nSome epilogue",
			expect: "Updated body here",
		},
		{
			name:   "no markers",
			input:  "Just some output without markers",
			expect: "",
		},
		{
			name:   "only begin marker",
			input:  "FABRIK_ISSUE_UPDATE_BEGIN\nBody without end",
			expect: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractUpdatedBody(tt.input)
			if got != tt.expect {
				t.Errorf("extractUpdatedBody() = %q, want %q", got, tt.expect)
			}
		})
	}
}
