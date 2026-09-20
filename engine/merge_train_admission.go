package engine

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// admitTrainMembers is the fresh-batch admission gate (#1821, ADR-1821): for each
// member fetchTrainMembers resolved, it reads the member's OWN PR check-runs at the
// snapshotted head SHA and classifies them with classifyLandingCI, unmodified. A
// member classified TrainCIRed is kept out of the returned slice and routed off
// Queued (deferRedMember) so the ordinary CI-fix path can address the failing check,
// rather than the failure being rediscovered hours later by a trial and a bisection.
//
// Polarity is fail-OPEN — the inverse of the singleton fast path's fail-closed rule
// (singletonFastPathEligible, ADR-1644). Only a positively confirmed red defers.
// Pending, green, zero check runs, a FetchCheckRuns error, or any other inability to
// classify all admit, byte-identical to pre-#1821 behavior: deferring on ambiguity
// would silently drain the queue and strand members that have done nothing wrong.
//
// Called only from prepareTrainWorker's fresh-batch fall-through, after
// reconstructTrainState has already handled both restart routes (R7) — it is never
// reached by completeDeferredLanding, resumeTrain, bisection sub-trials,
// landOneAtATime, or landGreenBatch.
//
// Precondition (R9): the gate is skipped entirely, admitting every member without a
// single read, unless the stage a deferred member is rerouted to has wait_for_ci
// enabled. checkCIGate is a no-op without it, so nothing would claim a rerouted red
// member — under yolo it would bounce straight back to Queued (and be re-deferred
// with a fresh comment every poll), under cruise it would strand.
func (e *Engine) admitTrainMembers(ctx context.Context, state *mergeTrainWorkerState, owner, repo string, members []trainMember) []trainMember {
	repoKey := owner + "/" + repo
	if len(members) == 0 {
		return members
	}

	target := stageBeforeHolding(e.cfg, holdingStage(e.cfg))
	if target == nil || target.WaitForCI == nil || !*target.WaitForCI {
		name := "(none)"
		if target != nil {
			name = target.Name
		}
		e.logfRepo(repoKey, "merge-train", "admission gate skipped for %s: reroute target stage %s does not have wait_for_ci — nothing would re-detect a deferred member's red CI, admitting all %d member(s)\n", repoKey, name, len(members))
		return members
	}

	admitted := make([]trainMember, 0, len(members))
	for i, m := range members {
		if ctx.Err() != nil {
			// Fail open: a cancelled worker is about to unwind anyway.
			e.logfRepo(repoKey, "merge-train", "admission gate interrupted by context cancellation — admitting remaining %d member(s) unchecked\n", len(members)-i)
			admitted = append(admitted, members[i:]...)
			break
		}

		runs, err := e.client.FetchCheckRuns(owner, repo, m.headSHA)
		if err != nil {
			e.logf(m.item.Number, "merge-train", "admitting #%d (fetch-error): could not read check runs for %s: %v\n", m.item.Number, m.headSHA, err)
			admitted = append(admitted, m)
			continue
		}

		// mergeableState is "" deliberately: red never depends on it (a check-run or
		// required-context failure is red unconditionally), and it only ever matters
		// for a zero-run GREEN, which this gate treats identically to pending — admit.
		result, detail := e.classifyLandingCI(owner, repo, "", m.headSHA, runs)
		if result != TrainCIRed {
			reason := "pending"
			if result == TrainCIGreen {
				reason = "green"
			}
			e.logf(m.item.Number, "merge-train", "admitting #%d (%s): %s\n", m.item.Number, reason, detail)
			e.clearCIDeferred(owner, repo, m.item.Number)
			admitted = append(admitted, m)
			continue
		}

		e.logf(m.item.Number, "merge-train", "deferring #%d (own PR CI confirmed red at %s): %s\n", m.item.Number, m.headSHA, detail)
		e.deferRedMember(state.projectID, owner, repo, m, runs, detail)
	}

	if len(admitted) != len(members) {
		e.logfRepo(repoKey, "merge-train", "admission gate: %d of %d member(s) admitted, %d deferred for own-PR CI red\n", len(admitted), len(members), len(members)-len(admitted))
	}
	return admitted
}

// deferRedMember routes a confirmed-red member off Queued (#1821 R4-R6, R9). The caller
// excludes m from the batch unconditionally — it is confirmed red — so a failed reroute
// still keeps it out of this batch; the next poll's fresh formation retries the whole
// operation.
//
// Unlike ejectMember it never touches mergeTrainEjectionCounts and never pauses (R5): a
// deferral is not train churn, and the failure is observable on the member's own PR
// check-runs, so the ordinary CI gate (checkCIGate -> ci-fix-reinvoke) has an external
// signal to act on. That is the reasoning ejectRedSingleton could not use (#1545) — its
// failure was only ever seen on a synthetic trial branch.
//
// Ordering mirrors ADR-1208/ADR-1545: reroute first; on failure nothing is posted,
// consumed, or recorded. On success any pending review-eject signal for the member is
// consumed (it would otherwise fire later on a re-queued member that already addressed
// the finding, and the member must not be double-routed), then the comment is posted —
// unless the same head SHA was already deferred (the in-memory ping-pong backstop).
func (e *Engine) deferRedMember(projectID, owner, repo string, m trainMember, runs []gh.CheckRun, detail string) {
	n := m.item.Number
	if !e.rerouteQueuedMemberOffHolding(projectID, m.item) {
		e.logf(n, "merge-train", "#%d is confirmed red but could not be rerouted off Queued — excluded from this batch, retrying on the next poll\n", n)
		return
	}

	repoKey := owner + "/" + repo
	if count, ok := e.takePendingReviewEject(repoKey, n); ok {
		e.logf(n, "merge-train", "dropping pending review-finding eject signal (%d finding(s)) for #%d — superseded by the CI deferral\n", count, n)
	}

	targetName := "the preceding stage"
	if target := stageBeforeHolding(e.cfg, holdingStage(e.cfg)); target != nil {
		targetName = target.Name
	}

	if !e.recordCIDeferred(owner, repo, n, m.headSHA) {
		e.logf(n, "merge-train", "#%d already deferred at head %s — rerouted again without a repeat comment\n", n, m.headSHA)
		return
	}

	msg := composeCIDeferralComment(n, m.prNum, m.headSHA, targetName, runs, detail)
	if _, err := e.client.AddComment(owner, repo, n, msg); err != nil {
		e.logf(n, "merge-train", "warn: could not post CI deferral comment: %v\n", err)
	}
	e.logf(n, "merge-train", "#%d deferred: rerouted to %s (not paused, no ejection counted)\n", n, targetName)
}

// composeCIDeferralComment renders the R6 comment. It is composed locally rather than via
// renderDiagnosticBlock/renderBatchContext/reentryInstruction: those carry wording that is
// wrong for this cause ("trial ..., integration PR #N" header, "moved base branch alone",
// and a recovery instruction that assumes a pause). Only renderFailedChecks/
// renderFailedContexts are shared, so failing checks are named identically to every other
// merge-train diagnostic (ADR-1420).
func composeCIDeferralComment(issueNum, prNum int, headSHA, targetName string, runs []gh.CheckRun, detail string) string {
	// Diagnostic only: the classifier has already said red, so this introduces no
	// independent notion of red. A required-context red has no failing runs to render,
	// so it falls back to the classifier's own detail string.
	var diag string
	if _, _, failed := gh.ClassifyCheckRuns(runs); len(failed) > 0 {
		diag = renderFailedChecks(failed)
	} else {
		diag = detail
	}

	sections := []string{
		fmt.Sprintf("#%d's own PR checks are failing at `%s` — this failure is the PR's own and is not a merge-train interaction, so it was kept out of the batch instead of being assembled into a trial that would fail for the same reason.", issueNum, headSHA),
		fmt.Sprintf("**Diagnostic** (PR #%d, head %s)\n\n%s", prNum, headSHA, diag),
		fmt.Sprintf("This issue has left the Queued column for %s and has **not** been paused. The ordinary CI gate will pick up the failing check(s) from %s and dispatch a fix automatically. Once CI is green and %s completes again, this issue will re-queue and rejoin a later batch.", targetName, targetName, targetName),
	}
	return "🏭 **Fabrik merge-train — deferred (own CI failing)**\n\n" + strings.Join(sections, "\n\n")
}

func ciDeferredKey(owner, repo string, n int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, n)
}

// recordCIDeferred records that issue n was deferred at headSHA and reports whether this
// is a NEW deferral (true) or a repeat of the same SHA (false) — the caller suppresses the
// comment on a repeat so a stale-cache bounce cannot produce a comment every poll (R9).
// In-memory only, like mergeTrainEjectionCounts; a restart costs at most one duplicate.
func (e *Engine) recordCIDeferred(owner, repo string, n int, headSHA string) bool {
	key := ciDeferredKey(owner, repo, n)
	e.mergeTrainCIDeferredMu.Lock()
	defer e.mergeTrainCIDeferredMu.Unlock()
	if e.mergeTrainCIDeferred == nil {
		e.mergeTrainCIDeferred = make(map[string]string)
	}
	if e.mergeTrainCIDeferred[key] == headSHA {
		return false
	}
	e.mergeTrainCIDeferred[key] = headSHA
	return true
}

// clearCIDeferred drops the dedupe record once the member's CI is no longer red, so a
// later regression on the same SHA-independent cycle is announced afresh.
func (e *Engine) clearCIDeferred(owner, repo string, n int) {
	e.mergeTrainCIDeferredMu.Lock()
	defer e.mergeTrainCIDeferredMu.Unlock()
	delete(e.mergeTrainCIDeferred, ciDeferredKey(owner, repo, n))
}
