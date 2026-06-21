package engine

import (
	"os/exec"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestPRInMergeQueue covers the FR-1 per-PR guard signal: it fires on
// LinkedPRIsInMergeQueue alone and is false-by-default (FR-3).
func TestPRInMergeQueue(t *testing.T) {
	tests := []struct {
		name string
		item gh.ProjectItem
		want bool
	}{
		{"default (no flags)", gh.ProjectItem{}, false},
		{"queue enabled but not in queue", gh.ProjectItem{LinkedPRIsMergeQueueEnabled: true}, false},
		{"in queue", gh.ProjectItem{LinkedPRIsInMergeQueue: true}, true},
		{"in queue and enabled", gh.ProjectItem{LinkedPRIsInMergeQueue: true, LinkedPRIsMergeQueueEnabled: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prInMergeQueue(tt.item); got != tt.want {
				t.Errorf("prInMergeQueue() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSuppressPreemptiveRebase covers the FR-2 per-repo guard signal across the
// full decision matrix: queue-disabled / enabled+off / enabled+auto, asserting
// false-by-default (FR-3) and that the "off" kill-switch restores legacy behavior.
func TestSuppressPreemptiveRebase(t *testing.T) {
	tests := []struct {
		name       string
		mergeQueue string
		item       gh.ProjectItem
		want       bool
	}{
		{"queue disabled, cfg auto", "auto", gh.ProjectItem{}, false},
		{"queue disabled, cfg off", "off", gh.ProjectItem{}, false},
		{"queue disabled, cfg empty", "", gh.ProjectItem{}, false},
		{"queue enabled, cfg auto", "auto", gh.ProjectItem{LinkedPRIsMergeQueueEnabled: true}, true},
		{"queue enabled, cfg empty (default != off)", "", gh.ProjectItem{LinkedPRIsMergeQueueEnabled: true}, true},
		{"queue enabled, cfg off (kill-switch)", "off", gh.ProjectItem{LinkedPRIsMergeQueueEnabled: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{cfg: Config{MergeQueue: tt.mergeQueue}}
			if got := e.suppressPreemptiveRebase(tt.item); got != tt.want {
				t.Errorf("suppressPreemptiveRebase() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPushBranchUnlessQueued_SkipsWhenQueued verifies, against a real git repo
// with a bare origin, that pushBranchUnlessQueued does NOT advance the origin
// branch when the PR is in the merge queue, and DOES when it is not.
func TestPushBranchUnlessQueued_SkipsWhenQueued(t *testing.T) {
	skipIfNoGit(t)

	sourceDir := initRepoWithRemote(t)
	wm := NewWorktreeManager(sourceDir)
	e := NewWithDeps(Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()},
		&mockGitHubClient{}, &mockClaudeInvoker{}, wm)

	// Helper: query the origin for the issue branch ref (empty when absent).
	remoteRef := func(wtDir, branch string) string {
		cmd := exec.Command("git", "ls-remote", "origin", branch)
		cmd.Dir = wtDir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git ls-remote: %v", err)
		}
		return strings.TrimSpace(string(out))
	}

	// Helper: create a fresh commit in the worktree so there is something to push.
	commit := func(wtDir, msg string) {
		cmd := exec.Command("git", "commit", "--allow-empty", "-m", msg)
		cmd.Dir = wtDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit: %s: %v", out, err)
		}
	}

	branch := "fabrik/issue-7"

	// ── Queued PR: push must be skipped, origin branch must not appear. ──
	wtDir, err := wm.EnsureWorktree(7, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	commit(wtDir, "queued change")

	queued := gh.ProjectItem{Number: 7, LinkedPRIsInMergeQueue: true}
	if err := e.pushBranchUnlessQueued(queued, wm); err != nil {
		t.Fatalf("pushBranchUnlessQueued (queued): %v", err)
	}
	if ref := remoteRef(wtDir, branch); ref != "" {
		t.Errorf("origin should not have %s after skipped push, got %q", branch, ref)
	}

	// ── Not-queued PR: push must proceed, origin branch must appear. ──
	notQueued := gh.ProjectItem{Number: 7, LinkedPRIsInMergeQueue: false}
	if err := e.pushBranchUnlessQueued(notQueued, wm); err != nil {
		t.Fatalf("pushBranchUnlessQueued (not queued): %v", err)
	}
	if ref := remoteRef(wtDir, branch); ref == "" {
		t.Errorf("origin should have %s after push, got empty", branch)
	}
}
