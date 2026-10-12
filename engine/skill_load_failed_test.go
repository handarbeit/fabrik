package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/warnings"
)

func skillLoadErrFor(skill string) *claudeSkillLoadError {
	return &claudeSkillLoadError{Skill: skill, Called: "bare", ToolUseID: "t1", Detail: "Unknown skill: " + skill}
}

// A skill-load failure on the stage-dispatch path pauses the item for a human:
// one comment naming the skill, plugins and error, fabrik:paused +
// fabrik:awaiting-input, a panel entry — and no retry consumed, no failed
// label, no completion (FR-001, FR-003, FR-004).
func TestSkillLoadFailed_StageDispatchPauses(t *testing.T) {
	skipIfNoGit(t)
	isolateWarningsFile(t)
	repoDir := initBareRepo(t)

	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, _ gh.ProjectItem, _ []gh.Comment, _ bool, _ string, _ InvokeOptions) (string, bool, TokenUsage, error) {
			return "FABRIK_STAGE_COMPLETE", false, TokenUsage{}, skillLoadErrFor("fabrik-stages:research")
		},
	}
	client := &mockGitHubClient{}
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "u", Token: "t", MaxRetries: 2,
		Stages: skillStages("fabrik-stages:research", "")}, client, claude, NewWorktreeManager(repoDir))

	item := gh.ProjectItem{Number: 400, Title: "t", Status: "Research", ItemID: "PVTI_400"}
	if err := eng.processItem(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item); err != nil {
		t.Fatal(err)
	}

	added := labelsAdded(client)
	if !added["fabrik:paused"] || !added["fabrik:awaiting-input"] {
		t.Errorf("want paused + awaiting-input, got %v", added)
	}
	if added["stage:Research:failed"] || added["stage:Research:complete"] {
		t.Errorf("must neither fail nor complete the stage: %v", added)
	}
	snap, err := eng.store.Get("owner/repo", 400)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts = %d, want 0 (does not consume max_retries)", got)
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("comments = %d, want 1", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.HasPrefix(body, "🏭 **Fabrik") {
		t.Errorf("comment must carry the Fabrik prefix (not steering): %q", body)
	}
	for _, want := range []string{"fabrik-stages:research", "bare", "Plugins loaded", "Unknown skill", "max_retries", "fabrik:paused"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	entries, err := warnings.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != skillLoadType+":Research.skill" {
		t.Errorf("panel entries = %+v", entries)
	}
}

// A later healthy run of the same stage clears the panel entry.
func TestSkillLoadFailed_HealthyRunClearsPanelEntry(t *testing.T) {
	skipIfNoGit(t)
	isolateWarningsFile(t)
	repoDir := initBareRepo(t)

	fail := true
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, _ gh.ProjectItem, _ []gh.Comment, _ bool, _ string, _ InvokeOptions) (string, bool, TokenUsage, error) {
			if fail {
				return "", false, TokenUsage{}, skillLoadErrFor("fabrik-stages:research")
			}
			return "ok", false, TokenUsage{}, nil
		},
	}
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "u", Token: "t", MaxRetries: 2,
		Stages: skillStages("fabrik-stages:research", "")}, &mockGitHubClient{}, claude, NewWorktreeManager(repoDir))

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 401, Title: "t", Status: "Research", ItemID: "PVTI_401"}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatal(err)
	}
	if entries, _ := warnings.Load(); len(entries) != 1 {
		t.Fatalf("panel entries after failure = %+v, want 1", entries)
	}

	fail = false
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatal(err)
	}
	if entries, _ := warnings.Load(); len(entries) != 0 {
		t.Errorf("panel entries after a healthy run = %+v, want none", entries)
	}
}

// The reinvoke path refunds the pre-charged cycle: the run did none of the work.
func TestClassifyDidNotRun_SkillLoadFailed(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", skillLoadErrFor("p:s"))
	if got := classifyDidNotRun(err); got != didNotRunSkillLoadFailed {
		t.Errorf("classifyDidNotRun = %q, want %q", got, didNotRunSkillLoadFailed)
	}
	if got := classifyDidNotRun(errors.New("other")); got != "" {
		t.Errorf("unrelated error classified as %q", got)
	}
}
