package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/warnings"
)

func isolateWarningsFile(t *testing.T) {
	t.Helper()
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
}

func skillStages(skill, commentSkill string) []*stages.Stage {
	return []*stages.Stage{{
		Name:         "Research",
		Order:        1,
		Skill:        skill,
		CommentSkill: commentSkill,
		Completion:   stages.CompletionCriteria{Type: "claude"},
	}}
}

func labelsAdded(c *mockGitHubClient) map[string]bool {
	out := map[string]bool{}
	for _, l := range c.addLabelCalls {
		out[l.labelName] = true
	}
	return out
}

// A stage whose skill reference resolves is dispatched with the fully
// qualified name in the stage the invoker receives (SC-001), and the bare
// original is left untouched on the engine's configured stage.
func TestSkillDispatch_ResolvedStageCarriesQualifiedName(t *testing.T) {
	skipIfNoGit(t)
	isolateWarningsFile(t)
	repoDir := initBareRepo(t)
	pdir := skillPlugin(t, t.TempDir(), "base", plugin.BasePluginName, "fabrik-research")

	var seen string
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, _ gh.ProjectItem, _ []gh.Comment, _ bool, _ string, _ InvokeOptions) (string, bool, TokenUsage, error) {
			seen = stage.Skill
			return "ok", false, TokenUsage{}, nil
		},
	}
	client := &mockGitHubClient{}
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "u", Token: "t", MaxRetries: 2,
		Stages: skillStages("fabrik-research", "")}, client, claude, NewWorktreeManager(repoDir))
	eng.skillResolver = newSkillResolver([]string{pdir})

	item := gh.ProjectItem{Number: 300, Title: "t", Status: "Research", ItemID: "PVTI_300"}
	if err := eng.processItem(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item); err != nil {
		t.Fatal(err)
	}
	if want := plugin.Qualify(plugin.BasePluginName, "fabrik-research"); seen != want {
		t.Errorf("invoker saw Skill %q, want %q", seen, want)
	}
	if eng.cfg.Stages[0].Skill != "fabrik-research" {
		t.Errorf("configured stage mutated: %q", eng.cfg.Stages[0].Skill)
	}
}

// SC-002: a stage pointing at a genuinely separate plugin runs that plugin's skill.
func TestSkillDispatch_SeparatePluginSkill(t *testing.T) {
	skipIfNoGit(t)
	isolateWarningsFile(t)
	repoDir := initBareRepo(t)
	root := t.TempDir()
	base := skillPlugin(t, root, "base", plugin.BasePluginName, "fabrik-research")
	org := skillPlugin(t, root, "org", "myorg", "review")

	var seen string
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, _ gh.ProjectItem, _ []gh.Comment, _ bool, _ string, _ InvokeOptions) (string, bool, TokenUsage, error) {
			seen = stage.Skill
			return "ok", false, TokenUsage{}, nil
		},
	}
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "u", Token: "t", MaxRetries: 2,
		Stages: skillStages("myorg:review", "")}, &mockGitHubClient{}, claude, NewWorktreeManager(repoDir))
	eng.skillResolver = newSkillResolver([]string{base, org})

	item := gh.ProjectItem{Number: 301, Title: "t", Status: "Research", ItemID: "PVTI_301"}
	if err := eng.processItem(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item); err != nil {
		t.Fatal(err)
	}
	if seen != "myorg:review" {
		t.Errorf("invoker saw Skill %q, want myorg:review", seen)
	}
}

// SC-004 / FR-005: an unresolved reference never reaches a worker, consumes no
// retry, applies neither failed nor paused, comments once and reaches the panel.
func TestSkillDispatch_UnresolvedNeverSpawnsWorker(t *testing.T) {
	skipIfNoGit(t)
	isolateWarningsFile(t)
	repoDir := initBareRepo(t)
	pdir := skillPlugin(t, t.TempDir(), "base", plugin.BasePluginName, "fabrik-research")

	claude := &mockClaudeInvoker{}
	client := &mockGitHubClient{}
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "u", Token: "t", MaxRetries: 2,
		Stages: skillStages("myorg:missing", "")}, client, claude, NewWorktreeManager(repoDir))
	eng.skillResolver = newSkillResolver([]string{pdir})

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 302, Title: "t", Status: "Research", ItemID: "PVTI_302"}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatal(err)
	}
	if len(claude.calls) != 0 {
		t.Fatalf("worker invoked %d time(s), want 0", len(claude.calls))
	}
	snap, err := eng.store.Get("owner/repo", 302)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts = %d, want 0", got)
	}
	if snap.LastAttemptAt("Research").IsZero() {
		t.Error("LastAttemptAt zero, want the dispatch cooldown to apply")
	}
	added := labelsAdded(client)
	if !added[skillUnresolvedLabel] {
		t.Errorf("want %s added, got %v", skillUnresolvedLabel, added)
	}
	if added["stage:Research:failed"] || added["fabrik:paused"] {
		t.Errorf("must not apply failed/paused: %v", added)
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("comments = %d, want 1", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	for _, want := range []string{"myorg:missing", plugin.BasePluginName, pdir, "max_retries"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q: %s", want, body)
		}
	}
	entries, err := warnings.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != skillUnresolvedType+":Research.skill" {
		t.Errorf("panel entries = %+v", entries)
	}

	// Second dispatch with the label present: still no worker, no second comment.
	again := item
	again.Labels = []string{skillUnresolvedLabel}
	if err := eng.processItem(context.Background(), board, again); err != nil {
		t.Fatal(err)
	}
	if len(claude.calls) != 0 || len(client.addCommentCalls) != 1 {
		t.Errorf("repeat: worker calls %d, comments %d; want 0 and 1", len(claude.calls), len(client.addCommentCalls))
	}
}

// Fixing the reference clears the label and the panel entry on the next run.
func TestSkillDispatch_RecoveryClearsLabelAndPanel(t *testing.T) {
	skipIfNoGit(t)
	isolateWarningsFile(t)
	repoDir := initBareRepo(t)
	pdir := skillPlugin(t, t.TempDir(), "base", plugin.BasePluginName)

	claude := &mockClaudeInvoker{}
	client := &mockGitHubClient{}
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "u", Token: "t", MaxRetries: 2,
		Stages: skillStages("fabrik-research", "")}, client, claude, NewWorktreeManager(repoDir))
	eng.skillResolver = newSkillResolver([]string{pdir})

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 303, Title: "t", Status: "Research", ItemID: "PVTI_303"}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatal(err)
	}
	if len(claude.calls) != 0 {
		t.Fatal("worker invoked before the skill existed")
	}

	// The plugin gains the skill on disk; the resolver rebuilds on the miss.
	writeSkill(t, filepath.Join(pdir, "skills", "fabrik-research", "SKILL.md"))
	recovered := item
	recovered.Labels = []string{skillUnresolvedLabel}
	if err := eng.processItem(context.Background(), board, recovered); err != nil {
		t.Fatal(err)
	}
	if len(claude.calls) != 1 {
		t.Fatalf("worker calls = %d, want 1 after recovery", len(claude.calls))
	}
	removed := false
	for _, r := range client.removeLabelCalls {
		if r.labelName == skillUnresolvedLabel {
			removed = true
		}
	}
	if !removed {
		t.Error("label not removed after recovery")
	}
	if entries, _ := warnings.Load(); len(entries) != 0 {
		t.Errorf("panel entries after recovery = %+v", entries)
	}
}

// Comment review, CI-fix and rebase all reach InvokeForComments through
// processCommentsClassified; an unresolved comment_skill is refused before any
// worker, classified did-not-run so the reinvoke cycle is refunded, and no
// error is returned (so no breaker is fed).
func TestSkillDispatch_CommentPathUnresolved(t *testing.T) {
	skipIfNoGit(t)
	isolateWarningsFile(t)
	pdir := skillPlugin(t, t.TempDir(), "base", plugin.BasePluginName)
	claude := &mockClaudeInvoker{}
	client := &mockGitHubClient{}
	eng := testEngineWithRepo(t, client, claude)
	eng.skillResolver = newSkillResolver([]string{pdir})

	stage := &stages.Stage{Name: "Review", Order: 1, CommentSkill: "myorg:nope", Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 304, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "human", Body: "please fix"}}

	kind, err := eng.processCommentsClassified(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, comments)
	if err != nil || kind != didNotRunSkillUnresolved {
		t.Fatalf("kind=%q err=%v, want %q and nil", kind, err, didNotRunSkillUnresolved)
	}
	if len(claude.forCommentsCalls) != 0 {
		t.Errorf("worker invoked for comments: %d", len(claude.forCommentsCalls))
	}
	if !labelsAdded(client)[skillUnresolvedLabel] || len(client.addCommentCalls) != 1 {
		t.Errorf("label/comment: %v / %d comments", labelsAdded(client), len(client.addCommentCalls))
	}
	if c := classifyDidNotRun(&skillUnresolvedError{}); c != didNotRunSkillUnresolved {
		t.Errorf("classifyDidNotRun = %q", c)
	}
}

// ci_fix_skill and rebase_skill reach the worker through stageVariant's swap
// into CommentSkill, so the same gate qualifies them.
func TestSkillDispatch_CommentPathQualifiesVariantSkill(t *testing.T) {
	skipIfNoGit(t)
	isolateWarningsFile(t)
	pdir := skillPlugin(t, t.TempDir(), "base", plugin.BasePluginName, "fix-ci")
	var seen string
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, _ gh.ProjectItem, _ []gh.Comment, _ string, _ InvokeOptions) (string, bool, TokenUsage, error) {
			seen = stage.CommentSkill
			return "ok", false, TokenUsage{}, nil
		},
	}
	eng := testEngineWithRepo(t, &mockGitHubClient{}, claude)
	eng.skillResolver = newSkillResolver([]string{pdir})

	base := &stages.Stage{Name: "Review", Order: 1, CIFixSkill: "fix-ci", Completion: stages.CompletionCriteria{Type: "claude"}}
	variant := *base
	variant.CommentSkill = base.CIFixSkill
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "human", Body: "ci red"}}
	if _, err := eng.processCommentsClassified(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, gh.ProjectItem{Number: 305, Body: "spec"}, &variant, comments); err != nil {
		t.Fatal(err)
	}
	if want := plugin.Qualify(plugin.BasePluginName, "fix-ci"); seen != want {
		t.Errorf("invoker saw CommentSkill %q, want %q", seen, want)
	}
}
