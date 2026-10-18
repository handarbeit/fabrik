package engine

import (
	"fmt"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tui"
)

// claudeClearLimitLabel is an operator-applied command label, applicable to
// any open board item, that clears an active account-wide Claude usage-limit
// usage-limit hold without an engine restart. Mirrors the fabrik:revalidate idiom
// (CLAUDE.md "Labels are state"): a label is read by a per-poll settle scan
// and consumed. See #1183.
const claudeClearLimitLabel = "fabrik:clear-claude-limit"

// settleClaudeLimitClearRequests is the per-poll settle scan for the
// operator-triggered restart-free clear (#1183). It runs unconditionally
// every poll over the raw board snapshot: the usage-limit hold this label clears
// is account-wide, not scoped to any one item's stage or column, so there is
// no itemMayNeedWork/itemNeedsWork dispatch relationship to piggyback on —
// same rationale as settleNonDefaultBaseCloses/settleMergeTrainMemberCloses.
//
// Any open item carrying the label is a valid trigger — the label's scope is
// intentionally not restricted to items also carrying fabrik:claude-limit,
// since the usage-limit hold it clears is account-wide, not per-issue. The label
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
	e.clearClaudeUsageLimit(fmt.Sprintf("operator requested via %s on issue #%d", claudeClearLimitLabel, triggered[0].Number))
	for _, item := range triggered {
		e.removeLabel(item, claudeClearLimitLabel)
	}
}

// settleClaudeLimitLabelSweep is the per-poll settle scan that clears the
// informational fabrik:claude-limit label account-wide once the underlying
// usage-limit hold has lifted (#1183). handleUsageLimitExit (item.go) only clears
// the label per-issue, on that issue's next dispatched invocation — an issue
// that is paused, blocked, or simply far down the queue keeps the label
// indefinitely after the account-wide usage-limit hold has already ended, making
// the board read as though a limit is still in force. This is the same
// durable-state leak as the orphaned stage:*:in_progress labels fixed in
// #1135.
//
// Dispatch is gated by claudeUsageLimitedUntil, not this label. Since #2349 the hold
// survives a restart (decision_state.go) and the label is the dedupe key for the
// explanatory comment, so the sweep must not run while the hold's true state is unknown:
// on the first poll after a start with no persisted hold file but labelled items (state from
// before the hold was persisted, or a lost file) the hold is seeded from the labels
// (seedUsageLimitFromLabels) and the sweep is skipped for that poll. A failed removal
// self-heals on the next poll.
//
// Scoped to open items only — cleanupClosedIssueTransientLabels already
// sweeps fabrik:claude-limit (and other transient lifecycle labels) from
// closed issues every poll.
func (e *Engine) settleClaudeLimitLabelSweep(board *gh.ProjectBoard) {
	if e.seedUsageLimitFromLabels(board) {
		return
	}
	if _, usageLimited := e.claudeUsageLimitedUntilTime(e.now()); usageLimited {
		return
	}
	for _, item := range board.Items {
		if item.IsClosed || !hasLabel(item.Labels, "fabrik:claude-limit") {
			continue
		}
		e.removeLabel(item, "fabrik:claude-limit")
	}
}

// usageLimitSeedMaxReads bounds the label-time reads the one-shot seeding makes.
const usageLimitSeedMaxReads = 5

// seedUsageLimitFromLabels runs once, on the first poll with a board (#2349). When
// persistence is on, no usable hold file existed at startup and open items carry
// fabrik:claude-limit, the account may still be limited: it seeds a hold lasting until the
// latest label-applied time plus the fallback backoff (bounded by the 8-day ceiling; a read
// error counts as "applied now", failing closed), persists it, and reports true so the
// sweep skips this poll. A usable file at startup is authoritative and nothing is seeded.
func (e *Engine) seedUsageLimitFromLabels(board *gh.ProjectBoard) bool {
	if e.usageLimitSeedChecked {
		return false
	}
	e.usageLimitSeedChecked = true
	if e.decisionStore == nil || e.usageLimitFileAtStart {
		return false
	}
	if _, active := e.claudeUsageLimitedUntilTime(e.now()); active {
		return false
	}
	var labelled []gh.ProjectItem
	for _, item := range board.Items {
		if !item.IsClosed && hasLabel(item.Labels, "fabrik:claude-limit") {
			labelled = append(labelled, item)
		}
	}
	if len(labelled) == 0 {
		return false
	}
	now := e.now()
	var latest time.Time
	for i, item := range labelled {
		if i >= usageLimitSeedMaxReads {
			break
		}
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		at, err := e.labelAppliedAt(item, owner, repo, "fabrik:claude-limit")
		if err != nil || at.IsZero() {
			at = now
		}
		if at.After(latest) {
			latest = at
		}
	}
	deadline := latest.Add(claudeUsageLimitFallbackBackoff)
	if ceiling := now.Add(usageLimitMaxResetHorizon); deadline.After(ceiling) {
		deadline = ceiling
	}
	if !now.Before(deadline) {
		return false // the evidence says the fallback window has already passed
	}
	e.claudeUsageLimitMu.Lock()
	e.claudeUsageLimitedUntil = deadline
	e.claudeUsageLimitMu.Unlock()
	e.persistUsageLimit()
	e.logf(0, "claude-limit", "%d item(s) carry fabrik:claude-limit with no persisted hold; seeding a Claude dispatch hold until %s and deferring the label sweep\n", len(labelled), deadline.Format(time.RFC3339))
	e.emitStructural(tui.ClaudeUsageLimitAlertEvent{UsageLimited: true, Reset: deadline})
	e.channelNudge()
	return true
}
