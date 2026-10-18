package engine

import (
	"context"
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// R1 (#2393): a failed lock write spawns no worker, attempts no in_progress
// label, charges nothing and records a short admission cooldown.
func TestProcessItem_LockWriteFailure_SpawnsNoWorker(t *testing.T) {
	skipIfNoGit(t)
	wm := NewWorktreeManager(initBareRepo(t))
	claude := &mockClaudeInvoker{}
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, n int, label string) error {
			return errors.New("executing request: context deadline exceeded")
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, PollSeconds: 30, Stages: testStages()},
		client, claude, wm,
	)
	eng.SetPauseGuardUnreadFailOpenForTest(true)
	item := gh.ProjectItem{Number: 301, Title: "lock test", Status: "Research", ItemID: "PVTI_301"}

	if err := eng.processItem(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Fatalf("claude.Invoke called %d times, want 0", len(claude.calls))
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Research:in_progress" || c.labelName == "stage:Research:failed" {
			t.Errorf("unexpected label write %q after a failed lock write", c.labelName)
		}
	}
	snap, err := eng.store.Get("owner/repo", 301)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts = %d, want 0", got)
	}
	if !snap.LastAttemptAt("Research").IsZero() {
		t.Error("LastAttemptAt set; a refused dispatch must not record StageAttempted")
	}
	if snap.CooldownAt(lockWriteRetryReason).IsZero() {
		t.Error("no lock-write-failed admission cooldown recorded")
	}
	if snap.Worker() != nil {
		t.Error("worker handle left behind")
	}
}
