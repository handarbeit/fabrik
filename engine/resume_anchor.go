package engine

import (
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// effectiveAnchor returns the instant a label-anchored CI/merge-gate timeout
// measures elapsed time from: the later of appliedAt (when the timeout's anchor
// label was applied) and the item's latest fabrik:paused removal, so paused time
// never counts toward the timeout (#2059 R1 for the CIBackstopTimeout backstop,
// extended to the sibling timeouts by #2064). It is the one definition every
// such site shares: the backstop in settleAwaitingCIScan, the two CIWaitTimeout
// dwells in classifyCIFromMergeableState, the merge-queue stall dwell and
// ConvergenceBudget in checkAutoMergeConvergence.
//
// Callers must first establish that the timeout is already exceeded on the
// cheap label anchor (appliedAt non-zero, time.Since(appliedAt) over the
// timeout) and only then call this, so the event-log read is paid only by items
// already past their timeout (#2064 R4). The comparison operator stays at each
// call site; this returns only a time, always >= appliedAt.
//
// The removal time is read from GitHub's issue event log through
// FetchLabelRemovedAt, never from the record-on-write label cache, so it
// survives restarts and sees removals made in the UI (the same reasoning as
// resumeAuthorised, ADR-1813). A read error or a missing event falls back to
// appliedAt — the direction that still escalates; a timeout must never fail
// open into "never escalate".
//
// window is the calling site's own timeout. A memoised resume still inside that
// window is returned without a read, so a recently resumed item costs one events
// page-through per episode rather than one per poll. The memo stores only the raw
// removal time and is shared across sites, so each call tests it against its own
// window: a resume that is "recent" for a 4h backstop is long past a 30m dwell.
// The memo can only be older than the truth, so it can only produce a skip, never
// an escalation: an escalation always follows a live read.
func (e *Engine) effectiveAnchor(item gh.ProjectItem, owner, repoName, logTag, purpose string, appliedAt time.Time, window time.Duration) time.Time {
	key := ciBackstopKey(itemOwnerRepoString(item, e.defaultRepo()), item.Number)
	if v, ok := e.ciBackstopResumeSeen.Load(key); ok {
		if memo, _ := v.(time.Time); memo.After(appliedAt) && time.Since(memo) < window {
			return memo
		}
	}
	resumedAt, err := e.client.FetchLabelRemovedAt(owner, repoName, item.Number, ciBackstopResumeLabel)
	if err != nil {
		e.logf(item.Number, logTag, "could not read %s removal time for %s (using the label anchor): %v\n", ciBackstopResumeLabel, purpose, err)
		return appliedAt
	}
	if resumedAt.After(appliedAt) {
		e.ciBackstopResumeSeen.Store(key, resumedAt)
		return resumedAt
	}
	return appliedAt
}
