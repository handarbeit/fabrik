package simgh

import "time"

// This file is the sequencing primitive: how a scripted surface returns a
// different value after a given instant, so a scenario can express "CI is
// pending for two polls, then fails" or "the reviewer responds on the third
// poll" without wall-clock sleeping.
//
// # Why the clock and not a read count
//
// R2 offers either. Read count is the wrong mechanism here, and the reason is
// not concurrency in the abstract — it is that **one poll is not one read.**
// settleAwaitingCIScan primes the store with a live check-run read
// (RefreshCheckRunsLive) precisely so a stale cached PENDING cannot wedge the
// item, and the handler chain it then runs reaches checkCIGate, which reads
// check-run state again: two reads of one SHA in a single poll, before
// MaxConcurrent workers enter the picture at all. Worse, the first of those
// reads is guarded on the engine having a boardcache wired, which is a harness
// configuration decision, not an engine invariant.
//
// So "pending, pending, failure" expressed in read counts does not mean
// "pending for two polls, then red" — it means "pending for the first two
// reads, whenever those happen to occur", and the poll-to-read mapping is an
// implementation detail of the very scan under test. That is a flaky-test
// generator inside the layer built to eliminate flaky tests.
//
// Read-count sequencing survives in exactly one place: prRecord's
// mergeableRecomputeReads, which models a genuine GitHub behaviour (a
// recompute that resolves after some reads) rather than a test's notion of
// elapsed time. Its reproducibility caveat is recorded in FIDELITY.md.
//
// # Steps are pending mutations applied on read, not a read filter
//
// When a step's time arrives, it *writes into the model*, and the write is
// permanent. Two consequences, both deliberate:
//
//   - Repeated reads at one clock instant are stable. The step applies once,
//     on the first read past its time; every later read sees the same model.
//     A read filter recomputed per call would be equally stable, but only by
//     accident of being pure — this is stable structurally.
//
//   - Engine mutations to the same surface remain observable afterwards. A
//     filter that overrode reviewRequests would mask AddReviewRequest, which
//     is the bot re-prompt ladder's own mutation (ADR-1283) — the engine would
//     make a call, the model would accept it, and the next read would deny it
//     ever happened.
//
// # Locking
//
// A schedule is part of the model, guarded by Sim.mu like everything else it
// sits next to. drain must be called with mu held, and a step's apply function
// therefore runs under mu — so no step may run git or take gitMu. Every step
// this package constructs is a pure in-memory append or replace.

// scheduleStep is one pending timed mutation of T.
type scheduleStep[T any] struct {
	at    time.Time
	apply func(T)
}

// schedule is an ordered queue of pending timed mutations. The zero value is
// an empty schedule.
type schedule[T any] struct {
	// steps is kept sorted by at, with insertion order preserved among steps
	// sharing an instant — two steps scheduled for the same moment apply in
	// the order they were seeded, which is the only ordering a scenario author
	// can reason about.
	steps []scheduleStep[T]
}

// add enqueues a step. Caller must hold mu.
func (sc *schedule[T]) add(at time.Time, apply func(T)) {
	i := len(sc.steps)
	for i > 0 && sc.steps[i-1].at.After(at) {
		i--
	}
	sc.steps = append(sc.steps, scheduleStep[T]{})
	copy(sc.steps[i+1:], sc.steps[i:])
	sc.steps[i] = scheduleStep[T]{at: at, apply: apply}
}

// drain applies every step whose time has arrived, in order, and removes them.
// Reports whether anything was applied. Caller must hold mu.
//
// "Arrived" is at <= now, not at < now: a step scheduled for exactly the
// current instant fires. A scenario advancing the clock by precisely the
// interval it scheduled against is the common case, and the alternative would
// make it silently a no-op.
func (sc *schedule[T]) drain(now time.Time, target T) bool {
	applied := 0
	for _, step := range sc.steps {
		if step.at.After(now) {
			break
		}
		step.apply(target)
		applied++
	}
	if applied == 0 {
		return false
	}
	remaining := make([]scheduleStep[T], len(sc.steps)-applied)
	copy(remaining, sc.steps[applied:])
	sc.steps = remaining
	return true
}

// pending reports how many steps have not yet fired. Caller must hold mu.
func (sc *schedule[T]) pending() int { return len(sc.steps) }

// clone deep-copies the queue for a snapshot. The apply closures are shared
// rather than reconstructed — they capture only the values the scenario
// seeded, and they take their target as a parameter, so a clone re-attached to
// a *different* repoState or prRecord after a restore still mutates the right
// one. That is why the target is a parameter rather than a captured pointer.
// Caller must hold mu.
func (sc *schedule[T]) clone() schedule[T] {
	if len(sc.steps) == 0 {
		return schedule[T]{}
	}
	out := make([]scheduleStep[T], len(sc.steps))
	copy(out, sc.steps)
	return schedule[T]{steps: out}
}

// drainCI applies every due check-run and commit-status step for a repo.
//
// Every read path that consults r.checkRuns or r.commitStatuses must call this
// first — FetchCheckRuns, both branches of FetchCombinedStatus, and
// deriveMergeableState ahead of contextsForSHA. Routing them all through one
// accessor rather than inlining the drain is what keeps two reads of one model
// from reporting two verdicts, the bug reviews.go already warns about one
// layer up.
//
// Caller must hold mu.
func (s *Sim) drainCI(r *repoState) { r.ciSchedule.drain(s.now(), r) }

// drainReviews applies every due review and review-request step for a PR.
//
// Called by every read path consulting pr.reviews or pr.reviewRequests
// (FetchPRReviews, FetchPRReviewRequests, FetchPRReviewDecision, and the board
// projection's linked-PR fields) and also by AddReviewRequest and
// DeleteReviewRequest before they mutate. Draining in the mutators is not
// belt-and-braces: without it a step due *before* an engine withdrawal would
// apply *after* it, re-adding a reviewer the engine had just removed and
// inverting the order in which the two happened.
//
// Caller must hold mu.
func (s *Sim) drainReviews(pr *prRecord) { pr.reviewSchedule.drain(s.now(), pr) }
