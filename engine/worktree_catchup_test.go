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
	wm, srcDir, head := catchUpFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	// A real conflicted merge: MERGE_HEAD plus unmerged index entries.
	advanceSrcMain(t, srcDir, "member.txt")
	mustGit(t, wtDir, "fetch", "origin", "main")
	if out, err := gitOutputIn(wtDir, "merge", "origin/main"); err == nil {
		t.Fatalf("fixture: expected a merge conflict, got %s", out)
	}

	if _, err := wm.PrepareCatchUp(7, "main", head); !errors.Is(err, ErrCatchUpBusy) {
		t.Fatalf("err = %v, want ErrCatchUpBusy for a tree mid-merge", err)
	}
	if _, err := gitOutputIn(wtDir, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err != nil {
		t.Error("MERGE_HEAD of a real merge must not be removed")
	}
}

// gitDirOf returns the worktree's own git directory.
func gitDirOf(t *testing.T, wtDir string) string {
	t.Helper()
	gitDir, err := gitOutputIn(wtDir, "rev-parse", "--git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wtDir, gitDir)
	}
	return gitDir
}

// readyWorktree returns a prepared, clean member worktree.
func readyWorktree(t *testing.T, wm *WorktreeManager, head string) string {
	t.Helper()
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	return wtDir
}

// #2615 R1: Fabrik's own untracked leftovers are not "uncommitted changes".
func TestPrepareCatchUp_FabrikScratchFilesAreNotBusy(t *testing.T) {
	wm, _, head := catchUpFixture(t)
	wtDir := readyWorktree(t, wm, head)
	writeFile(t, filepath.Join(wtDir, ".fabrik", "scratch", "validate.log"), "log\n")
	writeFile(t, filepath.Join(wtDir, ".fabrik", "scratch", "sub", "more.log"), "log\n")
	writeFile(t, filepath.Join(wtDir, ".fabrik-context", "issue.md"), "ctx\n")

	if _, err := wm.PrepareCatchUp(7, "main", head); err != nil {
		t.Fatalf("PrepareCatchUp with only Fabrik scratch files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtDir, ".fabrik", "scratch", "validate.log")); err != nil {
		t.Errorf("scratch files must not be deleted: %v", err)
	}
}

// #2615: a git warning on stderr during the untracked listing (here an unreadable scratch
// subdirectory) is never read as an untracked path, so it cannot make the worktree busy.
func TestPrepareCatchUp_GitStderrWarningIsNotAnUntrackedFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not restrict root")
	}
	wm, _, head := catchUpFixture(t)
	wtDir := readyWorktree(t, wm, head)
	locked := filepath.Join(wtDir, ".fabrik", "scratch", "locked")
	writeFile(t, filepath.Join(locked, "x.log"), "log\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if out, err := gitStdoutIn(wtDir, "ls-files", "--others", "--exclude-standard", "-z"); err != nil || strings.Contains(out, "warning") {
		t.Fatalf("gitStdoutIn must return stdout only: out=%q err=%v", out, err)
	}
	if _, err := wm.PrepareCatchUp(7, "main", head); err != nil {
		t.Fatalf("PrepareCatchUp with an unreadable scratch directory: %v", err)
	}
}

func TestPrepareCatchUp_ScratchExemption_NonVacuity(t *testing.T) {
	wm, _, head := catchUpFixture(t)
	wtDir := readyWorktree(t, wm, head)
	writeFile(t, filepath.Join(wtDir, ".fabrik", "scratch", "validate.log"), "log\n")
	wm.scratchExemptDisabled.Store(true)

	var busy *CatchUpBusyError
	if _, err := wm.PrepareCatchUp(7, "main", head); !errors.As(err, &busy) || !busy.Worktree {
		t.Fatalf("with the exemption neutralised err = %v, want a worktree-busy error", err)
	}
}

// #2615 FR-002: only Fabrik-written untracked paths are exempt.
func TestPrepareCatchUp_NonScratchChangesStayBusy(t *testing.T) {
	cases := map[string]func(t *testing.T, wtDir string){
		"stray untracked source file": func(t *testing.T, wtDir string) { writeFile(t, filepath.Join(wtDir, "stray.go"), "x\n") },
		"lookalike directory":         func(t *testing.T, wtDir string) { writeFile(t, filepath.Join(wtDir, ".fabrik-contextual", "x"), "x\n") },
		"other file under .fabrik":    func(t *testing.T, wtDir string) { writeFile(t, filepath.Join(wtDir, ".fabrik", "notes.md"), "x\n") },
		"modified tracked file": func(t *testing.T, wtDir string) {
			writeFile(t, filepath.Join(wtDir, "member.txt"), "changed\n")
		},
		"scratch next to a tracked change": func(t *testing.T, wtDir string) {
			writeFile(t, filepath.Join(wtDir, ".fabrik", "scratch", "a.log"), "x\n")
			writeFile(t, filepath.Join(wtDir, "member.txt"), "changed\n")
		},
		"tracked change under an exempt path": func(t *testing.T, wtDir string) {
			writeFile(t, filepath.Join(wtDir, ".fabrik", "scratch", "tracked.txt"), "v1\n")
			mustGit(t, wtDir, "add", "-f", ".fabrik/scratch/tracked.txt")
			mustGit(t, wtDir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "track")
			writeFile(t, filepath.Join(wtDir, ".fabrik", "scratch", "tracked.txt"), "v2\n")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			wm, _, head := catchUpFixture(t)
			wtDir := readyWorktree(t, wm, head)
			setup(t, wtDir)
			reason, err := wm.worktreeBusyReason(wtDir, "fabrik/issue-7")
			if err != nil || reason == "" {
				t.Fatalf("reason=%q err=%v, want busy", reason, err)
			}
		})
	}
}

// #2615 R2: a stale ref with no operation in progress is removed and is not busy.
func TestPrepareCatchUp_StaleRefsAreRemovedNotBusy(t *testing.T) {
	for _, ref := range []string{"REBASE_HEAD", "MERGE_HEAD", "CHERRY_PICK_HEAD"} {
		t.Run(ref, func(t *testing.T) {
			wm, _, head := catchUpFixture(t)
			wtDir := readyWorktree(t, wm, head)
			writeFile(t, filepath.Join(gitDirOf(t, wtDir), ref), head+"\n")

			if _, err := wm.PrepareCatchUp(7, "main", head); err != nil {
				t.Fatalf("PrepareCatchUp with a stale %s: %v", ref, err)
			}
			if _, err := gitOutputIn(wtDir, "rev-parse", "-q", "--verify", ref); err == nil {
				t.Errorf("stale %s must be removed", ref)
			}
		})
	}
}

func TestPrepareCatchUp_StaleRefAndScratchTogether(t *testing.T) {
	wm, _, head := catchUpFixture(t)
	wtDir := readyWorktree(t, wm, head)
	writeFile(t, filepath.Join(wtDir, ".fabrik", "scratch", "a.log"), "x\n")
	mustGit(t, wtDir, "update-ref", "REBASE_HEAD", head)
	if _, err := wm.PrepareCatchUp(7, "main", head); err != nil {
		t.Fatalf("scratch plus a stale ref: %v", err)
	}
}

func TestPrepareCatchUp_StaleRefCleanup_NonVacuity(t *testing.T) {
	wm, _, head := catchUpFixture(t)
	wtDir := readyWorktree(t, wm, head)
	mustGit(t, wtDir, "update-ref", "REBASE_HEAD", head)
	wm.staleRefCleanupDisabled.Store(true)
	if _, err := wm.PrepareCatchUp(7, "main", head); !errors.Is(err, ErrCatchUpBusy) {
		t.Fatalf("with the cleanup neutralised err = %v, want ErrCatchUpBusy", err)
	}
}

// A real rebase in progress keeps today's deferral and loses nothing.
func TestPrepareCatchUp_RealRebaseInProgressStaysBusy(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir := readyWorktree(t, wm, head)
	advanceSrcMain(t, srcDir, "member.txt")
	mustGit(t, wtDir, "fetch", "origin", "main")
	if out, err := gitOutputIn(wtDir, "rebase", "origin/main"); err == nil {
		t.Fatalf("fixture: expected a rebase conflict, got %s", out)
	}

	// An operation really in progress is busy but is not a leftover: it must not count
	// toward the bounded deferral (spec FR-007).
	var busy *CatchUpBusyError
	if _, err := wm.PrepareCatchUp(7, "main", head); !errors.As(err, &busy) || !busy.Worktree || !busy.Operation {
		t.Fatalf("err = %v, want a worktree-busy error marked Operation", err)
	}
	if _, err := gitOutputIn(wtDir, "rev-parse", "-q", "--verify", "REBASE_HEAD"); err != nil {
		t.Error("REBASE_HEAD of a real rebase must not be removed")
	}
}

// A merge whose conflicts are resolved and staged but not yet committed (so no rebase
// directory and no unmerged entries) is still an operation in progress: its MERGE_HEAD
// must survive, or the pending commit would lose its merge parent.
func TestWorktreeBusyReason_PendingMergeCommitKeepsMergeHead(t *testing.T) {
	for _, ref := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD"} {
		t.Run(ref, func(t *testing.T) {
			wm, _, head := catchUpFixture(t)
			wtDir := readyWorktree(t, wm, head)
			gd := gitDirOf(t, wtDir)
			writeFile(t, filepath.Join(gd, ref), head+"\n")
			writeFile(t, filepath.Join(gd, "MERGE_MSG"), "Merge pending\n")

			reason, operation, err := wm.worktreeBusyDetail(wtDir, "fabrik/issue-7")
			if err != nil || !operation || !strings.Contains(reason, ref+" present") {
				t.Fatalf("reason=%q operation=%v err=%v, want %s present as an operation", reason, operation, err, ref)
			}
			if _, err := os.Stat(filepath.Join(gd, ref)); err != nil {
				t.Errorf("%s of a pending commit must not be removed: %v", ref, err)
			}
		})
	}
}

// A rebase-merge directory alone (the ref gone) is still an operation in progress.
func TestWorktreeBusyReason_RebaseDirectoryWithStaleRefIsBusy(t *testing.T) {
	wm, _, head := catchUpFixture(t)
	wtDir := readyWorktree(t, wm, head)
	mustGit(t, wtDir, "update-ref", "REBASE_HEAD", head)
	if err := os.MkdirAll(filepath.Join(gitDirOf(t, wtDir), "rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	reason, err := wm.worktreeBusyReason(wtDir, "fabrik/issue-7")
	if err != nil || !strings.Contains(reason, "REBASE_HEAD present") {
		t.Fatalf("reason=%q err=%v, want REBASE_HEAD present", reason, err)
	}
	if _, err := gitOutputIn(wtDir, "rev-parse", "-q", "--verify", "REBASE_HEAD"); err != nil {
		t.Error("the ref must be left in place while a rebase directory exists")
	}
}

// The remote moving is a race, not a leftover: it must not count as a worktree deferral.
func TestPrepareCatchUp_RemoteMovedIsNotWorktreeBusy(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	readyWorktree(t, wm, head)
	advanceRemoteBranch(t, srcDir, "late.txt")
	var busy *CatchUpBusyError
	if _, err := wm.PrepareCatchUp(7, "main", head); !errors.As(err, &busy) || busy.Worktree {
		t.Fatalf("err = %v, want a non-worktree busy error", err)
	}
}

// writeGitExclude keeps both Fabrik paths out of git status and is idempotent.
func TestWriteGitExclude_ListsFabrikPathsOnce(t *testing.T) {
	wm, _, head := catchUpFixture(t)
	wtDir := readyWorktree(t, wm, head)
	wm.writeGitExclude(wtDir, 7)
	wm.writeGitExclude(wtDir, 7)
	data, err := os.ReadFile(filepath.Join(gitDirOf(t, wtDir), "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range gitExcludeEntries {
		if n := strings.Count(string(data), e+"\n"); n != 1 {
			t.Errorf("exclude has %d copies of %q, want 1:\n%s", n, e, data)
		}
	}
}

// #2295 shares the check: a branch sync must not touch a worktree mid-rebase.
func TestBranchSync_RealRebaseInProgressSkipsSync(t *testing.T) {
	wm, srcDir, head := catchUpFixture(t)
	wtDir := readyWorktree(t, wm, head)
	advanceSrcMain(t, srcDir, "member.txt")
	mustGit(t, wtDir, "fetch", "origin", "main")
	if _, err := gitOutputIn(wtDir, "rebase", "origin/main"); err == nil {
		t.Fatal("fixture: expected a rebase conflict")
	}
	rep, err := wm.syncIssueBranch(wtDir, "main", 7)
	if err != nil || rep.Outcome != SyncSkipped {
		t.Fatalf("rep=%+v err=%v, want a skipped sync", rep, err)
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
