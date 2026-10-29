package engine

import (
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// The review-wait anchor (#2467, ADR 2467).
//
// Every comparison here is against the host clock (time.Now), exactly like the
// label-time comparisons it extends: both sides are GitHub-sourced instants
// measured against one wall clock, and the sim drives this gate by seeding old
// GitHub times rather than by advancing the engine clock (see
// tests/sim/timeout_test.go), so an e.now() here would silently change what
// every one of those scenarios measures.
//
// The review-wait timeout used to be measured from the first application of
// fabrik:awaiting-review. That label stays on through every fix round, so a push
// asking the reviewer for a fresh look never restarted the clock and the timeout
// measured the length of the fix loop, not the reviewer's silence on the head
// they were asked to review. The anchor is now the later of the label's applied
// time and the arrival of the PR's current head.
//
// Head arrival is the head commit's GitHub-reported committer date
// (gh.Client.FetchCommitTime). GitHub exposes no reliable per-commit push time,
// so this is the one fact that is GitHub-sourced, available on poll-only
// daemons and without CI, and re-readable after a restart (R2). It is
// author-controlled and a rebase rewrites it, so it is bounded:
//
//   - a date more than headAnchorSkew in the future is ignored (label-only
//     anchor), a smaller lead is clamped to now, so the anchor cannot slide
//     forward for ever;
//   - it can only extend a wait past the label anchor (a max), never shorten it;
//   - any read failure falls back to the label anchor (FR-003, FR-007).

// headAnchorSkew is how far ahead of the engine's clock a commit date may be
// before it is distrusted.
const headAnchorSkew = 2 * time.Minute

// headAnchorFailureTTL is how long a failed commit-time read is remembered, so a
// commit GitHub cannot answer for costs one request a minute, not one per poll.
const headAnchorFailureTTL = time.Minute

type headTimeKey struct{ repo, sha string }

type headTimeEntry struct {
	at       time.Time // zero for a remembered failure
	failedAt time.Time
}

// headTimeCache memoises commit dates per SHA. A commit's date is immutable, so
// a success is kept for the process lifetime; a restart costs one re-read.
type headTimeCache struct {
	mu sync.Mutex
	m  map[headTimeKey]headTimeEntry
}

// reviewWaitHeadSHA resolves the PR head the wait is measured against. live
// reads it from GitHub (the poll-start snapshot can predate a push that just
// happened); otherwise the snapshot is used and only an empty one reads live.
func (e *Engine) reviewWaitHeadSHA(owner, repo string, item gh.ProjectItem, prNumber int, live bool) string {
	if !live && item.LinkedPRHeadSHA != "" {
		return item.LinkedPRHeadSHA
	}
	if prNumber > 0 {
		// live-read-audited: head of the PR being waited on; used once to find the head's commit time, never applied to the Store
		details, err := e.client.FetchPRDetails(owner, repo, prNumber)
		if err == nil && details != nil && details.HeadSHA != "" {
			return details.HeadSHA
		}
		if err != nil {
			e.logf(item.Number, "warn", "review wait: could not read PR #%d head: %v\n", prNumber, err)
		}
	}
	return item.LinkedPRHeadSHA
}

// headArrivalTime returns the bounded arrival time of the PR's current head, or
// ok=false when none can be determined (neutralised seam, no head, a read
// failure, a future-dated commit) — the caller then uses the label anchor.
func (e *Engine) headArrivalTime(owner, repo string, item gh.ProjectItem, prNumber int, live bool) (time.Time, bool) {
	if e.reviewWaitHeadAnchorOff.Load() {
		return time.Time{}, false
	}
	sha := e.reviewWaitHeadSHA(owner, repo, item, prNumber, live)
	if sha == "" {
		return time.Time{}, false
	}
	now := time.Now()
	at, ok := e.commitTime(owner, repo, sha, item.Number, now)
	if !ok {
		return time.Time{}, false
	}
	if at.After(now.Add(headAnchorSkew)) {
		e.logf(item.Number, "warn", "review wait: head %s is dated %s, beyond the %s clock-skew allowance — using the label time\n", shortSHA(sha), at.UTC().Format(time.RFC3339), headAnchorSkew)
		return time.Time{}, false
	}
	if at.After(now) {
		at = now
	}
	return at, true
}

func (e *Engine) commitTime(owner, repo, sha string, issueNumber int, now time.Time) (time.Time, bool) {
	key := headTimeKey{repo: owner + "/" + repo, sha: sha}
	c := &e.headTimes
	c.mu.Lock()
	if ent, ok := c.m[key]; ok {
		if !ent.at.IsZero() {
			c.mu.Unlock()
			return ent.at, true
		}
		if now.Sub(ent.failedAt) < headAnchorFailureTTL {
			c.mu.Unlock()
			return time.Time{}, false
		}
	}
	c.mu.Unlock()

	// live-read-audited: committer date of an immutable commit, memoised per SHA; not a Store fact
	at, err := e.client.FetchCommitTime(owner, repo, sha)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[headTimeKey]headTimeEntry)
	}
	if err != nil || at.IsZero() {
		c.m[key] = headTimeEntry{failedAt: now}
		if err != nil {
			e.logf(issueNumber, "warn", "review wait: could not read commit time for %s: %v — using the label time\n", shortSHA(sha), err)
		}
		return time.Time{}, false
	}
	c.m[key] = headTimeEntry{at: at}
	return at, true
}

// headRestartsWait reports whether the wait has been restarted by a head that
// arrived after the label and whose own window has not yet elapsed. It reads
// only when the label-only timeout would otherwise fire, so an idle wait pays
// nothing.
func (e *Engine) headRestartsWait(owner, repo string, item gh.ProjectItem, prNumber int, labelAt time.Time, timeout time.Duration) bool {
	headAt, ok := e.headArrivalTime(owner, repo, item, prNumber, true)
	if !ok || !headAt.After(labelAt) {
		return false
	}
	return time.Since(headAt) < timeout
}

// resetLadderOnNewHead ends the bot re-prompt ladder's current cycle when the
// PR head arrived after fabrik:bot-reprompted was applied (R4/FR-006): it
// removes only that label, so Phase 2 does not fire on the old clock and Phase 1
// re-fires a full window after the new head. fabrik:awaiting-review stays; its
// anchor is the head's arrival. The head is read live (a poll-start snapshot can
// predate the push); the per-SHA cache makes that one PR read per poll while the
// ladder is engaged. Reports whether the cycle was reset.
func (e *Engine) resetLadderOnNewHead(owner, repo string, item gh.ProjectItem, prNumber int) bool {
	repromptedAt, err := e.labelAppliedAt(item, owner, repo, botRepromptedLabel)
	if err != nil || repromptedAt.IsZero() {
		return false
	}
	headAt, ok := e.headArrivalTime(owner, repo, item, prNumber, true)
	if !ok || !headAt.After(repromptedAt) {
		return false
	}
	e.logf(item.Number, "review-gate", "new head since the bot re-prompt — restarting the re-prompt ladder\n")
	e.applyLabelRemove(item, botRepromptedLabel, false)
	return true
}

// SetReviewWaitHeadAnchorDisabledForTest neutralises the head anchor and the
// ladder reset, restoring the label-only wait (non-vacuity seam, #2467).
func (e *Engine) SetReviewWaitHeadAnchorDisabledForTest(off bool) {
	e.reviewWaitHeadAnchorOff.Store(off)
}
