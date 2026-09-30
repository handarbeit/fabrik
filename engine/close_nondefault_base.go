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
// touching the caller's already-completed board advance. A failed CloseIssue
// call is durably recorded via fabrik:awaiting-close so settleNonDefaultBaseCloses
// retries it on a later poll (ADR-1097), modeled on the
// fabrik:awaiting-member-close settle pattern (ADR-061).
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
		e.guardDefaultBaseAutoClose(item, prNumber)
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
		// A base: label naming the default branch: GitHub's own Closes #N
		// auto-close is supposed to handle it — guarded like the unlabelled case.
		e.guardDefaultBaseAutoClose(item, prNumber)
		return
	}

	if item.IsClosed {
		return
	}

	e.logf(item.Number, "pr-terminal", "issue #%d base %q ≠ default %q — closing explicitly (merged via PR #%d)\n", item.Number, base, def, prNumber)

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if err := e.client.CloseIssue(owner, repo, item.Number); err != nil {
		if errors.Is(err, gh.ErrNotFound) {
			// Issue no longer exists — already in the desired end state.
			return
		}
		e.markNonDefaultBaseCloseOutstanding(item, owner, repo)
		e.logf(item.Number, "pr-terminal", "explicit close of #%d failed: %v\n", item.Number, err)
		return
	}
	if c := e.cache(); c != nil {
		c.ApplyIssueClosed(boardcache.ItemKey(owner+"/"+repo, item.Number))
	}
}

// guardDefaultBaseAutoClose backstops GitHub's Closes #N auto-close for a PR
// merged into the repository's default branch (#1962). GitHub is supposed to
// close the issue 1–2 s after the merge, but can silently fail to: on
// 2026-09-30 it stopped creating closing-keyword links for new PRs and dropped
// the auto-close even for already-linked ones, with no declared incident
// (liminis-context-graph#629, fabrik-test-alpha#7203). A Done item whose issue
// stays open then never closes.
//
// It does not close immediately: a landing path such as the singleton fast
// path reaches here within the auto-close's own latency, and closing then
// would race GitHub and misreport a miss. Instead a live read — never the board
// snapshot, which predates the merge — decides: already closed means GitHub did
// its job and nothing happens; still open (or unreadable) records the
// fabrik:awaiting-close marker, and settleNonDefaultBaseCloses closes it on a
// later poll — by which point any genuine auto-close has long landed and simply
// clears the marker — through the same retry/escalation path the non-default
// base close uses (ADR-1097).
func (e *Engine) guardDefaultBaseAutoClose(item gh.ProjectItem, prNumber int) {
	if item.IsClosed {
		return
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	iss, err := e.client.FetchIssue(owner, repo, item.Number)
	switch {
	case err == nil && iss != nil && iss.State == "closed":
		return
	case err != nil || iss == nil:
		e.logf(item.Number, "pr-terminal", "could not read #%d's state after merge of PR #%d: %v — scheduling an explicit close check\n", item.Number, prNumber, err)
	default:
		e.logf(item.Number, "pr-terminal", "issue #%d still open after PR #%d merged into the default branch — scheduling an explicit close in case GitHub's auto-close does not fire\n", item.Number, prNumber)
	}
	e.markNonDefaultBaseCloseOutstanding(item, owner, repo)
}
