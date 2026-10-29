package engine

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
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
// diverged is refused with ErrCatchUpLocalDiverged — unless it is a pure rebase of the
// remote (#2150, ADR-2150): the same patches on a newer base, carrying no local-only
// patch and no local-only merge commit. That is the footprint of an engine pre-invocation
// rebase that was never pushed; it holds no work the remote lacks, so it is reset to the
// remote head (the head CI validated) and the local rebase is never pushed.
//
// The catch-up deliberately runs in the member's own worktree, not a disposable one:
// a push from a detached worktree would leave the local fabrik/issue-N stale, and a
// later stage's PushBranch (--force-with-lease against the tracking ref this push
// advances) could then overwrite the catch-up commit.
func (wm *WorktreeManager) PrepareCatchUp(issueNumber int, baseBranch, expectedHead string) (string, error) {
	// skipUpdate=true: never rebase the member's branch onto main as a side effect.
	wtDir, err := wm.EnsureWorktree(issueNumber, baseBranch, true, WithoutIssueBranchSync())
	if err != nil {
		return "", fmt.Errorf("ensuring member worktree: %w", err)
	}

	wm.mu.Lock()
	defer wm.mu.Unlock()
	branch := wm.branchName(issueNumber)

	if reason, err := worktreeBusyReason(wtDir, branch); err != nil {
		return "", err
	} else if reason != "" {
		return "", fmt.Errorf("%w: %s", ErrCatchUpBusy, reason)
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
	if !wm.pureRebaseDisabled.Load() {
		pure, rule, doubt := wm.localIsPureRebase(wtDir, branch, tracking, baseBranch)
		if pure {
			wm.logf(issueNumber, "worktree", "catch-up: local branch is a pure rebase of the remote (rule %s) — resetting to the remote head (local %s, remote %s)\n", rule, local, expectedHead)
			if out, err := gitOutputIn(wtDir, "reset", "--hard", expectedHead); err != nil {
				return "", fmt.Errorf("resetting local %s to %s: %s: %w", branch, expectedHead, out, err)
			}
			return wtDir, nil
		}
		if doubt != "" {
			wm.logf(issueNumber, "worktree", "catch-up: not treating local %s as a pure rebase: %s\n", branch, doubt)
		}
	}
	return "", fmt.Errorf("%w: local %s, remote %s", ErrCatchUpLocalDiverged, local, expectedHead)
}

// localIsPureRebase reports whether the worktree's HEAD differs from remoteRef only by
// being the same patches rebased onto a newer base (#2150): no local-only non-merge patch
// (compared by patch-id) and no local-only merge commit. The second return is a reason
// when the answer is "not pure" because of doubt rather than because of local-only work;
// every error is doubt, so the caller falls back to the trial path, never loses a commit.
//
// Plain `git cherry` is not usable here: base commits that are new to the local branch
// show as `+`, so every rebase onto an advanced base would read as impure. Excluding the
// base's history (`^baseRef`) removes them. Merge commits are invisible to patch-ids, so
// they are counted separately. Called with wm.mu held.
func (wm *WorktreeManager) localIsPureRebase(wtDir, branch, remoteRef, baseBranch string) (pure bool, rule, doubt string) {
	baseRef := "refs/remotes/origin/" + baseBranch
	fetch := exec.Command("git", "fetch", "origin", "--", "+refs/heads/"+baseBranch+":"+baseRef)
	fetch.Dir = wm.baseDir
	fetch.Env = nonInteractiveGitEnv()
	if out, err := fetch.CombinedOutput(); err != nil {
		return false, "", fmt.Sprintf("could not fetch base %s: %s", baseBranch, strings.TrimSpace(string(out)))
	}
	ok, why := holdsNothingBeyond(wtDir, "HEAD", remoteRef, baseRef)
	if ok {
		return true, rulePatchID, ""
	}
	if strings.Contains(why, "failed") {
		return false, "", why
	}
	// Patch-ids differ when a rebase only shifted a commit's diff context (#2517).
	if eq, _ := wm.subsumedByReplay(wtDir, "HEAD", remoteRef, baseRef); eq {
		return true, ruleReplayEquivalent, ""
	}
	return false, "", why
}

// holdsNothingBeyond reports whether subject carries nothing other lacks (#2295, shared
// by the #2150 pure-rebase test and the issue-branch sync): no subject-only non-merge
// patch (compared by patch-id, base history excluded) and no subject-only merge commit.
// The second return is a reason when the answer is "no" because of doubt (a git error)
// or a merge commit rather than because of a subject-only patch. Merge commits are
// invisible to patch-ids, so they are counted separately. Called with wm.mu held.
func holdsNothingBeyond(wtDir, subject, other, baseRef string) (bool, string) {
	patches, err := gitOutputIn(wtDir, "rev-list", "--right-only", "--cherry-pick", "--no-merges", "--count",
		other+"..."+subject, "^"+baseRef)
	if err != nil {
		return false, fmt.Sprintf("patch-id comparison failed: %s", patches)
	}
	if n, convErr := strconv.Atoi(patches); convErr != nil || n != 0 {
		return false, ""
	}
	merges, err := gitOutputIn(wtDir, "rev-list", "--merges", "--count", subject, "^"+other, "^"+baseRef)
	if err != nil {
		return false, fmt.Sprintf("merge-commit count failed: %s", merges)
	}
	if n, convErr := strconv.Atoi(merges); convErr != nil || n != 0 {
		return false, "local-only merge commit"
	}
	return true, ""
}

// worktreeBusyReason reports why the issue worktree must not be mutated right now: it is
// not on the issue branch, has uncommitted changes, or a session (or a crash) left a
// merge/rebase/cherry-pick in progress even though `status` can look empty. An empty
// reason means the tree is clean and idle; err is a failure to read the status.
// Shared by PrepareCatchUp (#2044) and the issue-branch sync (#2295 R2).
func worktreeBusyReason(wtDir, branch string) (string, error) {
	if out, err := gitOutputIn(wtDir, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || out != branch {
		return fmt.Sprintf("worktree is not on %s (HEAD %q)", branch, out), nil
	}
	status, err := gitOutputIn(wtDir, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("reading worktree status: %w", err)
	}
	if status != "" {
		return "worktree has uncommitted changes", nil
	}
	for _, marker := range []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD"} {
		if _, err := gitOutputIn(wtDir, "rev-parse", "-q", "--verify", marker); err == nil {
			return marker + " present", nil
		}
	}
	return "", nil
}

// CatchUpPushError is PushCatchUp's failure: git's combined output is kept raw so the
// rejection can be classified (classifyCatchUpPushRejection, #2065) without parsing the
// formatted message. Error() reads exactly as the flat error it replaced.
type CatchUpPushError struct {
	Branch string
	Output string
	Err    error
}

func (e *CatchUpPushError) Error() string {
	return fmt.Sprintf("pushing catch-up to %s: %s: %v", e.Branch, e.Output, e.Err)
}

func (e *CatchUpPushError) Unwrap() error { return e.Err }

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
		return &CatchUpPushError{Branch: branch, Output: strings.TrimSpace(string(out)), Err: err}
	}
	if out, err := gitOutputIn(wtDir, "update-ref", "refs/remotes/origin/"+branch, "HEAD"); err != nil {
		wm.logf(issueNumber, "worktree", "warn: could not advance the remote-tracking ref after the catch-up push: %s\n", out)
	}
	if head, err := gitRevParse(wtDir, "HEAD"); err == nil {
		wm.setReconciled(issueNumber, head) // the next stage push takes the "remote unchanged" path (#2295)
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
