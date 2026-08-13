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
	eng := testEngine(t, client, nil)

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
	eng := testEngine(t, client, nil)

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
	eng := testEngine(t, client, nil)

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
	eng := testEngine(t, client, nil)

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
	eng := testEngine(t, &mockGitHubClient{}, nil)
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
	eng := testEngine(t, client, nil)
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
	eng := testEngine(t, client, nil)
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
	closingIssuesCallCount := 0
	var capturedHealBody string
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRNumber = 0 // derived field never catches up within this test
			return nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, State: "open", HeadSHA: "abc123"}, nil
		},
		// Re-verification is now body-based (verifyAndHealLinkageByBody), not a
		// second FetchItemDetails call — mirrors
		// TestVerifyAndHealLinkage_NonDefaultBase_HealSuccess's existing pattern.
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			closingIssuesCallCount++
			if closingIssuesCallCount == 1 {
				return nil, nil // linkage missing on first check
			}
			return []int{1}, nil // linkage present after heal
		},
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "## Summary\n\nThis PR does something.", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			capturedHealBody = body
			return nil
		},
	}
	eng := testEngine(t, client, nil)
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

// TestVerifyAndHealLinkage_DerivedFieldLags_BodyAlreadyLinked is AC1's non-vacuous
// proof: a PR whose body already carries "Closes #N", read before GitHub has derived
// closedByPullRequestsReferences, must not pause the issue and must not have its body
// edited. fetchItemDetailsFn always reports LinkedPRNumber == 0 (the derived field
// never catches up within this test), simulating the #1598 race exactly. Reverting
// the R1 restructuring in verifyAndHealLinkage (delegating to
// verifyAndHealLinkageByBody instead of trusting the lagging derived field a second
// time) makes this test pause the issue — the old default-branch-only heal/re-verify
// block re-read the same lagging field and had no other source of truth.
func TestVerifyAndHealLinkage_DerivedFieldLags_BodyAlreadyLinked(t *testing.T) {
	bodyEdited := false
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRNumber = 0 // derived field never catches up within this test
			return nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, State: "open", HeadSHA: "abc123"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{1}, nil // body already carries "Closes #1"
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			bodyEdited = true
			return nil
		},
	}
	eng := testEngine(t, client, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Implement"}

	ok := eng.verifyAndHealLinkage(context.Background(), item, 42, stage, "owner", "repo", "owner/repo")
	if !ok {
		t.Error("should return true when the PR body already carries the closing keyword")
	}
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause when body already carries the closing keyword")
		}
	}
	if bodyEdited {
		t.Error("body should not be edited when the closing keyword is already present")
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
	eng := testEngine(t, client, nil)

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
	eng := testEngine(t, client, nil)
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

// ---- verifyAndHealLinkage tests: base:<branch> (non-default-base) repos ----
//
// On these repos closedByPullRequestsReferences/closingIssuesReferences are structurally
// empty (GitHub only populates them for PRs targeting the repo default branch), so
// linkage must be confirmed via FetchPRClosingIssues (PR body regex) instead of
// FetchItemDetails/item.LinkedPRNumber. See #1046/#1047.

func TestVerifyAndHealLinkage_NonDefaultBase_AlreadyLinkedViaBody(t *testing.T) {
	fetchItemDetailsCalls := 0
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			fetchItemDetailsCalls++
			return nil
		},
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, State: "open", HeadSHA: "abc123"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{1}, nil // body already contains "Closes #1"
		},
	}
	eng := testEngine(t, client, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo", Labels: []string{"base:develop"}}
	stage := &stages.Stage{Name: "Implement"}

	ok := eng.verifyAndHealLinkage(context.Background(), item, 42, stage, "owner", "repo", "owner/repo")
	if !ok {
		t.Error("should return true when linkage is already confirmed via PR body")
	}
	if fetchItemDetailsCalls != 0 {
		t.Errorf("FetchItemDetails should not be called on the non-default-base path, got %d calls", fetchItemDetailsCalls)
	}
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause when linkage already confirmed")
		}
	}
}

func TestVerifyAndHealLinkage_NonDefaultBase_HealSuccess(t *testing.T) {
	callCount := 0
	var capturedHealBody string
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, State: "open", HeadSHA: "abc123"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			callCount++
			if callCount == 1 {
				return nil, nil // linkage missing on first check
			}
			return []int{1}, nil // linkage present after heal
		},
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "## Summary\n\nThis PR does something.", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			capturedHealBody = body
			return nil
		},
	}
	eng := testEngine(t, client, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo", Labels: []string{"base:develop"}}
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
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause on successful heal")
		}
	}
}

func TestVerifyAndHealLinkage_NonDefaultBase_HealFails_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, State: "open", HeadSHA: "abc123"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return nil, nil // body never contains the closing keyword, even after heal
		},
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "## Summary\n\nThis PR does something.", nil
		},
	}
	eng := testEngine(t, client, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo", Labels: []string{"base:develop"}}
	stage := &stages.Stage{Name: "Implement"}

	ok := eng.verifyAndHealLinkage(context.Background(), item, 42, stage, "owner", "repo", "owner/repo")
	if ok {
		t.Error("should return false when body still lacks the closing keyword after heal")
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
		t.Error("issue should be paused when heal doesn't confirm linkage")
	}
}

func TestVerifyAndHealLinkage_NonDefaultBase_NoPRFound(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no PR on branch
		},
	}
	eng := testEngine(t, client, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo", Labels: []string{"base:develop"}}
	stage := &stages.Stage{Name: "Implement"}

	ok := eng.verifyAndHealLinkage(context.Background(), item, 42, stage, "owner", "repo", "owner/repo")
	if !ok {
		t.Error("should return true when no PR found via branch (user-diverged)")
	}
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause when no PR found via branch lookup")
		}
	}
}

// ---- attemptLinkageHeal tests: R2 (never double-write an existing keyword) ----

// TestAttemptLinkageHeal_KeywordAlreadyPresent_SkipsWrite is AC3's direct proof:
// attemptLinkageHeal itself must not prepend a second closing keyword when the
// fetched body already carries one for this issue, regardless of what the caller's
// own pre-check concluded.
func TestAttemptLinkageHeal_KeywordAlreadyPresent_SkipsWrite(t *testing.T) {
	bodyEdited := false
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "Closes #1\n\n## Summary\n\nThis PR does something.", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			bodyEdited = true
			return nil
		},
	}
	eng := testEngine(t, client, nil)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	pr := &gh.PRDetails{Number: 42, State: "open", HeadSHA: "abc123"}
	stage := &stages.Stage{Name: "Implement"}

	closingLine, ok := eng.attemptLinkageHeal(item, pr, stage, "owner", "repo", "owner/repo")
	if !ok {
		t.Error("should return ok=true when the keyword is already present")
	}
	if closingLine != "Closes #1" {
		t.Errorf("want closingLine %q, got %q", "Closes #1", closingLine)
	}
	if bodyEdited {
		t.Error("body should not be edited when the closing keyword is already present")
	}

	// The idempotency guard must not be consumed by a skipped write — a genuine
	// heal attempted later for the same head SHA must still be allowed to run.
	snap, _ := eng.store.Get("owner/repo", item.Number)
	if snap.LinkageHealAttempted(stage.Name, pr.HeadSHA) {
		t.Error("idempotency guard should not be recorded when no write occurred")
	}
}
