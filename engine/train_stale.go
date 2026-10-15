package engine

import (
	"context"
	"fmt"
)

// ─── Stale-trial rebuild (#2298, ADR 2298) ────────────────────────────────────────────
//
// Every trial is assembled off a base SHA pinned at batch start, while its draft CI PR
// targets the live base branch. When the base advances conflictingly mid-CI, GitHub
// reports the trial PR "dirty" — a fact about the trial's age, not about the members.
// evalTrialCI classifies that as TrainCIStale (see dirtyTrialVerdict), and every verdict
// consumer of the run state machine hands it to handleStaleTrial, BEFORE the red/green
// outcome functions run. Routing it there is what makes it uncharged by construction: it
// never reaches recordTrialIfCounts, Bisect.spent/apply, newBisectState, ejectRedSingleton
// (and so the #2045 auto-repair), ejectMember or markInfraAbandon.
//
// A stale trial is abandoned (PR closed, branch deleted), the base is re-pinned to the live
// origin/<base> and the same member set is re-formed on it, spending one cycle of the run's
// persisted RebaseCycles — the budget landGreenBatchRun shares, bounded by
// effectiveMaxTrainRebaseCycles. When the budget is spent the batch is dissolved back to
// Queued, uncharged, as landGreenBatch does.

// baseProbe names the base and trial branches evalTrialCI compares for trial t.
func (r *trainRun) baseProbe(t *runTrialRecord) trialBaseProbe {
	if t == nil || t.Name == "" {
		return trialBaseProbe{}
	}
	return trialBaseProbe{baseBranch: r.p.baseBranch, trialBranch: trainBranchPrefix + t.Name}
}

// repinBase re-pins the run's base to the live origin/<base> (a no-op under the
// trainValidateFn seam, where there is no git) and returns the old and new pinned SHAs.
// FetchOrigin is serialised under wm.mu, since the WorktreeManager is shared by every base
// partition of the repo (#1648). The update goes into r.p, the run's own copy.
func (e *Engine) repinBase(r *trainRun) (oldSHA, newSHA string) {
	oldSHA = r.p.baseSHA
	if e.trainValidateFn == nil {
		r.p.wm.FetchOrigin() // best-effort
		if sha, err := gitRevParse(r.p.wm.baseDir, "refs/remotes/origin/"+r.p.baseBranch); err == nil {
			r.p.baseSHA = sha
		}
	}
	return oldSHA, r.p.baseSHA
}

// staleSets returns the member numbers a stale trial of this kind re-forms (reform) and the
// ones a budget-exhausted dissolve comments on (dissolve). Caller holds r.mu.
//
//   - main: the trial's survivors (Current can still name members assembly ejected);
//   - half: the bisection's origin batch — the episode ends, nobody is isolated (R4);
//   - single: the cursor member retried on the new base; a dissolve covers the members of
//     the one-at-a-time list not yet processed, never ones already landed or ejected.
func (r *trainRun) staleSetsLocked(t *runTrialRecord) (reform, dissolve []int) {
	switch t.Kind {
	case "half":
		if r.rec.Bisect != nil {
			set := append([]int(nil), r.rec.Bisect.Origin...)
			return set, set
		}
		// The write-ahead already cleared the bisection (replay): Current holds the set.
		set := append([]int(nil), r.rec.Current...)
		return set, set
	case "single":
		if o := r.rec.OAT; o != nil && o.Index < len(o.Members) {
			return []int{o.Members[o.Index]}, append([]int(nil), o.Members[o.Index:]...)
		}
		return nil, nil
	}
	set := append([]int(nil), t.Members...)
	return set, set
}

// handleStaleTrial consumes a TrainCIStale verdict for the open trial t. See the file
// comment for the contract; the restart-safety rule is: the cycle count, re-pinned base and
// re-form set are persisted BEFORE the old trial is closed, keyed by r.rec.StaleTrial, so a
// restart that replays the verdict for the same trial finishes the rebuild without counting
// it a second time.
func (e *Engine) handleStaleTrial(ctx context.Context, r *trainRun, t *runTrialRecord, diag *trainCIDiagnostic) stepOut {
	state, ep := r.state, r.ep
	repoKey := r.repoKey()
	maxCycles := e.effectiveMaxTrainRebaseCycles()

	r.mu.Lock()
	replay := r.rec.StaleTrial == t.Name
	cycle := r.rec.RebaseCycles
	if !replay {
		cycle++
	}
	reform, dissolve := r.staleSetsLocked(t)
	r.mu.Unlock()

	if !replay && cycle > maxCycles {
		return e.dissolveStaleTrial(r, t, diag, dissolve, maxCycles)
	}

	oldSHA, newSHA := r.p.baseSHA, r.p.baseSHA
	if !replay {
		oldSHA, newSHA = e.repinBase(r)
		// Write-ahead: count the cycle and record the new base and re-form set while the
		// old trial is still the recorded one.
		r.mu.Lock()
		r.rec.RebaseCycles = cycle
		r.rec.StaleTrial = t.Name
		if t.Kind != "single" {
			r.rec.Current = reform
			r.rec.Bisect = nil
			r.rec.Rebuild = true
		}
		r.mu.Unlock()
		if t.Kind != "single" {
			r.remember(r.memberList(reform)...)
		}
		r.persist()
	}

	e.logfRepo(repoKey, "merge-train", "trial %s (PR #%d) is stale — %s; rebuilding %d member(s) on the new base (pinned %s → live %s, cycle %d/%d); nothing charged\n",
		t.Name, t.PRNum, staleNote(diag), len(reform), short(oldSHA), short(newSHA), cycle, maxCycles)

	// The old trial is now unwanted: close its PR and delete its branches.
	e.cleanupTrialArtifacts(r.p.repoKey(), r.p.wm, t.Name)
	r.clearTrial()
	state.mu.Lock()
	state.prNum = 0
	if t.Kind == "half" {
		state.bisecting = false
	}
	state.mu.Unlock()

	if t.Kind == "single" {
		r.setStep(stepOAT)
	} else {
		r.setStep(stepForm)
	}
	r.persist()

	ep.noteRebuilt(cycle)
	e.noteTrainPhase(ep, repoKey, r.memberList(reform), phaseRebuilding(cycle, maxCycles))
	return outContinue
}

// dissolveStaleTrial ends the run when the rebuild budget is spent: the batch is dissolved
// back to Queued, uncharged, with a comment naming the old and new base SHAs (R5).
func (e *Engine) dissolveStaleTrial(r *trainRun, t *runTrialRecord, diag *trainCIDiagnostic, dissolve []int, maxCycles int) stepOut {
	state, ep := r.state, r.ep
	oldSHA, newSHA := e.repinBase(r) // best-effort: for the message only
	var reason string
	switch {
	case diag != nil && diag.BaseMoveObserved && newSHA != "" && oldSHA != newSHA:
		reason = fmt.Sprintf("the base branch kept moving under the trial: it advanced from %s to %s and the trial still went stale after %d rebuild(s) (main-moved rebase limit)",
			short(oldSHA), short(newSHA), maxCycles)
	case diag != nil && diag.BaseMoveObserved:
		reason = fmt.Sprintf("the base branch kept moving under the trial (it advanced past %s) and the trial still went stale after %d rebuild(s) (main-moved rebase limit)",
			short(oldSHA), maxCycles)
	default:
		reason = fmt.Sprintf("the trial PR kept reporting a conflict with the base branch (pinned %s, live %s) and the base could not be read to confirm it moved, after %d rebuild(s) (main-moved rebase limit)",
			short(oldSHA), short(newSHA), maxCycles)
	}
	members := r.memberList(dissolve)
	e.dissolveBatch(state, r.p, t.PRNum, t.Name, membersToItems(members), reason)
	r.clearTrial()
	state.mu.Lock()
	state.bisecting = false
	state.mu.Unlock()
	e.noteTrainPhase(ep, r.repoKey(), members, phaseReleased())
	r.finish()
	return outDone
}

// staleNote is the diagnostic's one-line cause for logs.
func staleNote(d *trainCIDiagnostic) string {
	if d == nil || d.Note == "" {
		return "the base moved under the trial"
	}
	return d.Note
}
