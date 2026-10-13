package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// catchUpFixture builds a bare clone (the manager's baseDir) whose origin is a source
// repo carrying fabrik/issue-7 with one commit past main, and returns the manager, the
// source repo (so a test can move the "remote" branch) and the branch's head SHA.
func catchUpFixture(t *testing.T) (wm *WorktreeManager, srcDir, head string) {
	t.Helper()
	bareDir, wroot := setupBareRepoForTrain(t)
	srcDir = filepath.Join(filepath.Dir(bareDir), "src")
	head = addMemberBranch(t, srcDir, bareDir, "fabrik/issue-7", "member.txt", "member\n")
	mustGit(t, bareDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	return NewWorktreeManagerWithRoot(bareDir, wroot), srcDir, head
}

// advanceRemoteBranch adds a commit to the source repo's fabrik/issue-7 and returns its SHA.
func advanceRemoteBranch(t *testing.T, srcDir, file string) string {
	t.Helper()
	mustGit(t, srcDir, "checkout", "fabrik/issue-7")
	writeFile(t, filepath.Join(srcDir, file), file+"\n")
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "advance "+file)
	sha := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "HEAD"))
	mustGit(t, srcDir, "checkout", "main")
	return sha
}

func TestPrepareCatchUp_CleanWorktreeThenPushAdvancesRemoteAndTrackingRef(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)

	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatalf("PrepareCatchUp: %v", err)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != head {
		t.Fatalf("worktree HEAD = %s, want %s", got, head)
	}

	writeFile(t, filepath.Join(wtDir, "caught-up.txt"), "x\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "catch-up")
	newHead, _ := gitRevParse(wtDir, "HEAD")

	if err := wm.PushCatchUp(7, head); err != nil {
		t.Fatalf("PushCatchUp: %v", err)
	}
	if got := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "fabrik/issue-7")); got != newHead {
		t.Errorf("remote branch = %s, want the catch-up commit %s", got, newHead)
	}
	// The tracking ref must follow, or the next stage's --force-with-lease PushBranch
	// would be judged against the pre-catch-up head and could clobber the catch-up.
	if got, _ := gitRevParse(wm.BaseDir(), "refs/remotes/origin/fabrik/issue-7"); got != newHead {
		t.Errorf("tracking ref = %s, want %s", got, newHead)
	}
	if err := wm.PushBranch(7); err != nil {
		t.Errorf("a following ordinary PushBranch must still succeed: %v", err)
	}
}

func TestPrepareCatchUp_DirtyWorktreeIsBusy(t *testing.T) {
	wm, _, head := catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wtDir, "wip.txt"), "uncommitted\n")

	_, err = wm.PrepareCatchUp(7, "main", head)
	if !errors.Is(err, ErrCatchUpBusy) {
		t.Fatalf("err = %v, want ErrCatchUpBusy", err)
	}
	if _, statErr := os.Stat(filepath.Join(wtDir, "wip.txt")); statErr != nil {
		t.Errorf("the uncommitted file must survive: %v", statErr)
	}
}

func TestPrepareCatchUp_InProgressMergeIsBusy(t *testing.T) {
	wm, _, head := catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	gitDir, _ := gitOutputIn(wtDir, "rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wtDir, gitDir)
	}
	writeFile(t, filepath.Join(gitDir, "MERGE_HEAD"), head+"\n")

	if _, err := wm.PrepareCatchUp(7, "main", head); !errors.Is(err, ErrCatchUpBusy) {
		t.Fatalf("err = %v, want ErrCatchUpBusy for a tree mid-merge", err)
	}
}

func TestPrepareCatchUp_RemoteMovedIsBusy(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	advanceRemoteBranch(t, srcDir, "other.txt")

	if _, err := wm.PrepareCatchUp(7, "main", head); !errors.Is(err, ErrCatchUpBusy) {
		t.Fatalf("err = %v, want ErrCatchUpBusy when the remote moved past the snapshot head", err)
	}
}

func TestPrepareCatchUp_LocalStrictlyBehindIsFastForwarded(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	newRemote := advanceRemoteBranch(t, srcDir, "other.txt")

	got, err := wm.PrepareCatchUp(7, "main", newRemote)
	if err != nil {
		t.Fatalf("PrepareCatchUp: %v", err)
	}
	if got != wtDir {
		t.Errorf("worktree = %s, want %s", got, wtDir)
	}
	if local, _ := gitRevParse(wtDir, "HEAD"); local != newRemote {
		t.Errorf("local HEAD = %s, want it fast-forwarded to %s", local, newRemote)
	}
}

func TestPrepareCatchUp_LocalAheadIsRefusedAndUntouched(t *testing.T) {
	wm, _, head := catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wtDir, "unpushed.txt"), "work\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "unpushed work")
	local, _ := gitRevParse(wtDir, "HEAD")

	_, err = wm.PrepareCatchUp(7, "main", head)
	if !errors.Is(err, ErrCatchUpLocalDiverged) {
		t.Fatalf("err = %v, want ErrCatchUpLocalDiverged", err)
	}
	if after, _ := gitRevParse(wtDir, "HEAD"); after != local {
		t.Errorf("local HEAD moved from %s to %s — unpushed work must never be touched", local, after)
	}
}

func TestPushCatchUp_RejectedWhenRemoteMovedAfterPrepare(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wtDir, "caught-up.txt"), "x\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "catch-up")

	moved := advanceRemoteBranch(t, srcDir, "concurrent.txt") // a stage / human push lands first

	if err := wm.PushCatchUp(7, head); err == nil {
		t.Fatal("PushCatchUp succeeded although the remote moved after the head it was built on")
	}
	if got := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "fabrik/issue-7")); got != moved {
		t.Errorf("remote branch = %s, want the concurrent push %s left intact", got, moved)
	}
}

// advanceSrcMain adds a commit to the source repo's main (the fixture's "origin") so a
// worktree rebase onto origin/main actually rewrites the member branch.
func advanceSrcMain(t *testing.T, srcDir, file string) {
	t.Helper()
	mustGit(t, srcDir, "checkout", "main")
	writeFile(t, filepath.Join(srcDir, file), file+"\n")
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "main: "+file)
}

// rebasedMemberWorktree leaves the member's worktree holding a pure local rebase of the
// remote branch onto an advanced main — the footprint of an engine pre-invocation rebase
// that was never pushed (#2150) — and returns the worktree, the remote head and the
// rebased local head.
func rebasedMemberWorktree(t *testing.T, wm *WorktreeManager, srcDir, head string) (wtDir, localHead string) {
	t.Helper()
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	advanceSrcMain(t, srcDir, "newer-main.txt")
	mustGit(t, wtDir, "fetch", "origin", "main")
	mustGit(t, wtDir, "rebase", "origin/main")
	localHead, _ = gitRevParse(wtDir, "HEAD")
	if localHead == head {
		t.Fatal("fixture: the rebase did not rewrite the member branch")
	}
	return wtDir, localHead
}

func TestPrepareCatchUp_PureLocalRebaseIsResetToRemoteHead(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, _ := rebasedMemberWorktree(t, wm, srcDir, head)

	got, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatalf("PrepareCatchUp: %v (a pure local rebase must not defer)", err)
	}
	if got != wtDir {
		t.Errorf("worktree = %s, want %s", got, wtDir)
	}
	if local, _ := gitRevParse(wtDir, "HEAD"); local != head {
		t.Errorf("local HEAD = %s, want it reset to the remote head %s", local, head)
	}
	// The local rebase must never reach the remote.
	if remote := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "fabrik/issue-7")); remote != head {
		t.Errorf("remote branch = %s, want it untouched at %s", remote, head)
	}
}

func TestPrepareCatchUp_PureRebaseToleranceDisabledRefuses(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	rebasedMemberWorktree(t, wm, srcDir, head)
	wm.pureRebaseDisabled.Store(true)

	if _, err := wm.PrepareCatchUp(7, "main", head); !errors.Is(err, ErrCatchUpLocalDiverged) {
		t.Fatalf("err = %v, want ErrCatchUpLocalDiverged with the tolerance neutralised", err)
	}
}

func TestPrepareCatchUp_RebaseWithLocalOnlyCommitIsRefused(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, _ := rebasedMemberWorktree(t, wm, srcDir, head)
	writeFile(t, filepath.Join(wtDir, "unpushed.txt"), "work\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "unpushed work")
	local, _ := gitRevParse(wtDir, "HEAD")

	_, err := wm.PrepareCatchUp(7, "main", head)
	if !errors.Is(err, ErrCatchUpLocalDiverged) {
		t.Fatalf("err = %v, want ErrCatchUpLocalDiverged", err)
	}
	if after, _ := gitRevParse(wtDir, "HEAD"); after != local {
		t.Errorf("local HEAD moved from %s to %s — a local-only commit must never be discarded", local, after)
	}
}

func TestPrepareCatchUp_RebaseWithLocalMergeCommitIsRefused(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, _ := rebasedMemberWorktree(t, wm, srcDir, head)
	// A local-only merge commit with content: invisible to patch-ids.
	mustGit(t, wtDir, "checkout", "-b", "side")
	writeFile(t, filepath.Join(wtDir, "side.txt"), "side\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "side")
	mustGit(t, wtDir, "checkout", "fabrik/issue-7")
	mustGit(t, wtDir, "merge", "--no-ff", "-m", "merge side", "side")
	local, _ := gitRevParse(wtDir, "HEAD")

	_, err := wm.PrepareCatchUp(7, "main", head)
	if !errors.Is(err, ErrCatchUpLocalDiverged) {
		t.Fatalf("err = %v, want ErrCatchUpLocalDiverged", err)
	}
	if after, _ := gitRevParse(wtDir, "HEAD"); after != local {
		t.Errorf("local HEAD moved from %s to %s", local, after)
	}
}

func TestPrepareCatchUp_ConflictEditedRebaseIsRefused(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	// Main touches the member's file, so the rebase conflicts and is resolved by editing
	// the patch — its patch-id changes, so it is local-only work as far as git can tell.
	advanceSrcMain(t, srcDir, "member.txt")
	mustGit(t, wtDir, "fetch", "origin", "main")
	if out, err := gitOutputIn(wtDir, "rebase", "origin/main"); err == nil {
		t.Fatalf("fixture: expected a rebase conflict, got %s", out)
	}
	writeFile(t, filepath.Join(wtDir, "member.txt"), "resolved differently\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "-c", "core.editor=true", "rebase", "--continue")
	// git leaves REBASE_HEAD behind after a conflict-resolved rebase; clear it so the
	// busy check does not pre-empt the patch-id comparison under test.
	_, _ = gitOutputIn(wtDir, "update-ref", "-d", "REBASE_HEAD")
	local, _ := gitRevParse(wtDir, "HEAD")

	if _, err := wm.PrepareCatchUp(7, "main", head); !errors.Is(err, ErrCatchUpLocalDiverged) {
		t.Fatalf("err = %v, want ErrCatchUpLocalDiverged for a conflict-edited rebase", err)
	}
	if after, _ := gitRevParse(wtDir, "HEAD"); after != local {
		t.Errorf("local HEAD moved from %s to %s", local, after)
	}
}
