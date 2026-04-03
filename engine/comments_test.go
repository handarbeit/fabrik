package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

func TestFindStageComment(t *testing.T) {
	comments := []gh.Comment{
		{
			ID:         "c1",
			DatabaseID: 101,
			Author:     "fabrik",
			Body:       "🏭 **Fabrik — stage: Research**\n*branch: main | commit: abc | 2026-01-01*\n\nResearch output here.",
		},
		{
			ID:         "c2",
			DatabaseID: 102,
			Author:     "user",
			Body:       "This is a user comment.",
		},
		{
			ID:         "c3",
			DatabaseID: 103,
			Author:     "fabrik",
			Body:       "🏭 **Fabrik — stage: Plan**\n*branch: main | commit: def | 2026-01-02*\n\nPlan output here.",
		},
		{
			ID:         "c4",
			DatabaseID: 104,
			Author:     "fabrik",
			Body:       "🏭 **Fabrik — stage: Research**\n*branch: main | commit: xyz | 2026-01-03*\n\nUpdated research.",
		},
	}

	t.Run("finds research comment", func(t *testing.T) {
		c := findStageComment(comments, "Research")
		if c == nil {
			t.Fatal("expected non-nil comment")
		}
		// Should return the LAST matching comment (c4)
		if c.DatabaseID != 104 {
			t.Errorf("expected DatabaseID=104 (most recent), got %d", c.DatabaseID)
		}
	})

	t.Run("finds plan comment", func(t *testing.T) {
		c := findStageComment(comments, "Plan")
		if c == nil {
			t.Fatal("expected non-nil comment")
		}
		if c.DatabaseID != 103 {
			t.Errorf("expected DatabaseID=103, got %d", c.DatabaseID)
		}
	})

	t.Run("returns nil for missing stage", func(t *testing.T) {
		c := findStageComment(comments, "Implement")
		if c != nil {
			t.Errorf("expected nil, got comment with DatabaseID=%d", c.DatabaseID)
		}
	})

	t.Run("returns nil for empty comments", func(t *testing.T) {
		c := findStageComment(nil, "Research")
		if c != nil {
			t.Errorf("expected nil for empty comments slice")
		}
	})

	t.Run("does not match partial prefix", func(t *testing.T) {
		partialComments := []gh.Comment{
			{DatabaseID: 200, Body: "🏭 **Fabrik — stage: ResearchExtra**\nsome content"},
		}
		c := findStageComment(partialComments, "Research")
		if c != nil {
			t.Error("should not match ResearchExtra when searching for Research")
		}
	})
}

func TestCollectStageComments(t *testing.T) {
	researchComment := "🏭 **Fabrik — stage: Research**\n*branch: main*\n\nResearch findings."
	planComment := "🏭 **Fabrik — stage: Plan**\n*branch: main*\n\nPlan content."
	implementComment := "🏭 **Fabrik — stage: Implement**\n*branch: main*\n\nImplementation notes."

	item := gh.ProjectItem{
		Number: 42,
		Comments: []gh.Comment{
			{ID: "c1", DatabaseID: 1, Body: researchComment},
			{ID: "c2", DatabaseID: 2, Body: planComment},
			{ID: "c3", DatabaseID: 3, Body: implementComment},
		},
	}

	allStages := []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "research", Completion: stages.CompletionCriteria{Type: "claude"}},
		{Name: "Plan", Order: 2, Prompt: "plan", Completion: stages.CompletionCriteria{Type: "claude"}},
		{Name: "Implement", Order: 3, Prompt: "implement", Completion: stages.CompletionCriteria{Type: "claude"}},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}

	eng := NewWithDeps(
		Config{Stages: allStages},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		NewWorktreeManager("/tmp"),
	)

	implementStage := allStages[2]

	t.Run("prior stages only (stage workflow)", func(t *testing.T) {
		result := eng.collectStageComments(item, implementStage, false)
		if _, ok := result["Research"]; !ok {
			t.Error("expected Research in result")
		}
		if _, ok := result["Plan"]; !ok {
			t.Error("expected Plan in result")
		}
		if _, ok := result["Implement"]; ok {
			t.Error("Implement should not be included when includeCurrent=false")
		}
		if _, ok := result["Done"]; ok {
			t.Error("Done (CleanupWorktree) should never be included")
		}
	})

	t.Run("prior + current (comment workflow)", func(t *testing.T) {
		result := eng.collectStageComments(item, implementStage, true)
		if _, ok := result["Research"]; !ok {
			t.Error("expected Research in result")
		}
		if _, ok := result["Plan"]; !ok {
			t.Error("expected Plan in result")
		}
		if _, ok := result["Implement"]; !ok {
			t.Error("expected Implement in result when includeCurrent=true")
		}
		if _, ok := result["Done"]; ok {
			t.Error("Done (CleanupWorktree) should never be included")
		}
	})

	t.Run("first stage has no prior comments", func(t *testing.T) {
		result := eng.collectStageComments(item, allStages[0], false)
		if len(result) != 0 {
			t.Errorf("expected empty map for first stage, got %v", result)
		}
	})

	t.Run("skips stages with no matching comment", func(t *testing.T) {
		// Item with only a Research comment
		itemPartial := gh.ProjectItem{
			Number:   99,
			Comments: []gh.Comment{{ID: "c1", Body: researchComment}},
		}
		result := eng.collectStageComments(itemPartial, implementStage, false)
		if len(result) != 1 {
			t.Errorf("expected 1 entry (Research only), got %d", len(result))
		}
		if _, ok := result["Research"]; !ok {
			t.Error("expected Research in result")
		}
	})
}

func TestProcessComments_UpdatesStageComment(t *testing.T) {
	skipIfNoGit(t)
	userCommentID := 444
	existingCommentID := 555
	researchCommentBody := "🏭 **Fabrik — stage: Research**\n*branch: main | commit: abc | 2026-01-01*\n\nOriginal research."

	// Set up a fake claude binary that returns valid JSON output
	binDir := t.TempDir()
	fakeClaude := binDir + "/claude"
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"result":"Updated research findings with more detail about X.","session_id":"sess_cr","num_turns":1,"total_cost_usd":0.001}'
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	item := gh.ProjectItem{
		Number: 10,
		Status: "Research",
		Body:   "Issue body",
		Comments: []gh.Comment{
			{
				ID:         "comment-1",
				DatabaseID: userCommentID,
				Author:     "testuser",
				Body:       "Please add more detail about X",
				CreatedAt:  time.Now(),
			},
			{
				ID:         "stage-comment",
				DatabaseID: existingCommentID,
				Author:     "fabrik",
				Body:       researchCommentBody,
			},
		},
	}

	client := &mockGitHubClient{}

	allStages := []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "research", CommentPrompt: "research comment prompt", Completion: stages.CompletionCriteria{Type: "claude"}},
	}

	repoDir := initBareRepo(t)
	eng := NewWithDeps(
		Config{
			Owner:  "owner",
			Repo:   "repo",
			User:   "testuser",
			Stages: allStages,
			NoTmux: true,
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(repoDir),
	)

	stage := allStages[0]
	newComments := []gh.Comment{item.Comments[0]}

	err := eng.processComments(context.Background(), &gh.ProjectBoard{}, item, stage, newComments)
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// Should have called UpdateComment (not added a new comment for the stage output)
	if len(client.updateCommentCalls) == 0 {
		t.Error("expected UpdateComment to be called to rewrite stage comment")
	} else {
		call := client.updateCommentCalls[0]
		if call.commentDatabaseID != existingCommentID {
			t.Errorf("UpdateComment called with wrong ID: got %d, want %d", call.commentDatabaseID, existingCommentID)
		}
		if !strings.Contains(call.body, "Updated research findings") {
			t.Errorf("UpdateComment body should contain Claude output, got: %q", call.body)
		}
		if !strings.Contains(call.body, "🏭 **Fabrik — stage: Research**") {
			t.Errorf("UpdateComment body should have stage comment header")
		}
	}

	// Should have posted an acknowledgement comment
	ackFound := false
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "🏭 **Fabrik**") && strings.Contains(c.body, "Research") {
			ackFound = true
		}
	}
	if !ackFound {
		t.Error("expected acknowledgement comment to be posted")
	}
}

func TestProcessComments_FallbackAddComment(t *testing.T) {
	skipIfNoGit(t)
	// When no existing stage comment is found, AddComment should be called

	// Set up a fake claude binary that returns valid JSON output
	binDir := t.TempDir()
	fakeClaude := binDir + "/claude"
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"result":"Research output.","session_id":"sess_fb","num_turns":1,"total_cost_usd":0.001}'
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	item := gh.ProjectItem{
		Number: 11,
		Status: "Research",
		Body:   "Issue body",
		Comments: []gh.Comment{
			{
				ID:         "comment-1",
				DatabaseID: 100,
				Author:     "testuser",
				Body:       "A question",
				CreatedAt:  time.Now(),
			},
			// No existing stage comment
		},
	}

	client := &mockGitHubClient{}

	allStages := []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "research", Completion: stages.CompletionCriteria{Type: "claude"}},
	}

	repoDir := initBareRepo(t)
	eng := NewWithDeps(
		Config{
			Owner:  "owner",
			Repo:   "repo",
			User:   "testuser",
			Stages: allStages,
			NoTmux: true,
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(repoDir),
	)

	stage := allStages[0]
	newComments := []gh.Comment{item.Comments[0]}

	err := eng.processComments(context.Background(), &gh.ProjectBoard{}, item, stage, newComments)
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// UpdateComment should NOT be called (no existing comment to update)
	if len(client.updateCommentCalls) != 0 {
		t.Errorf("expected no UpdateComment calls, got %d", len(client.updateCommentCalls))
	}

	// AddComment should be called at least twice: stage comment + ack
	if len(client.addCommentCalls) < 2 {
		t.Errorf("expected at least 2 AddComment calls (stage comment + ack), got %d", len(client.addCommentCalls))
	}
}
