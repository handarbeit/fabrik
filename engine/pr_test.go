package engine

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func TestEnsureDraftPR_ExistingPR_SkipsCreate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, State: "open"}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Title: "Test Issue"}
	prNum, err := eng.ensureDraftPR(item, "main")

	if err != nil {
		t.Fatalf("ensureDraftPR returned unexpected error: %v", err)
	}
	if prNum != 42 {
		t.Errorf("ensureDraftPR returned %d, want 42", prNum)
	}
	// CreateDraftPR should not have been called
	if len(client.createDraftPRCalls) > 0 {
		t.Error("should not create PR when one already exists")
	}
}

func TestEnsurePRLinksIssue_AlreadyLinked_NoUpdate(t *testing.T) {
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "This PR closes Closes #5", nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	eng.ensurePRLinksIssue(gh.ProjectItem{Number: 5}, 10)

	// Should NOT update body since it already has Closes #5
	if len(client.updateCommentCalls) > 0 {
		t.Error("UpdateComment should not be called when already linked")
	}
	// UpdateIssueBody is tracked differently — check no update occurred
	// (the mock's updateIssueBodyFn not called means no update)
}

func TestEnsurePRLinksIssue_Missing_AddsKeyword(t *testing.T) {
	var updatedBody string
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "Some PR description", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			updatedBody = body
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	eng.ensurePRLinksIssue(gh.ProjectItem{Number: 7}, 10)

	if !strings.Contains(updatedBody, "Closes #7") {
		t.Errorf("expected Closes #7 in updated body, got: %q", updatedBody)
	}
}

func TestEnsurePRLinksIssue_FencedClosesN_SelfHeals(t *testing.T) {
	// Closes #7 is present in the body but inside an unclosed code fence, so GitHub's
	// parser would treat it as code. balanceFences must close the fence so Closes #7
	// becomes reachable, and no duplicate Closes #7 must be appended.
	fencedBody := "Some PR description\n\n```bash\ncode example\n\n---\n\nCloses #7"
	var updateCalls []string
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return fencedBody, nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			updateCalls = append(updateCalls, body)
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	eng.ensurePRLinksIssue(gh.ProjectItem{Number: 7}, 10)

	if len(updateCalls) != 1 {
		t.Fatalf("expected exactly 1 UpdateIssueBody call (fence fix), got %d", len(updateCalls))
	}
	got := updateCalls[0]
	if strings.Count(got, "Closes #7") != 1 {
		t.Errorf("Closes #7 must appear exactly once (no duplicate), got body: %q", got)
	}
	// The fence must be closed before ---
	if !strings.Contains(got, "```\n---") {
		t.Errorf("balanced body must have closing fence before ---, got: %q", got)
	}
}

func TestMarkPRReady_WithKnownPR_CallsMarkReady(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManagerWithRoot(repoDir, repoDir+"/.fabrik/worktrees")

	client := &mockGitHubClient{}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", User: "u", Token: "t", Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 5, Title: "test"}
	eng.markPRReady(item, 55) // known PR number 55

	if len(client.markPRReadyCalls) != 1 {
		t.Fatalf("expected 1 MarkPRReady call, got %d", len(client.markPRReadyCalls))
	}
	if client.markPRReadyCalls[0].prNumber != 55 {
		t.Errorf("MarkPRReady called with PR #%d, want 55", client.markPRReadyCalls[0].prNumber)
	}
}

func TestMarkPRReady_NoPRFound_NoCall(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManagerWithRoot(repoDir, repoDir+"/.fabrik/worktrees")

	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, nil // no PR
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", User: "u", Token: "t", Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 6, Title: "test"}
	eng.markPRReady(item, 0) // no known PR, lookup returns 0

	if len(client.markPRReadyCalls) > 0 {
		t.Error("MarkPRReady should not be called when no PR found")
	}
}

func TestPostOutputToPR_WithPR_PostsToPRAndIssue(t *testing.T) {
	var addCommentCalls []addCommentCall
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 20, nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			addCommentCalls = append(addCommentCalls, addCommentCall{owner, repo, issueNumber, body})
			return 0, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 3, Title: "Issue"}

	eng.postOutputToPR(item, "Implement", "detailed output", "", "main-branch", "abc123", "", "2024-01-01")

	// Should have posted to PR (#20) and to issue (#3)
	if len(addCommentCalls) != 2 {
		t.Fatalf("expected 2 AddComment calls (PR + issue), got %d", len(addCommentCalls))
	}
	var hasPR, hasIssue bool
	for _, c := range addCommentCalls {
		if c.issueNumber == 20 {
			hasPR = true
		}
		if c.issueNumber == 3 {
			hasIssue = true
		}
	}
	if !hasPR {
		t.Error("expected comment on PR #20")
	}
	if !hasIssue {
		t.Error("expected summary comment on issue #3")
	}
}

func TestPostOutputToPR_FindPRError_FallsBackToIssue(t *testing.T) {
	var addCommentIssues []int
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, errors.New("api error") // error + 0 PR number
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			addCommentIssues = append(addCommentIssues, issueNumber)
			return 0, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 5, Title: "Issue"}

	eng.postOutputToPR(item, "Review", "output", "", "", "", "", "")

	// With error+0, the no-PR fallback should post on the issue
	if len(addCommentIssues) != 1 || addCommentIssues[0] != 5 {
		t.Errorf("expected fallback comment on issue #5, got issues %v", addCommentIssues)
	}
}

func TestPostOutputToPR_AddCommentErrors_LogsWarnings(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 30, nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, errors.New("post error") // all AddComment calls fail
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 6, Title: "Issue"}

	// Should not panic when AddComment fails
	eng.postOutputToPR(item, "Implement", "output", "", "", "", "", "")
}

func TestPostOutputToPR_NoPR_FallsBackToIssue(t *testing.T) {
	var addCommentCalls []addCommentCall
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, nil // no PR
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			addCommentCalls = append(addCommentCalls, addCommentCall{owner, repo, issueNumber, body})
			return 0, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 4, Title: "Issue"}

	eng.postOutputToPR(item, "Review", "output", "", "", "", "", "")

	// Falls back to one comment on the issue
	if len(addCommentCalls) != 1 {
		t.Fatalf("expected 1 AddComment (fallback), got %d", len(addCommentCalls))
	}
	if addCommentCalls[0].issueNumber != 4 {
		t.Errorf("expected comment on issue #4, got #%d", addCommentCalls[0].issueNumber)
	}
}

// ── updatePRVerification ──────────────────────────────────────────────────────

func TestUpdatePRVerification_ReplacesSectionAndCallsUpdateIssueBody(t *testing.T) {
	var updatedBody string
	var updatedPRNum int
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "## Verification\n\n(Populated by Implement on completion)\n\n---\n\nCloses #10", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			updatedPRNum = issueNumber
			updatedBody = body
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 10, Title: "My issue"}
	eng.updatePRVerification(item, 99, "All tests pass.")

	if updatedPRNum != 99 {
		t.Errorf("UpdateIssueBody called with issueNumber=%d, want 99", updatedPRNum)
	}
	if !strings.Contains(updatedBody, "All tests pass.") {
		t.Errorf("updated body missing summary content: %q", updatedBody)
	}
	if !strings.Contains(updatedBody, "Closes #10") {
		t.Error("updated body must preserve Closes #10")
	}
	if strings.Contains(updatedBody, "(Populated by Implement on completion)") {
		t.Error("placeholder should have been replaced")
	}
}

func TestUpdatePRVerification_EmptySummaryIsNoop(t *testing.T) {
	called := false
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			called = true
			return "## Verification\n\nplaceholder.\n\n---\n\nCloses #1", nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.updatePRVerification(gh.ProjectItem{Number: 1}, 55, "")

	if called {
		t.Error("GetIssueBody should not be called when summary is empty")
	}
}

func TestUpdatePRVerification_SectionNotFound_WarnsAndSkips(t *testing.T) {
	updateCalled := false
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "## Summary\n\nSome summary.\n\n---\n\nCloses #2", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			updateCalled = true
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.updatePRVerification(gh.ProjectItem{Number: 2}, 88, "my summary")

	if updateCalled {
		t.Error("UpdateIssueBody should not be called when ## Verification section is missing")
	}
}

// ── ensureDraftPR — new-PR path (requires git) ────────────────────────────────

// initRepoWithRemote creates a source repo that has a bare repo as its "origin".
// Returns the source repo directory. The source repo has an initial commit and a
// configured remote so that PushBranch succeeds.
func initRepoWithRemote(t *testing.T) string {
	t.Helper()
	remoteDir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %s: %v", out, err)
	}
	sourceDir := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
		{"git", "remote", "add", "origin", remoteDir},
		{"git", "push", "-u", "origin", "main"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = sourceDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", args, out, err)
		}
	}
	return sourceDir
}

func TestEnsureDraftPR_NewPR_SeedsBodyFromContextFiles(t *testing.T) {
	skipIfNoGit(t)

	sourceDir := initRepoWithRemote(t)
	wm := NewWorktreeManager(sourceDir)

	wtDir, err := wm.EnsureWorktree(42, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	contextDir := filepath.Join(wtDir, ".fabrik-context")
	if err := os.MkdirAll(contextDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	issueContent := "## Summary\n\nBrief summary of the issue.\n\n## Problem\n\nThe problem statement.\n"
	planContent := "🏭 **Fabrik — stage: Plan**\n*branch: fabrik/issue-42*\n\n## Approach\n\nThe implementation approach.\n"
	if err := os.WriteFile(filepath.Join(contextDir, "issue.md"), []byte(issueContent), 0644); err != nil {
		t.Fatalf("WriteFile issue.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "stage-Plan.md"), []byte(planContent), 0644); err != nil {
		t.Fatalf("WriteFile stage-Plan.md: %v", err)
	}

	var createdBody string
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			createdBody = body
			return 77, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()},
		client,
		&mockClaudeInvoker{},
		wm,
	)

	item := gh.ProjectItem{Number: 42, Title: "My issue"}
	result, err := eng.ensureDraftPR(item, "main")
	if err != nil {
		t.Fatalf("ensureDraftPR returned unexpected error: %v", err)
	}
	if result != 77 {
		t.Fatalf("ensureDraftPR returned %d, want 77", result)
	}

	if !strings.Contains(createdBody, "## Summary") {
		t.Error("seed body missing ## Summary")
	}
	if !strings.Contains(createdBody, "Brief summary of the issue.") {
		t.Error("seed body missing summary content")
	}
	if !strings.Contains(createdBody, "## Problem") {
		t.Error("seed body missing ## Problem")
	}
	if !strings.Contains(createdBody, "The problem statement.") {
		t.Error("seed body missing problem content")
	}
	if !strings.Contains(createdBody, "## Approach") {
		t.Error("seed body missing ## Approach")
	}
	if !strings.Contains(createdBody, "The implementation approach.") {
		t.Error("seed body missing approach content")
	}
	if !strings.Contains(createdBody, "## Verification") {
		t.Error("seed body missing ## Verification")
	}
	if !strings.Contains(createdBody, "Closes #42") {
		t.Error("seed body missing Closes #42")
	}
	if !strings.HasSuffix(strings.TrimSpace(createdBody), "Closes #42") {
		t.Errorf("Closes #42 must be at the end of seed body")
	}
}

func TestEnsureDraftPR_NewPR_MissingContextFiles_UsesPlaceholders(t *testing.T) {
	skipIfNoGit(t)

	sourceDir := initRepoWithRemote(t)
	wm := NewWorktreeManager(sourceDir)

	_, err := wm.EnsureWorktree(43, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	var createdBody string
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			createdBody = body
			return 78, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()},
		client,
		&mockClaudeInvoker{},
		wm,
	)

	item := gh.ProjectItem{Number: 43, Title: "My issue"}
	result, err := eng.ensureDraftPR(item, "main")
	if err != nil {
		t.Fatalf("ensureDraftPR returned unexpected error: %v", err)
	}
	if result != 78 {
		t.Fatalf("ensureDraftPR returned %d, want 78", result)
	}

	if !strings.Contains(createdBody, "(Populated by Implement)") {
		t.Error("missing context files should produce placeholder for Approach")
	}
	if !strings.Contains(createdBody, "Closes #43") {
		t.Error("Closes #43 must always be present")
	}
}

// ── buildThreadEntries ────────────────────────────────────────────────────────

func TestBuildThreadEntries_DedupsByReviewThreadID(t *testing.T) {
	comments := []gh.Comment{
		{ReviewThreadID: "RT_1", Path: "engine/foo.go", Line: 42},
		{ReviewThreadID: "RT_1", Path: "engine/foo.go", Line: 43}, // duplicate thread
		{ReviewThreadID: "RT_2", Path: "engine/bar.go", Line: 10},
	}
	entries := buildThreadEntries(comments)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (deduped), got %d", len(entries))
	}
	if entries[0].Path != "engine/foo.go" || entries[0].Line != 42 {
		t.Errorf("entry[0] = {%q, %d}, want {engine/foo.go, 42}", entries[0].Path, entries[0].Line)
	}
	if entries[1].Path != "engine/bar.go" || entries[1].Line != 10 {
		t.Errorf("entry[1] = {%q, %d}, want {engine/bar.go, 10}", entries[1].Path, entries[1].Line)
	}
}

func TestBuildThreadEntries_FallsBackToOriginalLine(t *testing.T) {
	comments := []gh.Comment{
		{ReviewThreadID: "RT_1", Path: "engine/foo.go", Line: 0, OriginalLine: 55},
	}
	entries := buildThreadEntries(comments)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Line != 55 {
		t.Errorf("expected Line=55 (from OriginalLine fallback), got %d", entries[0].Line)
	}
}

func TestBuildThreadEntries_ZeroLineWhenBothZero(t *testing.T) {
	comments := []gh.Comment{
		{ReviewThreadID: "RT_1", Path: "engine/foo.go", Line: 0, OriginalLine: 0},
	}
	entries := buildThreadEntries(comments)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Line != 0 {
		t.Errorf("expected Line=0 when both Line and OriginalLine are zero, got %d", entries[0].Line)
	}
}

func TestBuildThreadEntries_SkipsNonReviewComments(t *testing.T) {
	comments := []gh.Comment{
		{ReviewThreadID: "", Path: "", Line: 0},         // regular issue comment
		{ReviewThreadID: "RT_1", Path: "x.go", Line: 1}, // review thread comment
	}
	entries := buildThreadEntries(comments)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (skipping non-review comment), got %d", len(entries))
	}
}

// ── formatReviewFeedbackComment ───────────────────────────────────────────────

func TestFormatReviewFeedbackComment_HeaderContainsTitle(t *testing.T) {
	threads := []reviewThreadEntry{{Path: "engine/foo.go", Line: 42}}
	result := formatReviewFeedbackComment("Review", "Claude output", "branch", "abc123", "main123", "2024-01-01", threads, 3)

	if !strings.Contains(result, "🏭 **Fabrik — stage: Review (review feedback addressed)**") {
		t.Errorf("header not found in:\n%s", result)
	}
}

func TestFormatReviewFeedbackComment_FooterSection(t *testing.T) {
	threads := []reviewThreadEntry{{Path: "engine/foo.go", Line: 42}}
	result := formatReviewFeedbackComment("Review", "output", "branch", "c", "m", "ts", threads, 3)

	if !strings.Contains(result, "**Threads addressed:**") {
		t.Errorf("missing 'Threads addressed:' section in:\n%s", result)
	}
}

func TestFormatReviewFeedbackComment_ThreadBulletWithLine(t *testing.T) {
	threads := []reviewThreadEntry{{Path: "engine/foo.go", Line: 42}}
	result := formatReviewFeedbackComment("Review", "output", "b", "c", "m", "ts", threads, 1)

	if !strings.Contains(result, "`engine/foo.go:42` — resolved") {
		t.Errorf("expected path:line bullet, got:\n%s", result)
	}
}

func TestFormatReviewFeedbackComment_ThreadBulletWithoutLine(t *testing.T) {
	threads := []reviewThreadEntry{{Path: "engine/bar.go", Line: 0}}
	result := formatReviewFeedbackComment("Review", "output", "b", "c", "m", "ts", threads, 1)

	if !strings.Contains(result, "`engine/bar.go` — resolved") {
		t.Errorf("expected path-only bullet, got:\n%s", result)
	}
	if strings.Contains(result, "engine/bar.go:0") {
		t.Error("zero line number must not appear as :0 in the bullet")
	}
}

func TestFormatReviewFeedbackComment_SummaryLine(t *testing.T) {
	threads := []reviewThreadEntry{
		{Path: "a.go", Line: 1},
		{Path: "b.go", Line: 2},
	}
	result := formatReviewFeedbackComment("Review", "output", "b", "c", "m", "ts", threads, 5)

	if !strings.Contains(result, "Resolved 2 review thread(s) across 5 comment(s).") {
		t.Errorf("expected summary line, got:\n%s", result)
	}
}

func TestFormatReviewFeedbackComment_EmptyPathFallback(t *testing.T) {
	threads := []reviewThreadEntry{{Path: "", Line: 0}}
	result := formatReviewFeedbackComment("Review", "output", "b", "c", "m", "ts", threads, 1)

	if !strings.Contains(result, "`(unknown path)` — resolved") {
		t.Errorf("expected (unknown path) fallback bullet, got:\n%s", result)
	}
}

func TestFormatReviewFeedbackComment_TruncatesLongOutput(t *testing.T) {
	long := strings.Repeat("x", 70000)
	threads := []reviewThreadEntry{{Path: "a.go", Line: 1}}
	result := formatReviewFeedbackComment("Review", long, "b", "c", "m", "ts", threads, 1)

	if !strings.Contains(result, "... (truncated)") {
		t.Error("expected truncation marker for output > 60000 chars")
	}
}

// ── syncPRBase ────────────────────────────────────────────────────────────────

func TestSyncPRBase_NoPR_NoUpdateAttempted(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, nil // no PR
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1}

	eng.syncPRBase(item, "main") // must not panic or call UpdatePRBase

	if len(client.updatePRBaseCalls) != 0 {
		t.Errorf("UpdatePRBase should not be called when no PR exists, got %d calls", len(client.updatePRBaseCalls))
	}
	if len(client.getPRBaseCalls) != 0 {
		t.Errorf("GetPRBase should not be called when no PR exists, got %d calls", len(client.getPRBaseCalls))
	}
}

func TestSyncPRBase_MatchingBase_NoUpdateAttempted(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 42, nil
		},
		getPRBaseFn: func(owner, repo string, prNumber int) (string, error) {
			return "main", nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1}

	eng.syncPRBase(item, "main") // base matches — no update

	if len(client.updatePRBaseCalls) != 0 {
		t.Errorf("UpdatePRBase should not be called when base already matches, got %d calls", len(client.updatePRBaseCalls))
	}
}

func TestSyncPRBase_MismatchedBase_UpdatesExactlyOnce(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 42, nil
		},
		getPRBaseFn: func(owner, repo string, prNumber int) (string, error) {
			return "main", nil // current base
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1}

	eng.syncPRBase(item, "feature/foo") // desired base differs

	if len(client.updatePRBaseCalls) != 1 {
		t.Fatalf("expected exactly 1 UpdatePRBase call, got %d", len(client.updatePRBaseCalls))
	}
	got := client.updatePRBaseCalls[0]
	if got.prNumber != 42 {
		t.Errorf("UpdatePRBase called with PR #%d, want 42", got.prNumber)
	}
	if got.newBase != "feature/foo" {
		t.Errorf("UpdatePRBase called with base %q, want %q", got.newBase, "feature/foo")
	}
}

func TestSyncPRBase_UpdateError_StageContinues(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 42, nil
		},
		getPRBaseFn: func(owner, repo string, prNumber int) (string, error) {
			return "main", nil
		},
		updatePRBaseFn: func(owner, repo string, prNumber int, newBase string) error {
			return errors.New("github: unprocessable entity")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1}

	// syncPRBase must not propagate the error — caller sees no return value
	eng.syncPRBase(item, "feature/bar")

	if len(client.updatePRBaseCalls) != 1 {
		t.Fatalf("expected 1 UpdatePRBase attempt even on error, got %d", len(client.updatePRBaseCalls))
	}
}

// ── processItem Verification update integration test ─────────────────────────

func TestProcessItem_ImplementStage_UpdatesVerificationOnComplete(t *testing.T) {
	skipIfNoGit(t)

	const issueNum = 50
	const prNum = 200

	var verificationUpdateBody string
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return prNum, nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNum, State: "open"}, nil
		},
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			if issueNumber == prNum {
				return "## Verification\n\n(Populated by Implement on completion)\n\n---\n\nCloses #50", nil
			}
			return "issue body", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			if issueNumber == prNum {
				verificationUpdateBody = body
			}
			return nil
		},
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return nil, nil
		},
	}

	const summary = "Tests pass, build clean."
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			output := "Implementation done.\nFABRIK_SUMMARY_BEGIN\n" + summary + "\nFABRIK_SUMMARY_END\nFABRIK_STAGE_COMPLETE"
			return output, true, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)

	stgs := []*stages.Stage{
		{
			Name:                  "Implement",
			Order:                 1,
			Prompt:                "implement it",
			CreateDraftPR:         true,
			MarkPRReadyOnComplete: true,
			Completion:            stages.CompletionCriteria{Type: "claude"},
		},
	}
	eng.cfg.Stages = stgs
	opts := make(map[string]string)
	for _, s := range stgs {
		opts[s.Name] = "OPT_" + s.Name
	}
	eng.statusField = &gh.StatusField{FieldID: "FIELD_1", Options: opts}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: issueNum,
		Title:  "My feature",
		Status: "Implement",
		ItemID: "PVTI_50",
	}

	eng.processItem(t.Context(), board, item)

	if verificationUpdateBody == "" {
		t.Fatal("expected UpdateIssueBody to be called on PR for Verification update")
	}
	if !strings.Contains(verificationUpdateBody, summary) {
		t.Errorf("Verification section should contain summary %q, got body: %q", summary, verificationUpdateBody)
	}
	if !strings.Contains(verificationUpdateBody, "Closes #50") {
		t.Error("Closes #50 must be preserved in updated body")
	}
}

func TestMarkPRReady_TransientThenSuccess(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManagerWithRoot(repoDir, repoDir+"/.fabrik/worktrees")

	orig := markPRReadyRetryDelay
	markPRReadyRetryDelay = 0
	t.Cleanup(func() { markPRReadyRetryDelay = orig })

	calls := 0
	client := &mockGitHubClient{
		markPRReadyFn: func(owner, repo string, prNumber int) error {
			calls++
			if calls < 3 {
				return errors.New("GitHub API returned 504: We couldn't respond to your request in time")
			}
			return nil
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", User: "u", Token: "t", Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 7, Title: "test"}
	eng.markPRReady(item, 77)

	if len(client.markPRReadyCalls) != 3 {
		t.Fatalf("expected 3 MarkPRReady calls (2 transient + 1 success), got %d", len(client.markPRReadyCalls))
	}
}

func TestMarkPRReady_AllTransientExhausted(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManagerWithRoot(repoDir, repoDir+"/.fabrik/worktrees")

	orig := markPRReadyRetryDelay
	markPRReadyRetryDelay = 0
	t.Cleanup(func() { markPRReadyRetryDelay = orig })

	client := &mockGitHubClient{
		markPRReadyFn: func(owner, repo string, prNumber int) error {
			return errors.New("GitHub API returned 504: We couldn't respond to your request in time")
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", User: "u", Token: "t", Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 8, Title: "test"}
	eng.markPRReady(item, 88)

	if len(client.markPRReadyCalls) != 3 {
		t.Fatalf("expected 3 MarkPRReady calls (all transient exhausted), got %d", len(client.markPRReadyCalls))
	}
}

// TestProcessItem_PostToPR_CreatesDraftPRBeforePosting verifies that when a stage
// completes with both create_draft_pr and post_to_pr true, the draft PR is created
// before postOutputToPR runs — so output is posted to the PR, not the issue fallback.
func TestProcessItem_PostToPR_CreatesDraftPRBeforePosting(t *testing.T) {
	skipIfNoGit(t)

	const issueNum = 60
	const prNum = 300

	// Set up a repo with a real remote so ensureDraftPR can push the branch.
	// initBareRepo creates a plain local repo; we use it as the "remote" and
	// clone it so the working repo has origin configured.
	remoteDir := initBareRepo(t)
	workingDir := t.TempDir()
	runCmd := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("cmd %v: %s: %v", args, out, err)
		}
	}
	runCmd("", "git", "clone", remoteDir, workingDir)
	runCmd(workingDir, "git", "config", "user.email", "test@test.com")
	runCmd(workingDir, "git", "config", "user.name", "Test")

	wm := NewWorktreeManager(workingDir)

	// prExists tracks whether the draft PR has been created yet.
	// FindPRForIssue returns 0 until CreateDraftPR is called, simulating the bug
	// scenario: no PR exists at stage start, PR is created by ensureDraftPR.
	var prExists bool
	var draftPRCreated bool
	// issueCommentBeforePR is set when AddComment is called on the issue number
	// before the draft PR has been created — this is the bug: full stage output
	// falls back to the issue instead of the PR.
	var issueCommentBeforePR bool
	// prCommentPosted is set when AddComment is called on the PR number,
	// confirming output was actually routed to the PR.
	var prCommentPosted bool

	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			if prExists {
				return prNum, nil
			}
			return 0, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			draftPRCreated = true
			prExists = true
			return prNum, nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			if issueNumber == issueNum && !prExists {
				// AddComment on the issue before the PR existed — this is the fallback bug.
				issueCommentBeforePR = true
			}
			if issueNumber == prNum {
				prCommentPosted = true
			}
			return issueNumber*10 + 1, nil
		},
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			if issueNumber == prNum {
				return "## Verification\n\n(Populated by Implement)\n\n---\n\nCloses #60", nil
			}
			return "issue body", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			return nil
		},
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return nil, nil
		},
	}

	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "Implementation done.\nFABRIK_STAGE_COMPLETE", true, TokenUsage{}, nil
		},
	}

	stgs := []*stages.Stage{
		{
			Name:          "Implement",
			Order:         1,
			Prompt:        "implement it",
			CreateDraftPR: true,
			PostToPR:      true,
			Completion:    stages.CompletionCriteria{Type: "claude"},
		},
	}
	statusOpts := make(map[string]string)
	for _, s := range stgs {
		statusOpts[s.Name] = "OPT_" + s.Name
	}

	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        stgs,
		},
		client,
		claude,
		wm,
	)
	eng.statusField = &gh.StatusField{FieldID: "FIELD_1", Options: statusOpts}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: issueNum,
		Title:  "My feature",
		Status: "Implement",
		ItemID: "PVTI_60",
	}

	eng.processItem(t.Context(), board, item)

	if !draftPRCreated {
		t.Fatal("expected CreateDraftPR to be called")
	}

	// The bug: full stage output posted to the issue before the PR existed.
	// With the fix, ensureDraftPR runs before postOutputToPR, so prExists is true
	// by the time any AddComment on the issue number fires (summary, not fallback).
	if issueCommentBeforePR {
		t.Error("AddComment called on issue before draft PR was created — output fell back to issue instead of PR")
	}
	if !prCommentPosted {
		t.Error("expected AddComment to be called on the PR — output was not posted to PR")
	}
}

func TestMarkPRReady_NonTransientNoRetry(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManagerWithRoot(repoDir, repoDir+"/.fabrik/worktrees")

	orig := markPRReadyRetryDelay
	markPRReadyRetryDelay = 0
	t.Cleanup(func() { markPRReadyRetryDelay = orig })

	client := &mockGitHubClient{
		markPRReadyFn: func(owner, repo string, prNumber int) error {
			return errors.New("GitHub API returned 422: Unprocessable Entity")
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", User: "u", Token: "t", Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 9, Title: "test"}
	eng.markPRReady(item, 99)

	if len(client.markPRReadyCalls) != 1 {
		t.Fatalf("expected 1 MarkPRReady call (non-transient, no retry), got %d", len(client.markPRReadyCalls))
	}
}

// ── ensureDraftPR — R2/R6 retry and closed-PR coverage ───────────────────────

// TestEnsureDraftPR_ExistingClosedPR_CreatesNew verifies R6: when FetchLinkedPR
// returns a closed PR, ensureDraftPR ignores it and creates a new draft PR.
func TestEnsureDraftPR_ExistingClosedPR_CreatesNew(t *testing.T) {
	skipIfNoGit(t)

	orig := ensureDraftPRRetryDelay
	ensureDraftPRRetryDelay = 0
	t.Cleanup(func() { ensureDraftPRRetryDelay = orig })

	sourceDir := initRepoWithRemote(t)
	wm := NewWorktreeManager(sourceDir)
	if _, err := wm.EnsureWorktree(50, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "closed"}, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			return 99, nil
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 50, Title: "Feature"}
	prNum, err := eng.ensureDraftPR(item, "main")
	if err != nil {
		t.Fatalf("ensureDraftPR returned unexpected error: %v", err)
	}
	if prNum != 99 {
		t.Errorf("expected new PR #99, got %d", prNum)
	}
	if len(client.createDraftPRCalls) != 1 {
		t.Errorf("expected 1 CreateDraftPR call, got %d", len(client.createDraftPRCalls))
	}
}

// TestEnsureDraftPR_TransientError_Retries verifies R2: a transient FetchLinkedPR
// error is retried; on the second attempt the call succeeds (returns nil, nil)
// and ensureDraftPR proceeds to push + create the PR.
func TestEnsureDraftPR_TransientError_Retries(t *testing.T) {
	skipIfNoGit(t)

	orig := ensureDraftPRRetryDelay
	ensureDraftPRRetryDelay = 0
	t.Cleanup(func() { ensureDraftPRRetryDelay = orig })

	sourceDir := initRepoWithRemote(t)
	wm := NewWorktreeManager(sourceDir)
	if _, err := wm.EnsureWorktree(51, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	var fetchCalls int
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			fetchCalls++
			if fetchCalls == 1 {
				return nil, fmt.Errorf("executing request: %w", &net.OpError{Op: "read", Net: "tcp"})
			}
			return nil, nil // no existing PR on second call
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			return 77, nil
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 51, Title: "Feature"}
	prNum, err := eng.ensureDraftPR(item, "main")
	if err != nil {
		t.Fatalf("ensureDraftPR returned unexpected error: %v", err)
	}
	if prNum != 77 {
		t.Errorf("expected PR #77, got %d", prNum)
	}
	if fetchCalls < 2 {
		t.Errorf("expected at least 2 FetchLinkedPR calls (retry), got %d", fetchCalls)
	}
}

// TestEnsureDraftPR_NonTransientError_ImmediateFailure verifies that a non-transient
// error from FetchLinkedPR (e.g. 422) returns (0, err) after exactly 1 attempt.
func TestEnsureDraftPR_NonTransientError_ImmediateFailure(t *testing.T) {
	orig := ensureDraftPRRetryDelay
	ensureDraftPRRetryDelay = 0
	t.Cleanup(func() { ensureDraftPRRetryDelay = orig })

	var fetchCalls int
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			fetchCalls++
			return nil, errors.New("GitHub API returned 422: validation failed")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 52, Title: "Feature"}
	prNum, err := eng.ensureDraftPR(item, "main")
	if err == nil {
		t.Fatal("expected error from non-transient FetchLinkedPR failure, got nil")
	}
	if prNum != 0 {
		t.Errorf("expected 0 on failure, got %d", prNum)
	}
	if fetchCalls != 1 {
		t.Errorf("expected exactly 1 FetchLinkedPR call (no retry on non-transient), got %d", fetchCalls)
	}
}

// TestEnsureDraftPR_AllRetriesExhausted_ReturnsError verifies R2: when all 3
// FetchLinkedPR attempts return transient errors, ensureDraftPR returns (0, err).
func TestEnsureDraftPR_AllRetriesExhausted_ReturnsError(t *testing.T) {
	orig := ensureDraftPRRetryDelay
	ensureDraftPRRetryDelay = 0
	t.Cleanup(func() { ensureDraftPRRetryDelay = orig })

	var fetchCalls int
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			fetchCalls++
			return nil, fmt.Errorf("executing request: %w", &net.OpError{Op: "read", Net: "tcp"})
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 53, Title: "Feature"}
	prNum, err := eng.ensureDraftPR(item, "main")
	if err == nil {
		t.Fatal("expected error after all retries exhausted, got nil")
	}
	if prNum != 0 {
		t.Errorf("expected 0 on exhausted retries, got %d", prNum)
	}
	if fetchCalls != 3 {
		t.Errorf("expected exactly 3 FetchLinkedPR calls (maxAttempts), got %d", fetchCalls)
	}
}

// TestEnsureDraftPR_CreateDraftPR_ReturnsZeroWithNilErr verifies Copilot finding:
// when CreateDraftPR returns (0, nil), ensureDraftPR treats it as a non-transient
// error rather than silently returning (0, nil) to the caller.
func TestEnsureDraftPR_CreateDraftPR_ReturnsZeroWithNilErr(t *testing.T) {
	skipIfNoGit(t)

	orig := ensureDraftPRRetryDelay
	ensureDraftPRRetryDelay = 0
	t.Cleanup(func() { ensureDraftPRRetryDelay = orig })

	sourceDir := initRepoWithRemote(t)
	wm := NewWorktreeManager(sourceDir)
	if _, err := wm.EnsureWorktree(54, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no existing PR
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			return 0, nil // unexpected: 0 with no error
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 54, Title: "Feature"}
	prNum, err := eng.ensureDraftPR(item, "main")
	if err == nil {
		t.Fatal("expected error when CreateDraftPR returns (0, nil), got nil")
	}
	if prNum != 0 {
		t.Errorf("expected 0 on zero-PR-number failure, got %d", prNum)
	}
}

// TestEnsureDraftPR_TitleFromDeepFetch verifies that when FetchItemDetails
// populates item.Title (the deep-fetch path, triggered after a cold-start probe),
// ensureDraftPR forwards that title to CreateDraftPR rather than passing "".
// This is the end-to-end regression test for the empty-title 422 bug.
func TestEnsureDraftPR_TitleFromDeepFetch(t *testing.T) {
	skipIfNoGit(t)

	orig := ensureDraftPRRetryDelay
	ensureDraftPRRetryDelay = 0
	t.Cleanup(func() { ensureDraftPRRetryDelay = orig })

	sourceDir := initRepoWithRemote(t)
	wm := NewWorktreeManager(sourceDir)
	if _, err := wm.EnsureWorktree(60, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	var capturedTitle string
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// Simulate what github.Client.FetchItemDetails now does after the fix:
			// populate item.Title from the GraphQL response.
			item.Title = "My Issue"
			return nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no existing PR
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			capturedTitle = title
			return 60, nil
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	// Simulate the cold-start path: item starts with empty title (probe never carries it).
	item := gh.ProjectItem{Number: 60, Title: "", Status: "Implement", ItemID: "PVTI_60"}

	// Simulate what the poll loop does before calling processItem: deep-fetch the item.
	if err := eng.readClient.FetchItemDetails(&item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	if item.Title != "My Issue" {
		t.Fatalf("FetchItemDetails did not populate title: got %q, want %q", item.Title, "My Issue")
	}

	// Now ensureDraftPR should receive and forward the populated title.
	prNum, err := eng.ensureDraftPR(item, "main")
	if err != nil {
		t.Fatalf("ensureDraftPR: %v", err)
	}
	if prNum != 60 {
		t.Errorf("expected prNum=60, got %d", prNum)
	}
	if capturedTitle != "My Issue" {
		t.Errorf("CreateDraftPR called with title=%q, want %q", capturedTitle, "My Issue")
	}
}
