package engine

import (
	"time"
)

// Admission-cooldown classification (#2532, ADR 2532).
//
// Every admission cooldown reason the engine records is one of three different
// things, and PollWithBackoff schedules each of them accordingly:
//
//   - short (cooldownShort): an engine-scheduled retry (ADR 2232/2393/2498/2499).
//     The idle interval is capped at its exact due.
//   - timer (cooldownTimer): a deadline no GitHub event announces. Cooldown-class
//     timers (the spawn-* live-read retries) keep the ADR 2528 cap (due + one
//     configured interval). Deadline timers that live inside a gate (review wait,
//     CI backstop, ...) are in the timer registry (timer_wake.go) and wake at the
//     exact due.
//   - event-replaceable (cooldownEventReplaceable): a re-check of a condition
//     GitHub already delivers an event for. While ingestion is reliable for the
//     item's repo (transportSet.ReliableFor) it is not counted by the idle cap;
//     otherwise it keeps today's due + one interval cap.
//   - safety net (cooldownSafetyNet): periodic-re-eval, the catch-all for a missed
//     admission or a lost delivery. It owns its own interval (periodicReEvalDue).
//   - exempt (cooldownExempt): not a gate at all (an absolute timestamp cache).
//
// The table below is the single source: shortAdmissionCooldown and gateCooldowns
// are derived from it, and cooldown_class_scan_test.go fails when a reason is
// recorded without an entry.

type cooldownClass int

const (
	cooldownShort cooldownClass = iota + 1
	cooldownTimer
	cooldownEventReplaceable
	cooldownSafetyNet
	cooldownExempt
)

func (c cooldownClass) String() string {
	switch c {
	case cooldownShort:
		return "short"
	case cooldownTimer:
		return "timer"
	case cooldownEventReplaceable:
		return "event-replaceable"
	case cooldownSafetyNet:
		return "safety-net"
	case cooldownExempt:
		return "exempt"
	}
	return "unknown"
}

// Cooldown reasons. Every CooldownRecorded in non-test engine code must name one
// of these (the scan test resolves identifiers to these constants).
const (
	reasonPeriodicReEval      = "periodic-re-eval"
	reasonDepBlocked          = "dep-blocked"
	reasonReviewBlocked       = "review-blocked"
	reasonFeedbackPending     = "feedback-pending"
	reasonFeedbackDispatchGap = "feedback-dispatcher-gap"
	reasonRetryBackoff        = "retry-backoff"
	reasonSentinelLive        = "sentinel-live"
	reasonSpawnResume         = "spawn-resume-deferred"
	reasonSpawnRecovery       = "spawn-recovery-deferred"
)

// cooldownSpec describes one reason: its class, the GitHub event(s) that replace
// it (event-replaceable only), whether an item carrying it is held by a gate
// rather than waiting for the dispatcher, and the evidence for the class.
type cooldownSpec struct {
	Class    cooldownClass
	Events   []string
	Gate     bool
	Evidence string
}

var cooldownClasses = map[string]cooldownSpec{
	reasonPeriodicReEval: {Class: cooldownSafetyNet,
		Evidence: "catches a missed admission (the #2528 shape) or a lost delivery; also the cleanup-stage retry"},
	reasonDepBlocked: {Class: cooldownEventReplaceable, Gate: true,
		Events:   []string{"issue_dependencies"},
		Evidence: "issue_dependencies invalidates and admits the item; DependentRefreshObserver covers a cached blocker closing"},
	reasonReviewBlocked: {Class: cooldownEventReplaceable, Gate: true,
		Events:   []string{"pull_request_review", "pull_request_review_comment", "pull_request_review_thread"},
		Evidence: "a review event admits the item via cycleSet; its silence deadline is carried by the timer registry"},
	reasonFeedbackPending: {Class: cooldownEventReplaceable, Gate: true,
		Events:   []string{"issue_comment", "pull_request_review", "pull_request_review_comment", "pull_request_review_thread"},
		Evidence: "comment, review and thread events, including edit and delete (#2352)"},
	feedbackReadFailedReason: {Class: cooldownShort, Gate: true,
		Evidence: "retry of a failed live feedback read (ADR 2498)"},
	reasonFeedbackDispatchGap: {Class: cooldownShort,
		Evidence: "recorded at PollSeconds/2 to refetch a cache the dispatcher cannot see through; an engine-scheduled retry"},
	mergeUnsettledCooldownReason: {Class: cooldownShort, Gate: true,
		Evidence: "transient GitHub merge state (ADR 2096)"},
	reasonRetryBackoff: {Class: cooldownShort,
		Evidence: "stage retry backoff (ADR 2232/2499)"},
	reasonSentinelLive: {Class: cooldownShort,
		Evidence: "dispatch refused while a cleared worker's process is alive (#1779)"},
	lockWriteRetryReason: {Class: cooldownShort,
		Evidence: "dispatch refused because the lock label could not be written (ADR 2393)"},
	reasonSpawnResume: {Class: cooldownTimer,
		Evidence: "retry of a failed live read; no GitHub event announces it"},
	reasonSpawnRecovery: {Class: cooldownTimer,
		Evidence: "retry of a failed live read; no GitHub event announces it"},
	archiveEligibleAtCooldownReason: {Class: cooldownExempt,
		Evidence: "an absolute eligible-since time, not a gate"},
}

// cooldownClassOf returns the class of reason (0 when unclassified).
func cooldownClassOf(reason string) cooldownClass { return cooldownClasses[reason].Class }

// isShortCooldown reports whether the idle interval must not sleep past reason's
// exact due (the engine-scheduled retries).
func isShortCooldown(reason string) bool { return cooldownClasses[reason].Class == cooldownShort }

// isGateCooldown reports whether an item carrying reason is held by a gate, not
// by the dispatcher.
func isGateCooldown(reason string) bool { return cooldownClasses[reason].Gate }

// minSafetyNetInterval floors an explicit periodic_reeval_interval.
const minSafetyNetInterval = time.Minute

// neutralisedSafetyNetInterval is the stand-in interval of the test seam that
// switches the safety net off (SetSafetyNetDisabledForTest): effectively never.
const neutralisedSafetyNetInterval = 24 * time.Hour

// autoSafetyNetInterval is the safety-net interval while ingestion is reliable
// for the repo and none is configured (ADR 2532): well above the 3-minute light
// reconcile that actually heals lost deliveries.
const autoSafetyNetInterval = 30 * time.Minute

// periodicReEvalDue returns the instant the periodic re-evaluation of an item in
// repo falls due.
func (e *Engine) periodicReEvalDue(repo string) time.Time {
	return e.now().Add(e.periodicReEvalInterval(repo))
}

// periodicReEvalInterval: while ingestion is reliable for the repo, the
// safety-net interval (configured, else autoSafetyNetInterval); otherwise
// githubRecheckInterval, today's value, which an explicit shorter setting
// still lowers.
func (e *Engine) periodicReEvalInterval(repo string) time.Duration {
	base := e.githubRecheckInterval()
	explicit := e.cfg.PeriodicReEvalInterval
	if explicit > 0 && explicit < minSafetyNetInterval {
		explicit = minSafetyNetInterval
	}
	if e.safetyNetStampOff.Load() {
		return neutralisedSafetyNetInterval
	}
	if !e.reliableFor(repo, "") {
		if explicit > 0 && explicit < base {
			return explicit
		}
		return base
	}
	if explicit > 0 {
		return explicit
	}
	return autoSafetyNetInterval
}
