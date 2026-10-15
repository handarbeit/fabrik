package engine

import (
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Cache-only inputs for the worker-kind, queued, waiting-for-dispatch, retry,
// gate-hold and cause parts of the attention classifier (#2121, ADR 2121). Like
// attention_input.go and localapi.go, nothing here may reach GitHub: it reads the
// copied-out ItemState, the slot semaphore's len/cap and engine-held config only.

// fillWorkerInputs sets the worker, conflict, slot, retry, gate-hold and cause
// facts of in from st.
func (e *Engine) fillWorkerInputs(in *attention.Input, st *itemstate.ItemState, now time.Time, usageLimitedUntil time.Time) {
	if w := st.Worker; w != nil {
		in.WorkerKind = w.Kind
		in.WorkerTrigger = w.Trigger
		in.WorkerQueued = w.Queued
		in.WorkerQueuedSince = w.QueuedSince
		if w.TurnsKnown && !w.Queued {
			in.WorkerTurnsKnown, in.WorkerTurnsUsed = true, w.TurnsUsed
		}
		// The invocation's own budget (comment review limit, fabrik:extend-turns
		// pre-grant, extensions), not the stage default.
		in.WorkerMaxTurns = w.TurnsMax
	}
	if c := st.ConflictResolution; c != nil {
		in.Conflict = &attention.ConflictFact{Queued: c.Queued, Since: c.Since}
	}
	in.SlotsBusy, in.SlotsMax = len(e.sem), cap(e.sem)
	if r := st.PendingRetry; r != nil {
		in.Retry = &attention.RetryFact{Stage: r.Stage, Cause: r.Cause, Due: r.Due}
	}
	// A gate hold the gate stopped confirming has ended (the same lapse the
	// item-wedged detector uses).
	if h := st.GateHold; h != nil && now.Sub(h.ConfirmedAt) <= e.wedgeLapseFor(h.Gate) {
		in.Gate = &attention.GateFact{Gate: h.Gate, Reason: h.Reason, IDs: append([]string(nil), h.IDs...), Since: h.Since}
	}
	in.RebaseAfterLanding = st.RebaseCause == rebaseCauseAfterLanding && hasLabelStr(st.Labels, "fabrik:rebase-needed")
	paused := hasLabelStr(st.Labels, "fabrik:paused")
	in.DaemonStop = paused && pausedByMarker(st, pauseCauseDaemonStopMarker)
	in.UnknownMergeState = paused && pausedByMarker(st, pauseCauseUnknownMergeStateMarker)
	in.Dispatchable = e.dispatchable(st, in, usageLimitedUntil)
}

// daemonStopMarkerSlack is how far after the clean-stop comment an observed
// eligibility change may fall before the marker is treated as belonging to an
// earlier pause.
const daemonStopMarkerSlack = 2 * time.Minute

// daemonStopPause reports whether the newest pause-bearing Fabrik comment is the
// daemon clean-stop pause (it carries pauseCauseDaemonStopMarker).
func daemonStopPause(st *itemstate.ItemState) bool {
	return pausedByMarker(st, pauseCauseDaemonStopMarker)
}

// pausedByMarker reports whether the newest pause-bearing Fabrik comment carries
// the given stable cause marker (daemon-stop #2121, unknown-merge-state #2302). Anything else
// — a different pause comment, a stage comment, no comment at all — is not
// evidence, so a crash or a bare stale in-progress label never reads as a stop.
func pausedByMarker(st *itemstate.ItemState, marker string) bool {
	for i := len(st.Comments) - 1; i >= 0; i-- {
		c := st.Comments[i]
		if c.FromPR != 0 || !strings.HasPrefix(c.Body, fabrikCommentPrefix) ||
			strings.HasPrefix(c.Body, overseerAuditPrefix) {
			continue
		}
		switch {
		case strings.Contains(c.Body, marker):
			// A later eligibility change the store watched (a resume followed by a
			// bare re-pause that wrote no Fabrik comment) means this marker belongs
			// to an earlier pause. A zero stamp is unknown and proves nothing.
			if !st.EligibilityChangedAt.IsZero() && !c.CreatedAt.IsZero() &&
				st.EligibilityChangedAt.After(c.CreatedAt.Add(daemonStopMarkerSlack)) {
				return false
			}
			return true
		case removePausedRE.MatchString(c.Body), commentResumeRE.MatchString(c.Body),
			strings.HasPrefix(c.Body, fabrikStageCommentPrefix):
			return false
		}
	}
	return false
}

// gateCooldowns are the cooldowns that mean a gate, not the dispatcher, is what
// the item waits on: an item carrying one is not "waiting for dispatch".
var gateCooldowns = map[string]bool{
	"feedback-pending": true,
	"review-blocked":   true,
	"merge-unsettled":  true,
	"dep-blocked":      true,
}

// dispatchable reports whether the engine would dispatch the item to a worker if
// a slot were free (#2121 R3): a managed stage that is not complete, open, and
// gated by nothing the cache can see. It is deliberately not conditioned on the
// autonomy labels — a stage is dispatched whatever the autonomy mode, which only
// governs advancing. Every unknown fails closed (not dispatchable), so the
// classifier never claims a dispatch the engine would not make; a sim test pins
// "dispatchable and a free slot implies the next poll dispatches it".
func (e *Engine) dispatchable(st *itemstate.ItemState, in *attention.Input, usageLimitedUntil time.Time) bool {
	if st.IsPR || st.IsClosed || st.Terminal || st.State == "closed" {
		return false
	}
	if in.StageKind != attention.KindManaged || in.Status == "" {
		return false
	}
	if usageLimitedUntil.After(in.Now) || in.PausedByEngine {
		return false
	}
	for _, l := range st.Labels {
		switch {
		case l == "stage:"+st.Status+":complete", l == "stage:"+st.Status+":failed",
			l == "fabrik:paused", l == "fabrik:blocked", l == "fabrik:awaiting-done", l == "fabrik:editing",
			l == "fabrik:api-key-helper-detected", l == "fabrik:skill-unresolved", l == "fabrik:claude-limit",
			l == "fabrik:awaiting-input", l == "fabrik:landing-verification-failed",
			strings.HasPrefix(l, "fabrik:awaiting-"),
			strings.HasPrefix(l, "fabrik:locked:"), strings.HasPrefix(l, "fabrik:reworking:"),
			strings.HasPrefix(l, "stage:") && strings.HasSuffix(l, ":in_progress"):
			return false
		}
	}
	for _, d := range st.BlockedBy {
		if !strings.EqualFold(d.State, "CLOSED") {
			return false
		}
	}
	if st.Worker != nil || st.ConflictResolution != nil || st.GateHold != nil {
		return false
	}
	for reason, until := range st.CooldownAt {
		if until.After(in.Now) && gateCooldowns[reason] {
			return false
		}
	}
	for _, c := range in.Counters {
		if c.AtLimit() {
			return false
		}
	}
	return true
}

// liveTurnRecorder returns the OnTurn hook for one item's invocation (#2121): it
// records the running turn count and the invocation's effective turn budget on the
// live worker so status can report them. Every turn is recorded — the write is a
// single in-memory field update that wakes nothing — so the count is exact; the call
// with zero turns at invocation start clears the previous invocation's count, so a
// worker whose turns are not yet known stays "unknown".
func (e *Engine) liveTurnRecorder(repo string, number int) func(turns, budget int) {
	return func(turns, budget int) {
		if turns < 0 {
			return
		}
		e.store.Apply(itemstate.WorkerTurnsRecorded{Repo: repo, Number: number, Turns: turns, Max: budget})
	}
}
