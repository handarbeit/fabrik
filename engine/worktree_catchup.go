package engine

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Sentinel errors for the merge-train singleton catch-up's worktree primitives
// (#2044, ADR-2044). They split the two ways PrepareCatchUp can decline:
// the member's branch is not ours to touch right now (defer, leave it Queued) versus
// the local branch holds work the remote does not (never touch it, fall back to a trial).
var (
	// ErrCatchUpBusy: the member's issue worktree is in use (uncommitted changes) or the
	// remote branch moved since the batch snapshot. The caller defers.
	ErrCatchUpBusy = errors.New("catch-up deferred: member branch is busy")
	// ErrCatchUpLocalDiverged: the local fabrik/issue-N branch is ahead of, or has
	// diverged from, the remote — it carries unpushed work. The caller must not touch it
	// and falls back to the trial path.
	ErrCatchUpLocalDiverged = errors.New("local member branch is ahead of or diverged from the remote")
)

// PrepareCatchUp readies the member's own issue worktree for a catch-up merge and
// returns its path. It never destroys anything: the worktree must be clean, the remote
// branch must still be exactly expectedHead (the head the batch snapshot — and so the
// pinned-base ancestry check — was taken against), and the local branch must be equal
// to it or strictly behind it (fast-forwarded here). A local branch that is ahead or
// diverged is refused with ErrCatchUpLocalDiverged.
//
// The catch-up deliberately runs in the member's own worktree, not a disposable one:
// a push from a detached worktree would leave the local fabrik/issue-N stale, and a
// later stage's PushBranch (--force-with-lease against the tracking ref this push
// advances) could then overwrite the catch-up commit.
func (wm *WorktreeManager) PrepareCatchUp(issueNumber int, baseBranch, expectedHead string) (string, error) {
	// skipUpdate=true: never rebase the member's branch onto main as a side effect.
	wtDir, err := wm.EnsureWorktree(issueNumber, baseBranch, true)
	if err != nil {
		return "", fmt.Errorf("ensuring member worktree: %w", err)
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()
	branch := wm.branchName(issueNumber)

	if out, err := gitOutputIn(wtDir, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || out != branch {
		return "", fmt.Errorf("%w: worktree is not on %s (HEAD %q)", ErrCatchUpBusy, branch, out)
	}
	status, err := gitOutputIn(wtDir, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("reading worktree status: %w", err)
	}
	if status != "" {
		return "", fmt.Errorf("%w: worktree has uncommitted changes", ErrCatchUpBusy)
	}
	// An in-progress merge/rebase/cherry-pick means a session (or a crash) left the
	// tree mid-operation even though `status` can look empty.
	for _, marker := range []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD"} {
		if _, err := gitOutputIn(wtDir, "rev-parse", "-q", "--verify", marker); err == nil {
			return "", fmt.Errorf("%w: %s present", ErrCatchUpBusy, marker)
		}
	}

	tracking := "refs/remotes/origin/" + branch
	fetch := exec.Command("git", "fetch", "origin", "--", "+refs/heads/"+branch+":"+tracking)
	fetch.Dir = wm.baseDir
	fetch.Env = nonInteractiveGitEnv()
	if out, err := fetch.CombinedOutput(); err != nil {
		return "", fmt.Errorf("fetching %s: %s: %w", branch, strings.TrimSpace(string(out)), err)
	}
	remote, err := gitRevParse(wtDir, tracking)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", tracking, err)
	}
	if remote != expectedHead {
		return "", fmt.Errorf("%w: remote %s is at %s, not the snapshot head %s", ErrCatchUpBusy, branch, remote, expectedHead)
	}

	local, err := gitRevParse(wtDir, "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolving local HEAD: %w", err)
	}
	if local == expectedHead {
		return wtDir, nil
	}
	// Strictly behind (an ancestor of the remote head): safe to fast-forward.
	if isAncestor(wtDir, local, expectedHead) {
		if out, err := gitOutputIn(wtDir, "merge", "--ff-only", expectedHead); err != nil {
			return "", fmt.Errorf("fast-forwarding local %s to %s: %s: %w", branch, expectedHead, out, err)
		}
		return wtDir, nil
	}
	return "", fmt.Errorf("%w: local %s, remote %s", ErrCatchUpLocalDiverged, local, expectedHead)
}

// PushCatchUp pushes the member's worktree branch after a catch-up commit, as a
// compare-and-swap: --force-with-lease=<ref>:<expectedHead> succeeds only if the remote
// branch is still exactly the head the catch-up was built on, so a concurrent push (a
// stage session, a human, a review-fix) is rejected rather than overwritten. The new
// commit descends from expectedHead, so this is always a fast-forward — never a real
// force. On success the remote-tracking ref is advanced to match, so the next stage's
// ordinary PushBranch lease starts from the catch-up commit.
func (wm *WorktreeManager) PushCatchUp(issueNumber int, expectedHead string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wtDir := wm.worktreeDir(issueNumber)
	branch := wm.branchName(issueNumber)
	ref := "refs/heads/" + branch
	cmd := exec.Command("git", "push", "--force-with-lease="+ref+":"+expectedHead, "origin", branch)
	cmd.Dir = wtDir
	cmd.Env = nonInteractiveGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pushing catch-up to %s: %s: %w", branch, strings.TrimSpace(string(out)), err)
	}
	if out, err := gitOutputIn(wtDir, "update-ref", "refs/remotes/origin/"+branch, "HEAD"); err != nil {
		wm.logf(issueNumber, "worktree", "warn: could not advance the remote-tracking ref after the catch-up push: %s\n", out)
	}
	wm.logf(issueNumber, "worktree", "pushed catch-up commit to %s\n", branch)
	return nil
}

// gitOutputIn runs git in dir and returns its trimmed combined output.
func gitOutputIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// isAncestor reports whether ancestor is an ancestor of (or equal to) descendant.
func isAncestor(dir, ancestor, descendant string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = dir
	return cmd.Run() == nil
}
