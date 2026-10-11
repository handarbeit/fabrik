package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// restoreEngine builds an engine over the catch-up fixture with fabrik/issue-7's
// worktree already created and main advanced, so processComments' pre-invocation
// rebase actually rewrites the branch (#2150).
func restoreEngine(t *testing.T, invoke func(workDir string)) (e *Engine, wm *WorktreeManager, srcDir, wtDir, head string) {
	t.Helper()
	wm, srcDir, head = catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	advanceSrcMain(t, srcDir, "newer-main.txt")

	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			invoke(workDir)
			return "resolved the thread.", false, TokenUsage{}, nil
		},
	}
	e = NewWithDeps(Config{
		Owner: "owner", Repo: "repo", User: "testuser", Token: "token",
		MaxConcurrent: 5, Stages: testStages(),
	}, &mockGitHubClient{}, claude, wm)
	return e, wm, srcDir, wtDir, head
}

func runRestoreCommentPass(t *testing.T, e *Engine) {
	t.Helper()
	stage := &stages.Stage{Name: "Research", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 7, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 101, Author: "testuser", Body: "please look"}}
	if err := e.processComments(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, comments); err != nil {
		t.Fatalf("processComments: %v", err)
	}
}

// A comment pass that commits nothing leaves the branch equal to its remote, not a
// rebased copy of it (#2150 R1).
func TestProcessComments_NoCommitPassLeavesBranchEqualToRemote(t *testing.T) {
	e, _, _, wtDir, head := restoreEngine(t, func(string) {})

	runRestoreCommentPass(t, e)

	if got, _ := gitRevParse(wtDir, "HEAD"); got != head {
		t.Fatalf("HEAD = %s, want the remote head %s (the unpushed engine rebase must be undone)", got, head)
	}
}

// Neutralising R1 makes the engine's rebase stick — the test above bites.
func TestProcessComments_NoCommitPassRestoreNeutralisedLeavesRebase(t *testing.T) {
	e, _, _, wtDir, head := restoreEngine(t, func(string) {})
	e.SetCommentRebaseRestoreDisabledForTest(true)

	runRestoreCommentPass(t, e)

	if got, _ := gitRevParse(wtDir, "HEAD"); got == head {
		t.Fatal("HEAD equals the remote head with the restore neutralised — the seam is not wired")
	}
}

// A pass that does commit keeps its commit; nothing is restored or discarded.
func TestProcessComments_CommittingPassKeepsItsCommit(t *testing.T) {
	var committed string
	e, _, _, wtDir, head := restoreEngine(t, func(workDir string) {
		if err := os.WriteFile(filepath.Join(workDir, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
			t.Error(err)
		}
		mustGit(t, workDir, "add", "-A")
		mustGit(t, workDir, "commit", "-m", "fix: address review")
		committed, _ = gitRevParse(workDir, "HEAD")
	})

	runRestoreCommentPass(t, e)

	if got, _ := gitRevParse(wtDir, "HEAD"); got != committed || got == head {
		t.Fatalf("HEAD = %s, want the pass's own commit %s", got, committed)
	}
}
