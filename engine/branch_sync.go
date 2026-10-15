package engine

import (
	"errors"
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
)

// Engine side of the issue-branch sync and verified-lease push (#2295, ADR 2295). The
// WorktreeManager (worktree_sync.go) classifies and reports; it has no item or pause
// capability, so the pause for a diverged branch is applied here.

// ErrPushRefusedDiverged is returned by the push wrappers when the divergence check refused
// the push. It wraps ErrPushRefused, so the consumers that stop the completion chain on a
// zero-ahead refusal stop on this one too.
var ErrPushRefusedDiverged = fmt.Errorf("%w: issue branch has diverged from its remote", ErrPushRefused)

// divergedPauseFragment is the stable prose a divergence comment's body starts with
// (hasPauseComment, #1408 episode dedup). It is distinct from zeroAheadPauseFragment so an
// item that moves between the two shapes still gets the right text.
const divergedPauseFragment = "The issue branch and its remote counterpart have each moved"

// buildBranchDivergedComment names the branch and both heads, says both were left untouched
// and how to resume.
func buildBranchDivergedComment(d *BranchDivergedError) string {
	return fmt.Sprintf(
		"🏭 **Fabrik — issue branch diverged from origin**\n\n"+
			"%s: `%s` holds commits that `origin/%s` lacks, and the remote holds commits the local branch lacks "+
			"(local `%s`, remote `%s`). Fabrik will not guess which side wins.\n\n"+
			"Nothing was rebased, reset, merged or pushed, and no stage was run on it: both sides were left untouched.\n\n"+
			"To resume, reconcile the branch yourself — push a merge or rebase of the two to `%s` — or remove this issue's local worktree and branch so Fabrik starts again from the remote. Then comment on this issue.",
		divergedPauseFragment, d.Branch, d.Branch, d.LocalHead, d.RemoteHead, d.Branch,
	)
}

// pauseForBranchDivergence pauses item for d, once per episode (#1408 dedup). It never holds
// wm.mu: the sync already returned.
func (e *Engine) pauseForBranchDivergence(item gh.ProjectItem, d *BranchDivergedError) {
	e.logf(item.Number, "branch-sync", "REFUSING to continue on %s: local %s and remote %s diverged — both left untouched, pausing\n", d.Branch, d.LocalHead, d.RemoteHead)
	if hasPauseComment(item, divergedPauseFragment) {
		e.reapplyPauseLabels(item)
		return
	}
	e.pauseIssue(item, buildBranchDivergedComment(d), pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
}

// SetBranchSyncDisabledForTest neutralises the R1 sync on every registered worktree manager
// (non-vacuity seam, #2295).
func (e *Engine) SetBranchSyncDisabledForTest(disabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.branchSyncDisabledForTest = disabled
	for _, wm := range e.worktreeManagers {
		wm.branchSyncDisabled.Store(disabled)
	}
}

// SetBareLeasePushForTest restores the pre-#2295 bare --force-with-lease push on every
// registered worktree manager (non-vacuity seam, #2295).
func (e *Engine) SetBareLeasePushForTest(enabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bareLeasePushForTest = enabled
	for _, wm := range e.worktreeManagers {
		wm.bareLeasePush.Store(enabled)
	}
}

// pushVerified is the common tail of the three push wrappers (R4/R5): it pushes the issue
// branch with an explicit lease on a verified remote head. A divergence refuses the push and,
// when pause is true, pauses the item; a cancelled worker's push (pause=false) only logs, as
// the zero-ahead guard does on that path. Other errors (ErrPushUnverified,
// ErrPushLeaseRejected, plain git failures) are returned for the caller to log; none pauses.
func (e *Engine) pushVerified(item gh.ProjectItem, wm *WorktreeManager, pause bool) error {
	baseBranch, err := e.baseBranchForItem(item, wm)
	if err != nil || baseBranch == "" {
		return fmt.Errorf("%w: could not resolve base branch: %v", ErrPushUnverified, err)
	}
	err = wm.PushVerified(item.Number, baseBranch)
	var d *BranchDivergedError
	if !errors.As(err, &d) {
		return err
	}
	if pause {
		e.pauseForBranchDivergence(item, d)
	} else {
		e.logf(item.Number, "branch-sync", "REFUSING to push %s: local %s and remote %s diverged — both left untouched (item already being paused by the interruption)\n", d.Branch, d.LocalHead, d.RemoteHead)
	}
	return fmt.Errorf("%w (local %s, remote %s)", ErrPushRefusedDiverged, shortSHA(d.LocalHead), shortSHA(d.RemoteHead))
}
