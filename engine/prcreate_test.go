package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// ---- ParsePRCreateBlock unit tests ----

func TestParsePRCreateBlock_NotFound(t *testing.T) {
	block, err := ParsePRCreateBlock("no marker here")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if block != nil {
		t.Fatalf("expected nil block, got %+v", block)
	}
}

func TestParsePRCreateBlock_ValidNoTargetRepo(t *testing.T) {
	input := `
Some output before the marker.

FABRIK_PR_CREATE_BEGIN
TITLE: Add authentication module

This PR implements the authentication feature.

Key changes:
- JWT validation
- Session handling
FABRIK_PR_CREATE_END

Some output after.
`
	block, err := ParsePRCreateBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block == nil {
		t.Fatal("expected non-nil block")
	}
	if block.TargetRepo != "" {
		t.Errorf("TargetRepo: got %q, want empty", block.TargetRepo)
	}
	if block.Title != "Add authentication module" {
		t.Errorf("Title: got %q, want %q", block.Title, "Add authentication module")
	}
	if !strings.Contains(block.Body, "JWT validation") {
		t.Errorf("Body should contain 'JWT validation', got: %q", block.Body)
	}
}

func TestParsePRCreateBlock_ValidWithTargetRepo(t *testing.T) {
	input := `FABRIK_PR_CREATE_BEGIN owner/other-repo
TITLE: Cross-repo PR

This is a cross-repo PR body.
FABRIK_PR_CREATE_END`
	block, err := ParsePRCreateBlock(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block == nil {
		t.Fatal("expected non-nil block")
	}
	if block.TargetRepo != "owner/other-repo" {
		t.Errorf("TargetRepo: got %q, want %q", block.TargetRepo, "owner/other-repo")
	}
	if block.Title != "Cross-repo PR" {
		t.Errorf("Title: got %q", block.Title)
	}
}

func TestParsePRCreateBlock_MissingTitle(t *testing.T) {
	input := `FABRIK_PR_CREATE_BEGIN
Some body content without a TITLE: line.
FABRIK_PR_CREATE_END`
	block, err := ParsePRCreateBlock(input)
	if err == nil {
		t.Fatal("expected error for missing TITLE, got nil")
	}
	if block != nil {
		t.Fatalf("expected nil block, got %+v", block)
	}
	if !strings.Contains(err.Error(), "TITLE") {
		t.Errorf("error should mention TITLE, got: %v", err)
	}
}

func TestParsePRCreateBlock_EmptyBody(t *testing.T) {
	input := `FABRIK_PR_CREATE_BEGIN
TITLE: PR with empty body
FABRIK_PR_CREATE_END`
	block, err := ParsePRCreateBlock(input)
	if err == nil {
		t.Fatal("expected error for empty body, got nil")
	}
	if block != nil {
		t.Fatalf("expected nil block, got %+v", block)
	}
}

func TestParsePRCreateBlock_MissingEnd(t *testing.T) {
	input := `FABRIK_PR_CREATE_BEGIN
TITLE: Missing end marker

PR body content.`
	block, err := ParsePRCreateBlock(input)
	if err == nil {
		t.Fatal("expected error for missing END, got nil")
	}
	if block != nil {
		t.Fatalf("expected nil block, got %+v", block)
	}
}

func TestParsePRCreateBlock_InvalidRepoFormat(t *testing.T) {
	input := `FABRIK_PR_CREATE_BEGIN noslash
TITLE: Bad repo

Body.
FABRIK_PR_CREATE_END`
	_, err := ParsePRCreateBlock(input)
	if err == nil {
		t.Fatal("expected error for invalid repo format, got nil")
	}
}

func TestParsePRCreateBlock_InvalidRepoFormatExtraSegment(t *testing.T) {
	// "owner/repo/extra" has two slashes — old validation passed it, new validation rejects it.
	input := `FABRIK_PR_CREATE_BEGIN owner/repo/extra
TITLE: Bad repo

Body.
FABRIK_PR_CREATE_END`
	_, err := ParsePRCreateBlock(input)
	if err == nil {
		t.Fatal("expected error for owner/repo/extra format, got nil")
	}
}

// ---- processPRCreateMarker tests ----

func TestProcessPRCreateMarker_Success(t *testing.T) {
	ensureDraftPRRetryDelay = 0
	var capturedBody string
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no existing PR
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			capturedBody = body
			return 42, nil
		},
	}
	eng := testEngine(client, nil)

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	block := &PRCreateBlock{
		Title: "My PR Title",
		Body:  "This implements the feature.",
	}

	prNum, err := eng.processPRCreateMarker(context.Background(), item, block, "owner", "repo", "main", "owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prNum != 42 {
		t.Errorf("prNum: got %d, want 42", prNum)
	}
	// Verify "Closes #1" is the FIRST line of the PR body.
	if !strings.HasPrefix(capturedBody, "Closes #1\n\n") {
		t.Errorf("PR body should start with 'Closes #1\\n\\n', got: %q", capturedBody)
	}
	// Verify the skill body is included.
	if !strings.Contains(capturedBody, "This implements the feature.") {
		t.Errorf("PR body should contain skill body content")
	}
}

func TestProcessPRCreateMarker_IdempotencyExistingPR(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, State: "open"}, nil
		},
	}
	eng := testEngine(client, nil)

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	block := &PRCreateBlock{Title: "T", Body: "B"}

	prNum, err := eng.processPRCreateMarker(context.Background(), item, block, "owner", "repo", "main", "owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prNum != 99 {
		t.Errorf("should reuse existing PR #99, got %d", prNum)
	}
	// Verify no new PR was created.
	client.mu.Lock()
	created := len(client.createDraftPRCalls)
	client.mu.Unlock()
	if created != 0 {
		t.Errorf("should not create a new PR when one exists, createDraftPRCalls = %d", created)
	}
}

func TestProcessPRCreateMarker_CrossRepoNotSupported(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, nil)

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	block := &PRCreateBlock{
		TargetRepo: "other-org/other-repo",
		Title:      "T",
		Body:       "B",
	}

	prNum, err := eng.processPRCreateMarker(context.Background(), item, block, "owner", "repo", "main", "owner/repo")
	if err == nil {
		t.Fatal("expected error for cross-repo, got nil")
	}
	if prNum != 0 {
		t.Errorf("expected prNum 0 on error, got %d", prNum)
	}
	// Issue should be paused.
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Error("issue should be paused for cross-repo PR creation attempt")
	}
}

func TestProcessPRCreateMarker_CreateFailurePauses(t *testing.T) {
	ensureDraftPRRetryDelay = 0
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			return 0, fmt.Errorf("API rate limit exceeded")
		},
	}
	eng := testEngine(client, nil)

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	block := &PRCreateBlock{Title: "T", Body: "Body content here."}

	prNum, err := eng.processPRCreateMarker(context.Background(), item, block, "owner", "repo", "main", "owner/repo")
	if err == nil {
		t.Fatal("expected error on create failure, got nil")
	}
	if prNum != 0 {
		t.Errorf("expected prNum 0 on error, got %d", prNum)
	}
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Error("issue should be paused on PR creation failure")
	}
}

// ---- verifyAndHealLinkage tests ----

func TestVerifyAndHealLinkage_NoPR(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Implement"}
	ok := eng.verifyAndHealLinkage(context.Background(), item, 0, stage, "owner", "repo", "owner/repo")
	if !ok {
		t.Error("should return true when prNumber == 0")
	}
}

func TestVerifyAndHealLinkage_AlreadyLinked(t *testing.T) {
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRNumber = 42
			return nil
		},
	}
	eng := testEngine(client, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Implement"}
	ok := eng.verifyAndHealLinkage(context.Background(), item, 42, stage, "owner", "repo", "owner/repo")
	if !ok {
		t.Error("should return true when linkage already confirmed")
	}
}

func TestVerifyAndHealLinkage_BranchMismatch_NoLinkedPR(t *testing.T) {
	// FetchItemDetails returns LinkedPRNumber=0 but FetchLinkedPR finds nothing.
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRNumber = 0
			return nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no PR on branch
		},
	}
	eng := testEngine(client, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Implement"}
	ok := eng.verifyAndHealLinkage(context.Background(), item, 42, stage, "owner", "repo", "owner/repo")
	if !ok {
		t.Error("should return true when no PR found via branch (user-diverged)")
	}
	// Issue should NOT be paused.
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause when no PR found via branch lookup")
		}
	}
}

func TestVerifyAndHealLinkage_HealSuccess(t *testing.T) {
	callCount := 0
	var capturedHealBody string
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			callCount++
			if callCount == 1 {
				item.LinkedPRNumber = 0 // linkage missing on first check
			} else {
				item.LinkedPRNumber = 42 // linkage present after heal
			}
			return nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, State: "open", HeadSHA: "abc123"}, nil
		},
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "## Summary\n\nThis PR does something.", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			capturedHealBody = body
			return nil
		},
	}
	eng := testEngine(client, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Implement"}

	ok := eng.verifyAndHealLinkage(context.Background(), item, 42, stage, "owner", "repo", "owner/repo")
	if !ok {
		t.Error("heal should succeed, expected true")
	}
	if !strings.HasPrefix(capturedHealBody, "Closes #1\n\n") {
		t.Errorf("healed body should start with 'Closes #1\\n\\n', got: %q", capturedHealBody)
	}
	if !strings.Contains(capturedHealBody, "## Summary") {
		t.Error("healed body should contain original body content")
	}
	// Issue should NOT be paused on successful heal.
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause on successful heal")
		}
	}
}

func TestVerifyAndHealLinkage_HealAlreadyAttempted(t *testing.T) {
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRNumber = 0
			return nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, State: "open", HeadSHA: "abc123"}, nil
		},
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "PR body.", nil
		},
	}
	eng := testEngine(client, nil)

	// Record that we've already attempted healing for this SHA.
	eng.store.Apply(itemstate.LinkageHealAttempted{
		Repo:      "owner/repo",
		Number:    1,
		StageName: "Implement",
		PRSHA:     "abc123",
	})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Implement"}

	ok := eng.verifyAndHealLinkage(context.Background(), item, 42, stage, "owner", "repo", "owner/repo")
	if ok {
		t.Error("should return false when heal was already attempted")
	}
	// Issue should be paused.
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Error("issue should be paused when heal was already attempted")
	}
}

func TestVerifyAndHealLinkage_BodyTooLong(t *testing.T) {
	// Generate a body that exceeds the 65300 char limit when closing line is prepended.
	longBody := strings.Repeat("x", 65300)
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRNumber = 0
			return nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, State: "open", HeadSHA: "sha1"}, nil
		},
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return longBody, nil
		},
	}
	eng := testEngine(client, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Implement"}

	ok := eng.verifyAndHealLinkage(context.Background(), item, 42, stage, "owner", "repo", "owner/repo")
	if ok {
		t.Error("should return false when body is too long for auto-heal")
	}
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Error("issue should be paused when body is too long")
	}
}
