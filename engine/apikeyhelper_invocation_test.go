package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestAPIKeyHelperDetected_ExemptFromRetryAndLabeledDistinctly is the R13
// end-to-end test: a worktree carrying a repo-resident .claude/settings.json
// that sets apiKeyHelper must fail the invocation without ever calling
// Claude, without counting against max_retries, and without stage:failed or
// fabrik:paused — mirroring TestUsageLimitExit_ExemptFromRetryAndLabeledDistinctly's
// harness and assertions exactly, since apiKeyHelperDetectedError shares
// claudeUsageLimitError's "stage never ran" shape (see item.go).
func TestAPIKeyHelperDetected_ExemptFromRetryAndLabeledDistinctly(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	commitAPIKeyHelperSettings(t, repoDir, `{"apiKeyHelper": "/bin/echo fake-key"}`)

	wm := NewWorktreeManager(repoDir)
	claude := &mockClaudeInvoker{}
	client := &mockGitHubClient{}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 200, Title: "apiKeyHelper test", Status: "Research", ItemID: "PVTI_200"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if len(claude.calls) != 0 {
		t.Errorf("claude.Invoke was called %d time(s), want 0 — apiKeyHelper must short-circuit before invocation", len(claude.calls))
	}

	snap, err := eng.store.Get("owner/repo", 200)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 (apiKeyHelper detection must not count against max_retries)", got)
	}
	if snap.LastAttemptAt("Research").IsZero() {
		t.Error("LastAttemptAt(\"Research\") is zero, want set (cooldown must apply)")
	}

	var sawDetectedLabel, sawFailedLabel, sawPausedLabel bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:api-key-helper-detected":
			sawDetectedLabel = true
		case "stage:Research:failed":
			sawFailedLabel = true
		case "fabrik:paused":
			sawPausedLabel = true
		}
	}
	if !sawDetectedLabel {
		t.Error("expected fabrik:api-key-helper-detected label to be added")
	}
	if sawFailedLabel {
		t.Error("stage:Research:failed must NOT be applied for apiKeyHelper detection")
	}
	if sawPausedLabel {
		t.Error("fabrik:paused must NOT be applied for apiKeyHelper detection")
	}

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly 1 comment posted, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "apiKeyHelper") {
		t.Errorf("comment does not name the condition: %s", body)
	}
	if !strings.Contains(body, "max_retries") {
		t.Errorf("comment does not clarify retry-budget exemption: %s", body)
	}

	// Second invocation while the label is still (per our mocked item.Labels)
	// present and the worktree file still sets apiKeyHelper: the check fires
	// again idempotently (no duplicate comment), and still never invokes Claude.
	itemWithLabel := item
	itemWithLabel.Labels = []string{"fabrik:api-key-helper-detected"}
	if err := eng.processItem(context.Background(), board, itemWithLabel); err != nil {
		t.Fatalf("processItem (second call): %v", err)
	}
	if len(claude.calls) != 0 {
		t.Errorf("claude.Invoke was called after second detection, want 0")
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected still exactly 1 comment posted (no duplicate on repeated detection), got %d", len(client.addCommentCalls))
	}
}

// TestAPIKeyHelperDetected_ClearsOnNextSuccessfulInvocation confirms that
// once a human fixes the worktree's .claude/settings.json (removes
// apiKeyHelper), the next invocation reaches Claude and the
// fabrik:api-key-helper-detected label clears automatically — no manual
// label removal required, mirroring fabrik:claude-limit's self-clearing
// behavior.
func TestAPIKeyHelperDetected_ClearsOnNextSuccessfulInvocation(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	commitAPIKeyHelperSettings(t, repoDir, `{"apiKeyHelper": "/bin/echo fake-key"}`)

	wm := NewWorktreeManager(repoDir)
	claude := &mockClaudeInvoker{}
	client := &mockGitHubClient{}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 201, Title: "apiKeyHelper clears test", Status: "Research", ItemID: "PVTI_201"}

	// First call: detected, never invokes Claude, creates the worktree.
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (detect): %v", err)
	}
	if len(claude.calls) != 0 {
		t.Fatalf("claude.Invoke was called during detection, want 0")
	}

	// Simulate a human fixing the repo-resident settings file directly in the
	// worktree (the worktree already exists on disk from the call above).
	fixAPIKeyHelperSettings(t, wm.WorktreeDir(201))

	// Second call, with the label present (as GitHub state would now show):
	// the check no longer fires, Claude is invoked, and the label clears.
	itemWithLabel := item
	itemWithLabel.Labels = []string{"fabrik:api-key-helper-detected"}
	if err := eng.processItem(context.Background(), board, itemWithLabel); err != nil {
		t.Fatalf("processItem (fixed): %v", err)
	}
	if len(claude.calls) != 1 {
		t.Fatalf("claude.Invoke call count = %d, want 1 after the fix", len(claude.calls))
	}

	var sawRemoval bool
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:api-key-helper-detected" {
			sawRemoval = true
		}
	}
	if !sawRemoval {
		t.Error("expected fabrik:api-key-helper-detected to be removed once the worktree file no longer sets apiKeyHelper")
	}
}

// commitAPIKeyHelperSettings commits a .claude/settings.json file with the
// given body into repoDir on its current branch, so a worktree branch forked
// from it (via git worktree add) checks the file out too — simulating a
// repo-resident setting a managed repo's own commit history carries (R13).
func commitAPIKeyHelperSettings(t *testing.T, repoDir, body string) {
	t.Helper()
	dir := filepath.Join(repoDir, ".claude")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0644); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
	for _, args := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", "add apiKeyHelper settings"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit %v: %s: %v", args, out, err)
		}
	}
}

// fixAPIKeyHelperSettings removes apiKeyHelper from the worktree's own
// .claude/settings.json and commits the fix directly in the worktree,
// simulating a human resolving the R13 condition in place.
func fixAPIKeyHelperSettings(t *testing.T, wtDir string) {
	t.Helper()
	path := filepath.Join(wtDir, ".claude", "settings.json")
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write fixed settings.json: %v", err)
	}
	for _, args := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", "remove apiKeyHelper"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = wtDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit %v: %s: %v", args, out, err)
		}
	}
}
