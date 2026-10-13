package engine

import (
	"context"
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
)

// trialCIState is the per-trial CI-evaluation memory (#2052's startup watch and
// failed-job re-run bookkeeping, plus the last observed pending/failed runs for the
// timeout log line). One value belongs to exactly one trial: evalTrialCI mutates it in
// place, and the per-poll evaluator persists it with the trial record (#2051) so a
// retrigger or re-run budget already spent survives from one poll — and one daemon
// restart — to the next. The blocking pollTrainCI keeps a local one for its single call.
type trialCIState struct {
	sw             startupWatch
	rr             rerunState
	actionsRefused bool
	lastPending    []gh.CheckRun
	lastFailed     []gh.CheckRun
}

// evalTrialCI performs ONE evaluation of a trial PR's CI: the body of what used to be
// pollTrainCI's loop. decided=false means "no verdict yet — evaluate again later";
// decided=true carries the verdict (and, for red, the diagnostic that observed it).
// It never sleeps and never checks a deadline, so the blocking pollTrainCI (a sleep
// loop around it) and the per-poll evaluator (settleTrainRuns) share every decision —
// the CI classification, the #2052 retrigger and the single failed-job re-run — byte
// for byte.
func (e *Engine) evalTrialCI(ctx context.Context, owner, repo string, prNum int, trialSHA string, st *trialCIState) (TrainCIResult, *trainCIDiagnostic, bool) {
	timing := e.ciInfraTimingOrDefault()
	logRepo := owner + "/" + repo

	_, mergeableState, err := e.client.FetchPRMergeableFields(owner, repo, prNum)
	mergeableAccepted := false
	if err != nil {
		e.logfRepo(owner+"/"+repo, "merge-train", "warn: FetchPRMergeableFields failed for PR #%d: %v\n", prNum, err)
	} else if mergeableState == "dirty" {
		return TrainCIRed, &trainCIDiagnostic{
			Note:     "The trial branch stopped merging cleanly onto its base (mergeable_state \"dirty\") — the base moved again after the trial was assembled.",
			PRNum:    prNum,
			TrialSHA: trialSHA,
		}, true
	} else if gh.MergeableStateAccepted(mergeableState) {
		mergeableAccepted = true
	}

	// Check individual check runs via the shared classifier (mirrors
	// settlePRMergeState/checkCIGate in engine/ci.go — this used to be an
	// inline duplicate with its own dedup-by-ID drift; ClassifyCheckRuns
	// fixes that as a side effect of sharing it here). This is now
	// reachable on every iteration regardless of mergeable_state, so it
	// is the thing that actually determines completeness.
	//
	// #2052 R5: once the re-run's settle dwell has passed, whether one of the
	// re-run workflow runs is still queued or running MUST be read BEFORE the
	// check runs. Read the other way round, a re-run finishing between the two
	// reads is seen as "failure still latest" (stale check runs) plus "nothing
	// in flight" (fresh workflow runs), and the stale failure is judged the
	// second one. Read in this order, "not in flight" means the re-run had
	// already finished before the check-run read, so its check runs are in it.
	rerunPastDwell := st.rr.done && timing.now().Sub(st.rr.at) >= timing.rerunSettleDwell
	rerunWaiting := rerunPastDwell && timing.now().Sub(st.rr.at) < timing.rerunSettleDwell+timing.rerunMaxWait &&
		e.rerunInFlight(logRepo, owner, repo, trialSHA, st.rr.runIDs)

	checkRuns, err := e.client.FetchCheckRuns(owner, repo, trialSHA)
	if err != nil {
		e.logfRepo(owner+"/"+repo, "merge-train", "warn: FetchCheckRuns failed for %s: %v\n", trialSHA, err)
	} else if len(checkRuns) > 0 {
		status, pending, failed := gh.ClassifyCheckRuns(checkRuns)
		st.lastPending, st.lastFailed = pending, failed
		if status == gh.CheckRunsFailed {
			// Strict non-required-failure policy (adrs/1153-*.md): any
			// confirmed check-run failure blocks the train, required or
			// not — Fabrik has no general way to distinguish the two
			// beyond the opt-in RequiredStatusContexts config, and a
			// wrong-direction Strict call costs one bisection cycle, not
			// a silently reintroduced version of this issue.
			//
			// #2052 R5 softens "any failure is red" by exactly one re-run: the first
			// failure re-runs the failed jobs and keeps polling, only a second failure
			// is red. A failure with no re-runnable Actions run behind it, or a re-run
			// request that errors, is red at once, exactly as before.
			if !st.rr.done {
				if rerunIDs, ok := e.rerunFailedWorkflowRuns(logRepo, owner, repo, failed); ok {
					st.rr = rerunState{done: true, runIDs: rerunIDs, firstFailedIDs: checkRunIDSet(failed), at: timing.now()}
					e.logfRepo(logRepo, "merge-train", "trial %s: failed check(s): %s — re-running the failed jobs once before judging the trial red\n", trialSHA, describeCheckRuns(failed))
				} else {
					e.logfRepo(logRepo, "merge-train", "trial %s red — failed check(s): %s\n", trialSHA, describeCheckRuns(failed))
					return TrainCIRed, &trainCIDiagnostic{FailedChecks: failed, PRNum: prNum, TrialSHA: trialSHA}, true
				}
			} else if hasNewCheckRun(failed, st.rr.firstFailedIDs) || (rerunPastDwell && !rerunWaiting) {
				// A failing latest-per-name run the first failure did not contain means the
				// re-run itself failed; the dwell bounds the case where the re-run never
				// materialised and only the stale original is visible. A workflow run still
				// queued or running on the SHA means the re-run is merely waiting for a
				// runner, so the stale failure is not yet a verdict — for at most
				// rerunMaxWait past the dwell, its own bound rather than CIBackstopTimeout.
				e.logfRepo(logRepo, "merge-train", "trial %s red after the failed-job re-run — failed check(s): %s\n", trialSHA, describeCheckRuns(failed))
				return TrainCIRed, &trainCIDiagnostic{FailedChecks: failed, PRNum: prNum, TrialSHA: trialSHA}, true
			}
			// Otherwise the re-run was accepted but its check runs have not replaced
			// the stale failure yet: keep polling rather than count the same failure twice.
		}
		if status == gh.CheckRunsReady {
			// ADR-933: don't declare the trial green until any configured
			// required context has confirmed success on this exact trial
			// SHA — mirrors settlePRMergeState's guard in pr_settle.go. A
			// required context that's merely missing/pending falls through
			// to keep polling (nothing has regressed).
			rcStatus, _, _, rcFailed := e.classifyRequiredContexts(0, owner, repo, trialSHA, checkRuns)
			switch rcStatus {
			case gh.RequiredContextsSatisfied:
				// #1822: same suite-aware completeness rule as
				// classifyLandingCI — an all-green run set on a trial SHA can
				// still be a prefix while a needs:-dependent job is queued for a
				// runner. Hold and keep polling; CIBackstopTimeout bounds it.
				if hold, why := e.ciSuiteHold(e.client, owner, repo, trialSHA); hold {
					e.logfRepo(owner+"/"+repo, "merge-train", "trial %s checks green but %s — still waiting\n", trialSHA, why)
					break
				}
				e.logfRepo(owner+"/"+repo, "merge-train", "trial %s green — checks: %s\n", trialSHA, describeCheckRuns(checkRuns))
				return TrainCIGreen, nil, true
			case gh.RequiredContextsFailed:
				e.logfRepo(owner+"/"+repo, "merge-train", "required status context(s) failed for %s: %v\n", trialSHA, rcFailed)
				return TrainCIRed, &trainCIDiagnostic{FailedContexts: rcFailed, PRNum: prNum, TrialSHA: trialSHA}, true
			}
		}
		// CheckRunsPending (or a required context still pending above):
		// fall through and keep polling — this is the #1150 case, a
		// non-required check still queued/in_progress while
		// mergeable_state already reads accepted.
	} else {
		// ADR-933: zero check runs at all (e.g. GitHub Actions disabled —
		// the local-CI-takeover case #933 was filed for) must still be
		// checked against configured required contexts, mirroring
		// settlePRMergeState's zero-check-runs branch (pr_settle.go rule
		// 13). Without this, a confirmed required-context failure on a
		// trial branch with no check-run footprint at all would never
		// resolve to TrainCIRed — it would just poll to CIBackstopTimeout and
		// return TrainCIPending, stalling the batch instead of ejecting
		// the poisoning member. A merely missing/pending required context
		// is not short-circuited here — it keeps polling like any other
		// not-yet-settled signal.
		rcStatus, _, _, rcFailed := e.classifyRequiredContexts(0, owner, repo, trialSHA, nil)
		if rcStatus == gh.RequiredContextsFailed {
			e.logfRepo(owner+"/"+repo, "merge-train", "required status context(s) failed for %s: %v\n", trialSHA, rcFailed)
			return TrainCIRed, &trainCIDiagnostic{FailedContexts: rcFailed, PRNum: prNum, TrialSHA: trialSHA}, true
		}
		// #2052: zero check runs may be a CI run that never started (a
		// startup_failure creates no job, so no check run). Read the workflow
		// runs — only on this path, so the extra API cost is bounded — BEFORE the
		// green shortcut below, which would otherwise land a trial whose CI never
		// ran whenever branch protection requires no checks. A refused or failed
		// read behaves exactly as before.
		holdForCI := false
		if !st.actionsRefused {
			runs, ok, refused := e.fetchWorkflowRunsSoft(logRepo, owner, repo, trialSHA)
			st.actionsRefused = refused
			if ok {
				now := timing.now()
				action, run := st.sw.observe(runs, now, timing.retriggerNewRunDwell)
				switch action {
				case startupRetrigger:
					e.logfRepo(logRepo, "merge-train", "trial %s: CI never started — %s; retriggering by closing and reopening PR #%d (retrigger %d/%d)\n",
						trialSHA, describeStartupRun(run), prNum, st.sw.retriggers+1, maxCIRetriggers)
					if err := e.retriggerPR(logRepo, owner, repo, prNum); err != nil {
						e.logfRepo(logRepo, "merge-train", "trial %s: %v — abandoning the trial\n", trialSHA, err)
						return TrainCIInfra, &trainCIDiagnostic{Note: err.Error(), PRNum: prNum, TrialSHA: trialSHA}, true
					}
					st.sw.retriggered(runs, timing.now())
					holdForCI = true
				case startupWait:
					holdForCI = true
				case startupAbandon:
					note := fmt.Sprintf("CI never started on the trial after %d retrigger(s): %s", st.sw.retriggers, describeStartupRun(run))
					e.logfRepo(logRepo, "merge-train", "trial %s: %s — abandoning the trial; members stay Queued, nothing charged\n", trialSHA, note)
					return TrainCIInfra, &trainCIDiagnostic{Note: note, PRNum: prNum, TrialSHA: trialSHA}, true
				default:
					// A workflow run that is queued or running is CI on its way: with no
					// check run yet there is nothing else to say so, and the green shortcut
					// would read the absence as completeness. Only those two states hold —
					// a run in `waiting` (environment approval) or `pending` is not CI on
					// its way, and holding for it would sit on the worker slot to the 4h
					// backstop where the trial used to go green.
					for _, r := range runs {
						if r.Status == "queued" || r.Status == "in_progress" {
							e.logfRepo(logRepo, "merge-train", "trial %s has zero check runs but %s is %s — still waiting\n", trialSHA, describeStartupRun(r), r.Status)
							holdForCI = true
							break
						}
					}
				}
			}
		}
		// #1153: with zero check runs there is no per-check completeness
		// signal to consult at all, so an accepted mergeable_state is the
		// only remaining evidence that nothing is outstanding — this is
		// the one place mergeable_state is genuinely load-bearing for
		// green.
		if !holdForCI && mergeableAccepted && rcStatus == gh.RequiredContextsSatisfied {
			// #1822: see the all-green branch above.
			if hold, why := e.ciSuiteHold(e.client, owner, repo, trialSHA); hold {
				e.logfRepo(owner+"/"+repo, "merge-train", "trial %s has zero check runs but %s — still waiting\n", trialSHA, why)
			} else {
				e.logfRepo(owner+"/"+repo, "merge-train", "trial %s green — mergeable_state %q accepted, zero check runs, required contexts satisfied\n", trialSHA, mergeableState)
				return TrainCIGreen, nil, true
			}
		}
	}
	return TrainCIPending, nil, false
}

// logTrialCITimeout logs the CI-wait timeout line for a trial PR, naming the last
// observed pending and failed runs when there were any.
func (e *Engine) logTrialCITimeout(owner, repo string, prNum int, st *trialCIState) {
	if len(st.lastFailed) > 0 || len(st.lastPending) > 0 {
		e.logfRepo(owner+"/"+repo, "merge-train", "CI wait timeout for integration PR #%d — pending: %s; failed: %s\n",
			prNum, describeCheckRuns(st.lastPending), describeCheckRuns(st.lastFailed))
	} else {
		e.logfRepo(owner+"/"+repo, "merge-train", "CI wait timeout for integration PR #%d\n", prNum)
	}
}
