package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStageYAML writes a minimal stage YAML to dir and returns the dir.
func writeStageYAML(t *testing.T, dir, name, model string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "name: " + name + "\norder: 1\nskill: dummy-skill\nmodel: " + model + "\nmax_turns: 10\n"
	if err := os.WriteFile(filepath.Join(dir, strings.ToLower(name)+".yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// withExecCapture replaces execFn for the duration of the test, capturing the
// arguments that would be passed to claude. Returns a pointer to the captured slice.
func withExecCapture(t *testing.T) *[]string {
	t.Helper()
	var captured []string
	orig := execFn
	execFn = func(argv0 string, argv []string, envv []string) error {
		captured = append([]string{argv0}, argv[1:]...)
		return nil
	}
	t.Cleanup(func() { execFn = orig })
	return &captured
}

func TestRunResume_MissingWorktree(t *testing.T) {
	tmp := t.TempDir()
	stagesDir := filepath.Join(tmp, "stages")
	writeStageYAML(t, stagesDir, "Research", "sonnet")

	// cwd has no .fabrik/worktrees/issue-42
	orig, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	err := runResume([]string{"42", "--stage", "Research", "--stages", stagesDir})
	if err == nil {
		t.Fatal("expected error for missing worktree")
	}
	if !strings.Contains(err.Error(), "worktree") {
		t.Errorf("error should mention worktree, got: %v", err)
	}
}

func TestRunResume_MissingStageFlag(t *testing.T) {
	tmp := t.TempDir()
	stagesDir := filepath.Join(tmp, "stages")
	writeStageYAML(t, stagesDir, "Research", "sonnet")

	// Create worktree dir so that check passes
	worktreeDir := filepath.Join(tmp, ".fabrik", "worktrees", "issue-5")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	err := runResume([]string{"5", "--stages", stagesDir})
	if err == nil {
		t.Fatal("expected error for missing --stage flag")
	}
	if !strings.Contains(err.Error(), "--stage") {
		t.Errorf("error should mention --stage, got: %v", err)
	}
}

func TestRunResume_StageNotFound(t *testing.T) {
	tmp := t.TempDir()
	stagesDir := filepath.Join(tmp, "stages")
	writeStageYAML(t, stagesDir, "Research", "sonnet")

	worktreeDir := filepath.Join(tmp, ".fabrik", "worktrees", "issue-3")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	err := runResume([]string{"3", "--stage", "Nonexistent", "--stages", stagesDir})
	if err == nil {
		t.Fatal("expected error for unknown stage")
	}
	if !strings.Contains(err.Error(), "Nonexistent") {
		t.Errorf("error should name the missing stage, got: %v", err)
	}
}

func TestRunResume_ClaudeNotInPath(t *testing.T) {
	tmp := t.TempDir()
	stagesDir := filepath.Join(tmp, "stages")
	writeStageYAML(t, stagesDir, "Research", "sonnet")

	worktreeDir := filepath.Join(tmp, ".fabrik", "worktrees", "issue-10")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	// Hide claude from PATH
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", tmp) // tmp has no claude binary
	t.Cleanup(func() { os.Setenv("PATH", origPath) })

	err := runResume([]string{"10", "--stage", "Research", "--stages", stagesDir})
	if err == nil {
		t.Fatal("expected error when claude not in PATH")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should mention claude, got: %v", err)
	}
}

func TestRunResume_SuccessfulExecConstruction(t *testing.T) {
	tmp := t.TempDir()
	stagesDir := filepath.Join(tmp, "stages")
	writeStageYAML(t, stagesDir, "Plan", "opus")

	worktreeDir := filepath.Join(tmp, ".fabrik", "worktrees", "issue-99")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a fake claude binary in tmp
	fakeClaude := filepath.Join(tmp, "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", tmp)
	t.Cleanup(func() { os.Setenv("PATH", origPath) })

	captured := withExecCapture(t)

	err := runResume([]string{"99", "--stage", "Plan", "--stages", stagesDir, "--plugin-dir", "/some/plugin"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// argv[0] is the claude binary path
	if !strings.HasSuffix((*captured)[0], "claude") {
		t.Errorf("expected claude binary as argv[0], got %q", (*captured)[0])
	}
	// --model opus from stage config
	if !containsSeq(*captured, "--model", "opus") {
		t.Errorf("expected --model opus in args, got %v", *captured)
	}
	// --plugin-dir passed through
	if !containsSeq(*captured, "--plugin-dir", "/some/plugin") {
		t.Errorf("expected --plugin-dir in args, got %v", *captured)
	}
	// no --resume (no session file)
	for _, a := range *captured {
		if a == "--resume" {
			t.Errorf("expected no --resume when no session file, got %v", *captured)
		}
	}
}

func TestRunResume_SuccessfulExecWithSession(t *testing.T) {
	tmp := t.TempDir()
	stagesDir := filepath.Join(tmp, "stages")
	writeStageYAML(t, stagesDir, "Implement", "sonnet")

	worktreeDir := filepath.Join(tmp, ".fabrik", "worktrees", "issue-55")
	if err := os.MkdirAll(worktreeDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write a session file under the CWD-relative path (os.Chdir(tmp) is called below,
	// so os.Getwd() == tmp when ReadSessionID runs).
	sessDir := filepath.Join(tmp, ".fabrik", "sessions", "issue-55")
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "Implement.session"), []byte("sess-abc-xyz\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Fake claude binary
	fakeClaude := filepath.Join(tmp, "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", tmp)
	t.Cleanup(func() { os.Setenv("PATH", origPath) })

	captured := withExecCapture(t)

	err := runResume([]string{"55", "--stage", "Implement", "--stages", stagesDir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !containsSeq(*captured, "--resume", "sess-abc-xyz") {
		t.Errorf("expected --resume sess-abc-xyz in args, got %v", *captured)
	}
}

func TestRunResume_BadIssueNumber(t *testing.T) {
	err := runResume([]string{"notanumber", "--stage", "Research"})
	if err == nil {
		t.Fatal("expected error for non-integer issue number")
	}
	if !strings.Contains(err.Error(), "positive integer") {
		t.Errorf("expected helpful message, got: %v", err)
	}
}

func TestRunResume_NoArgs(t *testing.T) {
	err := runResume([]string{})
	if err == nil {
		t.Fatal("expected error for no arguments")
	}
	if !strings.Contains(err.Error(), "<issue-number>") {
		t.Errorf("expected usage hint, got: %v", err)
	}
}

// containsSeq reports whether slice contains the consecutive subsequence [a, b].
func containsSeq(slice []string, a, b string) bool {
	for i := 0; i+1 < len(slice); i++ {
		if slice[i] == a && slice[i+1] == b {
			return true
		}
	}
	return false
}
