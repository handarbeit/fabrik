package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

func TestInjectClaudeArtifacts_CopiesSubdirs(t *testing.T) {
	mainRepo := t.TempDir()
	worktree := t.TempDir()

	// Create .claude/skills, .claude/agents, .claude/rules in main repo
	for _, subdir := range []string{"skills/my-skill", "agents", "rules"} {
		dir := filepath.Join(mainRepo, ".claude", subdir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	// Write a skill file
	skillFile := filepath.Join(mainRepo, ".claude", "skills", "my-skill", "SKILL.md")
	if err := os.WriteFile(skillFile, []byte("# My Skill\nDo stuff."), 0644); err != nil {
		t.Fatalf("writing skill file: %v", err)
	}
	// Write an agent file
	agentFile := filepath.Join(mainRepo, ".claude", "agents", "my-agent.md")
	if err := os.WriteFile(agentFile, []byte("# My Agent"), 0644); err != nil {
		t.Fatalf("writing agent file: %v", err)
	}

	stage := &stages.Stage{Name: "Test", Prompt: "test"}
	if err := injectClaudeArtifacts(worktree, mainRepo, stage); err != nil {
		t.Fatalf("injectClaudeArtifacts: %v", err)
	}

	// Verify skill was copied
	dstSkill := filepath.Join(worktree, ".claude", "skills", "my-skill", "SKILL.md")
	data, err := os.ReadFile(dstSkill)
	if err != nil {
		t.Fatalf("reading copied skill: %v", err)
	}
	if !strings.Contains(string(data), "My Skill") {
		t.Errorf("copied skill content = %q, expected 'My Skill'", string(data))
	}

	// Verify agent was copied
	dstAgent := filepath.Join(worktree, ".claude", "agents", "my-agent.md")
	if _, err := os.Stat(dstAgent); err != nil {
		t.Errorf("agent not copied: %v", err)
	}

	// Verify settings.json was generated
	settingsPath := filepath.Join(worktree, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); err != nil {
		t.Errorf("settings.json not generated: %v", err)
	}
}

func TestInjectClaudeArtifacts_MissingSourceSubdir(t *testing.T) {
	mainRepo := t.TempDir()
	worktree := t.TempDir()

	// Only create skills, not agents or rules
	skillDir := filepath.Join(mainRepo, ".claude", "skills")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("creating skill dir: %v", err)
	}

	stage := &stages.Stage{Name: "Test", Prompt: "test"}
	// Should not fail even though agents/ and rules/ don't exist
	if err := injectClaudeArtifacts(worktree, mainRepo, stage); err != nil {
		t.Fatalf("injectClaudeArtifacts failed unexpectedly: %v", err)
	}

	// agents/ should not exist in worktree
	if _, err := os.Stat(filepath.Join(worktree, ".claude", "agents")); !os.IsNotExist(err) {
		t.Error("agents/ should not exist when not present in main repo")
	}
}

func TestInjectClaudeArtifacts_OverwritesExisting(t *testing.T) {
	mainRepo := t.TempDir()
	worktree := t.TempDir()

	skillDir := filepath.Join(mainRepo, ".claude", "skills", "my-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("creating skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("new content"), 0644); err != nil {
		t.Fatalf("writing skill: %v", err)
	}

	// Pre-create the destination with old content
	dstDir := filepath.Join(worktree, ".claude", "skills", "my-skill")
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		t.Fatalf("creating dst dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "SKILL.md"), []byte("old content"), 0644); err != nil {
		t.Fatalf("writing old skill: %v", err)
	}

	stage := &stages.Stage{Name: "Test", Prompt: "test"}
	if err := injectClaudeArtifacts(worktree, mainRepo, stage); err != nil {
		t.Fatalf("injectClaudeArtifacts: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dstDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("reading skill: %v", err)
	}
	if string(data) != "new content" {
		t.Errorf("skill content = %q, expected 'new content' (overwrite failed)", string(data))
	}
}

func TestGenerateSettingsJSON_Basic(t *testing.T) {
	dir := t.TempDir()
	stage := &stages.Stage{
		Name:   "Research",
		Prompt: "test",
		AllowedTools: []string{
			"Read",
			"Grep",
			"Glob",
		},
	}

	if err := generateSettingsJSON(stage, dir); err != nil {
		t.Fatalf("generateSettingsJSON: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parsing settings.json: %v", err)
	}

	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatal("settings.json missing 'permissions' object")
	}
	allow, ok := perms["allow"].([]any)
	if !ok {
		t.Fatal("settings.json missing 'permissions.allow' array")
	}

	// Check base entries are present
	allowStrs := make([]string, len(allow))
	for i, v := range allow {
		allowStrs[i], _ = v.(string)
	}
	for _, required := range []string{"Bash(git *)", "Bash(gh *)", "Read", "Grep"} {
		found := false
		for _, s := range allowStrs {
			if s == required {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("settings.json allow list missing %q", required)
		}
	}
}

func TestGenerateSettingsJSON_NoAllowedTools(t *testing.T) {
	dir := t.TempDir()
	stage := &stages.Stage{Name: "Implement", Prompt: "test"}

	if err := generateSettingsJSON(stage, dir); err != nil {
		t.Fatalf("generateSettingsJSON: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parsing settings.json: %v", err)
	}
	perms := settings["permissions"].(map[string]any)
	allow := perms["allow"].([]any)
	if len(allow) == 0 {
		t.Error("settings.json should have base allow entries even with no stage tools")
	}
}

func TestWriteContextFile(t *testing.T) {
	workDir := t.TempDir()

	contextPath := filepath.Join(workDir, ".fabrik", "context.md")

	ghIssue := gh.ProjectItem{
		Number: 42,
		Title:  "Test Issue",
		URL:    "https://github.com/org/repo/issues/42",
		Body:   "Issue body text",
		Labels: []string{"bug", "priority"},
	}

	if err := writeContextFile(workDir, ghIssue, nil); err != nil {
		t.Fatalf("writeContextFile: %v", err)
	}

	data, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatalf("reading context file: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "# Issue #42: Test Issue") {
		t.Error("context missing issue header")
	}
	if !strings.Contains(content, "https://github.com/org/repo/issues/42") {
		t.Error("context missing URL")
	}
	if !strings.Contains(content, "Issue body text") {
		t.Error("context missing body")
	}
	if !strings.Contains(content, "bug, priority") {
		t.Error("context missing labels")
	}
}

func TestResolveWorkflowPrompt_NoWorkflow(t *testing.T) {
	stage := &stages.Stage{Name: "Test", Prompt: "raw prompt text"}
	prompt, err := resolveWorkflowPrompt(stage, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prompt != "raw prompt text" {
		t.Errorf("prompt = %q, want 'raw prompt text'", prompt)
	}
}

func TestResolveWorkflowPrompt_SkillFound(t *testing.T) {
	workDir := t.TempDir()
	skillDir := filepath.Join(workDir, ".claude", "skills", "my-workflow")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("creating skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Skill Prompt"), 0644); err != nil {
		t.Fatalf("writing skill: %v", err)
	}

	stage := &stages.Stage{Name: "Test", Prompt: "fallback", Workflow: "my-workflow"}
	prompt, err := resolveWorkflowPrompt(stage, workDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prompt != "# Skill Prompt" {
		t.Errorf("prompt = %q, want '# Skill Prompt'", prompt)
	}
}

func TestResolveWorkflowPrompt_SkillMissing(t *testing.T) {
	workDir := t.TempDir()

	stage := &stages.Stage{Name: "Test", Prompt: "fallback", Workflow: "missing-skill"}
	prompt, err := resolveWorkflowPrompt(stage, workDir)
	// Should return fallback and a non-nil error
	if err == nil {
		t.Error("expected error when skill file is missing")
	}
	if prompt != "fallback" {
		t.Errorf("prompt = %q, want 'fallback' (expected fallback on missing skill)", prompt)
	}
}

func TestResolveCommentWorkflowPrompt_NoWorkflow(t *testing.T) {
	stage := &stages.Stage{Name: "Test", CommentPrompt: "comment prompt"}
	prompt, err := resolveCommentWorkflowPrompt(stage, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prompt != "comment prompt" {
		t.Errorf("prompt = %q, want 'comment prompt'", prompt)
	}
}

func TestResolveCommentWorkflowPrompt_SkillFound(t *testing.T) {
	workDir := t.TempDir()
	skillDir := filepath.Join(workDir, ".claude", "skills", "my-comment-workflow")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("creating skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Comment Skill"), 0644); err != nil {
		t.Fatalf("writing skill: %v", err)
	}

	stage := &stages.Stage{Name: "Test", CommentWorkflow: "my-comment-workflow"}
	prompt, err := resolveCommentWorkflowPrompt(stage, workDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prompt != "# Comment Skill" {
		t.Errorf("prompt = %q, want '# Comment Skill'", prompt)
	}
}
