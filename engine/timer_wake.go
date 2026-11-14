package engine

import (
	"fmt"
	"sync"
	"time"
)

// The deadline timer registry (#2532, ADR 2532).
//
// Some checks that run on admission are timers: a deadline no GitHub event
// announces (the review-wait timeout and its bot re-prompt ladder, the CI backstop
// and dwells, the merge-queue stall dwell, the unknown-merge-state bound, the
// lock-orphan grace). They used to fire only when the item happened to be
// re-admitted by periodic-re-eval or review-blocked, so lengthening the safety net
// would have made them late.
//
// Each gate records its own deadline here from the branch where it already says
// "not yet elapsed", using the anchor and timeout variables that branch computed.
// The registry decides nothing: it only (a) caps PollWithBackoff at the exact due
// and (b) re-queues the item into mayNeedWork when due, so the gate is evaluated
// again. It is deliberately not an admission cooldown, so ADR 2096's consume and
// re-arm contract and ADR 039's cycleSet rules are untouched.
//
// A deadline is stored as an engine-clock instant: the gates measure against the
// host clock (time.Since) or the engine clock depending on the gate, so each hook
// passes the time remaining by its own clock and the registry anchors it to e.now().
// A re-note is floored at mergeGateRecheckInterval, so a gate that still says "not
// yet" at the deadline cannot hot-loop the poll.

type timerKey struct {
	repo   string
	number int
	kind   string
}

type timerRegistry struct {
	mu  sync.Mutex
	due map[timerKey]time.Time
}

// Timer kinds (one deadline per item and kind).
const (
	timerReviewWait    = "review-wait"
	timerReviewPhase2  = "review-phase2"
	timerCIBackstop    = "ci-backstop"
	timerCIWait        = "ci-wait"
	timerMergeStall    = "merge-stall"
	timerUnknownMerge  = "unknown-merge"
	timerLockOrphan    = "lock-orphan"
	timerRegistryLimit = 20000
)

// noteTimerDeadline records that the check kind for repo#number should run again
// after remaining (by the gate's own clock). remaining <= 0 is floored.
func (e *Engine) noteTimerDeadline(repo string, number int, kind string, remaining time.Duration) {
	if e.timerWakeOff.Load() || repo == "" {
		return
	}
	floor := e.mergeGateRecheckInterval()
	if remaining < floor {
		remaining = floor
	}
	due := e.now().Add(remaining)
	r := &e.timers
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.due == nil {
		r.due = make(map[timerKey]time.Time)
	}
	if len(r.due) >= timerRegistryLimit {
		return // bounded; periodic-re-eval still covers the item
	}
	r.due[timerKey{repo, number, kind}] = due
}

// clearTimer forgets a deadline whose gate no longer applies.
func (e *Engine) clearTimer(repo string, number int, kind string) {
	r := &e.timers
	r.mu.Lock()
	delete(r.due, timerKey{repo, number, kind})
	r.mu.Unlock()
}

// consumeDueTimers removes every entry due at or before now and returns the
// issue keys to re-admit (deduplicated).
func (e *Engine) consumeDueTimers(now time.Time) []string {
	r := &e.timers
	r.mu.Lock()
	defer r.mu.Unlock()
	var keys []string
	seen := map[string]bool{}
	for k, t := range r.due {
		if now.Before(t) {
			continue
		}
		delete(r.due, k)
		ik := fmt.Sprintf("%s#%d", k.repo, k.number)
		if !seen[ik] {
			seen[ik] = true
			keys = append(keys, ik)
		}
	}
	return keys
}

// earliestTimerDeadline returns the earliest unexpired deadline (zero if none).
// Entries for items that closed or left the Store are dropped.
func (e *Engine) earliestTimerDeadline(now time.Time) time.Time {
	r := &e.timers
	r.mu.Lock()
	keys := make([]timerKey, 0, len(r.due))
	for k := range r.due {
		keys = append(keys, k)
	}
	r.mu.Unlock()
	var stale []timerKey
	for _, k := range keys {
		if snap, ok := e.store.Peek(k.repo, k.number); !ok || snap.IsClosed() {
			stale = append(stale, k)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range stale {
		delete(r.due, k)
	}
	var earliest time.Time
	for _, t := range r.due {
		if !now.Before(t) {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}
