package engine

import (
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func TestBuildPrompt_Basic(t *testing.T) {
	stage := &stages.Stage{
		Name:   "Research",
		Prompt: "You are a research agent.",
	}
	issue := gh.ProjectItem{
		Number: 42,
		Title:  "Fix the bug",
		URL:    "https://github.com/owner/repo/issues/42",
		Body:   "It is broken",
	}

	prompt := buildPrompt(stage, issue, nil)

	if !strings.Contains(prompt, "You are a research agent.") {
		t.Error("prompt missing stage prompt")
	}
	if !strings.Contains(prompt, "# Issue #42: Fix the bug") {
		t.Error("prompt missing issue header")
	}
	if !strings.Contains(prompt, "https://github.com/owner/repo/issues/42") {
		t.Error("prompt missing URL")
	}
	if !strings.Contains(prompt, "It is broken") {
		t.Error("prompt missing body")
	}
	if !strings.Contains(prompt, "FABRIK_STAGE_COMPLETE") {
		t.Error("prompt missing completion instruction")
	}
}

func TestBuildPrompt_WithLabels(t *testing.T) {
	stage := &stages.Stage{Name: "Test", Prompt: "prompt"}
	issue := gh.ProjectItem{
		Number: 1,
		Title:  "T",
		Labels: []string{"bug", "priority"},
	}

	prompt := buildPrompt(stage, issue, nil)
	if !strings.Contains(prompt, "## Labels") {
		t.Error("prompt missing labels section")
	}
	if !strings.Contains(prompt, "bug, priority") {
		t.Error("prompt missing label values")
	}
}

func TestBuildPrompt_WithComments(t *testing.T) {
	stage := &stages.Stage{Name: "Test", Prompt: "prompt"}
	issue := gh.ProjectItem{Number: 1, Title: "T"}
	comments := []gh.Comment{
		{
			Author:    "alice",
			Body:      "Please fix this",
			CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		},
	}

	prompt := buildPrompt(stage, issue, comments)
	if !strings.Contains(prompt, "## New Comments") {
		t.Error("prompt missing comments section")
	}
	if !strings.Contains(prompt, "@alice") {
		t.Error("prompt missing comment author")
	}
	if !strings.Contains(prompt, "Please fix this") {
		t.Error("prompt missing comment body")
	}
}

func TestBuildPrompt_NoLabelsSection(t *testing.T) {
	stage := &stages.Stage{Name: "Test", Prompt: "prompt"}
	issue := gh.ProjectItem{Number: 1, Title: "T"}

	prompt := buildPrompt(stage, issue, nil)
	if strings.Contains(prompt, "## Labels") {
		t.Error("prompt should not have labels section when no labels")
	}
}

func TestBuildPrompt_NoCommentsSection(t *testing.T) {
	stage := &stages.Stage{Name: "Test", Prompt: "prompt"}
	issue := gh.ProjectItem{Number: 1, Title: "T"}

	prompt := buildPrompt(stage, issue, nil)
	if strings.Contains(prompt, "## New Comments") {
		t.Error("prompt should not have comments section when no comments")
	}
}

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

func TestBuildCommentReviewPrompt_CommentSkill(t *testing.T) {
	stage := &stages.Stage{
		Name:          "Specify",
		CommentSkill:  "fabrik-specify-comment",
		CommentPrompt: "should not appear",
	}
	item := gh.ProjectItem{
		Number: 42,
		Title:  "Add feature",
	}
	comments := []gh.Comment{
		{Author: "user", Body: "clarification", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	prompt := buildCommentReviewPrompt(stage, item, comments)

	if !strings.Contains(prompt, "fabrik-specify-comment") {
		t.Error("expected comment skill name in prompt")
	}
	if !strings.Contains(prompt, "Specify comment reviewer") {
		t.Error("expected stage name in skill directive")
	}
	if strings.Contains(prompt, "should not appear") {
		t.Error("CommentPrompt should not be used when CommentSkill is set")
	}
	if strings.Contains(prompt, "comment review agent") {
		t.Error("default prompt should not be used when CommentSkill is set")
	}
}

func TestBuildCommentReviewPrompt_CommentSkillPR(t *testing.T) {
	stage := &stages.Stage{
		Name:         "Review",
		CommentSkill: "fabrik-review-comment",
	}
	item := gh.ProjectItem{
		Number: 55,
		Title:  "Some PR",
		IsPR:   true,
	}
	comments := []gh.Comment{
		{Author: "user", Body: "looks good", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	prompt := buildCommentReviewPrompt(stage, item, comments)

	if !strings.Contains(prompt, "PR #55") {
		t.Errorf("expected 'PR #55' in prompt, got: %s", prompt)
	}
	if strings.Contains(prompt, "issue #55") {
		t.Error("should not say 'issue' when item is a PR")
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
