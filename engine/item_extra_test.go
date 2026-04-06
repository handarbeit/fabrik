package engine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
)

// TestEmitStructural_WithChannel sends a structural event and verifies it's received.
func TestEmitStructural_WithChannel(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	ch := make(chan tui.Event, 4)
	eng.events = ch

	eng.emitStructural(tui.PollStartedEvent{Owner: "owner", Repo: "repo", Project: 1})

	select {
	case ev := <-ch:
		if _, ok := ev.(tui.PollStartedEvent); !ok {
			t.Errorf("expected PollStartedEvent, got %T", ev)
		}
	default:
		t.Error("expected event in channel")
	}
}

// TestItemMayNeedWork_StaleButCooldownExpired verifies that a stale item is
// retried after the cooldown period expires.
func TestItemMayNeedWork_StaleButCooldownExpired(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1 // short cooldown

	ts := time.Now().Add(-time.Minute)
	item := gh.ProjectItem{
		Number:    42,
		Status:    "Research",
		ItemID:    "PVTI_42",
		UpdatedAt: ts,
	}

	// Record the last-seen timestamp so the "unchanged" path triggers
	eng.mu.Lock()
	eng.lastUpdatedAt["owner/repo#42"] = ts
	// Record a processedSet entry from >cooldown ago
	eng.processedSet["owner/repo#42-Research"] = time.Now().Add(-2 * time.Minute)
	eng.mu.Unlock()

	if !eng.itemMayNeedWork(item) {
		t.Error("stale item with expired cooldown should need work")
	}
}

// TestItemMayNeedWork_StaleWithinCooldown verifies that a stale item within
// cooldown is skipped.
func TestItemMayNeedWork_StaleWithinCooldown(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 60 // long cooldown

	ts := time.Now().Add(-time.Minute)
	item := gh.ProjectItem{
		Number:    43,
		Status:    "Research",
		ItemID:    "PVTI_43",
		UpdatedAt: ts,
	}

	eng.mu.Lock()
	eng.lastUpdatedAt["owner/repo#43"] = ts
	eng.processedSet["owner/repo#43-Research"] = time.Now() // just processed
	eng.mu.Unlock()

	if eng.itemMayNeedWork(item) {
		t.Error("stale item within cooldown should not need work")
	}
}

// TestItemMayNeedWork_LockedByOtherUser verifies locked items are skipped.
func TestItemMayNeedWork_LockedByOtherUser(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 44,
		Status: "Research",
		Labels: []string{"fabrik:locked:otheruser"},
	}
	if eng.itemMayNeedWork(item) {
		t.Error("item locked by other user should not need work")
	}
}

// TestBlockOnInput_Success covers both AddLabel calls.
func TestBlockOnInput_Success(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{Number: 5}
	eng.blockOnInput(item, stage)

	// Both fabrik:paused and fabrik:awaiting-input should have been added
	if len(client.addLabelCalls) < 2 {
		t.Errorf("expected 2 AddLabel calls, got %d", len(client.addLabelCalls))
	}
}

// TestBlockOnInput_LabelErrors_LogsWarning covers the warning log branches.
func TestBlockOnInput_LabelErrors_LogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("label error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{Number: 6}
	// Should not panic when labels fail
	eng.blockOnInput(item, stage)
}

// TestCommitWIP_ExcludesContextFiles verifies that commitWIP does not include
// files under .fabrik-context/ in the WIP commit, even when they were previously
// committed (making them tracked by git).
func TestCommitWIP_ExcludesContextFiles(t *testing.T) {
	skipIfNoGit(t)

	// Set up a minimal git repo.
	workDir := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", args, out, err)
		}
	}

	// Create a regular file and a context file — both with changes.
	regularFile := filepath.Join(workDir, "app.go")
	if err := os.WriteFile(regularFile, []byte("package main\n"), 0644); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	contextDir := filepath.Join(workDir, ".fabrik-context")
	if err := os.MkdirAll(contextDir, 0755); err != nil {
		t.Fatalf("mkdir context dir: %v", err)
	}
	contextFile := filepath.Join(contextDir, "issue.md")
	if err := os.WriteFile(contextFile, []byte("# Issue\n"), 0644); err != nil {
		t.Fatalf("write context file: %v", err)
	}

	// Commit both files so the context file is tracked.
	for _, args := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", "seed both files"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("seed commit %v: %s: %v", args, out, err)
		}
	}

	// Now modify both files so they appear as uncommitted changes.
	if err := os.WriteFile(regularFile, []byte("package main\n\n// changed\n"), 0644); err != nil {
		t.Fatalf("modify regular file: %v", err)
	}
	if err := os.WriteFile(contextFile, []byte("# Issue\n\nupdated context\n"), 0644); err != nil {
		t.Fatalf("modify context file: %v", err)
	}

	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.commitWIP(workDir, 42, "Research")

	// Verify the WIP commit was created.
	logCmd := exec.Command("git", "log", "--oneline", "-1")
	logCmd.Dir = workDir
	logOut, err := logCmd.Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(logOut), "WIP") {
		t.Errorf("expected WIP commit, got: %s", string(logOut))
	}

	// Verify the context file is NOT in the WIP commit.
	showCmd := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	showCmd.Dir = workDir
	showOut, err := showCmd.Output()
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	filesInCommit := string(showOut)
	if strings.Contains(filesInCommit, ".fabrik-context") {
		t.Errorf(".fabrik-context files should not appear in WIP commit, got:\n%s", filesInCommit)
	}
	if !strings.Contains(filesInCommit, "app.go") {
		t.Errorf("app.go should appear in WIP commit, got:\n%s", filesInCommit)
	}

	// Verify the context file change is preserved on disk (not lost).
	data, err := os.ReadFile(contextFile)
	if err != nil {
		t.Fatalf("read context file after commitWIP: %v", err)
	}
	if !strings.Contains(string(data), "updated context") {
		t.Errorf("context file content should be preserved on disk")
	}
}
