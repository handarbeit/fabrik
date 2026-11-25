package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	if reason, operation, err := wm.worktreeBusyDetail(wtDir, branch); err != nil {
		return "", err
	} else if reason != "" {
		return "", &CatchUpBusyError{Reason: reason, Worktree: true, Operation: operation}
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
		return "", &CatchUpBusyError{Reason: fmt.Sprintf("remote %s is at %s, not the snapshot head %s", branch, remote, expectedHead)}
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

// CatchUpBusyError is PrepareCatchUp's "defer" error (#2615). It matches ErrCatchUpBusy
// (errors.Is) and says which kind of busy it is: Worktree is true when the member's own
// worktree holds the reason (off-branch, uncommitted tracked or foreign files, an operation
// in progress, a stale ref that could not be removed), false for the remote having moved
// since the snapshot. Only the worktree kind counts toward the bounded deferral — a moved
// remote is a race, not a leftover that nothing will clear.
type CatchUpBusyError struct {
	Reason   string
	Worktree bool
	// Operation is true when the reason is a rebase, merge or cherry-pick really in progress
	// in the worktree. That is real activity, not a leftover: it keeps today's uncharged
	// deferral but never counts toward the bounded deferral (spec FR-007/FR-010).
	Operation bool
}

func (e *CatchUpBusyError) Error() string { return ErrCatchUpBusy.Error() + ": " + e.Reason }

func (e *CatchUpBusyError) Is(target error) bool { return target == ErrCatchUpBusy }

// fabrikScratchPaths lists the directory prefixes (relative to a worktree root) whose
// untracked files are Fabrik's or its workers' own leftovers, never someone's work (#2615).
// The busy check ignores untracked files under them; a tracked change there still counts.
// One list, so a new path is added once. `.fabrik-context/` is written by the engine
// (context files); `.fabrik/scratch/` is where workers put logs and temp output.
var fabrikScratchPaths = []string{".fabrik-context", ".fabrik/scratch"}

// isFabrikScratchPath reports whether the repo-relative path is at or under one of
// fabrikScratchPaths, matched on a directory boundary (".fabrik-contextual/x" is not).
func isFabrikScratchPath(path string) bool {
	path = strings.TrimSuffix(strings.TrimPrefix(path, "./"), "/")
	for _, dir := range fabrikScratchPaths {
		if path == dir || strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
}

// gitPathIn resolves a path inside the worktree's own git directory (correct in a linked
// worktree, where .git is a file) via `git rev-parse --git-path`.
func gitPathIn(wtDir, name string) (string, error) {
	out, err := gitStdoutIn(wtDir, "rev-parse", "--git-path", name)
	if err != nil {
		return "", fmt.Errorf("resolving git path %s: %s: %w", name, out, err)
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(wtDir, out)
	}
	return out, nil
}

// operationInProgress reports whether a rebase is running (rebase-merge/ or rebase-apply/
// in the worktree gitdir) or the index holds unmerged entries. busy names the evidence.
func operationInProgress(wtDir, marker string) (busy bool, why string, err error) {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		p, perr := gitPathIn(wtDir, dir)
		if perr != nil {
			return false, "", perr
		}
		if fi, serr := os.Stat(p); serr == nil && fi.IsDir() {
			return true, dir + "/ present", nil
		}
	}
	if marker == "MERGE_HEAD" || marker == "CHERRY_PICK_HEAD" {
		// A merge or cherry-pick whose conflicts are all resolved and staged, but not yet
		// committed, has neither a rebase directory nor unmerged entries. Git removes
		// MERGE_MSG when the commit is made or the operation aborted, so its presence means
		// the operation is still waiting for its commit — deleting the ref would turn that
		// commit into a plain one and drop the merge parent. REBASE_HEAD is the ref git
		// really leaves behind after a finished rebase, so it is not guarded this way.
		p, perr := gitPathIn(wtDir, "MERGE_MSG")
		if perr != nil {
			return false, "", perr
		}
		if _, serr := os.Stat(p); serr == nil {
			return true, "MERGE_MSG present, commit pending", nil
		}
	}
	out, lerr := gitStdoutIn(wtDir, "ls-files", "-u")
	if lerr != nil {
		return false, "", fmt.Errorf("reading unmerged index entries: %s: %w", out, lerr)
	}
	if out != "" {
		return true, "unmerged index entries", nil
	}
	return false, "", nil
}

// removeStaleOperationRef deletes a leftover MERGE_HEAD/REBASE_HEAD/CHERRY_PICK_HEAD.
// `git update-ref -d` handles the latter two but refuses the MERGE_HEAD pseudoref, so
// that one falls back to removing the file in the worktree's own git directory.
func removeStaleOperationRef(wtDir, ref string) (string, error) {
	out, err := gitOutputIn(wtDir, "update-ref", "-d", ref)
	if _, verr := gitOutputIn(wtDir, "rev-parse", "-q", "--verify", ref); verr != nil {
		return "", nil // gone
	}
	p, perr := gitPathIn(wtDir, ref)
	if perr != nil {
		return perr.Error(), perr
	}
	if rerr := os.Remove(p); rerr != nil {
		if err == nil {
			err = rerr
		}
		return strings.TrimSpace(out + " " + rerr.Error()), err
	}
	return "", nil
}

// worktreeBusyReason reports why the issue worktree must not be mutated right now: it is
// not on the issue branch, has uncommitted changes, or a merge/rebase/cherry-pick is really
// in progress. An empty reason means the tree is clean and idle; err is a failure to read
// the status. Shared by PrepareCatchUp (#2044) and the issue-branch sync (#2295 R2).
//
// #2615: untracked files under fabrikScratchPaths are Fabrik's own leftovers and are not
// "uncommitted changes" (tracked changes are read separately, so a tracked change under
// those paths still counts); and MERGE_HEAD / REBASE_HEAD / CHERRY_PICK_HEAD mean busy only
// when an operation is really in progress — otherwise the stale ref is removed (logged).
func (wm *WorktreeManager) worktreeBusyReason(wtDir, branch string) (string, error) {
	reason, _, err := wm.worktreeBusyDetail(wtDir, branch)
	return reason, err
}

// worktreeBusyDetail is worktreeBusyReason plus whether the reason is an operation
// (rebase, merge, cherry-pick) really in progress, as opposed to a leftover.
func (wm *WorktreeManager) worktreeBusyDetail(wtDir, branch string) (reason string, operation bool, err error) {
	if out, err := gitOutputIn(wtDir, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || out != branch {
		// A rebase in progress detaches HEAD, so this is also how a real rebase shows up:
		// say so, because an operation really in progress is not a leftover.
		inProgress, _, perr := operationInProgress(wtDir, "")
		return fmt.Sprintf("worktree is not on %s (HEAD %q)", branch, out), perr == nil && inProgress, nil
	}
	if wm.scratchExemptDisabled.Load() {
		status, err := gitStdoutIn(wtDir, "status", "--porcelain")
		if err != nil {
			return "", false, fmt.Errorf("reading worktree status: %w", err)
		}
		if status != "" {
			return "worktree has uncommitted changes", false, nil
		}
	} else {
		tracked, err := gitStdoutIn(wtDir, "status", "--porcelain", "--untracked-files=no")
		if err != nil {
			return "", false, fmt.Errorf("reading worktree status: %w", err)
		}
		if tracked != "" {
			return "worktree has uncommitted changes", false, nil
		}
		untracked, err := gitStdoutIn(wtDir, "ls-files", "--others", "--exclude-standard", "-z")
		if err != nil {
			return "", false, fmt.Errorf("listing untracked files: %w", err)
		}
		for _, f := range strings.Split(untracked, "\x00") {
			if f = strings.TrimSpace(f); f != "" && !isFabrikScratchPath(f) {
				return "worktree has uncommitted changes", false, nil
			}
		}
	}
	for _, marker := range []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD"} {
		if _, err := gitOutputIn(wtDir, "rev-parse", "-q", "--verify", marker); err != nil {
			continue
		}
		if wm.staleRefCleanupDisabled.Load() {
			return marker + " present", false, nil
		}
		inProgress, why, err := operationInProgress(wtDir, marker)
		if err != nil {
			return "", false, err
		}
		if inProgress {
			return marker + " present (" + why + ")", true, nil
		}
		// Nothing can resume from this ref: it is a leftover of a finished operation.
		if out, err := removeStaleOperationRef(wtDir, marker); err != nil {
			return fmt.Sprintf("stale %s present and could not be removed: %s", marker, out), false, nil
		}
		wm.logf(0, "worktree", "removed stale %s from %s (no rebase or merge in progress)\n", marker, wtDir)
	}
	return "", false, nil
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

// gitStdoutIn is gitOutputIn for calls whose stdout is parsed (#2615): only stdout is
// returned, so a git warning on stderr (for example "could not open directory ...:
// Permission denied") can never be read as a listed path or a status line. On failure the
// returned text is stdout followed by stderr, for the caller's error message.
func gitStdoutIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(stdout.String() + " " + stderr.String()), err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// isAncestor reports whether ancestor is an ancestor of (or equal to) descendant.
func isAncestor(dir, ancestor, descendant string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = dir
	return cmd.Run() == nil
}
