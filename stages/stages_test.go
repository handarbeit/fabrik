package stages

import (
	"os"
	"path/filepath"
	"testing"
)

func writeStageFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAll_Success(t *testing.T) {
	dir := t.TempDir()
	writeStageFile(t, dir, "research.yaml", `
name: Research
order: 1
prompt: "Do research"
completion:
  type: claude
`)
	writeStageFile(t, dir, "plan.yml", `
name: Plan
order: 2
prompt: "Make a plan"
`)

	stages, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(stages))
	}
	if stages[0].Name != "Research" {
		t.Errorf("first stage name = %q, want Research", stages[0].Name)
	}
	if stages[1].Name != "Plan" {
		t.Errorf("second stage name = %q, want Plan", stages[1].Name)
	}
	if stages[0].Order != 1 || stages[1].Order != 2 {
		t.Errorf("stages not sorted by order")
	}
}

func TestLoadAll_SortsByOrder(t *testing.T) {
	dir := t.TempDir()
	writeStageFile(t, dir, "b.yaml", `
name: Second
order: 10
prompt: "prompt"
`)
	writeStageFile(t, dir, "a.yaml", `
name: First
order: 1
prompt: "prompt"
`)

	stages, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if stages[0].Name != "First" || stages[1].Name != "Second" {
		t.Errorf("expected stages sorted by order, got %q then %q", stages[0].Name, stages[1].Name)
	}
}

func TestLoadAll_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	stages, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(stages) != 0 {
		t.Errorf("expected 0 stages from empty dir, got %d", len(stages))
	}
}

func TestLoadAll_NonExistentDir(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nonexistent-subdir")
	_, err := LoadAll(missing)
	if err == nil {
		t.Fatal("expected error for nonexistent dir")
	}
}

func TestLoadAll_SkipsNonYAML(t *testing.T) {
	dir := t.TempDir()
	writeStageFile(t, dir, "readme.txt", "not yaml")
	writeStageFile(t, dir, "stage.yaml", `
name: Valid
order: 1
prompt: "prompt"
`)

	stages, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(stages) != 1 {
		t.Errorf("expected 1 stage, got %d", len(stages))
	}
}

func TestLoadAll_MalformedYAML(t *testing.T) {
	dir := t.TempDir()
	writeStageFile(t, dir, "bad.yaml", `{{{not valid yaml`)

	_, err := LoadAll(dir)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}

func TestLoadAll_MissingName(t *testing.T) {
	dir := t.TempDir()
	writeStageFile(t, dir, "noname.yaml", `
order: 1
prompt: "prompt"
`)

	_, err := LoadAll(dir)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestLoadAll_MissingPrompt(t *testing.T) {
	dir := t.TempDir()
	writeStageFile(t, dir, "noprompt.yaml", `
name: Test
order: 1
`)

	_, err := LoadAll(dir)
	if err == nil {
		t.Fatal("expected error for missing prompt")
	}
}

func TestLoadAll_DefaultsCompletionTypeToClaude(t *testing.T) {
	dir := t.TempDir()
	writeStageFile(t, dir, "stage.yaml", `
name: Test
order: 1
prompt: "prompt"
`)

	stages, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if stages[0].Completion.Type != "claude" {
		t.Errorf("default completion type = %q, want claude", stages[0].Completion.Type)
	}
}

func TestLoadAll_AllFields(t *testing.T) {
	dir := t.TempDir()
	writeStageFile(t, dir, "full.yaml", `
name: Implement
order: 3
prompt: "Implement the thing"
model: opus
allowed_tools:
  - Read
  - Write
max_turns: 50
completion:
  type: claude
  value: "done"
auto_advance: true
`)

	stages, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	s := stages[0]
	if s.Name != "Implement" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.Model != "opus" {
		t.Errorf("Model = %q", s.Model)
	}
	if len(s.AllowedTools) != 2 || s.AllowedTools[0] != "Read" {
		t.Errorf("AllowedTools = %v", s.AllowedTools)
	}
	if s.MaxTurns != 50 {
		t.Errorf("MaxTurns = %d", s.MaxTurns)
	}
	if s.Completion.Type != "claude" || s.Completion.Value != "done" {
		t.Errorf("Completion = %+v", s.Completion)
	}
	if s.AutoAdvance == nil || !*s.AutoAdvance {
		t.Errorf("AutoAdvance = %v", s.AutoAdvance)
	}
}

func TestLoadAll_UnsupportedCompletionType(t *testing.T) {
	for _, typ := range []string{"tasklist", "label", "approval", "unknown"} {
		dir := t.TempDir()
		writeStageFile(t, dir, "stage.yaml", `
name: Test
prompt: "do stuff"
completion:
  type: `+typ+`
`)
		_, err := LoadAll(dir)
		if err == nil {
			t.Errorf("type %q: expected error for unsupported completion type, got nil", typ)
		}
	}
}

func TestFindStage(t *testing.T) {
	stages := []*Stage{
		{Name: "A"},
		{Name: "B"},
		{Name: "C"},
	}

	if s := FindStage(stages, "B"); s == nil || s.Name != "B" {
		t.Errorf("FindStage(B) = %v", s)
	}
	if s := FindStage(stages, "X"); s != nil {
		t.Errorf("FindStage(X) = %v, want nil", s)
	}
	if s := FindStage(nil, "A"); s != nil {
		t.Errorf("FindStage(nil, A) = %v, want nil", s)
	}
}

func TestLoadAll_ReadError(t *testing.T) {
	dir := t.TempDir()
	// Create a directory named bad.yaml so os.ReadFile fails deterministically
	// (works on all platforms, unlike chmod 0000 which fails as root or on Windows)
	path := filepath.Join(dir, "bad.yaml")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatalf("failed to create directory %q: %v", path, err)
	}

	_, err := LoadAll(dir)
	if err == nil {
		t.Fatal("expected error for unreadable file")
	}
}

func TestLoadAll_CleanupWorktreeStage(t *testing.T) {
	dir := t.TempDir()
	writeStageFile(t, dir, "done.yaml", `
name: Done
order: 99
cleanup_worktree: true
`)

	stages, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stages))
	}
	s := stages[0]
	if s.Name != "Done" {
		t.Errorf("Name = %q, want Done", s.Name)
	}
	if !s.CleanupWorktree {
		t.Error("CleanupWorktree should be true")
	}
	if s.Prompt != "" {
		t.Errorf("Prompt should be empty, got %q", s.Prompt)
	}
	// Completion.Type should not be set (no validation for cleanup stages)
	if s.Completion.Type != "" {
		t.Errorf("Completion.Type = %q, want empty", s.Completion.Type)
	}
}

func TestNextStage(t *testing.T) {
	stages := []*Stage{
		{Name: "A"},
		{Name: "B"},
		{Name: "C"},
	}

	if s := NextStage(stages, "A"); s == nil || s.Name != "B" {
		t.Errorf("NextStage(A) = %v, want B", s)
	}
	if s := NextStage(stages, "B"); s == nil || s.Name != "C" {
		t.Errorf("NextStage(B) = %v, want C", s)
	}
	// Last stage returns nil
	if s := NextStage(stages, "C"); s != nil {
		t.Errorf("NextStage(C) = %v, want nil", s)
	}
	// Unknown stage returns nil
	if s := NextStage(stages, "X"); s != nil {
		t.Errorf("NextStage(X) = %v, want nil", s)
	}
}
