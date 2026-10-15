package engine

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Issue-branch sync and verified-lease push (#2295, ADR 2295, docs/state-machine.md
// "Zero-ahead push guard").
//
// An existing worktree's local fabrik/issue-N used to be rebased onto base and pushed
// without ever looking at origin/fabrik/issue-N (report #2058), behind a bare
// --force-with-lease that compares against a remote-tracking ref many other paths
// refresh. Two halves close that:
//
//   - syncIssueBranch (R1-R3) runs inside EnsureWorktree before the rebase: it
//     classifies the local branch against the freshly fetched remote head and
//     fast-forwards / resets / keeps / reports divergence.
//   - PushVerified (R4) replaces the bare lease with an explicit
//     --force-with-lease=<ref>:<sha> on a remote head the engine has verified
//     against the local branch.
//
// The head the engine last reconciled the branch against is kept in memory
// (WorktreeManager.reconciled, guarded by wm.mu). After a restart it is empty and the
// first push takes the always-correct fetch-and-classify path.

// SyncOutcome says what the pre-work sync did.
type SyncOutcome string

const (
	// SyncSkipped: nothing was done — sync disabled, remote absent, tree busy or doubt
	// (fetch/git error). Reason says which.
	SyncSkipped SyncOutcome = "skipped"
	// SyncEqual: local and remote heads are identical.
	SyncEqual SyncOutcome = "equal"
	// SyncFastForwarded: local was strictly behind the remote and was fast-forwarded.
	SyncFastForwarded SyncOutcome = "fast-forwarded"
	// SyncReset: local held nothing the remote lacks (a pure rebase, or a remote rebased
	// onto a newer base) and was reset to the remote head.
	SyncReset SyncOutcome = "reset"
	// SyncKeptLocal: the remote holds nothing local lacks (unpushed local work); local kept.
	SyncKeptLocal SyncOutcome = "kept-local"
	// SyncDiverged: both sides hold something the other lacks. EnsureWorktree returns a
	// *BranchDivergedError.
	SyncDiverged SyncOutcome = "diverged"
)

// SyncReport is filled by EnsureWorktree when WithSyncReport is passed (R6).
type SyncReport struct {
	Outcome SyncOutcome
	Reason  string
	// PreRebaseHead is the worktree HEAD after the sync and before the engine's rebase onto
	// base: the head a no-commit comment pass must restore to (#2150). Empty when the
	// worktree did not exist yet or EnsureWorktree skipped its rebase path.
	PreRebaseHead string
}

// ErrBranchDiverged matches a *BranchDivergedError with errors.Is.
var ErrBranchDiverged = errors.New("issue branch has diverged from its remote")

// BranchDivergedError reports that the local issue branch and its remote counterpart each
// hold something the other lacks. Nothing was rebased, reset or pushed.
type BranchDivergedError struct {
	Branch     string
	LocalHead  string
	RemoteHead string
}

func (e *BranchDivergedError) Error() string {
	return fmt.Sprintf("%s: local %s, remote %s", ErrBranchDiverged.Error(), e.Branch+"@"+shortSHA(e.LocalHead), shortSHA(e.RemoteHead))
}

func (e *BranchDivergedError) Is(target error) bool { return target == ErrBranchDiverged }

// Push-time outcomes that are not divergence. Neither pauses the item; the next poll retries.
var (
	// ErrPushUnverified: the remote head could not be read or classified (network or git
	// error). Nothing was pushed (fail closed).
	ErrPushUnverified = errors.New("push not attempted: remote branch head could not be verified")
	// ErrPushLeaseRejected: the remote moved after verification, twice. Nothing was overwritten.
	ErrPushLeaseRejected = errors.New("push rejected: remote branch moved after verification")
)

// WithSyncReport has EnsureWorktree fill r with the sync outcome and the post-sync head (R6).
func WithSyncReport(r *SyncReport) EnsureWorktreeOption {
	return func(o *ensureOpts) { o.syncReport = r }
}

// WithoutIssueBranchSync skips the R1 sync for this call. PrepareCatchUp passes it so its
// four outcomes (busy, remote moved, diverged, pure rebase) stay exactly its own (R7), and
// the engine passes it for an item whose PR sits in a merge queue (no push can happen).
func WithoutIssueBranchSync() EnsureWorktreeOption {
	return func(o *ensureOpts) { o.noSync = true }
}

// SetBranchSyncDisabledForTest neutralises the R1 sync (non-vacuity seam, per manager).
func (wm *WorktreeManager) SetBranchSyncDisabledForTest(disabled bool) {
	wm.branchSyncDisabled.Store(disabled)
}

// SetBareLeasePushForTest restores the pre-#2295 bare --force-with-lease push (non-vacuity seam).
func (wm *WorktreeManager) SetBareLeasePushForTest(enabled bool) {
	wm.bareLeasePush.Store(enabled)
}

// SetPrePushHookForTest installs fn, called after PushVerified has verified the remote head
// and before it pushes — the window the lease-race test moves the remote in. Set it before
// any concurrent use.
func (wm *WorktreeManager) SetPrePushHookForTest(fn func()) { wm.prePushHook = fn }

// ReconciledHead returns the remote head the engine last reconciled issue N's branch against.
func (wm *WorktreeManager) ReconciledHead(issueNumber int) string {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	return wm.reconciled[issueNumber]
}

// setReconciled records (sha != "") or forgets the reconciled head. Called with wm.mu held.
func (wm *WorktreeManager) setReconciled(issueNumber int, sha string) {
	if sha == "" {
		delete(wm.reconciled, issueNumber)
		return
	}
	if wm.reconciled == nil {
		wm.reconciled = make(map[int]string)
	}
	wm.reconciled[issueNumber] = sha
}

// remoteHead reads the branch head on origin with `git ls-remote`. present=false with a nil
// error is a positive "the ref does not exist" (success and no matching line); any failure
// is err and must never be read as absence (R2). Called with wm.mu held.
func (wm *WorktreeManager) remoteHead(branch string) (sha string, present bool, err error) {
	ref := "refs/heads/" + branch
	cmd := exec.Command("git", "ls-remote", "origin", "--", ref)
	cmd.Dir = wm.baseDir
	cmd.Env = nonInteractiveGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("reading remote head of %s: %w", branch, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref {
			return fields[0], true, nil
		}
	}
	return "", false, nil
}

// fetchBranchAndBase fetches the issue branch and the base branch into their tracking refs in
// one round trip. The base must be fresh before classifying: a stale base ref would show
// commits that arrived through base as branch-only patches. Called with wm.mu held.
func (wm *WorktreeManager) fetchBranchAndBase(branch, baseBranch string) error {
	cmd := exec.Command("git", "fetch", "origin", "--",
		"+refs/heads/"+branch+":refs/remotes/origin/"+branch,
		"+refs/heads/"+baseBranch+":refs/remotes/origin/"+baseBranch)
	cmd.Dir = wm.baseDir
	cmd.Env = nonInteractiveGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetching %s and %s: %s: %w", branch, baseBranch, strings.TrimSpace(string(out)), err)
	}
	return nil
}

type branchRelation int

const (
	relEqual          branchRelation = iota
	relLocalBehind                   // local is an ancestor of the remote
	relLocalSubsumed                 // local holds nothing the remote lacks
	relRemoteSubsumed                // remote holds nothing local lacks
	relDiverged
)

// classifyBranch relates the worktree's HEAD to remoteRef, in the order the spec fixes
// (a pure unpushed rebase satisfies both subsumption tests, so reset is tested first):
// equal, strictly behind, local-subsumed, remote-subsumed, diverged. doubt is non-empty
// (and the relation meaningless) when a git error prevented an answer. Called with wm.mu held.
func (wm *WorktreeManager) classifyBranch(wtDir, remoteRef, baseBranch string) (rel branchRelation, doubt string) {
	local, err := gitRevParse(wtDir, "HEAD")
	if err != nil {
		return 0, fmt.Sprintf("resolving local HEAD: %v", err)
	}
	remote, err := gitRevParse(wtDir, remoteRef)
	if err != nil {
		return 0, fmt.Sprintf("resolving %s: %v", remoteRef, err)
	}
	if local == remote {
		return relEqual, ""
	}
	if isAncestor(wtDir, local, remote) {
		return relLocalBehind, ""
	}
	baseRef := "refs/remotes/origin/" + baseBranch
	localNothing, why := holdsNothingBeyond(wtDir, "HEAD", remoteRef, baseRef)
	if strings.Contains(why, "failed") {
		return 0, why
	}
	if localNothing {
		return relLocalSubsumed, ""
	}
	remoteNothing, why := holdsNothingBeyond(wtDir, remoteRef, "HEAD", baseRef)
	if strings.Contains(why, "failed") {
		return 0, why
	}
	if remoteNothing {
		return relRemoteSubsumed, ""
	}
	return relDiverged, ""
}

// syncIssueBranch is R1-R3: called by EnsureWorktree under wm.mu for an existing worktree
// that is on its issue branch, before any rebase onto base. It never pushes. Every doubt
// (disabled, busy tree, ls-remote or fetch error) skips the sync with a logged reason and
// changes nothing; only a diverged branch is an error.
func (wm *WorktreeManager) syncIssueBranch(wtDir, baseBranch string, issueNumber int) (SyncReport, error) {
	skip := func(reason string) (SyncReport, error) {
		wm.logf(issueNumber, "worktree", "issue-branch sync skipped: %s\n", reason)
		return SyncReport{Outcome: SyncSkipped, Reason: reason}, nil
	}
	if wm.branchSyncDisabled.Load() {
		return SyncReport{Outcome: SyncSkipped, Reason: "sync disabled"}, nil
	}
	branch := wm.branchName(issueNumber)
	busy, err := worktreeBusyReason(wtDir, branch)
	if err != nil {
		return skip(err.Error())
	}
	if busy != "" {
		return skip(busy)
	}
	remote, present, err := wm.remoteHead(branch)
	if err != nil {
		return skip(err.Error())
	}
	if !present {
		wm.setReconciled(issueNumber, "")
		return SyncReport{Outcome: SyncSkipped, Reason: "remote branch absent"}, nil
	}
	local, err := gitRevParse(wtDir, "HEAD")
	if err != nil {
		return skip(fmt.Sprintf("resolving local HEAD: %v", err))
	}
	if local == remote {
		wm.setReconciled(issueNumber, remote)
		return SyncReport{Outcome: SyncEqual}, nil
	}
	if err := wm.fetchBranchAndBase(branch, baseBranch); err != nil {
		return skip(err.Error())
	}
	tracking := "refs/remotes/origin/" + branch
	fetched, err := gitRevParse(wtDir, tracking)
	if err != nil {
		return skip(fmt.Sprintf("resolving %s: %v", tracking, err))
	}
	rel, doubt := wm.classifyBranch(wtDir, tracking, baseBranch)
	if doubt != "" {
		return skip(doubt)
	}
	switch rel {
	case relEqual:
		wm.setReconciled(issueNumber, fetched)
		return SyncReport{Outcome: SyncEqual}, nil
	case relLocalBehind:
		if out, err := gitOutputIn(wtDir, "merge", "--ff-only", fetched); err != nil {
			return skip(fmt.Sprintf("fast-forwarding to %s: %s", fetched, out))
		}
		wm.logf(issueNumber, "worktree", "local %s was behind origin — fast-forwarded %s to %s\n", branch, shortSHA(local), shortSHA(fetched))
		wm.setReconciled(issueNumber, fetched)
		return SyncReport{Outcome: SyncFastForwarded}, nil
	case relLocalSubsumed:
		if out, err := gitOutputIn(wtDir, "reset", "--hard", fetched); err != nil {
			return skip(fmt.Sprintf("resetting to %s: %s", fetched, out))
		}
		wm.logf(issueNumber, "worktree", "local %s holds nothing origin lacks — reset %s to the remote head %s\n", branch, shortSHA(local), shortSHA(fetched))
		wm.setReconciled(issueNumber, fetched)
		return SyncReport{Outcome: SyncReset}, nil
	case relRemoteSubsumed:
		wm.logf(issueNumber, "worktree", "local %s holds unpushed work and origin holds nothing it lacks — keeping local (%s, remote %s)\n", branch, shortSHA(local), shortSHA(fetched))
		wm.setReconciled(issueNumber, fetched)
		return SyncReport{Outcome: SyncKeptLocal}, nil
	}
	wm.logf(issueNumber, "worktree", "REFUSING to work on %s: local %s and remote %s each hold commits the other lacks — nothing rebased, reset or pushed\n", branch, shortSHA(local), shortSHA(fetched))
	return SyncReport{Outcome: SyncDiverged}, &BranchDivergedError{Branch: branch, LocalHead: local, RemoteHead: fetched}
}

// PushVerified pushes the issue's worktree branch with an explicit lease on a remote head
// verified against the local branch (R4), replacing the bare --force-with-lease whose
// tracking ref any fetch refreshes. All git work runs under wm.mu; it makes no GitHub call.
//
//   - remote absent: push with a lease requiring the ref not to exist.
//   - remote == the head last reconciled: the local branch is authoritative; lease on it.
//   - otherwise (moved, or nothing recorded — e.g. after a restart): fetch and classify;
//     equal / behind / local-subsumed publish nothing (a push would rewind the remote) and
//     return nil; remote-subsumed pushes with the lease on the fetched head; diverged
//     returns a *BranchDivergedError and pushes nothing.
//
// A rejected lease re-verifies once; the second rejection is ErrPushLeaseRejected. An
// unreadable remote is ErrPushUnverified (fail closed). On success the pushed SHA is recorded
// and the tracking ref advanced, as PushCatchUp does.
func (wm *WorktreeManager) PushVerified(issueNumber int, baseBranch string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	if wm.bareLeasePush.Load() {
		return wm.pushBareLocked(issueNumber)
	}
	wtDir := wm.worktreeDir(issueNumber)
	branch := wm.branchName(issueNumber)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		lease, skip, err := wm.verifyForPush(wtDir, branch, baseBranch, issueNumber)
		if err != nil {
			return err
		}
		if skip {
			return nil
		}
		if wm.prePushHook != nil {
			wm.prePushHook()
		}
		cmd := exec.Command("git", "push", "--force-with-lease=refs/heads/"+branch+":"+lease, "-u", "origin", branch)
		cmd.Dir = wtDir
		cmd.Env = nonInteractiveGitEnv()
		out, pushErr := cmd.CombinedOutput()
		if pushErr == nil {
			if head, err := gitRevParse(wtDir, "HEAD"); err == nil {
				wm.setReconciled(issueNumber, head)
				if o, err := gitOutputIn(wtDir, "update-ref", "refs/remotes/origin/"+branch, head); err != nil {
					wm.logf(issueNumber, "worktree", "warn: could not advance the remote-tracking ref after the push: %s\n", o)
				}
			}
			wm.logf(issueNumber, "worktree", "pushed %s to origin\n", branch)
			return nil
		}
		text := strings.TrimSpace(string(out))
		if !strings.Contains(text, "stale info") && !strings.Contains(text, "rejected") {
			return fmt.Errorf("pushing branch %s: %s: %w", branch, text, pushErr)
		}
		lastErr = fmt.Errorf("%w: pushing branch %s: %s", ErrPushLeaseRejected, branch, text)
		wm.logf(issueNumber, "worktree", "push of %s rejected — the remote moved after verification (attempt %d)\n", branch, attempt+1)
	}
	return lastErr
}

// verifyForPush decides what PushVerified does: the lease value to push with, or skip=true when
// there is nothing to publish. A divergence is returned as *BranchDivergedError. wm.mu held.
func (wm *WorktreeManager) verifyForPush(wtDir, branch, baseBranch string, issueNumber int) (lease string, skip bool, err error) {
	remote, present, err := wm.remoteHead(branch)
	if err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrPushUnverified, err)
	}
	if !present {
		return "", false, nil // lease "ref:" — the ref must not exist
	}
	if wm.reconciled[issueNumber] == remote {
		return remote, false, nil
	}
	local, err := gitRevParse(wtDir, "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("%w: resolving local HEAD: %v", ErrPushUnverified, err)
	}
	if local == remote {
		wm.setReconciled(issueNumber, remote)
		return "", true, nil
	}
	if err := wm.fetchBranchAndBase(branch, baseBranch); err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrPushUnverified, err)
	}
	tracking := "refs/remotes/origin/" + branch
	fetched, err := gitRevParse(wtDir, tracking)
	if err != nil {
		return "", false, fmt.Errorf("%w: resolving %s: %v", ErrPushUnverified, tracking, err)
	}
	rel, doubt := wm.classifyBranch(wtDir, tracking, baseBranch)
	if doubt != "" {
		return "", false, fmt.Errorf("%w: %s", ErrPushUnverified, doubt)
	}
	switch rel {
	case relEqual:
		wm.setReconciled(issueNumber, fetched)
		return "", true, nil
	case relLocalBehind, relLocalSubsumed:
		wm.logf(issueNumber, "worktree", "nothing to publish: local %s holds nothing origin lacks (local %s, remote %s) — push skipped\n", branch, shortSHA(local), shortSHA(fetched))
		return "", true, nil
	case relRemoteSubsumed:
		return fetched, false, nil
	}
	return "", false, &BranchDivergedError{Branch: branch, LocalHead: local, RemoteHead: fetched}
}
