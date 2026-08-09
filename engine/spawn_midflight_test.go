package engine

import (
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// reviewStages returns a Review stage (PostToPR: true, no CreateDraftPR,
// matching the real .fabrik/stages/review.yaml shape) followed by a cleanup
// Done stage, for exercising the #1419 mid-flight spawn hook.
func reviewStages() []*stages.Stage {
	return []*stages.Stage{
		{
			Name:       "Review",
			Order:      1,
			Prompt:     "review it",
			PostToPR:   true,
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
		{
			Name:            "Done",
			Order:           99,
			CleanupWorktree: true,
		},
	}
}

// validateStages mirrors reviewStages for the Validate stage.
func validateStages() []*stages.Stage {
	return []*stages.Stage{
		{
			Name:       "Validate",
			Order:      1,
			Prompt:     "validate it",
			PostToPR:   true,
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
		{
			Name:            "Done",
			Order:           99,
			CleanupWorktree: true,
		},
	}
}

// midflightSpawnClient builds a mockGitHubClient wired for a successful
// same-repo spawn: CreateIssue and AddProjectV2ItemById both succeed, and a
// PR is already linked to the issue (Review/Validate post to an existing PR,
// unlike Implement which creates one).
func midflightSpawnClient(childNodeID string, childNumber int, prNumber int) *mockGitHubClient {
	return &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return prNumber, nil
		},
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			return childNumber, childNodeID, nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
}

// TestFinalizeStageOutcome_ReviewSpawn_FullyWired is the requirement-2
// integration guard: a Review-stage output declaring a well-formed
// FABRIK_SPAWN_CHILD block gets full wiring — created, boarded, assigned,
// linked as a blocker of the parent, and labeled — exactly as a Plan-stage
// spawn would, via the same shared spawnChildren.
func TestFinalizeStageOutcome_ReviewSpawn_FullyWired(t *testing.T) {
	skipIfNoGit(t)

	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	client := midflightSpawnClient("I_midflight1", 201, 42)
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			output := "Found a blocker while reviewing.\n" +
				"FABRIK_SPAWN_CHILD_BEGIN owner/repo\n" +
				"TITLE: Fix the underlying library first\n" +
				"Discovered mid-review; must land before this PR.\n" +
				"FABRIK_SPAWN_CHILD_END\n" +
				"FABRIK_STAGE_COMPLETE\n"
			return output, true, TokenUsage{TurnsUsed: 3, MaxTurns: 30}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, reviewStages())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{ID: "I_parent_review", Number: 102, Title: "Parent under review", Status: "Review", ItemID: "PVTI_102"}

	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if len(client.createIssueCalls) != 1 {
		t.Fatalf("expected 1 CreateIssue call, got %d: %v", len(client.createIssueCalls), client.createIssueCalls)
	}
	call := client.createIssueCalls[0]
	if call.title != "Fix the underlying library first" {
		t.Errorf("title: got %q", call.title)
	}
	if len(call.assignees) != 1 || call.assignees[0] != "testuser" {
		t.Errorf("assignees: got %v, want [testuser]", call.assignees)
	}

	if len(client.addProjectV2ItemCalls) != 1 {
		t.Errorf("expected 1 AddProjectV2ItemById call, got %d", len(client.addProjectV2ItemCalls))
	}

	if len(client.addBlockedByIssueCalls) != 1 {
		t.Fatalf("expected 1 AddBlockedByIssue call, got %d", len(client.addBlockedByIssueCalls))
	}
	if client.addBlockedByIssueCalls[0].issueNodeID != "I_parent_review" {
		t.Errorf("AddBlockedByIssue issueNodeID: got %q, want I_parent_review", client.addBlockedByIssueCalls[0].issueNodeID)
	}
	if client.addBlockedByIssueCalls[0].blockerNodeID != "I_midflight1" {
		t.Errorf("AddBlockedByIssue blockerNodeID: got %q, want I_midflight1", client.addBlockedByIssueCalls[0].blockerNodeID)
	}

	var subIssueAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:sub-issue" {
			subIssueAdded = true
		}
	}
	if !subIssueAdded {
		t.Error("expected fabrik:sub-issue label on the spawned child")
	}

	// The stage itself must still complete normally.
	var stageComplete bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Review:complete" {
			stageComplete = true
		}
	}
	if !stageComplete {
		t.Error("expected stage:Review:complete to be added — a mid-flight spawn must not block the stage's own completion")
	}

	// The receipt note must appear in the detailed PR comment.
	var prComment string
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 42 {
			prComment = c.body
		}
	}
	if prComment == "" {
		t.Fatalf("expected a comment posted to PR #42, calls: %v", client.addCommentCalls)
	}
	if !strings.Contains(prComment, "Spawned 1 sub-issue") {
		t.Errorf("expected mid-flight spawn receipt note in PR comment, got: %s", prComment)
	}
	if !strings.Contains(prComment, "owner/repo#201") {
		t.Errorf("expected receipt note to name the spawned child, got: %s", prComment)
	}
	// The raw declaration block must not also leak into the posted comment —
	// the receipt note already says what was spawned; leaving the internal
	// BEGIN/TITLE/END marker syntax visible would duplicate that information
	// verbatim (pruefer review finding on #1419's PR).
	if strings.Contains(prComment, "FABRIK_SPAWN_CHILD_BEGIN") || strings.Contains(prComment, "FABRIK_SPAWN_CHILD_END") {
		t.Errorf("expected raw FABRIK_SPAWN_CHILD markers to be stripped from the posted comment, got: %s", prComment)
	}
	if strings.Contains(prComment, "TITLE: Fix the underlying library first") {
		t.Errorf("expected the raw TITLE: line to be stripped from the posted comment, got: %s", prComment)
	}
	if !strings.Contains(prComment, "Found a blocker while reviewing.") {
		t.Errorf("expected the stage's own preceding narrative line to survive stripping (only the block itself is removed), got: %s", prComment)
	}
}

// TestFinalizeStageOutcome_ValidateSpawn_FullyWired mirrors the Review test
// for the Validate stage — requirement 1's "at minimum Review and Validate."
func TestFinalizeStageOutcome_ValidateSpawn_FullyWired(t *testing.T) {
	skipIfNoGit(t)

	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	client := midflightSpawnClient("I_midflight2", 202, 43)
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			output := "FABRIK_SPAWN_CHILD_BEGIN owner/repo\n" +
				"TITLE: Blocker discovered during validation\n" +
				"Body.\n" +
				"FABRIK_SPAWN_CHILD_END\n" +
				"FABRIK_STAGE_COMPLETE\n"
			return output, true, TokenUsage{TurnsUsed: 3, MaxTurns: 30}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, validateStages())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{ID: "I_parent_validate", Number: 103, Title: "Parent under validation", Status: "Validate", ItemID: "PVTI_103"}

	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if len(client.createIssueCalls) != 1 {
		t.Fatalf("expected 1 CreateIssue call, got %d", len(client.createIssueCalls))
	}
	if len(client.addBlockedByIssueCalls) != 1 || client.addBlockedByIssueCalls[0].issueNodeID != "I_parent_validate" {
		t.Fatalf("expected 1 AddBlockedByIssue call linking the parent, got %v", client.addBlockedByIssueCalls)
	}

	var stageComplete bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			stageComplete = true
		}
	}
	if !stageComplete {
		t.Error("expected stage:Validate:complete to be added")
	}
}

// TestFinalizeStageOutcome_ReviewSpawn_ProseOnlyMention_NoSpawn is the #1263
// hardening regression guard applied to the new call site: Review output that
// merely mentions the marker in prose (without a well-formed own-line block)
// must not spawn anything — the same own-line discipline ParseSpawnBlocks
// already enforces for Plan applies identically here, since both call sites
// share the same parser.
func TestFinalizeStageOutcome_ReviewSpawn_ProseOnlyMention_NoSpawn(t *testing.T) {
	skipIfNoGit(t)

	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 44, nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			output := "- [ ] Emit `FABRIK_SPAWN_CHILD_BEGIN owner/repo` block if a blocker turns up\n" +
				"No blocker found this cycle.\n" +
				"FABRIK_STAGE_COMPLETE\n"
			return output, true, TokenUsage{TurnsUsed: 3, MaxTurns: 30}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, reviewStages())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{ID: "I_parent_prose", Number: 104, Title: "Parent, no real spawn", Status: "Review", ItemID: "PVTI_104"}

	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if len(client.createIssueCalls) != 0 {
		t.Errorf("expected 0 CreateIssue calls for a prose-only mention, got %d", len(client.createIssueCalls))
	}
	if len(client.addBlockedByIssueCalls) != 0 {
		t.Errorf("expected 0 AddBlockedByIssue calls, got %d", len(client.addBlockedByIssueCalls))
	}

	var stageComplete bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Review:complete" {
			stageComplete = true
		}
	}
	if !stageComplete {
		t.Error("expected stage:Review:complete to be added — no genuine spawn block present")
	}
}

// TestFinalizeStageOutcome_ReviewSpawn_Failure_PausesParentSuppressesCompletion
// verifies that when spawnChildren fails (e.g. CreateIssue errors), the parent
// is paused (spawnChildren's own fail-loud path) and this dispatch's own
// stage-complete comment/label is suppressed — mirroring the FABRIK_PR_CREATE
// failure precedent in finalizeStageOutcome exactly.
func TestFinalizeStageOutcome_ReviewSpawn_Failure_PausesParentSuppressesCompletion(t *testing.T) {
	skipIfNoGit(t)

	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 45, nil
		},
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			return 0, "", errors.New("github: 500 internal server error")
		},
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			output := "FABRIK_SPAWN_CHILD_BEGIN owner/repo\n" +
				"TITLE: Will fail to create\n" +
				"Body.\n" +
				"FABRIK_SPAWN_CHILD_END\n" +
				"FABRIK_STAGE_COMPLETE\n"
			return output, true, TokenUsage{TurnsUsed: 3, MaxTurns: 30}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, reviewStages())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{ID: "I_parent_fail", Number: 105, Title: "Parent whose spawn fails", Status: "Review", ItemID: "PVTI_105"}

	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	var pausedAdded, stageCompleteAdded bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			pausedAdded = true
		case "stage:Review:complete":
			stageCompleteAdded = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused when the mid-flight spawn fails")
	}
	if stageCompleteAdded {
		t.Error("expected stage:Review:complete to be suppressed when the mid-flight spawn fails")
	}

	// The failure comment (posted by spawnChildren's own pauseIssue call) must
	// exist, but the stage's own narrative output must not also be posted.
	var sawFailureComment bool
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "spawn failed") {
			sawFailureComment = true
		}
		if strings.Contains(c.body, "Will fail to create") {
			t.Errorf("stage's own narrative output must not be posted when the spawn fails, got: %s", c.body)
		}
	}
	if !sawFailureComment {
		t.Error("expected a spawn-failure comment to be posted")
	}
}
