package engine

import (
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// planStages returns a Plan stage (no PostToPR, no CreateDraftPR — matching
// the real .fabrik/stages/plan.yaml shape) followed by a cleanup Done stage,
// for exercising the issue-comment posting path a decomposing Plan uses.
func planStages() []*stages.Stage {
	return []*stages.Stage{
		{
			Name:       "Plan",
			Order:      1,
			Prompt:     "plan it",
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
		{
			Name:            "Done",
			Order:           99,
			CleanupWorktree: true,
		},
	}
}

// TestFinalizeStageOutcome_SpawnBlocks_NoteAndFooterBothPosted is the AC4
// integration guard: a decomposing Plan completion posts both the spawn
// receipt note and the existing stats footer on the issue comment, in that
// order (#1338).
func TestFinalizeStageOutcome_SpawnBlocks_NoteAndFooterBothPosted(t *testing.T) {
	skipIfNoGit(t)

	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			output := "FABRIK_SPAWN_CHILD_BEGIN owner/child-repo\n" +
				"TITLE: Add authentication module\n" +
				"Implement OAuth2 authentication.\n" +
				"FABRIK_SPAWN_CHILD_END\n" +
				"FABRIK_STAGE_COMPLETE\n"
			return output, true, TokenUsage{TurnsUsed: 5, MaxTurns: 30, InputTokens: 1000, OutputTokens: 2000}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, planStages())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 90, Title: "Decomposing issue", Status: "Plan", ItemID: "PVTI_90"}

	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment posted, got %d: %v", len(client.addCommentCalls), client.addCommentCalls)
	}
	body := client.addCommentCalls[0].body

	noteIdx := strings.Index(body, "1 sub-issue")
	footerIdx := strings.Index(body, "Used 5 turns")
	if noteIdx == -1 {
		t.Errorf("expected spawn receipt note in comment body, got: %s", body)
	}
	if footerIdx == -1 {
		t.Errorf("expected stats footer in comment body, got: %s", body)
	}
	if noteIdx != -1 && footerIdx != -1 && noteIdx > footerIdx {
		t.Errorf("expected note before stats footer, note at %d, footer at %d", noteIdx, footerIdx)
	}
	if !strings.Contains(body, "FABRIK_SPAWN_CHILD_BEGIN owner/child-repo") {
		t.Errorf("expected raw spawn block to still appear in the posted comment, got: %s", body)
	}
}

// researchStages returns a Research stage (not "Plan") followed by a cleanup
// Done stage, for exercising the #1338 review-finding guard below.
func researchStages() []*stages.Stage {
	return []*stages.Stage{
		{
			Name:       "Research",
			Order:      1,
			Prompt:     "research it",
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
		{
			Name:            "Done",
			Order:           99,
			CleanupWorktree: true,
		},
	}
}

// TestFinalizeStageOutcome_SpawnBlocks_NonPlanStage_NoNote is the #1338
// review-finding guard: preImplement only ever reads the comment literally
// named "Plan" (engine/spawn.go, findStageComment(item.Comments, "Plan")), so
// a note posted on any other stage's comment would promise a spawn that
// mechanism never performs — e.g. if a later stage's Claude quotes a spawn
// block back verbatim from context (later stages receive the Plan comment
// through .fabrik-context/stage-Plan.md). This proves the stage.Name ==
// "Plan" gate in finalizeStageOutcome actually suppresses the note when
// well-formed spawn blocks appear in a non-Plan stage's output, while the raw
// marker text and the stats footer remain unaffected.
func TestFinalizeStageOutcome_SpawnBlocks_NonPlanStage_NoNote(t *testing.T) {
	skipIfNoGit(t)

	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			output := "As declared in the plan:\n" +
				"FABRIK_SPAWN_CHILD_BEGIN owner/child-repo\n" +
				"TITLE: Add authentication module\n" +
				"Implement OAuth2 authentication.\n" +
				"FABRIK_SPAWN_CHILD_END\n" +
				"FABRIK_STAGE_COMPLETE\n"
			return output, true, TokenUsage{TurnsUsed: 5, MaxTurns: 30, InputTokens: 1000, OutputTokens: 2000}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, researchStages())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 92, Title: "Decomposing issue", Status: "Research", ItemID: "PVTI_92"}

	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment posted, got %d: %v", len(client.addCommentCalls), client.addCommentCalls)
	}
	body := client.addCommentCalls[0].body

	if strings.Contains(body, "sub-issue") {
		t.Errorf("expected no spawn receipt note on a non-Plan stage comment, got: %s", body)
	}
	if !strings.Contains(body, "FABRIK_SPAWN_CHILD_BEGIN owner/child-repo") {
		t.Errorf("expected raw spawn block text to still appear in the posted comment, got: %s", body)
	}
	if !strings.Contains(body, "Used 5 turns") {
		t.Errorf("expected stats footer in comment body, got: %s", body)
	}
}

// TestPostOutputToPR_SpawnBlocks_NoteAppearsInComment is the AC5 guard: the
// post_to_pr path renders the note under the same N>0 condition as the
// issue-comment path, since both flow through the same footer value computed
// once at its origin (finalizeStageOutcome).
func TestPostOutputToPR_SpawnBlocks_NoteAppearsInComment(t *testing.T) {
	var addCommentCalls []addCommentCall
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 42, nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			addCommentCalls = append(addCommentCalls, addCommentCall{owner, repo, issueNumber, body})
			return 0, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 91, Title: "Decomposing issue"}

	postOutput := "FABRIK_SPAWN_CHILD_BEGIN owner/child-repo\n" +
		"TITLE: Add authentication module\n" +
		"Implement OAuth2 authentication.\n" +
		"FABRIK_SPAWN_CHILD_END"
	footer := formatSpawnReceiptNote(postOutput) + formatStatsFooter(TokenUsage{TurnsUsed: 5, MaxTurns: 30, InputTokens: 1000, OutputTokens: 2000}, true)

	eng.postOutputToPR(item, "Plan", postOutput, footer, "branch", "sha", "main-sha", "2024-01-01")

	var prComment string
	for _, c := range addCommentCalls {
		if c.issueNumber == 42 {
			prComment = c.body
		}
	}
	if prComment == "" {
		t.Fatalf("expected a comment posted to PR #42, calls: %v", addCommentCalls)
	}
	if !strings.Contains(prComment, "1 sub-issue") {
		t.Errorf("expected spawn receipt note in PR comment, got: %s", prComment)
	}
	if !strings.Contains(prComment, "Used 5/30 turns") {
		t.Errorf("expected stats footer in PR comment, got: %s", prComment)
	}
}
