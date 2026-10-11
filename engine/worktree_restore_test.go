package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// rebasedByEngine reproduces the engine's pre-invocation rebase: the member worktree is
// created, main advances, and EnsureWorktree(skipUpdate=false) rebases the branch. It
// returns the worktree and the heads before and after the rebase.
func rebasedByEngine(t *testing.T, wm *WorktreeManager, srcDir, head string) (wtDir, pre, post string) {
	t.Helper()
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	pre, post = enginePreRebase(t, wm, srcDir, wtDir)
	return wtDir, pre, post
}

// enginePreRebase advances main and runs the engine's rebase on an existing worktree.
func enginePreRebase(t *testing.T, wm *WorktreeManager, srcDir, wtDir string) (pre, post string) {
	t.Helper()
	pre, _ = gitRevParse(wtDir, "HEAD")
	advanceSrcMain(t, srcDir, "newer-main-"+pre[:6]+".txt")
	if _, err := wm.EnsureWorktree(7, "main", false); err != nil {
		t.Fatal(err)
	}
	post, _ = gitRevParse(wtDir, "HEAD")
	if post == pre {
		t.Fatal("fixture: the engine rebase did not move HEAD")
	}
	return pre, post
}

func TestRestoreUnpushedRebase_RestoresPreRebaseHead(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	pre, post := enginePreRebase(t, wm, srcDir, wtDir)

	ok, reason := wm.RestoreUnpushedRebase(7, "main", pre, post)
	if !ok {
		t.Fatalf("restore refused: %q", reason)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != pre {
		t.Errorf("HEAD = %s, want the pre-rebase head %s", got, pre)
	}
	// The branch now equals the remote branch again.
	if remote := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "fabrik/issue-7")); remote != head {
		t.Errorf("remote = %s, want it untouched at %s", remote, head)
	}
}

func TestRestoreUnpushedRebase_DisabledIsANoOp(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, pre, post := rebasedByEngine(t, wm, srcDir, head)
	wm.restoreRebaseDisabled.Store(true)

	if ok, _ := wm.RestoreUnpushedRebase(7, "main", pre, post); ok {
		t.Fatal("restore ran with the seam disabled")
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != post {
		t.Errorf("HEAD = %s, want it left at %s", got, post)
	}
}

func TestRestoreUnpushedRebase_ClaudeCommitIsNeverDiscarded(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, pre, post := rebasedByEngine(t, wm, srcDir, head)
	writeFile(t, filepath.Join(wtDir, "claude.txt"), "work\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "claude work")
	committed, _ := gitRevParse(wtDir, "HEAD")

	if ok, _ := wm.RestoreUnpushedRebase(7, "main", pre, post); ok {
		t.Fatal("restore ran after Claude committed")
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != committed {
		t.Errorf("HEAD = %s, want the commit %s untouched", got, committed)
	}
}

func TestRestoreUnpushedRebase_PushedRebaseIsLeftAlone(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, pre, post := rebasedByEngine(t, wm, srcDir, head)
	// Something pushed the rebased head (Claude, markPRReady, ...).
	mustGit(t, srcDir, "config", "receive.denyCurrentBranch", "ignore")
	if err := wm.PushBranch(7); err != nil {
		t.Fatalf("PushBranch: %v", err)
	}

	ok, reason := wm.RestoreUnpushedRebase(7, "main", pre, post)
	if ok {
		t.Fatal("restore ran although the rebased head is on the remote — it would manufacture a divergence")
	}
	if !strings.Contains(reason, "already on the remote") {
		t.Errorf("reason = %q", reason)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != post {
		t.Errorf("HEAD = %s, want %s", got, post)
	}
}

func TestRestoreUnpushedRebase_DirtyTreeIsLeftAlone(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, pre, post := rebasedByEngine(t, wm, srcDir, head)
	writeFile(t, filepath.Join(wtDir, "wip.txt"), "uncommitted\n")

	if ok, _ := wm.RestoreUnpushedRebase(7, "main", pre, post); ok {
		t.Fatal("restore ran on a dirty tree")
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != post {
		t.Errorf("HEAD = %s, want %s", got, post)
	}
}

func TestRestoreUnpushedRebase_CommitAddedByTheRebaseStepIsLeftAlone(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	// A commit that is in post but was never on the pre-rebase head, standing in for the
	// engine's own "remove stale .fabrik-context" commit.
	pre, _ := gitRevParse(wtDir, "HEAD")
	advanceSrcMain(t, srcDir, "newer-main.txt")
	mustGit(t, wtDir, "fetch", "origin", "main")
	mustGit(t, wtDir, "rebase", "origin/main")
	writeFile(t, filepath.Join(wtDir, "extra.txt"), "x\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "chore: extra")
	post, _ := gitRevParse(wtDir, "HEAD")

	ok, reason := wm.RestoreUnpushedRebase(7, "main", pre, post)
	if ok {
		t.Fatal("restore would have discarded a commit that was not on the pre-rebase head")
	}
	if !strings.Contains(reason, "not on the pre-rebase head") {
		t.Errorf("reason = %q", reason)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != post {
		t.Errorf("HEAD = %s, want %s", got, post)
	}
}

func TestRestoreUnpushedRebase_FetchFailureLeavesBranchAlone(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, pre, post := rebasedByEngine(t, wm, srcDir, head)
	mustGitDir(t, wm.BaseDir(), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	ok, reason := wm.RestoreUnpushedRebase(7, "main", pre, post)
	if ok {
		t.Fatal("restore ran without a fresh view of the remote")
	}
	if !strings.Contains(reason, "could not fetch") {
		t.Errorf("reason = %q", reason)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != post {
		t.Errorf("HEAD = %s, want %s", got, post)
	}
}
