package engine

import (
	"errors"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
)

// closeIssueIfNonDefaultBase explicitly closes item's GitHub issue when its
// resolved base branch (via the base:<branch> label — see baseBranchForItem)
// differs from the repository's actual default branch. GitHub only honours
// `Closes #N` auto-close when a PR merges into the repository's *default*
// branch (see #1096); on any other base the keyword is inert and nothing
// closes the issue, which stalls dependent issues that unblock on close, not
// on merge.
//
// This is the single shared guard called from both terminal merge-advance
// sites — runValidatePRTerminalAdvance's cruise path and
// advanceConvergedPRToDone's non-train yolo path — so the base != default
// decision and the double-close guard live in exactly one place. Callers are
// responsible for only invoking this on a *confirmed merge* — a PR closed
// without merging must never reach here.
//
// Best-effort and non-blocking: every failure mode (missing WorktreeManager,
// DefaultBaseBranch error, CloseIssue error) is logged and returns without
// touching the caller's already-completed board advance. Durable retry for a
// failed close is explicitly out of scope for this issue (tracked as a
// chained follow-up, modeled on the fabrik:awaiting-member-close settle
// pattern).
func (e *Engine) closeIssueIfNonDefaultBase(item gh.ProjectItem, prNumber int) {
	if item.IsPR {
		return
	}

	// baseBranchForItem's documented contract: an item with no base: label
	// always resolves to exactly the repository default. Checking the label
	// first avoids a WorktreeManager lookup and any git shell-out for the
	// overwhelming common case (no base: label), and — critically — avoids
	// the only path by which this function could reach the panicking
	// e.worktreesFor pattern other callers use, since runValidatePRTerminalAdvance
	// has no guarantee a WorktreeManager is registered for item.Repo yet.
	if !itemHasBaseLabel(item) {
		return
	}

	key := item.Repo
	if key == "" {
		key = e.defaultRepo()
	}
	e.mu.Lock()
	wm, ok := e.worktreeManagers[key]
	e.mu.Unlock()
	if !ok {
		e.logf(item.Number, "warn", "pr-terminal: no WorktreeManager registered for %s — skipping non-default-base close check for #%d\n", key, item.Number)
		return
	}

	base, err := e.baseBranchForItem(item, wm)
	if err != nil {
		e.logf(item.Number, "warn", "pr-terminal: could not resolve base branch for #%d: %v — skipping non-default-base close check\n", item.Number, err)
		return
	}
	def, err := wm.DefaultBaseBranch()
	if err != nil {
		e.logf(item.Number, "warn", "pr-terminal: could not resolve default branch for #%d: %v — skipping non-default-base close check\n", item.Number, err)
		return
	}
	if base == def {
		// The common case: GitHub's own Closes #N auto-close already handles this.
		return
	}

	e.logf(item.Number, "pr-terminal", "issue #%d base %q ≠ default %q — closing explicitly\n", item.Number, base, def)

	if item.IsClosed {
		return
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if err := e.client.CloseIssue(owner, repo, item.Number); err != nil {
		if errors.Is(err, gh.ErrNotFound) {
			// Issue no longer exists — already in the desired end state.
			return
		}
		e.logf(item.Number, "pr-terminal", "explicit close of #%d failed: %v\n", item.Number, err)
		return
	}
	if c := e.cache(); c != nil {
		c.ApplyIssueClosed(boardcache.ItemKey(owner+"/"+repo, item.Number))
	}
}
