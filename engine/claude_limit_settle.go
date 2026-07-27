package engine

import (
	"fmt"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// claudeClearLimitLabel is an operator-applied command label, applicable to
// any open board item, that clears an active account-wide Claude usage-limit
// suspension without an engine restart. Mirrors the fabrik:revalidate idiom
// (CLAUDE.md "Labels are state"): a label is read by a per-poll settle scan
// and consumed. See #1183.
const claudeClearLimitLabel = "fabrik:clear-claude-limit"

// settleClaudeLimitClearRequests is the per-poll settle scan for the
// operator-triggered restart-free clear (#1183). It runs unconditionally
// every poll over the raw board snapshot: the suspension this label clears
// is account-wide, not scoped to any one item's stage or column, so there is
// no itemMayNeedWork/itemNeedsWork dispatch relationship to piggyback on —
// same rationale as settleNonDefaultBaseCloses/settleMergeTrainMemberCloses.
//
// Any open item carrying the label is a valid trigger — the label's scope is
// intentionally not restricted to items also carrying fabrik:claude-limit,
// since the suspension it clears is account-wide, not per-issue. The label
// is a one-shot command: every carrying item has it removed in the same
// pass, and there is no retry counter — a failed removal simply leaves the
// label for the next poll to consume again, which just re-runs the
// (idempotent) clear.
func (e *Engine) settleClaudeLimitClearRequests(board *gh.ProjectBoard) {
	var triggered []gh.ProjectItem
	for _, item := range board.Items {
		if item.IsClosed || !hasLabel(item.Labels, claudeClearLimitLabel) {
			continue
		}
		triggered = append(triggered, item)
	}
	if len(triggered) == 0 {
		return
	}
	e.clearClaudeSuspension(fmt.Sprintf("operator requested via %s on issue #%d", claudeClearLimitLabel, triggered[0].Number))
	for _, item := range triggered {
		e.removeLabel(item, claudeClearLimitLabel)
	}
}

// settleClaudeLimitLabelSweep is the per-poll settle scan that clears the
// informational fabrik:claude-limit label account-wide once the underlying
// suspension has lifted (#1183). handleUsageLimitExit (item.go) only clears
// the label per-issue, on that issue's next dispatched invocation — an issue
// that is paused, blocked, or simply far down the queue keeps the label
// indefinitely after the account-wide suspension has already ended, making
// the board read as though a limit is still in force. This is the same
// durable-state leak as the orphaned stage:*:in_progress labels fixed in
// #1135.
//
// The label is purely cosmetic: dispatch is gated by claudeSuspendedUntil,
// not this label (see item.go's claudeSuspendedUntilTime call sites), so a
// failed removal self-heals on the next poll and no retry/escalate
// machinery is needed here — unlike settleMergeTrainMemberCloses/
// settleNonDefaultBaseCloses, which guard an outstanding mutation with real
// consequences.
//
// Scoped to open items only — cleanupClosedIssueTransientLabels already
// sweeps fabrik:claude-limit (and other transient lifecycle labels) from
// closed issues every poll.
func (e *Engine) settleClaudeLimitLabelSweep(board *gh.ProjectBoard) {
	if _, suspended := e.claudeSuspendedUntilTime(time.Now()); suspended {
		return
	}
	for _, item := range board.Items {
		if item.IsClosed || !hasLabel(item.Labels, "fabrik:claude-limit") {
			continue
		}
		e.removeLabel(item, "fabrik:claude-limit")
	}
}
