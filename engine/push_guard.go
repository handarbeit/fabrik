package engine

import (
	"errors"
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
)

// Zero-ahead push guard (#2089, ADR 2089, docs/state-machine.md "Zero-ahead push guard").
//
// An existing worktree's local fabrik/issue-N branch is never synced from
// origin/fabrik/issue-N (report #2058), so an out-of-band push to the issue
// branch is invisible to the engine, and the bare --force-with-lease in
// PushBranch compares against a remote-tracking ref that many other paths
// refresh. The worst outcome is a stale local branch that — once rebased onto
// base — is zero commits ahead: pushing it makes the remote issue branch equal to
// base and GitHub closes the PR as having no diff.
//
// This guard is a safety net, not a resync (R3/FR-006): it never fetches the
// issue branch, fast-forwards or resets the local branch, or sets an explicit
// lease. It refuses exactly one shape — literal zero commits ahead of the
// resolved base while an open linked PR exists — and fails open on every
// unreadable input (the log line surfaces it).

// ErrPushRefusedZeroAhead is returned (wrapped) by pushBranchUnlessQueued when
// the zero-ahead guard refused the push and has already paused the item.
var ErrPushRefusedZeroAhead = errors.New("push refused: issue branch has no commits ahead of base while its PR is open")

// zeroAheadPauseFragment is the stable prose a refusal comment's body starts
// with; hasPauseComment (#1408 episode dedup) matches on it.
const zeroAheadPauseFragment = "The local issue branch carries no commits ahead of"

// SetPushZeroAheadGuardDisabledForTest neutralises the zero-ahead guard so a
// test can show its refusal assertions bite (the non-vacuity seam).
func (e *Engine) SetPushZeroAheadGuardDisabledForTest(disabled bool) {
	e.pushGuardDisabled.Store(disabled)
}

// buildZeroAheadPauseComment builds the refusal comment. It names the PR and says
// the remote was left untouched, without asserting a cause: a stale branch and a
// rebase that dropped already-absorbed commits both produce zero ahead.
func buildZeroAheadPauseComment(branch, baseBranch string, prNumber int) string {
	return fmt.Sprintf(
		"🏭 **Fabrik — push refused: local branch has no commits**\n\n"+
			"%s `origin/%s`, and PR #%d is open. Pushing it would replace `%s` on the remote with the base tip, which can discard commits pushed to that branch outside Fabrik and make GitHub close the PR for having no diff.\n\n"+
			"Fabrik did not push: the remote branch was left untouched. Inspect `%s` on the remote, and once it is in the state you want, comment on this issue to resume.",
		zeroAheadPauseFragment, baseBranch, prNumber, branch, branch,
	)
}

// zeroAheadPushRefused reports whether the push of item's issue branch must be
// refused, and — when it is — pauses the item (once per episode). Order, cheapest
// first: resolve the base, count commits ahead locally, and only when the count
// is exactly zero make one live PR read (ADR-957). Every error fails open.
// It never holds wm.mu across an API call or a pause.
func (e *Engine) zeroAheadPushRefused(item gh.ProjectItem, wm *WorktreeManager) bool {
	if e.pushGuardDisabled.Load() {
		return false
	}
	baseBranch, err := e.baseBranchForItem(item, wm)
	if err != nil || baseBranch == "" {
		e.logf(item.Number, "push-guard", "warn: could not resolve base branch (%v) — zero-ahead check skipped, pushing\n", err)
		return false
	}
	ahead, err := wm.CommitsAheadOfRef(item.Number, baseBranch)
	if err != nil {
		e.logf(item.Number, "push-guard", "warn: could not count commits ahead (%v) — zero-ahead check skipped, pushing\n", err)
		return false
	}
	if ahead != 0 {
		return false
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "push-guard", "warn: could not read linked PR (%v) — zero-ahead check skipped, pushing\n", err)
		return false
	}
	if pr == nil || pr.State != "open" || pr.Merged {
		return false
	}

	branch := wm.branchName(item.Number)
	e.logf(item.Number, "push-guard", "REFUSING to push %s: 0 commits ahead of origin/%s while PR #%d is open — remote branch left untouched, pausing\n", branch, baseBranch, pr.Number)
	if hasPauseComment(item, zeroAheadPauseFragment) {
		e.reapplyPauseLabels(item)
		return true
	}
	e.pauseIssue(item, buildZeroAheadPauseComment(branch, baseBranch, pr.Number), pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})
	return true
}
