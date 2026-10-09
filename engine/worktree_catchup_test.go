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
