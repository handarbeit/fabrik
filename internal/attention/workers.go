package attention

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Worker kinds (#2121). The strings equal itemstate's WorkerKind* constants
// (this package imports nothing from the engine; an engine test pins the
// equality). An empty kind is an unknown kind, never an error: the item is still
// reported as working.
const (
	WorkerKindStageRun       = "stage-run"
	WorkerKindCommentReview  = "comment-review"
	WorkerKindReviewReinvoke = "review-reinvoke"
	WorkerKindCIFixReinvoke  = "ci-fix-reinvoke"
	WorkerKindRebaseReinvoke = "rebase-reinvoke"
	WorkerKindTrainConflict  = "merge-train-conflict"
)

// Codes added by #2121.
const (
	// CodeQueuedForSlot: a worker is dispatched but holds no worker slot yet.
	CodeQueuedForSlot = "queued-for-slot"
	// CodeWaitingForDispatch: the item has work to do and no worker; the engine
	// will dispatch it (all slots busy, the next poll, or a dispatch cooldown).
	CodeWaitingForDispatch = "waiting-for-dispatch"
	// CodeRetryPending / CodeRetryWaitingForSlot: an invocation failed and the
	// engine has scheduled a retry (not yet due / waiting for a free slot).
	CodeRetryPending        = "retry-pending"
	CodeRetryWaitingForSlot = "retry-waiting-for-slot"
	// CodeGateHold: a gate the engine applies is holding the item's advance.
	CodeGateHold = "gate-hold"
)

// The cause vocabulary (#2121). One cause per result, present only when the
// inputs that establish it are, absent otherwise — never guessed. A new cause
// is a documented addition (docs/state-machine.md §7.13).
const (
	CauseDaemonStop                 = "daemon-stop"
	CauseSlotStarved                = "slot-starved"
	CauseWallTimeKill               = "wall-time-kill"
	CauseMaxTurns                   = "max-turns"
	CauseRebaseConflictAfterLanding = "rebase-conflict-after-landing"
	causeGateHoldPrefix             = "gate-hold:"
)

// CauseGateHold is the cause of an item held by the named gate.
func CauseGateHold(gate string) string { return causeGateHoldPrefix + gate }

// Causes lists the fixed cause vocabulary; the gate-hold entry stands for
// gate-hold:<gate>. Used by the docs and by tests that pin the vocabulary.
func Causes() []string {
	return []string{CauseDaemonStop, CauseSlotStarved, CauseWallTimeKill, CauseMaxTurns,
		causeGateHoldPrefix + "<gate>", CauseRebaseConflictAfterLanding}
}

// RetryFact is a retry the engine has scheduled after a failed invocation.
type RetryFact struct {
	Stage string
	// Cause is the engine's own cause (wall-time-kill, inactivity-kill, max-turns,
	// api-error, ...); only wall-time-kill and max-turns are vocabulary causes.
	Cause string
	// Due is when the retry becomes dispatchable; zero = unknown.
	Due time.Time
}

// GateFact is a gate currently holding an item's advance. The caller drops a
// hold the gate has stopped confirming, so a present fact is a fresh one.
type GateFact struct {
	// Gate is feedback, review, ci or merge.
	Gate   string
	Reason string
	// IDs are the pending feedback ids (feedback gate).
	IDs   []string
	Since time.Time
}

// ConflictFact is a merge-train conflict resolution for the item.
type ConflictFact struct {
	Queued bool
	Since  time.Time
}

// slotsBusy reports whether the worker pool is known to be full.
func slotsBusy(in Input) bool { return in.SlotsMax > 0 && in.SlotsBusy >= in.SlotsMax }

func slotsText(in Input) string {
	return fmt.Sprintf("all %d of %d worker slots are busy", in.SlotsBusy, in.SlotsMax)
}

// kindLabel names a worker kind in prose ("a review reinvoke").
func kindLabel(kind string) string {
	switch kind {
	case WorkerKindCommentReview:
		return "a comment review"
	case WorkerKindReviewReinvoke:
		return "a review reinvoke"
	case WorkerKindCIFixReinvoke:
		return "a CI-fix reinvoke"
	case WorkerKindRebaseReinvoke:
		return "a rebase reinvoke"
	case WorkerKindTrainConflict:
		return "a merge-train conflict resolution"
	}
	return "a worker"
}

// triggerText renders a worker trigger ("a,b,c,d" -> "a, b, c and 1 more").
func triggerText(trigger string) string {
	if trigger == "" {
		return ""
	}
	parts := strings.Split(trigger, ",")
	const show = 3
	if len(parts) <= show {
		return strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(parts[:show], ", "), len(parts)-show)
}

// workerTextFor describes the in-flight worker for a summary.
func workerTextFor(in Input) string {
	kind, trigger := in.WorkerKind, in.WorkerTrigger
	if in.Conflict != nil && !in.HasWorker {
		kind, trigger = WorkerKindTrainConflict, ""
	}
	text := kindLabel(kind)
	if t := triggerText(trigger); t != "" {
		text += " addressing " + t
	}
	return text
}

func turnsText(in Input) string {
	if !in.HasWorker || !in.WorkerTurnsKnown {
		return ""
	}
	if in.WorkerMaxTurns > 0 {
		return fmt.Sprintf(" (turn %d of %d)", in.WorkerTurnsUsed, in.WorkerMaxTurns)
	}
	return fmt.Sprintf(" (turn %d)", in.WorkerTurnsUsed)
}

// withoutCooldownReasons drops "cooldown:*" reasons (R4: while a worker is in
// flight or queued a dispatch cooldown is neither the verdict nor a reason).
func withoutCooldownReasons(rs []Reason) []Reason {
	var out []Reason
	for _, r := range rs {
		if !strings.HasPrefix(r.Code, "cooldown:") {
			out = append(out, r)
		}
	}
	return out
}

// hasWorkerLike reports whether a worker, or a merge-train conflict resolution,
// is queued or running for the item.
func hasWorkerLike(in Input) bool { return in.HasWorker || in.Conflict != nil }

// workerRow classifies an item with a worker in flight or queued (#2121 R1, R2,
// R4). It runs ahead of the wait labels and cooldowns: a worker that exists is
// what the item is doing, whatever else is also true of it.
func workerRow(res Result, in Input) (Result, bool) {
	if !hasWorkerLike(in) {
		return res, false
	}
	// R4: a dispatch cooldown is not the verdict, a reason or a deadline.
	res.Reasons = withoutCooldownReasons(res.Reasons)
	res.Next.Deadlines = withoutCooldowns(res.Next.Deadlines)
	res.Next.At = earliestKnown(res.Next.Deadlines)

	queued := in.HasWorker && in.WorkerQueued
	since := in.WorkerQueuedSince
	if !in.HasWorker && in.Conflict != nil {
		queued, since = in.Conflict.Queued, in.Conflict.Since
	}
	what := workerTextFor(in)
	if queued {
		res.State, res.Code, res.Rank, res.Cause = Waiting, CodeQueuedForSlot, NotRanked, CauseSlotStarved
		when := ""
		if !since.IsZero() {
			when = " (since " + since.UTC().Format(time.RFC3339) + ")"
		}
		res.Summary = "queued: waiting for a worker slot" + when + "; " + what + " will start when one is free"
		if slotsBusy(in) {
			res.Summary += " — " + slotsText(in)
		}
		res.Next.Action = "the worker starts when a worker slot is free"
		return res, true
	}
	if in.HasWorker && workerOverdue(res, in) {
		res.State, res.Code, res.Rank = Stalled, CodeWorkerNoProgress, RankStalled
		res.Summary = fmt.Sprintf("%s is in flight but nothing observable changed for %s", what, res.ProgressAge.Round(time.Second))
		res.Next.Action = "the worker's own inactivity/wall-time limits will end it; the engine then reads its result"
		return res, true
	}
	res.State, res.Code, res.Rank = Working, "worker-in-flight", NotRanked
	summary := what + " is running"
	if what == "a worker" {
		summary = "a worker is running"
	}
	summary += turnsText(in)
	if in.HasWorker && in.Cfg.StallThreshold > 0 && res.ProgressAge > in.Cfg.StallThreshold {
		summary += fmt.Sprintf(" (long-running: %s, within its stage's wall-clock budget)", res.ProgressAge.Round(time.Second))
	}
	res.Summary = summary
	res.Next.Action = "the engine reads the invocation result when the worker exits"
	return res, true
}

// retryCause maps the engine's retry cause to the vocabulary; other causes
// (inactivity-kill, api-error, ...) stay out of it and yield no cause.
func retryCause(c string) string {
	switch c {
	case CauseWallTimeKill, CauseMaxTurns:
		return c
	}
	return ""
}

func killText(c string) string {
	switch c {
	case "wall-time-kill":
		return "killed by the stage's wall-time limit"
	case "inactivity-kill":
		return "killed after the inactivity timeout"
	case "max-turns":
		return "stopped at the turn limit"
	case "api-error":
		return "ended by an API error"
	case "":
		return "did not complete"
	}
	return "did not complete (" + c + ")"
}

// retryRow explains an item that has no worker, waits on no label and has a
// retry the engine has scheduled (#2121 FR-014/FR-015): it is a retry waiting to
// run, not a stall. The leftover in-progress and lock labels are explained while
// it is pending.
func retryRow(res Result, in Input) (Result, bool) {
	r := in.Retry
	if r == nil || hasWorkerLike(in) {
		return res, false
	}
	for _, l := range in.Labels {
		if !inFlightMarker(l) {
			continue
		}
		reason := Reason{Code: l, Meaning: "left over from the killed run until the retry runs; not a live worker"}
		replaced := false
		for i := range res.Reasons {
			if res.Reasons[i].Code == l {
				res.Reasons[i], replaced = reason, true
			}
		}
		if !replaced {
			res.Reasons = append(res.Reasons, reason)
		}
	}
	stage := r.Stage
	if stage == "" {
		stage = in.Status
	}
	base := fmt.Sprintf("the last %s run was %s; a retry is scheduled", stage, killText(r.Cause))
	res.State, res.Rank, res.Cause = Waiting, NotRanked, retryCause(r.Cause)
	dueText := ""
	if !r.Due.IsZero() {
		dueText = " for " + r.Due.UTC().Format(time.RFC3339)
	}
	switch {
	case slotsBusy(in) && !r.Due.After(in.Now):
		res.Code = CodeRetryWaitingForSlot
		res.Summary = base + dueText + " and is waiting for a worker slot (" + slotsText(in) + ")"
		res.Next.Action = "the retry starts when a worker slot is free"
	case !r.Due.IsZero() && r.Due.After(in.Now):
		res.Code = CodeRetryPending
		res.Summary = base + dueText
		res.Next.Deadlines = append([]Deadline{{Kind: "retry-due", At: r.Due, Basis: "retry backoff expiry"}}, withoutCooldowns(res.Next.Deadlines)...)
		res.Next.At = earliestKnown(res.Next.Deadlines)
		res.Next.Action = "the engine retries the stage at " + r.Due.UTC().Format(time.RFC3339)
	default:
		res.Code = CodeRetryPending
		res.Summary = base + dueText + "; the next poll dispatches it"
		res.Next.Action = "the next poll dispatches the retry"
		if !r.Due.IsZero() && in.Cfg.StallThreshold > 0 && in.Now.Sub(r.Due) > in.Cfg.StallThreshold && !slotsBusy(in) && in.SlotsMax > 0 {
			res.State, res.Code, res.Rank = Stalled, CodeOverdue, RankStalled
			res.Summary = fmt.Sprintf("%s, but it was due %s ago and a worker slot is free", base, in.Now.Sub(r.Due).Round(time.Second))
		}
	}
	return res, true
}

// gateHoldRow explains an item held from advancing by a gate (#2121 FR-016).
func gateHoldRow(res Result, in Input) (Result, bool) {
	h := in.Gate
	if h == nil || hasWorkerLike(in) {
		return res, false
	}
	res.State, res.Code, res.Rank, res.Cause = Waiting, CodeGateHold, NotRanked, CauseGateHold(h.Gate)
	what := h.Reason
	if what == "" {
		what = "the " + h.Gate + " gate is holding"
	}
	if len(h.IDs) > 0 {
		ids := append([]string(nil), h.IDs...)
		sort.Strings(ids)
		what += " — pending " + triggerText(strings.Join(ids, ","))
	}
	since := ""
	if !h.Since.IsZero() {
		since = " (held since " + h.Since.UTC().Format(time.RFC3339) + ")"
	}
	res.Summary = fmt.Sprintf("the %s gate is holding this item from advancing: %s%s", h.Gate, what, since)
	res.Next.Action = "the item advances when the " + h.Gate + " gate clears"
	return res, true
}

// realDispatchCooldown reports whether a cooldown reason will really re-dispatch
// the item when it expires: only the retry backoff and the live-sentinel
// refusal do. Gate cooldowns, dependency waits and the safety-net re-evaluation
// are not a dispatch.
func realDispatchCooldown(reason string) bool {
	return reason == "retry-backoff" || reason == "sentinel-live"
}

// dispatchRow reports an item that has work to do and no worker (#2121 R3). It
// is never idle: the reason is all slots busy, a dispatch cooldown that will
// really re-dispatch it, or the next poll. stalledAge lets the unexplained case
// (slots free, no cooldown, long past the stall threshold) fall through to the
// stalled row.
func dispatchRow(res Result, in Input, stalledAge bool) (Result, bool) {
	if !in.Dispatchable || hasWorkerLike(in) {
		return res, false
	}
	var cooldown string
	var until time.Time
	var cds []string
	for r, t := range in.Cooldowns {
		if t.After(in.Now) && realDispatchCooldown(r) {
			cds = append(cds, r)
		}
	}
	if len(cds) > 0 {
		sort.Strings(cds)
		cooldown, until = cds[0], in.Cooldowns[cds[0]]
	}
	res.State, res.Code, res.Rank = Waiting, CodeWaitingForDispatch, NotRanked
	switch {
	case slotsBusy(in):
		res.Cause = CauseSlotStarved
		res.Summary = "waiting for dispatch: " + slotsText(in) + "; it is dispatched when one is free"
		res.Next.Action = "the engine dispatches it when a worker slot is free"
	case cooldown != "":
		res.Summary = fmt.Sprintf("waiting for dispatch: the %q cooldown ends at %s, then the engine dispatches it", cooldown, until.UTC().Format(time.RFC3339))
		res.Next.Action = fmt.Sprintf("the engine dispatches it when the %q cooldown ends at %s", cooldown, until.UTC().Format(time.RFC3339))
	case stalledAge:
		return res, false
	default:
		res.Summary = "waiting for dispatch: a worker slot is free and the next poll dispatches it"
		res.Next.Action = "the next poll dispatches it"
	}
	return res, true
}
