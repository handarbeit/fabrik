package engine

import (
	"errors"
	"sync/atomic"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Two timing concepts used to share one expression, 10 × PollSeconds, and so
// were silently coupled (#1831). They answer different questions and must not
// be conflated again:
//
//   - stageRetryBackoff: how long to wait before re-dispatching a stage after
//     an attempt that did not complete. Its only job is to stop a stage that
//     exits incomplete immediately from hot-looping. It is NOT a fairness or
//     runaway control — those are max_turns (per attempt, which also releases
//     the worker slot) and max_retries (across attempts). It costs no GitHub
//     API calls beyond the retry itself, so it must not scale with poll.
//
//   - githubRecheckInterval: how often to re-read from GitHub an item we will
//     not be notified about (a dependency closing, a silent bot reviewer, a
//     terminal item's periodic re-eval). This one genuinely is about API
//     cost, so it scales with poll — which operators raise precisely to
//     protect a shared rate limit.
//
// Before #1831, raising poll from 30 to 180 to protect the GraphQL budget
// silently turned a 5-minute stage retry into a 30-minute one.

// stageRetryBackoff returns the delay before a stage is re-dispatched after an
// incomplete attempt. It is read by the dispatch gate, processItem's gate, and
// the "will retry after" message, so all three always agree.
//
// A zero Config.RetryBackoff means unset: the CLI always supplies a value
// (default 60s), so this fallback is reached only by code that builds a Config
// directly (tests, embedders), which keeps their existing timing unchanged.
func (e *Engine) stageRetryBackoff() time.Duration {
	if e.cfg.RetryBackoff > 0 {
		return e.cfg.RetryBackoff
	}
	return e.githubRecheckInterval()
}

// githubRecheckInterval returns the cadence for re-reading an item from GitHub
// when nothing will notify us of a change (the API-cost knob). Deliberately
// coupled to PollSeconds — see the file comment.
func (e *Engine) githubRecheckInterval() time.Duration {
	return time.Duration(e.cfg.PollSeconds*10) * time.Second
}

// mergeGateRecheckInterval is the cooldown a merge-gate claim records
// ("merge-unsettled", #2096): half a poll interval, floored at one second. The
// entry is stamped mid-poll, so a full PollSeconds would land after the next
// poll tick and cost the item one extra poll; half an interval is reliably
// expired by then while still sitting out any early wake-ups. Deliberately not
// githubRecheckInterval: that is the API-cost knob (ADR-1831), and a claim on
// transient merge state must be re-checked on the very next poll.
func (e *Engine) mergeGateRecheckInterval() time.Duration {
	d := time.Duration(e.cfg.PollSeconds) * time.Second / 2
	if d < time.Second {
		d = time.Second
	}
	return d
}

// Engine-scheduled retries own an admission cooldown (#2232, ADR 2232). The
// engine's own commits, pushes and label writes are self-writes that raise no
// change signal, so an item whose stage ended incomplete is admitted by nothing
// until the periodic re-evaluation (10 × PollSeconds) unless the decision to
// retry also records the instant it falls due.

// shortAdmissionCooldownReasons are the engine-scheduled retries whose due time
// the poll loop must not sleep past (FR-003): the interval is capped at their
// exact due. Every other reason (periodic-re-eval, dep-blocked, review-blocked,
// feedback-pending, spawn-*) is a long cooldown; since #2528 (ADR 2528) the
// interval is also capped on those, but at due + one configured interval
// (earliestLongCooldown), so staggered dues coalesce.
var shortAdmissionCooldownReasons = map[string]bool{
	"retry-backoff":   true,
	"sentinel-live":   true,
	"merge-unsettled": true,
	// lockWriteRetryReason (#2393): a refused dispatch must be retried on the
	// next poll, not after an idle back-off.
	"lock-write-failed": true,
	// feedbackReadFailedReason (#2498): a feedback-gate hold caused only by a
	// failed live read is retried on the next poll.
	feedbackReadFailedReason: true,
}

// minDueWakeInterval floors the FR-003 cap, matching the retry_backoff minimum.
const minDueWakeInterval = time.Second

// Test-only switches (non-vacuity of the #2232 requirements). Not for production.
var (
	postRunRetryRecordDisabled       atomic.Bool
	postRunFeedbackAdmissionDisabled atomic.Bool
	intervalCapDisabled              atomic.Bool
	timeSliceBackoffDisabled         atomic.Bool
	dispatchOrderDisabled            atomic.Bool
	// postRunArrivalRestrictionDisabled lifts FR-001b's "arrived during the run"
	// restriction, so any pending feedback admits (the unbounded-loop shape).
	postRunArrivalRestrictionDisabled atomic.Bool
)

// SetTimeSliceBackoffDisabledForTest neutralises (or restores) the time-slice
// exemption from the retry backoff.
func SetTimeSliceBackoffDisabledForTest(disabled bool) (restore func()) {
	prev := timeSliceBackoffDisabled.Swap(disabled)
	return func() { timeSliceBackoffDisabled.Store(prev) }
}

// SetDispatchOrderDisabledForTest neutralises (or restores) FR-006's
// longest-waiting-first dispatch order, restoring board order.
func SetDispatchOrderDisabledForTest(disabled bool) (restore func()) {
	prev := dispatchOrderDisabled.Swap(disabled)
	return func() { dispatchOrderDisabled.Store(prev) }
}

// isTimeSlice reports whether an incomplete invocation ended because the engine
// bounded it, not because it failed: a turn-cap stop (error_max_turns) or a
// max_wall_time kill. The worker just ran a full slice, and the existing
// counters (SliceRetries, max_retries) bound the retries, so the backoff that
// exists to stop a fast failure from looping does not apply (#2232). An
// inactivity kill is a hang, not a slice, and keeps the backoff. Deciding by the
// engine's own structural signals rather than by run duration keeps a fast
// error exit from ever qualifying.
func isTimeSlice(err error, turnLimited bool) bool {
	if timeSliceBackoffDisabled.Load() {
		return false
	}
	if turnLimited {
		return true
	}
	var killed *claudeKilledError
	return errors.As(err, &killed) && killed.Cause == claudeerr.KillWallTime
}

// SetPostRunRetryRecordDisabledForTest neutralises (or restores) FR-001.
func SetPostRunRetryRecordDisabledForTest(disabled bool) (restore func()) {
	prev := postRunRetryRecordDisabled.Swap(disabled)
	return func() { postRunRetryRecordDisabled.Store(prev) }
}

// SetPostRunFeedbackAdmissionDisabledForTest neutralises (or restores) FR-001b.
func SetPostRunFeedbackAdmissionDisabledForTest(disabled bool) (restore func()) {
	prev := postRunFeedbackAdmissionDisabled.Swap(disabled)
	return func() { postRunFeedbackAdmissionDisabled.Store(prev) }
}

// SetPostRunArrivalRestrictionDisabledForTest lifts (or restores) FR-001b's
// restriction to feedback created at or after the run's start.
func SetPostRunArrivalRestrictionDisabledForTest(disabled bool) (restore func()) {
	prev := postRunArrivalRestrictionDisabled.Swap(disabled)
	return func() { postRunArrivalRestrictionDisabled.Store(prev) }
}

// SetPostRunAdvanceAdmissionDisabledForTest neutralises (or restores) the
// column-change admission at worker exit on this engine (#2528).
func (e *Engine) SetPostRunAdvanceAdmissionDisabledForTest(off bool) { e.postRunAdvanceOff.Store(off) }

// SetLongCooldownCapDisabledForTest neutralises (or restores) the idle-interval
// cap on long admission cooldowns on this engine (#2528).
func (e *Engine) SetLongCooldownCapDisabledForTest(off bool) { e.longCooldownCapOff.Store(off) }

// SetAfterAdvanceHookForTest registers a hook advanceToNextStage calls right
// after a successful Status write, with the item and the new Status. It lets a
// test run a poll while the item's own worker is still registered (#2528). Nil
// clears it; production never sets it.
func (e *Engine) SetAfterAdvanceHookForTest(fn func(item gh.ProjectItem, newStatus string)) {
	if fn == nil {
		e.afterAdvanceHook.Store(nil)
		return
	}
	e.afterAdvanceHook.Store(&fn)
}

// SetIntervalCapDisabledForTest neutralises (or restores) FR-003.
func SetIntervalCapDisabledForTest(disabled bool) (restore func()) {
	prev := intervalCapDisabled.Swap(disabled)
	return func() { intervalCapDisabled.Store(prev) }
}

// recordStageRetryCooldown records the "retry-backoff" admission cooldown that
// makes the item admissible at e.now()+d (FR-001). It is the single writer: the
// post-run decision points pass the same value they log and hand to
// emitRetryScheduled (FR-004), and the dispatch-time backstop
// (recordRetryBackoffCooldown) passes the remaining wait. d <= 0 records nothing.
func (e *Engine) recordStageRetryCooldown(item gh.ProjectItem, d time.Duration) {
	if d <= 0 {
		return
	}
	e.recordStageRetryCooldownUntil(item, e.now().Add(d))
}

func (e *Engine) recordStageRetryCooldownUntil(item gh.ProjectItem, until time.Time) {
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   itemOwnerRepoString(item, e.defaultRepo()),
		Number: item.Number,
		Reason: "retry-backoff",
		Until:  until,
	})
}

// recordPostRunRetryCooldown is recordStageRetryCooldown for the post-run exits
// (honours the FR-001 test switch; the dispatch-time backstop does not).
func (e *Engine) recordPostRunRetryCooldown(item gh.ProjectItem, d time.Duration) {
	if postRunRetryRecordDisabled.Load() || d < 0 {
		return
	}
	// d == 0 is a time slice: already due, so the very next poll admits the item.
	e.recordStageRetryCooldownUntil(item, e.now().Add(d))
}

// capIntervalToDue shortens effective so the next poll lands at due, floored at
// floor. A zero due, or a due that is not earlier than now+effective, leaves
// effective unchanged: the cap only ever shortens the interval.
func capIntervalToDue(effective time.Duration, due, now time.Time, floor time.Duration) time.Duration {
	if due.IsZero() {
		return effective
	}
	d := due.Sub(now)
	if d >= effective {
		return effective
	}
	if d < floor {
		d = floor
	}
	if d > effective {
		return effective
	}
	return d
}

// earliestShortCooldown returns the earliest unexpired short engine-scheduled
// cooldown across the store (zero if none). Expired entries are ignored — they
// are admitted (and consumed) by the next poll, and counting them would pin the
// interval at the floor — as are cleanup-stage items, which the engine never
// admits for work. In memory only; no GitHub call.
func (e *Engine) earliestShortCooldown(now time.Time) time.Time {
	var earliest time.Time
	e.store.Scan(func(st *itemstate.ItemState) {
		if st.IsClosed {
			return
		}
		if s := stages.FindStage(e.cfg.Stages, st.Status); s != nil && s.CleanupWorktree {
			return
		}
		for reason, t := range st.CooldownAt {
			if !shortAdmissionCooldownReasons[reason] || !now.Before(t) {
				continue
			}
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
	})
	return earliest
}

// earliestLongCooldown returns the earliest unexpired long admission cooldown
// (any reason that is neither short nor archive-eligible-at) across the store
// (zero if none), with the same exclusions as earliestShortCooldown (#2528).
// archive-eligible-at holds an absolute "eligible since" time, not a gate, and
// would pin the cap. Admission consumes an expired entry and the poll() defer
// re-stamps it a full githubRecheckInterval later, so counting only unexpired
// entries cannot loop. In memory only.
func (e *Engine) earliestLongCooldown(now time.Time) time.Time {
	var earliest time.Time
	e.store.Scan(func(st *itemstate.ItemState) {
		if st.IsClosed {
			return
		}
		if s := stages.FindStage(e.cfg.Stages, st.Status); s != nil && s.CleanupWorktree {
			return
		}
		for reason, t := range st.CooldownAt {
			if shortAdmissionCooldownReasons[reason] || reason == itemstate.ArchiveEligibleCooldown || !now.Before(t) {
				continue
			}
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
	})
	return earliest
}
