package engine

import (
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Stage-gate flake re-run (#2072, ADR 2072).
//
// The merge train re-runs a trial's failed jobs once before treating it as red
// (#2052 R5). The stage `wait_for_ci` gate had no equivalent: a confirmed
// check-run failure always dispatched a CI-fix worker, and a worker that found
// the failure flaky and pushed nothing left the item red until the CI backstop.
//
// ciFlakeRerun is called from handleMergeAndCIGates' ciFailure branch, ahead of
// the ci-fix-reinvoke dispatch. Per PR head it grants at most one re-run before
// the first dispatch (R1) and one after a recorded no-op fix (R2); a new head
// resets both. The gate is re-evaluated once per poll, so — unlike pollTrainCI's
// blocking loop — the re-run bookkeeping is engine-held, keyed by PR and head.

// flakeRerunRecord is an accepted re-run the gate is still waiting on.
type flakeRerunRecord struct {
	runIDs         []int64
	firstFailedIDs map[int64]bool // the stale-failure guard: failures with these IDs predate the re-run
	at             time.Time
}

// prFlakeState is the flake re-run bookkeeping for one PR head.
type prFlakeState struct {
	headSHA string
	r1Done  bool // the re-run before the first CI-fix dispatch has been spent
	r2Done  bool // the re-run after a recorded no-op fix has been spent
	// degraded is set once a re-run could not be made (no Actions run behind the
	// failure, permission refused, API error): the head falls back to today's
	// dispatch and the re-run is never retried for it (R4).
	degraded bool
	pending  *flakeRerunRecord
	touched  time.Time
}

// flakeAction is what ciFlakeRerun's decision tells its caller to do.
type flakeAction int

const (
	// flakeProceed: no re-run is owed — fall through to today's dispatch path.
	flakeProceed flakeAction = iota
	// flakeWait: a re-run is outstanding and only the stale original failure is
	// visible — claim the item without dispatching.
	flakeWait
	// flakeRerunR1: re-run the failed jobs before the first CI-fix dispatch.
	flakeRerunR1
	// flakeRerunR2: re-run the failed jobs after a recorded no-op fix.
	flakeRerunR2
)

// decideFlakeRerun is the pure decision for one eligible poll. It mutates st
// only to clear a settled pending record. inFlight is consulted lazily, and only
// once the settle dwell has elapsed with just the stale failure visible.
//
// The stale-failure guard mirrors pollTrainCI: a failing check run whose ID is
// not in firstFailedIDs is a failure the re-run produced and counts at once;
// otherwise the failure is the stale original, waited out for the settle dwell,
// then for as long as the re-run is still queued or running (bounded by
// maxWait). After that the stale failure is the verdict.
func decideFlakeRerun(st *prFlakeState, failed []gh.CheckRun, noOpRecorded bool, now time.Time, settleDwell, maxWait time.Duration, inFlight func(runIDs []int64) bool) flakeAction {
	if st.degraded {
		return flakeProceed
	}
	if rec := st.pending; rec != nil {
		if hasNewCheckRun(failed, rec.firstFailedIDs) {
			st.pending = nil
		} else {
			elapsed := now.Sub(rec.at)
			if elapsed < settleDwell {
				return flakeWait
			}
			if elapsed < settleDwell+maxWait && inFlight(rec.runIDs) {
				return flakeWait
			}
			st.pending = nil
		}
	}
	if noOpRecorded {
		if !st.r2Done {
			return flakeRerunR2
		}
		return flakeProceed
	}
	if !st.r1Done {
		return flakeRerunR1
	}
	return flakeProceed
}

// flakeStateForLocked returns the flake state for a PR, resetting it when the
// head SHA moved (a push is a fresh CI opportunity and a fresh budget, R3) and
// pruning entries past startupStateTTL. Caller holds flakeRerunMu.
func (e *Engine) flakeStateForLocked(repoStr string, prNum int, headSHA string, now time.Time) *prFlakeState {
	if e.flakeReruns == nil {
		e.flakeReruns = make(map[string]*prFlakeState)
	}
	for k, old := range e.flakeReruns {
		if now.Sub(old.touched) > startupStateTTL {
			delete(e.flakeReruns, k)
		}
	}
	key := startupWatchKey(repoStr, prNum)
	st := e.flakeReruns[key]
	if st == nil || st.headSHA != headSHA {
		st = &prFlakeState{headSHA: headSHA}
		e.flakeReruns[key] = st
	}
	st.touched = now
	return st
}

// SetCIFlakeRerunDisabledForTest turns the stage-gate flake re-run off, so a
// test can show that the R1/R2 assertions depend on it. Test-only: production
// never calls it.
func (e *Engine) SetCIFlakeRerunDisabledForTest(disabled bool) {
	e.flakeRerunMu.Lock()
	defer e.flakeRerunMu.Unlock()
	e.flakeRerunDisabled = disabled
}

// ciFlakeRerun is the stage gate's flake re-run step (#2072). It reports whether
// the item was claimed — a re-run was just made or is outstanding — in which
// case the caller must not dispatch a CI-fix worker. A false return means
// "proceed exactly as before": nothing is owed, the budget is spent, or a
// re-run could not be made (R4, never a pause or failure).
func (e *Engine) ciFlakeRerun(pctx *phase1Ctx, settle PRSettleResult) bool {
	pr := settle.PR
	if pr == nil || pr.HeadSHA == "" {
		return false
	}
	status, _, failed := gh.ClassifyCheckRuns(settle.CheckRuns)
	if status != gh.CheckRunsFailed || len(failed) == 0 {
		// A required-context or classic-status failure has no check run to re-run.
		return false
	}

	repoStr := itemOwnerRepoString(pctx.item, e.defaultRepo())
	owner, repo := itemOwnerRepo(pctx.item, e.defaultRepo())
	if owner == "" || repo == "" {
		return false
	}

	noOp := false
	if snap, err := e.store.Get(repoStr, pctx.item.Number); err == nil {
		if snap.Worker() != nil {
			// A reinvoke is in flight; dispatchWithCycleLimit logs and claims.
			return false
		}
		noOp = snap.LastCIFixNoOpSHA() == pr.HeadSHA
	}

	timing := e.ciInfraTimingOrDefault()
	now := e.now()
	short := pr.HeadSHA[:min(8, len(pr.HeadSHA))]

	// Everything that reads or writes the state, including marking the budget
	// spent, happens under the lock before any re-run API call: the poll loop and
	// settleAwaitingCIScan can both reach this code, and a head must never be
	// re-run twice (R3).
	//
	// rerunInFlight is a GitHub read, so it never runs under the lock (one slow
	// call would stall every other PR's flake handling). decideFlakeRerun is run
	// once with a probe that only notes the read is needed and answers "still in
	// flight" (which makes it return flakeWait without touching the state); the
	// read then happens unlocked, and the decision is re-taken against the state
	// as it is by then, with the probed answer.
	var (
		st             *prFlakeState
		action         flakeAction
		probeRunIDs    []int64
		inFlightNeeded bool
	)
	decide := func(inFlight func([]int64) bool) bool {
		e.flakeRerunMu.Lock()
		if e.flakeRerunDisabled {
			e.flakeRerunMu.Unlock()
			return false
		}
		st = e.flakeStateForLocked(repoStr, pr.Number, pr.HeadSHA, now)
		action = decideFlakeRerun(st, failed, noOp, now, timing.rerunSettleDwell, timing.rerunMaxWait, inFlight)
		return true
	}
	if !decide(func(runIDs []int64) bool {
		inFlightNeeded = true
		probeRunIDs = append([]int64(nil), runIDs...)
		return true
	}) {
		return false
	}
	if inFlightNeeded {
		e.flakeRerunMu.Unlock()
		inFlight := e.rerunInFlight(repoStr, owner, repo, pr.HeadSHA, probeRunIDs)
		if !decide(func([]int64) bool { return inFlight }) {
			return false
		}
	}
	var label string
	switch action {
	case flakeRerunR1:
		st.r1Done = true
		label = "R1"
	case flakeRerunR2:
		st.r2Done = true
		label = "R2"
	}
	if label != "" {
		st.pending = &flakeRerunRecord{firstFailedIDs: checkRunIDSet(failed), at: now}
	}
	e.flakeRerunMu.Unlock()

	switch action {
	case flakeWait:
		e.logf(pctx.item.Number, "ci-gate", "flake re-run outstanding on PR #%d head %s — waiting for its check runs instead of dispatching a CI-fix worker\n", pr.Number, short)
		return true
	case flakeRerunR1, flakeRerunR2:
		runIDs, ok := e.rerunFailedWorkflowRuns(repoStr, owner, repo, failed)
		e.flakeRerunMu.Lock()
		if !ok {
			// Degrade (R4): fall back to today's dispatch and never retry this head.
			// The budget stays spent so a refusal is not re-attempted every poll.
			st.degraded = true
			st.pending = nil
			e.flakeRerunMu.Unlock()
			e.logf(pctx.item.Number, "ci-gate", "flake re-run (%s) could not be made on PR #%d head %s — falling back to the CI-fix path for this head\n", label, pr.Number, short)
			return false
		}
		if st.pending != nil {
			st.pending.runIDs = runIDs
		}
		e.flakeRerunMu.Unlock()
		e.logf(pctx.item.Number, "ci-gate", "flake re-run (%s): re-ran failed jobs of workflow run(s) %v on PR #%d head %s — waiting instead of dispatching a CI-fix worker\n", label, runIDs, pr.Number, short)
		return true
	}
	return false
}
