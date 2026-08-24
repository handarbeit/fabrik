package engine

import (
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
)

// nonDefaultBaseLinkageNoticeLabel marks an issue that has already received the
// one-time explanatory comment naming its PR and explaining that GitHub does
// not create a Development-panel issue↔PR link for a PR that targets a
// non-default base branch (#1649). Unlike nonDefaultBaseAwaitingCloseLabel
// (ADR-1097), this label carries no outstanding-action semantics — once the
// notice has been posted (or attempted), there is nothing left to retry
// toward, so its only job is idempotency: "has this ever been posted."
const nonDefaultBaseLinkageNoticeLabel = "fabrik:nondefault-base-pr-noted"

// settleNonDefaultBaseLinkageNotice is the per-poll scan that posts the
// one-time non-default-base linkage notice (#1649). It runs unconditionally
// every poll over the raw board snapshot, sourced directly from board.Items
// like every other member of the ADR-1270 settle-scan family, independent of
// itemMayNeedWork/itemNeedsWork dispatch — an item is eligible the moment its
// PR is discoverable, regardless of which stage or board column it's in.
func (e *Engine) settleNonDefaultBaseLinkageNotice(board *gh.ProjectBoard) {
	for _, item := range board.Items {
		e.noteNonDefaultBasePRLinkage(item)
	}
}

// noteNonDefaultBasePRLinkage posts a one-time comment on item naming its
// linked PR and explaining that GitHub creates no issue↔PR linkage at all for
// a PR targeting a non-default base branch (not merely no auto-close — see
// #1646/#1649). It is the human-visibility counterpart to
// closeIssueIfNonDefaultBase (ADR-1096), which already handles the engine's
// own explicit-close half of this gap.
//
// Deliberately lighter than closeIssueIfNonDefaultBase's retry/escalation
// machinery (ADR-1097): posting an informational comment is a one-shot side
// effect with no durable outstanding action to retry, so a failed
// postItemComment call (already logged) is not retried — the marker label is
// still added unconditionally afterward, matching handleAPIKeyHelperDetected's
// accepted trade-off. R2's "no duplicates" requirement is satisfied either
// way, since the label — not delivery confirmation — is what prevents a
// duplicate.
func (e *Engine) noteNonDefaultBasePRLinkage(item gh.ProjectItem) {
	if item.IsPR {
		return
	}

	// itemHasBaseLabel is a zero-cost pre-filter (no git/API call): per
	// baseBranchForItem's documented contract, an item with no base: label
	// always resolves to exactly the repository default, so R3's
	// "default-base items are entirely unaffected" is guaranteed without ever
	// touching a WorktreeManager for the overwhelming common case.
	if !itemHasBaseLabel(item) {
		return
	}

	// R1: no PR yet — nothing to name.
	if item.LinkedPRNumber == 0 {
		return
	}

	// R2 idempotency.
	if hasLabel(item.Labels, nonDefaultBaseLinkageNoticeLabel) {
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
		e.logf(item.Number, "warn", "linkage-notice: no WorktreeManager registered for %s — skipping non-default-base linkage notice check for #%d\n", key, item.Number)
		return
	}

	base, err := e.baseBranchForItem(item, wm)
	if err != nil {
		e.logf(item.Number, "warn", "linkage-notice: could not resolve base branch for #%d: %v — skipping non-default-base linkage notice check\n", item.Number, err)
		return
	}
	def, err := wm.DefaultBaseBranch()
	if err != nil {
		e.logf(item.Number, "warn", "linkage-notice: could not resolve default branch for #%d: %v — skipping non-default-base linkage notice check\n", item.Number, err)
		return
	}
	if base == def {
		// Either genuinely default-base, or baseBranchForItem's own
		// remote-fallback already reduced the effective base to default
		// (e.g. the labeled branch doesn't exist on the remote) — either way,
		// GitHub's ordinary linkage applies and R3 says no comment.
		return
	}

	e.logf(item.Number, "linkage-notice", "issue #%d base %q ≠ default %q — posting non-default-base linkage notice (PR #%d)\n", item.Number, base, def, item.LinkedPRNumber)

	comment := fmt.Sprintf(
		"🏭 **Fabrik — non-default-base PR**\n\nPR #%d targets this issue's `base:%s` branch (repository default is `%s`). GitHub does not create a Development-panel issue↔PR link for a PR that targets a non-default branch — closing keywords like `Closes #%d` are silently ignored there, so this issue may look unattached to any PR. This is purely a human-visibility gap: Fabrik's own PR discovery, and the eventual explicit close on merge (see `fabrik:awaiting-close`), are unaffected.",
		item.LinkedPRNumber, base, def, item.Number,
	)
	e.postItemComment(item, comment, false)
	e.addLabel(item, nonDefaultBaseLinkageNoticeLabel)
}
