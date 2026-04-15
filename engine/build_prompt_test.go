package engine

import (
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
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

	prompt := buildPrompt(stage, issue, nil, "")

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

	prompt := buildPrompt(stage, issue, nil, "")
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

	prompt := buildPrompt(stage, issue, comments, "")
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

	prompt := buildPrompt(stage, issue, nil, "")
	if strings.Contains(prompt, "## Labels") {
		t.Error("prompt should not have labels section when no labels")
	}
}

func TestBuildPrompt_NoCommentsSection(t *testing.T) {
	stage := &stages.Stage{Name: "Test", Prompt: "prompt"}
	issue := gh.ProjectItem{Number: 1, Title: "T"}

	prompt := buildPrompt(stage, issue, nil, "")
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

	prompt := buildCommentReviewPrompt(stage, item, comments, "")

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

	prompt := buildCommentReviewPrompt(stage, item, comments, "")

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

	prompt := buildCommentReviewPrompt(stage, item, comments, "")

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

	prompt := buildCommentReviewPrompt(stage, item, comments, "")

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

	prompt := buildCommentReviewPrompt(stage, item, comments, "")

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

func TestBuildCommentReviewPrompt_ReviewThreadComment(t *testing.T) {
	stage := &stages.Stage{Name: "Review"}
	item := gh.ProjectItem{
		Number: 42,
		Title:  "My PR",
		URL:    "https://github.com/org/repo/pull/42",
		Body:   "PR body",
		IsPR:   true,
	}
	comments := []gh.Comment{
		{
			Author:         "copilot",
			Body:           "Fix the error handling here",
			CreatedAt:      time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
			ReviewThreadID: "RT_abc123",
			Path:           "engine/claude.go",
			Line:           243,
			OriginalLine:   240,
			DiffHunk:       "@@ -241,7 +241,7 @@\n-\tfoo()\n+\tbar()",
		},
	}

	prompt := buildCommentReviewPrompt(stage, item, comments, "")

	if !strings.Contains(prompt, "[Thread: RT_abc123]") {
		t.Error("expected thread ID in prompt")
	}
	if !strings.Contains(prompt, "engine/claude.go") {
		t.Error("expected file path in prompt")
	}
	if !strings.Contains(prompt, "243") {
		t.Error("expected line number in prompt")
	}
	if !strings.Contains(prompt, "@@ -241,7 +241,7 @@") {
		t.Error("expected diff hunk in prompt")
	}
	if !strings.Contains(prompt, "```diff") {
		t.Error("expected fenced diff block in prompt")
	}
	if !strings.Contains(prompt, "Fix the error handling here") {
		t.Error("expected comment body in prompt")
	}
}

func TestBuildCommentReviewPrompt_RegularComment_NoLocation(t *testing.T) {
	stage := &stages.Stage{Name: "Review"}
	item := gh.ProjectItem{
		Number: 42,
		Title:  "My PR",
		IsPR:   true,
	}
	comments := []gh.Comment{
		{
			Author:    "alice",
			Body:      "LGTM overall",
			CreatedAt: time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
			// No Path, Line, DiffHunk — regular PR body comment
		},
	}

	prompt := buildCommentReviewPrompt(stage, item, comments, "")

	if !strings.Contains(prompt, "**@alice**") {
		t.Error("expected author in prompt")
	}
	if !strings.Contains(prompt, "LGTM overall") {
		t.Error("expected body in prompt")
	}
	if strings.Contains(prompt, "**File:**") {
		t.Error("regular comment should not include File: field")
	}
	if strings.Contains(prompt, "**Diff context:**") {
		t.Error("regular comment should not include Diff context: field")
	}
	if strings.Contains(prompt, "[Thread:") {
		t.Error("regular comment should not include Thread: reference")
	}
}

func TestBuildCommentReviewPrompt_ReviewThreadComment_ZeroLine(t *testing.T) {
	stage := &stages.Stage{Name: "Review"}
	item := gh.ProjectItem{Number: 42, Title: "My PR", IsPR: true}

	// Case 1: Line == 0, OriginalLine != 0 — should use OriginalLine
	c1 := gh.Comment{
		Author:         "bot",
		Body:           "deleted line comment",
		CreatedAt:      time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
		ReviewThreadID: "RT_del",
		Path:           "foo.go",
		Line:           0,
		OriginalLine:   99,
		DiffHunk:       "@@ -97,5 +97,4 @@",
	}
	prompt1 := buildCommentReviewPrompt(stage, item, []gh.Comment{c1}, "")
	if !strings.Contains(prompt1, "99") {
		t.Error("expected OriginalLine (99) in prompt when Line is 0")
	}
	if strings.Contains(prompt1, "**Line:** 0") {
		t.Error("should not render line 0 in prompt")
	}

	// Case 2: both Line and OriginalLine are 0 — line number should be omitted
	c2 := gh.Comment{
		Author:         "bot",
		Body:           "no line info",
		CreatedAt:      time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC),
		ReviewThreadID: "RT_noline",
		Path:           "bar.go",
		Line:           0,
		OriginalLine:   0,
	}
	prompt2 := buildCommentReviewPrompt(stage, item, []gh.Comment{c2}, "")
	if !strings.Contains(prompt2, "bar.go") {
		t.Error("expected file path in prompt even without line number")
	}
	if strings.Contains(prompt2, "**Line:**") {
		t.Error("should not render **Line:** when both Line and OriginalLine are 0")
	}
}

func TestBuildPrompt_BaseBranch(t *testing.T) {
	stage := &stages.Stage{Name: "Research", Prompt: "Do research."}
	issue := gh.ProjectItem{Number: 1, Title: "T"}

	// Non-empty baseBranch: branch name should appear in codebase-changes line and explicit statement.
	prompt := buildPrompt(stage, issue, nil, "liminis")
	if !strings.Contains(prompt, "liminis") {
		t.Error("expected branch name 'liminis' in prompt when baseBranch is non-empty")
	}
	if !strings.Contains(prompt, "files changed on `liminis`") {
		t.Errorf("expected 'files changed on `liminis`' in codebase-changes description, got prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "default base branch is `liminis`") {
		t.Errorf("expected explicit base branch statement with 'liminis', got prompt:\n%s", prompt)
	}

	// Empty baseBranch: fallback text should appear instead.
	promptEmpty := buildPrompt(stage, issue, nil, "")
	if !strings.Contains(promptEmpty, "the default branch") {
		t.Errorf("expected 'the default branch' fallback in prompt when baseBranch is empty, got:\n%s", promptEmpty)
	}
	if strings.Contains(promptEmpty, "files changed on main") {
		t.Error("prompt should not hardcode 'main' as the branch name")
	}
}

func TestBuildCommentReviewPrompt_BaseBranch(t *testing.T) {
	stage := &stages.Stage{Name: "Review"}
	item := gh.ProjectItem{Number: 7, Title: "Fix bug", IsPR: true}
	comments := []gh.Comment{
		{Author: "alice", Body: "LGTM", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	// Non-empty baseBranch: branch name should appear in explicit statement.
	prompt := buildCommentReviewPrompt(stage, item, comments, "liminis")
	if !strings.Contains(prompt, "default base branch is `liminis`") {
		t.Errorf("expected explicit base branch statement with 'liminis', got:\n%s", prompt)
	}

	// Empty baseBranch: no base branch statement should be emitted.
	promptEmpty := buildCommentReviewPrompt(stage, item, comments, "")
	if strings.Contains(promptEmpty, "default base branch is") {
		t.Errorf("prompt should not include base branch statement when baseBranch is empty, got:\n%s", promptEmpty)
	}
}
