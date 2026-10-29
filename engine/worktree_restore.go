package engine

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// RestoreUnpushedRebase undoes the engine's own pre-invocation rebase (#2150, ADR-2150)
// after a comment-processing pass that made no commit. updateWorktreeFromMain rebases
// the issue branch onto origin/<base> before Claude runs; when nothing is committed
// nothing is pushed either, so the local branch is left a rebased copy of the remote one
// (same patches, newer base, new SHAs) — diverged, with no work on either side the other
// lacks, and a head nobody has CI for.
//
// It resets the branch to preRebaseSHA (the head captured before the rebase) and returns
// true only when every one of these holds; on any failure or doubt it leaves the branch
// alone and returns false with the reason, for the caller to log:
//
//   - the worktree is on the issue branch and HEAD is still exactly postRebaseSHA (no
//     Claude commit, reset or amend since);
//   - the tree is clean and no merge/rebase/cherry-pick is in progress;
//   - a fresh fetch of the issue branch succeeds and postRebaseSHA is not already on it
//     (a pushed rebase must not be un-done — that would manufacture the divergence);
//   - post carries no local-only patch relative to pre (patch-id comparison with the
//     base's history excluded), no merge commit, so nothing but the rebase is discarded.
//     When the patch-ids differ only because the rebase shifted a commit's diff context,
//     the same holds if replaying pre's net change onto post's base gives post's tree
//     (replayEquivalent, #2517, ADR 2517).
func (wm *WorktreeManager) RestoreUnpushedRebase(issueNumber int, baseBranch, preRebaseSHA, postRebaseSHA string) (bool, string) {
	if wm.restoreRebaseDisabled.Load() {
		return false, "restore disabled"
	}
	if preRebaseSHA == "" || postRebaseSHA == "" || preRebaseSHA == postRebaseSHA {
		return false, ""
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()
	wtDir := wm.worktreeDir(issueNumber)
	branch := wm.branchName(issueNumber)

	if out, err := gitOutputIn(wtDir, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || out != branch {
		return false, fmt.Sprintf("worktree is not on %s (HEAD %q)", branch, out)
	}
	head, err := gitRevParse(wtDir, "HEAD")
	if err != nil || head != postRebaseSHA {
		return false, fmt.Sprintf("HEAD %s is not the post-rebase head %s", head, postRebaseSHA)
	}
	if dirty, err := isWorkingTreeDirty(wtDir); err != nil || dirty {
		return false, "worktree has uncommitted changes"
	}
	for _, marker := range []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD"} {
		if _, err := gitOutputIn(wtDir, "rev-parse", "-q", "--verify", marker); err == nil {
			return false, marker + " present"
		}
	}
	if _, err := gitOutputIn(wtDir, "cat-file", "-e", preRebaseSHA+"^{commit}"); err != nil {
		return false, "pre-rebase head " + preRebaseSHA + " is not a commit"
	}

	// A fresh view of the remote branch: updateWorktreeFromMain fetches only the base.
	tracking := "refs/remotes/origin/" + branch
	fetch := exec.Command("git", "fetch", "origin", "--", "+refs/heads/"+branch+":"+tracking)
	fetch.Dir = wm.baseDir
	fetch.Env = nonInteractiveGitEnv()
	if out, err := fetch.CombinedOutput(); err != nil {
		return false, fmt.Sprintf("could not fetch %s: %s", branch, strings.TrimSpace(string(out)))
	}
	if isAncestor(wtDir, postRebaseSHA, tracking) {
		return false, "the rebased head is already on the remote"
	}

	baseRef := "refs/remotes/origin/" + baseBranch
	patches, err := gitOutputIn(wtDir, "rev-list", "--right-only", "--cherry-pick", "--no-merges", "--count",
		preRebaseSHA+"..."+postRebaseSHA, "^"+baseRef)
	if err != nil {
		return false, "patch-id comparison failed: " + patches
	}
	rule := rulePatchID
	if n, convErr := strconv.Atoi(patches); convErr != nil || n != 0 {
		// A clean rebase that only shifted a commit's diff context changes its patch-id
		// (#2517): fall back to comparing content. A conflict-edited rebase or an extra
		// commit changes the tree and still declines.
		if wm.restoreContentCheckDisabled.Load() {
			return false, "the branch carries a commit that was not on the pre-rebase head"
		}
		if ok, _ := replayEquivalent(wtDir, postRebaseSHA, preRebaseSHA, baseRef); !ok {
			return false, "the branch carries a commit that was not on the pre-rebase head"
		}
		rule = ruleReplayEquivalent
	}
	merges, err := gitOutputIn(wtDir, "rev-list", "--merges", "--count", postRebaseSHA, "^"+preRebaseSHA, "^"+baseRef)
	if err != nil {
		return false, "merge-commit count failed: " + merges
	}
	if n, convErr := strconv.Atoi(merges); convErr != nil || n != 0 {
		return false, "the branch carries a merge commit that was not on the pre-rebase head"
	}

	if out, err := gitOutputIn(wtDir, "reset", "--hard", preRebaseSHA); err != nil {
		return false, fmt.Sprintf("reset failed: %s", out)
	}
	if rule != rulePatchID {
		wm.logf(issueNumber, "worktree", "restore matched rule %s\n", rule)
	}
	return true, ""
}

// SetCommentRebaseRestoreDisabledForTest neutralises the post-invocation restore on
// every registered worktree manager (the R1 non-vacuity seam, #2150).
func (e *Engine) SetCommentRebaseRestoreDisabledForTest(disabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.restoreRebaseDisabledForTest = disabled
	for _, wm := range e.worktreeManagers {
		wm.restoreRebaseDisabled.Store(disabled)
	}
}

// SetCatchUpPureRebaseDisabledForTest neutralises PrepareCatchUp's pure-rebase tolerance
// on every registered worktree manager (the R2 non-vacuity seam, #2150).
func (e *Engine) SetCatchUpPureRebaseDisabledForTest(disabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pureRebaseDisabledForTest = disabled
	for _, wm := range e.worktreeManagers {
		wm.pureRebaseDisabled.Store(disabled)
	}
}
