package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// CI infrastructure failures (#2052, ADR 2052).
//
// Two failures of the CI *provider*, not of the code under test, used to be
// read as the members' fault or as "still pending":
//
//   - a workflow run that fails before creating any job (`startup_failure`)
//     leaves zero check runs on the commit, which every gate reads as "CI has
//     not reported yet" and waits out to the 4h backstop — or, with no required
//     checks, as a green;
//   - a single flaky test turns a trial red and starts a full bisection.
//
// This file holds the pure decision logic and the small engine helpers; the
// call sites are pollTrainCI (the merge train's trial) and settlePRMergeState
// (a stage's own PR under wait_for_ci).

const (
	// maxCIRetriggers bounds how many times one trial (or one stage PR head)
	// is retriggered by close/reopen before it is abandoned (R3).
	maxCIRetriggers = 2

	// defaultCIRetriggerNewRunDwell is how long to wait, after a retrigger, for
	// a *new* workflow run to appear before concluding the reopen started
	// nothing — for example a workflow whose `pull_request` `types:` omits
	// `reopened`. Minutes, nowhere near CIBackstopTimeout (R4).
	defaultCIRetriggerNewRunDwell = 5 * time.Minute

	// defaultTrainRerunSettleDwell bounds how long a trial waits, after a
	// re-run of its failed jobs was accepted, for the re-run's check runs to
	// replace the original failures before the original failure is accepted as
	// the verdict (R5; see pollTrainCI's guard).
	defaultTrainRerunSettleDwell = 2 * time.Minute

	// defaultTrainRerunMaxWait caps how long, past the settle dwell, a trial keeps
	// waiting for a re-run that is still queued or running before the stale first
	// failure becomes the verdict. It is its own bound, far below
	// CIBackstopTimeout, so one wedged re-run cannot hold a worker slot for hours.
	defaultTrainRerunMaxWait = 30 * time.Minute

	// defaultTrainInfraAbandonCooldown delays re-dispatching a (repo, base)
	// partition's train after a trial was abandoned for CI infrastructure, so a
	// permanently broken workflow retries every few minutes instead of every
	// poll. Nothing is charged for an abandon, so the runaway guard cannot
	// bound this.
	defaultTrainInfraAbandonCooldown = 5 * time.Minute

	// defaultCIReopenBackoff is the pause between attempts to reopen a PR the
	// retrigger just closed.
	defaultCIReopenBackoff = time.Second

	// reopenAttempts is how many times retriggerPR tries to reopen a PR it
	// closed before giving up and reporting an error.
	reopenAttempts = 3
)

// ciInfraTiming holds the dwells above so a test can shrink them. A zero field
// means "use the default".
type ciInfraTiming struct {
	retriggerNewRunDwell time.Duration
	rerunSettleDwell     time.Duration
	rerunMaxWait         time.Duration
	abandonCooldown      time.Duration
	reopenBackoff        time.Duration

	// clock, when set (tests only), is the time source of pollTrainCI's retrigger
	// and re-run dwells. It is deliberately not the engine clock: the sim's engine
	// clock advances only between polls, so a dwell inside one worker's polling
	// loop would never elapse on it.
	clock Clock
}

// now is the time source for the dwells above: the injected clock, else real time.
func (t ciInfraTiming) now() time.Time {
	if t.clock != nil {
		return t.clock.Now()
	}
	return time.Now()
}

func (e *Engine) ciInfraTimingOrDefault() ciInfraTiming {
	t := e.ciInfraTiming
	if t.retriggerNewRunDwell <= 0 {
		t.retriggerNewRunDwell = defaultCIRetriggerNewRunDwell
	}
	if t.rerunSettleDwell <= 0 {
		t.rerunSettleDwell = defaultTrainRerunSettleDwell
	}
	if t.rerunMaxWait <= 0 {
		t.rerunMaxWait = defaultTrainRerunMaxWait
	}
	if t.abandonCooldown <= 0 {
		t.abandonCooldown = defaultTrainInfraAbandonCooldown
	}
	if t.reopenBackoff == 0 {
		t.reopenBackoff = defaultCIReopenBackoff
	} else if t.reopenBackoff < 0 {
		t.reopenBackoff = 0
	}
	return t
}

// SetCIInfraTimingForTest overrides the CI infrastructure dwells. Pass a
// negative reopenBackoff for "no pause"; a zero argument keeps that dwell's
// default. Test-only: production never calls it.
func (e *Engine) SetCIInfraTimingForTest(retriggerNewRunDwell, rerunSettleDwell, abandonCooldown, reopenBackoff time.Duration) {
	e.ciInfraTiming = ciInfraTiming{
		retriggerNewRunDwell: retriggerNewRunDwell,
		rerunSettleDwell:     rerunSettleDwell,
		abandonCooldown:      abandonCooldown,
		reopenBackoff:        reopenBackoff,
		clock:                e.ciInfraTiming.clock,
	}
}

// SetCIInfraClockForTest makes pollTrainCI's retrigger and re-run dwells elapse
// on c instead of real time, so a test advances them explicitly. Test-only.
func (e *Engine) SetCIInfraClockForTest(c Clock) {
	e.ciInfraTiming.clock = c
}

// SetRerunMaxWaitForTest overrides the cap on waiting for an in-flight re-run.
// Test-only: production never calls it.
func (e *Engine) SetRerunMaxWaitForTest(d time.Duration) {
	e.ciInfraTiming.rerunMaxWait = d
}

// isStartupFailure reports whether a workflow run is a CI startup failure
// (R1): completed, and either concluded `startup_failure` or created no jobs.
//
// A completed zero-job run concluded `cancelled` is NOT one: that is what a
// concurrency group's superseded-run cancellation leaves behind (#2033), a run
// someone chose to stop rather than one the provider failed to start. Treating
// it as infrastructure would close and reopen a PR over a deliberate cancel.
// A genuine startup failure that reports `cancelled` falls through to the
// pre-#2052 behaviour.
//
// The zero-job fallback is an allow-list: only a `failure` conclusion counts.
// Other zero-job completions are not provider failures — `action_required` is a
// run held for a human's approval (a first-time or fork contributor), `skipped`
// and `stale` are deliberate, `neutral`/`success` are not failures at all — and
// closing and reopening a PR over any of them would burn the retrigger budget
// and then abandon or pause on a run that needs no retrigger.
func isStartupFailure(run gh.WorkflowRun) bool {
	if run.Status != "completed" {
		return false
	}
	if run.Conclusion == "startup_failure" {
		return true
	}
	return run.JobCount == 0 && run.Conclusion == "failure"
}

// startupAction is what a startupWatch tells its caller to do next.
type startupAction int

const (
	// startupNone: no startup failure in view — carry on exactly as before.
	startupNone startupAction = iota
	// startupRetrigger: a fresh startup-failed run was seen and budget remains —
	// close and reopen the PR, then call retriggered.
	startupRetrigger
	// startupWait: a retrigger was issued and no new run has appeared yet, but
	// the dwell has not elapsed — keep polling.
	startupWait
	// startupAbandon: CI has not started and retriggering has not helped (budget
	// spent, or no new run appeared within the dwell) — give up.
	startupAbandon
)

// startupWatch is the per-trial (or per-stage-PR-head) retrigger state. It is
// owned by one goroutine and held in that caller's local state — never shared —
// so every bisection sub-trial gets its own.
type startupWatch struct {
	retriggers    int
	baseline      map[int64]bool // run IDs that existed when the last retrigger was issued
	retriggeredAt time.Time
	lastFailed    gh.WorkflowRun // the startup-failed run most recently acted on
}

// observe classifies the current workflow runs for the head SHA.
//
// R3a: after a retrigger the startup-failed run is still on the same head SHA
// and would satisfy isStartupFailure on every later poll, burning the budget in
// one loop. observe therefore considers only runs whose ID was not present when
// the last retrigger was issued.
func (w *startupWatch) observe(runs []gh.WorkflowRun, now time.Time, newRunDwell time.Duration) (startupAction, gh.WorkflowRun) {
	var fresh []gh.WorkflowRun
	for _, r := range runs {
		if !w.baseline[r.ID] {
			fresh = append(fresh, r)
		}
	}
	for _, r := range fresh {
		if !isStartupFailure(r) {
			continue
		}
		w.lastFailed = r
		if w.retriggers < maxCIRetriggers {
			return startupRetrigger, r
		}
		return startupAbandon, r
	}
	if w.retriggers == 0 {
		return startupNone, gh.WorkflowRun{}
	}
	if len(fresh) > 0 {
		// A new run exists and is not (yet) a startup failure: CI has started.
		return startupNone, gh.WorkflowRun{}
	}
	if now.Sub(w.retriggeredAt) >= newRunDwell {
		return startupAbandon, w.lastFailed
	}
	return startupWait, w.lastFailed
}

// retriggered records that the caller closed and reopened the PR: it counts the
// retrigger and snapshots every run ID visible now as the new baseline.
func (w *startupWatch) retriggered(runs []gh.WorkflowRun, now time.Time) {
	w.retriggers++
	w.retriggeredAt = now
	w.baseline = make(map[int64]bool, len(runs))
	for _, r := range runs {
		w.baseline[r.ID] = true
	}
}

// describeStartupRun names a workflow run for logs and pause messages.
func describeStartupRun(r gh.WorkflowRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "workflow run %d", r.ID)
	if r.Name != "" {
		fmt.Fprintf(&b, " (%s)", r.Name)
	}
	if r.HTMLURL != "" {
		fmt.Fprintf(&b, " %s", r.HTMLURL)
	}
	if r.Conclusion != "" {
		fmt.Fprintf(&b, " — %s", r.Conclusion)
	}
	return b.String()
}

// isPermissionRefusal reports whether err is GitHub refusing the credential
// access to an endpoint (403/404) — the soft, degradable condition for the
// optional `actions` permission (R7).
func isPermissionRefusal(err error) bool {
	return errors.Is(err, gh.ErrForbidden) || errors.Is(err, gh.ErrNotFound)
}

// fetchWorkflowRunsSoft reads the workflow runs for a head SHA through the live
// client. ok is false when the read failed; a permission refusal additionally
// logs once per engine that startup-failure detection and flake re-run are
// disabled (R7). Callers behave exactly as before #2052 on !ok.
func (e *Engine) fetchWorkflowRunsSoft(logRepo, owner, repo, sha string) (runs []gh.WorkflowRun, ok bool, refused bool) {
	runs, err := e.client.FetchWorkflowRuns(owner, repo, sha)
	if err == nil {
		return runs, true, false
	}
	if isPermissionRefusal(err) {
		e.logActionsDegradedOnce(logRepo, err)
		return nil, false, true
	}
	e.logfRepo(logRepo, "ci-infra", "warn: FetchWorkflowRuns failed for %s: %v\n", sha, err)
	return nil, false, false
}

// logActionsDegradedOnce logs, once per process, that the Actions permission was
// refused at runtime and CI-infrastructure handling is therefore off. Startup
// guarantees `actions: write` was granted (#2105), so this means it was revoked
// or the request was never accepted after startup.
func (e *Engine) logActionsDegradedOnce(logRepo string, err error) {
	if e.actionsDegradeLogged.CompareAndSwap(false, true) {
		e.logfRepo(logRepo, "ci-infra", "warn: the GitHub credential was refused the `actions` permission (%v) — it was granted at startup, so it has likely been revoked since. CI startup-failure detection and failed-job re-run are disabled until `actions: write` is granted again.\n", err)
	}
}

// retriggerPR closes and immediately reopens a PR so GitHub fires a fresh
// `pull_request` event (a run that never started cannot be re-run). The window
// in which the PR is closed is kept as short as possible: reopen follows the
// close with no work in between, and a failed reopen is retried. A PR that
// could not be reopened is reported as an error and the caller abandons it.
// A failed close leaves the PR open and untouched.
func (e *Engine) retriggerPR(logRepo, owner, repo string, prNum int) error {
	if err := e.client.CloseIssue(owner, repo, prNum); err != nil {
		return fmt.Errorf("closing PR #%d to retrigger CI: %w", prNum, err)
	}
	backoff := e.ciInfraTimingOrDefault().reopenBackoff
	var err error
	for attempt := 1; attempt <= reopenAttempts; attempt++ {
		if err = e.client.ReopenIssue(owner, repo, prNum); err == nil {
			return nil
		}
		e.logfRepo(logRepo, "ci-infra", "warn: reopening PR #%d (attempt %d/%d) failed: %v\n", prNum, attempt, reopenAttempts, err)
		if attempt < reopenAttempts && backoff > 0 {
			time.Sleep(backoff)
		}
	}
	return fmt.Errorf("PR #%d was closed to retrigger CI but could not be reopened: %w", prNum, err)
}

// rerunState is the per-trial single-re-run bookkeeping for R5.
type rerunState struct {
	done           bool
	runIDs         []int64 // the workflow runs whose failed jobs were re-run
	firstFailedIDs map[int64]bool
	at             time.Time
}

// checkRunIDSet returns the set of check-run IDs in runs.
func checkRunIDSet(runs []gh.CheckRun) map[int64]bool {
	out := make(map[int64]bool, len(runs))
	for _, r := range runs {
		out[r.ID] = true
	}
	return out
}

// hasNewCheckRun reports whether any run in failed has an ID not in seen — i.e.
// a failure produced by the re-run rather than the stale original.
func hasNewCheckRun(failed []gh.CheckRun, seen map[int64]bool) bool {
	for _, r := range failed {
		if !seen[r.ID] {
			return true
		}
	}
	return false
}

// rerunInFlight reports whether one of the workflow runs that were re-run
// (runIDs) is still queued or running on sha. pollTrainCI consults it once the
// re-run settle dwell has elapsed with only the stale original failure visible,
// so a re-run waiting behind runner capacity is waited for instead of the stale
// failure being counted as the second one. Only the re-run runs count, and only
// in the `queued`/`in_progress` states: an unrelated workflow, or a run held in
// `waiting`/`pending` for an approval, would otherwise postpone the verdict
// indefinitely. The caller bounds the wait with rerunMaxWait. A read error or
// refused permission reports false: the caller then falls back to the dwell's
// verdict, as before.
func (e *Engine) rerunInFlight(logRepo, owner, repo, sha string, runIDs []int64) bool {
	if len(runIDs) == 0 {
		return false
	}
	runs, ok, _ := e.fetchWorkflowRunsSoft(logRepo, owner, repo, sha)
	if !ok {
		return false
	}
	reran := make(map[int64]bool, len(runIDs))
	for _, id := range runIDs {
		reran[id] = true
	}
	for _, r := range runs {
		if reran[r.ID] && (r.Status == "queued" || r.Status == "in_progress") {
			return true
		}
	}
	return false
}

// rerunFailedWorkflowRuns asks GitHub to re-run the failed jobs of every
// workflow run behind the failed check runs (R5). It returns false — and the
// caller keeps today's behaviour, red — when any failed check has no Actions
// workflow run behind it (a third-party status), or any re-run request errors.
// On success it returns the IDs of the workflow runs that were re-run.
func (e *Engine) rerunFailedWorkflowRuns(logRepo, owner, repo string, failed []gh.CheckRun) ([]int64, bool) {
	var runIDs []int64
	seen := map[int64]bool{}
	for _, cr := range failed {
		id, ok := gh.ActionsRunIDFromDetailsURL(cr.DetailsURL)
		if !ok {
			e.logfRepo(logRepo, "ci-infra", "failed check %q has no re-runnable Actions workflow run (details: %q) — treating the failure as real\n", cr.Name, cr.DetailsURL)
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			runIDs = append(runIDs, id)
		}
	}
	if len(runIDs) == 0 {
		return nil, false
	}
	for _, id := range runIDs {
		if err := e.client.RerunFailedJobs(owner, repo, id); err != nil {
			if isPermissionRefusal(err) {
				e.logActionsDegradedOnce(logRepo, err)
			}
			e.logfRepo(logRepo, "ci-infra", "warn: re-running failed jobs of workflow run %d failed: %v — treating the failure as real\n", id, err)
			return nil, false
		}
	}
	return runIDs, true
}

// infraNote renders a TrainCIInfra diagnostic's note for logs.
func infraNote(diag *trainCIDiagnostic) string {
	if diag == nil || diag.Note == "" {
		return "no detail"
	}
	return diag.Note
}

// markInfraAbandon starts the post-abandon cooldown for a train partition.
func (e *Engine) markInfraAbandon(trainKey string) {
	until := time.Now().Add(e.ciInfraTimingOrDefault().abandonCooldown)
	e.mergeTrainInfraMu.Lock()
	defer e.mergeTrainInfraMu.Unlock()
	if e.mergeTrainInfraCooldown == nil {
		e.mergeTrainInfraCooldown = make(map[string]time.Time)
	}
	e.mergeTrainInfraCooldown[trainKey] = until
}

// infraCooldownRemaining reports whether trainKey is still inside its
// post-abandon cooldown and how long remains. An elapsed entry is dropped.
func (e *Engine) infraCooldownRemaining(trainKey string) (time.Duration, bool) {
	e.mergeTrainInfraMu.Lock()
	defer e.mergeTrainInfraMu.Unlock()
	until, ok := e.mergeTrainInfraCooldown[trainKey]
	if !ok {
		return 0, false
	}
	remaining := time.Until(until)
	if remaining <= 0 {
		delete(e.mergeTrainInfraCooldown, trainKey)
		return 0, false
	}
	return remaining, true
}

// prStartupState is the stage gate's retrigger bookkeeping for one PR head.
type prStartupState struct {
	headSHA       string
	watch         startupWatch
	lastRetrigger time.Time
	touched       time.Time // last consultation, for pruning
}

// startupStateTTL is how long a PR's retrigger state is kept after the gate last
// consulted it. Entries are only ever created for PRs on the zero-check-run
// path, and nothing else removes them (a merged or closed PR is simply never
// consulted again), so each consultation prunes the entries past this age.
const startupStateTTL = 24 * time.Hour

// ciRetriggerGrace is how long after a retrigger a closed PR reads as
// transient rather than "closed without merging" (see settlePRMergeStateWith).
const ciRetriggerGrace = 2 * time.Minute

func startupWatchKey(repoStr string, prNum int) string {
	return fmt.Sprintf("%s#%d", repoStr, prNum)
}

// startupStateFor returns the retrigger state for a PR, resetting it when the
// head SHA moved (a push is a fresh CI opportunity). Caller holds startupWatchMu.
func (e *Engine) startupStateForLocked(repoStr string, prNum int, headSHA string) *prStartupState {
	if e.startupWatches == nil {
		e.startupWatches = make(map[string]*prStartupState)
	}
	now := time.Now()
	for k, old := range e.startupWatches {
		if now.Sub(old.touched) > startupStateTTL {
			delete(e.startupWatches, k)
		}
	}
	key := startupWatchKey(repoStr, prNum)
	st := e.startupWatches[key]
	if st == nil || st.headSHA != headSHA {
		st = &prStartupState{headSHA: headSHA}
		e.startupWatches[key] = st
	}
	st.touched = now
	return st
}

// resetStartupState forgets a PR's retrigger state. The stage gate calls it when
// it pauses an item for a CI startup failure: the human who resumes it has
// usually fixed the workflow on the base branch, which does not move the PR's
// head SHA, so without a reset the spent budget and the old baseline would make
// the next consultation abandon straight away, with no retrigger, and re-pause.
func (e *Engine) resetStartupState(repoStr string, prNum int) {
	e.startupWatchMu.Lock()
	defer e.startupWatchMu.Unlock()
	delete(e.startupWatches, startupWatchKey(repoStr, prNum))
}

// ciRetriggerGraceActive reports whether the engine retriggered this PR within
// ciRetriggerGrace.
func (e *Engine) ciRetriggerGraceActive(repoStr string, prNum int) bool {
	e.startupWatchMu.Lock()
	defer e.startupWatchMu.Unlock()
	st := e.startupWatches[startupWatchKey(repoStr, prNum)]
	return st != nil && !st.lastRetrigger.IsZero() && time.Since(st.lastRetrigger) < ciRetriggerGrace
}

// ciStartupCheck is the stage gate's counterpart of pollTrainCI's zero-check-run
// handling (#2052 R6): the PR head has no check runs, so read its workflow runs
// and act on a startup failure. handled is false when nothing needs doing (no
// startup failure in view, or the read was refused or failed) and the caller
// continues exactly as before.
func (e *Engine) ciStartupCheck(item gh.ProjectItem, owner, repo, repoStr string, pr *gh.PRDetails) (PRSettleResult, bool) {
	runs, ok, _ := e.fetchWorkflowRunsSoft(repoStr, owner, repo, pr.HeadSHA)
	if !ok {
		return PRSettleResult{}, false
	}
	timing := e.ciInfraTimingOrDefault()
	now := time.Now()

	e.startupWatchMu.Lock()
	st := e.startupStateForLocked(repoStr, pr.Number, pr.HeadSHA)
	action, run := st.watch.observe(runs, now, timing.retriggerNewRunDwell)
	retriggers := st.watch.retriggers
	e.startupWatchMu.Unlock()

	switch action {
	case startupRetrigger:
		e.logf(item.Number, "ci-gate", "CI never started on PR #%d — %s; retriggering by closing and reopening it (retrigger %d/%d)\n",
			pr.Number, describeStartupRun(run), retriggers+1, maxCIRetriggers)
		// Mark the retrigger window before closing, so no read in between can see a
		// closed PR as terminal.
		e.startupWatchMu.Lock()
		st.lastRetrigger = time.Now()
		e.startupWatchMu.Unlock()
		if err := e.retriggerPR(repoStr, owner, repo, pr.Number); err != nil {
			e.logf(item.Number, "ci-gate", "%v\n", err)
			return PRSettleResult{
				Status: PRMergeUnsettled, Reason: err.Error(), PR: pr,
				StartupFailure: &CIStartupFailure{Run: run, PRNum: pr.Number, Retriggers: retriggers, ReopenErr: err},
			}, true
		}
		e.startupWatchMu.Lock()
		st.watch.retriggered(runs, time.Now())
		e.startupWatchMu.Unlock()
		// MergeableState omitted: this is a wait for a fresh run, not an R3 case.
		return PRSettleResult{Status: PRMergeUnsettled, Reason: "CI startup failure — retriggered", PR: pr}, true
	case startupWait:
		return PRSettleResult{Status: PRMergeUnsettled, Reason: "CI startup failure — waiting for the retriggered run", PR: pr}, true
	case startupAbandon:
		e.logf(item.Number, "ci-gate", "CI never started on PR #%d after %d retrigger(s) — %s\n", pr.Number, retriggers, describeStartupRun(run))
		return PRSettleResult{
			Status: PRMergeUnsettled, Reason: "CI startup failure — retriggering did not help", PR: pr,
			StartupFailure: &CIStartupFailure{Run: run, PRNum: pr.Number, Retriggers: retriggers},
		}, true
	}
	return PRSettleResult{}, false
}
