package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the replay-equivalence fallback (#2517, ADR 2517). The fixture reproduces the
// #2498 shape: a branch commit edits one line of a shared file and main later edits a
// nearby (but not adjacent) line, so a clean rebase leaves the commit's content intact but
// changes its diff context — and therefore its patch-id.

const (
	shiftBranchRef = "refs/remotes/origin/fabrik/issue-7"
	shiftBaseRef   = "refs/remotes/origin/main"
)

func sharedFileContent(l3, l5 string) string {
	return "l1\nl2\n" + l3 + "\nl4\n" + l5 + "\nl6\nl7\nl8\n"
}

// contextShiftFixture is catchUpFixture with fabrik/issue-7 carrying one commit that edits
// line 3 of a file main already has. It returns the manager, the source repo and the
// remote branch head.
func contextShiftFixture(t *testing.T) (wm *WorktreeManager, srcDir, head string) {
	t.Helper()
	bareDir, wroot := setupBareRepoForTrain(t)
	srcDir = filepath.Join(filepath.Dir(bareDir), "src")
	writeFile(t, filepath.Join(srcDir, "shared.txt"), sharedFileContent("l3", "l5"))
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "main: shared.txt")
	head = addMemberBranch(t, srcDir, bareDir, "fabrik/issue-7", "shared.txt", sharedFileContent("l3-branch", "l5"))
	mustGit(t, bareDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	return NewWorktreeManagerWithRoot(bareDir, wroot), srcDir, head
}

// shiftMain edits line 5 of the shared file on the source repo's main: a clean rebase of
// the branch commit (which edits line 3) but a change to its diff context.
func shiftMain(t *testing.T, srcDir string) {
	t.Helper()
	mustGit(t, srcDir, "checkout", "main")
	writeFile(t, filepath.Join(srcDir, "shared.txt"), sharedFileContent("l3", "l5-main"))
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "main: shift context")
}

// rebaseShifted shifts main, then rebases the worktree's branch onto it by hand and
// returns the rebased head.
func rebaseShifted(t *testing.T, srcDir, wtDir string) string {
	t.Helper()
	shiftMain(t, srcDir)
	mustGit(t, wtDir, "fetch", "origin", "main")
	mustGit(t, wtDir, "rebase", "origin/main")
	local, _ := gitRevParse(wtDir, "HEAD")
	return local
}

// requirePatchIDMiss pins the fixture: the patch-id check alone must call subject
// non-subsumed, otherwise the test could pass without the fallback.
func requirePatchIDMiss(t *testing.T, wtDir, subject, other string) {
	t.Helper()
	if ok, why := holdsNothingBeyond(wtDir, subject, other, shiftBaseRef); ok || why != "" {
		t.Fatalf("fixture: expected a patch-id miss between %s and %s, got ok=%v why=%q", subject, other, ok, why)
	}
}

func TestReplayEquivalent_ContextShiftedRebase(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	local := rebaseShifted(t, srcDir, wtDir)
	if local == head {
		t.Fatal("fixture: rebase did not rewrite the branch")
	}
	requirePatchIDMiss(t, wtDir, "HEAD", shiftBranchRef)

	if ok, why := replayEquivalent(wtDir, "HEAD", shiftBranchRef, shiftBaseRef); !ok {
		t.Fatalf("replayEquivalent = false (%s), want true for a context-only shift", why)
	}
}

func TestReplayEquivalent_RebasedTwiceOntoSuccessiveBases(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	rebaseShifted(t, srcDir, wtDir)
	advanceSrcMain(t, srcDir, "second-base.txt")
	mustGit(t, wtDir, "fetch", "origin", "main")
	mustGit(t, wtDir, "rebase", "origin/main")

	if ok, why := replayEquivalent(wtDir, "HEAD", shiftBranchRef, shiftBaseRef); !ok {
		t.Fatalf("replayEquivalent = false (%s), want true after a second rebase", why)
	}
}

func TestReplayEquivalent_LocalOnlyCommitIsNotEquivalent(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	rebaseShifted(t, srcDir, wtDir)
	writeFile(t, filepath.Join(wtDir, "unpushed.txt"), "work\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "unpushed work")

	if ok, _ := replayEquivalent(wtDir, "HEAD", shiftBranchRef, shiftBaseRef); ok {
		t.Fatal("a local-only commit must not read as equivalent")
	}
}

func TestReplayEquivalent_LocalOnlyMergeIsNotEquivalent(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	rebaseShifted(t, srcDir, wtDir)
	// An empty-content merge: the tree stays equal, only the merge commit is local-only.
	mustGit(t, wtDir, "checkout", "-b", "side")
	mustGit(t, wtDir, "commit", "--allow-empty", "-m", "side")
	mustGit(t, wtDir, "checkout", "fabrik/issue-7")
	mustGit(t, wtDir, "merge", "--no-ff", "-m", "merge side", "side")

	ok, why := replayEquivalent(wtDir, "HEAD", shiftBranchRef, shiftBaseRef)
	if ok || !strings.Contains(why, "merge") {
		t.Fatalf("replayEquivalent = %v (%q), want false for a local-only merge commit", ok, why)
	}
}

func TestReplayEquivalent_ConflictEditedRebaseIsNotEquivalent(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	// Main edits the very line the branch edited: the rebase conflicts and is resolved by
	// writing different content.
	mustGit(t, srcDir, "checkout", "main")
	writeFile(t, filepath.Join(srcDir, "shared.txt"), sharedFileContent("l3-main", "l5"))
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "main: same line")
	mustGit(t, wtDir, "fetch", "origin", "main")
	if out, err := gitOutputIn(wtDir, "rebase", "origin/main"); err == nil {
		t.Fatalf("fixture: expected a conflict, got %s", out)
	}
	writeFile(t, filepath.Join(wtDir, "shared.txt"), sharedFileContent("l3-resolved", "l5"))
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "-c", "core.editor=true", "rebase", "--continue")

	if ok, _ := replayEquivalent(wtDir, "HEAD", shiftBranchRef, shiftBaseRef); ok {
		t.Fatal("a conflict-edited rebase must not read as equivalent")
	}
}

func TestReplayEquivalent_GitErrorIsNotEquivalent(t *testing.T) {
	wm, _, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)

	ok, why := replayEquivalent(wtDir, "HEAD", "refs/remotes/origin/does-not-exist", shiftBaseRef)
	if ok {
		t.Fatal("an unresolvable ref must not read as equivalent")
	}
	if strings.Contains(why, "failed") {
		t.Errorf("reason %q must not contain \"failed\": callers read that as git doubt", why)
	}
}

func TestSubsumedByReplay_SeamIsHonoured(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	rebaseShifted(t, srcDir, wtDir)

	if ok, _ := wm.subsumedByReplay(wtDir, "HEAD", shiftBranchRef, shiftBaseRef); !ok {
		t.Fatal("expected equivalence with the seam off")
	}
	wm.replayEquivalenceDisabled.Store(true)
	if ok, _ := wm.subsumedByReplay(wtDir, "HEAD", shiftBranchRef, shiftBaseRef); ok {
		t.Fatal("the seam did not neutralise the fallback")
	}
}

// --- RestoreUnpushedRebase (FR-001) ---

// engineShiftRebase runs the engine's own rebase (EnsureWorktree, skipUpdate=false) after
// shifting main, returning the heads before and after.
func engineShiftRebase(t *testing.T, wm *WorktreeManager, srcDir, wtDir string) (pre, post string) {
	t.Helper()
	pre, _ = gitRevParse(wtDir, "HEAD")
	shiftMain(t, srcDir)
	if _, err := wm.EnsureWorktree(7, "main", false, WithoutIssueBranchSync()); err != nil {
		t.Fatal(err)
	}
	post, _ = gitRevParse(wtDir, "HEAD")
	if post == pre {
		t.Fatal("fixture: the engine rebase did not move HEAD")
	}
	requirePatchIDMiss(t, wtDir, post, pre)
	return pre, post
}

func TestRestoreUnpushedRebase_RestoresWhenOnlyContextShifted(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, err := wm.PrepareCatchUp(7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	pre, post := engineShiftRebase(t, wm, srcDir, wtDir)

	ok, reason := wm.RestoreUnpushedRebase(7, "main", pre, post)
	if !ok {
		t.Fatalf("restore refused: %q", reason)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != pre {
		t.Errorf("HEAD = %s, want %s", got, pre)
	}
}

func TestRestoreUnpushedRebase_ContentCheckDisabledLeavesShiftedRebase(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	pre, post := engineShiftRebase(t, wm, srcDir, wtDir)
	wm.restoreContentCheckDisabled.Store(true)

	ok, reason := wm.RestoreUnpushedRebase(7, "main", pre, post)
	if ok {
		t.Fatal("restore ran with the content check neutralised")
	}
	if !strings.Contains(reason, "not on the pre-rebase head") {
		t.Errorf("reason = %q", reason)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != post {
		t.Errorf("HEAD = %s, want %s", got, post)
	}
}

func TestRestoreUnpushedRebase_ShiftedRebaseWithExtraCommitIsLeftAlone(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	pre, _ := gitRevParse(wtDir, "HEAD")
	rebaseShifted(t, srcDir, wtDir)
	writeFile(t, filepath.Join(wtDir, "extra.txt"), "x\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "chore: extra")
	post, _ := gitRevParse(wtDir, "HEAD")

	if ok, _ := wm.RestoreUnpushedRebase(7, "main", pre, post); ok {
		t.Fatal("restore would have discarded a commit that was not on the pre-rebase head")
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != post {
		t.Errorf("HEAD = %s, want %s", got, post)
	}
}

// --- issue-branch sync (FR-002) ---

func TestBranchSync_ResetsContextShiftedLocalRebase(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	rebaseShifted(t, srcDir, wtDir)

	var rep SyncReport
	if _, err := wm.EnsureWorktree(7, "main", true, WithSyncReport(&rep)); err != nil {
		t.Fatalf("EnsureWorktree: %v (a content-equivalent local must not diverge)", err)
	}
	if rep.Outcome != SyncReset {
		t.Fatalf("outcome = %s, want %s", rep.Outcome, SyncReset)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != head {
		t.Errorf("HEAD = %s, want the remote head %s", got, head)
	}
}

func TestBranchSync_NonVacuous_ReplaySeamOffDiverges(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	local := rebaseShifted(t, srcDir, wtDir)
	wm.replayEquivalenceDisabled.Store(true)

	_, err := wm.EnsureWorktree(7, "main", true, WithSyncReport(&SyncReport{}))
	var div *BranchDivergedError
	if !errors.As(err, &div) {
		t.Fatalf("err = %v, want *BranchDivergedError with the fallback neutralised", err)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != local {
		t.Errorf("HEAD moved to %s, want %s untouched", got, local)
	}
}

func TestBranchSync_ContextShiftedLocalWithOwnCommitStillDiverges(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	rebaseShifted(t, srcDir, wtDir)
	writeFile(t, filepath.Join(wtDir, "unpushed.txt"), "work\n")
	mustGit(t, wtDir, "add", "-A")
	mustGit(t, wtDir, "commit", "-m", "unpushed work")
	// Make the remote move too, so each side holds something the other lacks.
	advanceRemoteBranch(t, srcDir, "remote-only.txt")

	_, err := wm.EnsureWorktree(7, "main", true, WithSyncReport(&SyncReport{}))
	var div *BranchDivergedError
	if !errors.As(err, &div) {
		t.Fatalf("err = %v, want *BranchDivergedError", err)
	}
}

func TestVerifyForPush_SkipsContextShiftedLocal(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	rebaseShifted(t, srcDir, wtDir)

	wm.mu.Lock()
	defer wm.mu.Unlock()
	_, skip, err := wm.verifyForPush(wtDir, "fabrik/issue-7", "main", 7)
	if err != nil || !skip {
		t.Fatalf("verifyForPush skip=%v err=%v, want skip=true (a push would rewind the remote)", skip, err)
	}
}

// --- catch-up preparation (FR-002) ---

func TestPrepareCatchUp_ContextShiftedRebaseIsResetToRemoteHead(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	rebaseShifted(t, srcDir, wtDir)

	if _, err := wm.PrepareCatchUp(7, "main", head); err != nil {
		t.Fatalf("PrepareCatchUp: %v", err)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != head {
		t.Errorf("HEAD = %s, want %s", got, head)
	}
	if remote := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "fabrik/issue-7")); remote != head {
		t.Errorf("remote = %s, want untouched %s", remote, head)
	}
}

func TestPrepareCatchUp_NonVacuous_ReplaySeamOffRefuses(t *testing.T) {
	wm, srcDir, head := contextShiftFixture(t)
	wtDir, _ := wm.PrepareCatchUp(7, "main", head)
	local := rebaseShifted(t, srcDir, wtDir)
	wm.replayEquivalenceDisabled.Store(true)

	if _, err := wm.PrepareCatchUp(7, "main", head); !errors.Is(err, ErrCatchUpLocalDiverged) {
		t.Fatalf("err = %v, want ErrCatchUpLocalDiverged", err)
	}
	if got, _ := gitRevParse(wtDir, "HEAD"); got != local {
		t.Errorf("HEAD moved to %s", got)
	}
}
