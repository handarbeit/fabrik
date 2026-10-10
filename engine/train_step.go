package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/handarbeit/fabrik/tui"
)

// ─── The merge-train run as an explicit state machine (#2051, ADR 2051) ─────────────
//
// runMergeTrainWorker used to hold a whole episode in one goroutine: assemble, open the
// trial PR, block for the CI wait, then (recursively) bisect and land. The logic between
// two CI waits is unchanged — it now lives in advance(), which runs from a persisted
// position (trainRunRecord.Step) up to the NEXT point where it must wait for a trial's
// CI, opens that trial, and returns.
//
// Two drivers run advance():
//
//   - the synchronous driver (runMergeTrainWorker when the engine has no run store — every
//     NewWithDeps engine, so the whole pre-existing merge-train test corpus): loops
//     advance and obtains each verdict by blocking (trainValidateFn under the seam,
//     otherwise pollTrainCI);
//   - the asynchronous driver (production, New()): the worker's goroutine exits once the
//     trial PR is open and its state is recorded; settleTrainRuns evaluates every open
//     trial's CI on each poll (no goroutine, no slot) and launches a short step goroutine
//     — advance(verdict) — only when a verdict exists.
//
// Because both drivers execute the same advance(), the unmodified bisection tests prove
// the state machine and the sim scenarios prove the asynchronous path.

// trialNamer issues unique, monotonic trial names (the first call returns the base name).
// Its position is persisted with the run.
type trialNamer struct {
	mu   sync.Mutex
	base string
	seq  int
}

func (n *trialNamer) next() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	name := n.base
	if n.seq > 0 {
		name = fmt.Sprintf("%s-t%d", n.base, n.seq)
	}
	n.seq++
	return name
}

func (n *trialNamer) position() (string, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.base, n.seq
}

// trialVerdict is the outcome of waiting for one trial's CI.
type trialVerdict struct {
	result TrainCIResult
	diag   *trainCIDiagnostic
}

// stepOut tells advance's loop what a step did.
type stepOut int

const (
	outContinue stepOut = iota // run another step now
	outAwait                   // a trial is open: wait for its verdict
	outDone                    // the run is finished (finish() was called)
	outStop                    // leave the record untouched and stop (shutdown mid-step)
)

// trainRun is the runtime of one merge-train run: the persisted record plus the
// in-memory handles (trial parameters, the worker-state claim, the TUI episode) that are
// rebuilt, not stored, on adoption.
type trainRun struct {
	e        *Engine
	trainKey string
	store    *trainRunStore // nil on the synchronous driver

	mu  sync.Mutex // guards rec
	rec trainRunRecord

	p       trialParams
	state   *mergeTrainWorkerState
	ep      *trainEpisode
	members map[int]trainMember

	ci trialCIState // evaluator memory of the open trial; mirrored into rec.Trial.CI

	async      bool // true on the asynchronous driver
	detached   bool // landOneAtATime wrapper: no claim, no store, no completion event
	sawRunaway bool // detached only: the runaway guard tripped (the caller fires it)

	stepping atomic.Bool // FR-011: one actor advances a run at a time
	finished atomic.Bool
	started  time.Time // JobStarted time, for the completion event's duration
}

func (r *trainRun) repoKey() string { return r.p.owner + "/" + r.p.repo }

func (r *trainRun) step() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec.Step
}

// setStep moves to a step, restarting the phase clock when the phase changes.
func (r *trainRun) setStep(step string) {
	r.mu.Lock()
	if runPhaseOf(r.rec.Step) != runPhaseOf(step) || r.rec.PhaseStartedAt.IsZero() {
		r.rec.PhaseStartedAt = r.e.now()
	}
	r.rec.Step = step
	r.mu.Unlock()
}

// runPhaseOf maps a step to the spec's phase vocabulary.
func runPhaseOf(step string) string {
	switch step {
	case stepBisect, stepHalf:
		return runPhaseBisect
	case stepOAT, stepSingle:
		return runPhaseOneAtATime
	case stepLanding:
		return runPhaseLanding
	}
	return runPhaseTrialCI
}

func (r *trainRun) memberList(nums []int) []trainMember {
	out := make([]trainMember, 0, len(nums))
	for _, n := range nums {
		if m, ok := r.members[n]; ok {
			out = append(out, m)
		}
	}
	return out
}

func memberNumbers(ms []trainMember) []int {
	out := make([]int, len(ms))
	for i, m := range ms {
		out[i] = m.item.Number
	}
	return out
}

func (r *trainRun) remember(ms ...trainMember) {
	for _, m := range ms {
		r.members[m.item.Number] = m
	}
}

func (r *trainRun) current() []trainMember {
	r.mu.Lock()
	nums := append([]int(nil), r.rec.Current...)
	r.mu.Unlock()
	return r.memberList(nums)
}

func (r *trainRun) setCurrent(ms []trainMember) {
	r.remember(ms...)
	r.mu.Lock()
	r.rec.Current = memberNumbers(ms)
	r.mu.Unlock()
}

func (r *trainRun) trial() *runTrialRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rec.Trial == nil {
		return nil
	}
	cp := *r.rec.Trial
	cp.Members = append([]int(nil), r.rec.Trial.Members...)
	return &cp
}

func (r *trainRun) clearTrial() {
	r.mu.Lock()
	r.rec.Trial = nil
	r.ci = trialCIState{}
	r.mu.Unlock()
}

// referenced is every member number any step may still touch.
func (r *trainRun) referencedLocked() []int {
	seen := map[int]bool{}
	var out []int
	add := func(ns []int) {
		for _, n := range ns {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	add(r.rec.Current)
	if r.rec.Trial != nil {
		add(r.rec.Trial.Members)
	}
	if r.rec.Bisect != nil {
		add(r.rec.Bisect.Origin)
		add(r.rec.Bisect.Red)
	}
	if r.rec.OAT != nil {
		add(r.rec.OAT.Members)
	}
	if r.rec.Poisoner != 0 {
		add([]int{r.rec.Poisoner})
	}
	sort.Ints(out)
	return out
}

// persist writes the record through the store (a no-op on the synchronous driver). It is
// called at every transition that changes what a restart should do, always BEFORE the
// side effect it licenses (write-ahead), and logs — never fails — on a write error.
func (r *trainRun) persist() {
	if r.store == nil || r.detached || r.finished.Load() {
		return
	}
	r.mu.Lock()
	base, seq := r.p.namer.position()
	r.rec.BaseTrialName, r.rec.TrialSeq = base, seq
	r.rec.BaseSHA = r.p.baseSHA
	r.rec.Members = r.rec.Members[:0]
	for _, n := range r.referencedLocked() {
		if m, ok := r.members[n]; ok {
			r.rec.Members = append(r.rec.Members, runMemberRecord{Number: n, PRNum: m.prNum, HeadSHA: m.headSHA, CaughtUpFrom: m.caughtUpFrom})
		}
	}
	if r.rec.Trial != nil {
		r.rec.Trial.CI = r.ci.record()
	}
	if r.rec.Bisect != nil {
		r.rec.Bisect.RedDiag = compactDiag(r.rec.Bisect.RedDiag)
	}
	rec := r.rec
	rec.Original = append([]int(nil), r.rec.Original...)
	r.mu.Unlock()
	if err := r.store.write(&rec); err != nil {
		r.e.logfRepo(r.repoKey(), "merge-train", "warn: %v — a restart will rebuild this train instead of resuming it\n", err)
	}
}

// finish ends the run exactly once: prefix-cache refs, the claim and liveness marker, the
// record, and the episode's single completion event — in the order the deferred calls of
// the old worker ran them.
func (r *trainRun) finish() {
	if r.detached {
		return
	}
	if !r.finished.CompareAndSwap(false, true) {
		return
	}
	e := r.e
	r.p.prefixCache.cleanup()
	e.finishTrain(r.trainKey)
	if r.store != nil {
		r.store.unregister(r.trainKey)
	}
	e.completeTrainEpisode(r.ep, r.repoKey(), r.started)
}

// completeTrainEpisode emits the episode's one JobCompletedEvent (#2050).
func (e *Engine) completeTrainEpisode(ep *trainEpisode, repoKey string, startedAt time.Time) {
	if e.trainOutcomeNeutralisedForTest {
		// FR-013: the pre-#2050 blanket completion, for the neutralisation test.
		e.emitStructural(tui.JobCompletedEvent{IssueNumber: 0, Repo: repoKey, Title: ep.title(), StageName: "Merge Train", Skipped: true})
		return
	}
	out := ep.resolve()
	now := time.Now()
	e.emitStructural(tui.JobCompletedEvent{
		IssueNumber: 0,
		Repo:        repoKey,
		Title:       ep.title(),
		StageName:   "Merge Train",
		Success:     out.Success,
		Completed:   true,
		Duration:    now.Sub(startedAt),
		CompletedAt: now,
		Outcome:     out.Outcome,
		Detail:      out.Detail,
	})
}

// leaveStep marks that no goroutine is executing a step of this run: it clears the
// liveness marker the idle guard reads, but NOT the claim — an open run still owns its
// partition (ADR-1208 / dispatch guard).
func (r *trainRun) leaveStep() {
	if r.detached || r.finished.Load() {
		return
	}
	r.e.store.ExitRepoWorker(r.trainKey)
}

// ─── advance ─────────────────────────────────────────────────────────────────────────

// advance runs the state machine from the run's current step until it must wait for a
// trial's CI (returns true: a trial is open and persisted) or the run ends or must stop
// (returns false). v is the verdict of the open trial when the caller has one.
func (e *Engine) advance(ctx context.Context, r *trainRun, v *trialVerdict) (awaiting bool) {
	for {
		var out stepOut
		switch r.step() {
		case stepForm:
			out = e.stepForm(ctx, r)
		case stepMain, stepHalf, stepSingle:
			if v == nil {
				return true
			}
			vv := *v
			v = nil
			if r.async && ctx.Err() != nil {
				return false // shutdown: leave the record exactly as it is
			}
			switch r.step() {
			case stepMain:
				out = e.stepMainVerdict(ctx, r, vv)
			case stepHalf:
				out = e.stepHalfVerdict(ctx, r, vv)
			default:
				out = e.stepSingleVerdict(ctx, r, vv)
			}
		case stepBisect:
			out = e.stepBisectNext(ctx, r)
		case stepOAT:
			out = e.stepOATNext(ctx, r)
		default: // stepLanding: nothing to advance
			return false
		}
		switch out {
		case outAwait:
			return true
		case outDone, outStop:
			return false
		}
	}
}

// openAndRecord opens a trial for members and, on success, records it as the run's one
// open trial (write-ahead: persisted before anyone waits on it). ok=false means the trial
// could not be opened; the caller handles aerr / an empty survivor set as the old
// assembleAndValidate caller did.
func (e *Engine) openAndRecord(ctx context.Context, r *trainRun, p trialParams, kind string, members []trainMember, trialName string) (survivors []trainMember, aerr error, ok bool) {
	survivors, trialSHA, prNum, err := e.openTrial(ctx, p, members, trialName)
	if err != nil || len(survivors) == 0 {
		return survivors, err, false
	}
	now := e.now()
	r.remember(survivors...)
	r.mu.Lock()
	r.rec.Trial = &runTrialRecord{
		Kind:     kind,
		Name:     trialName,
		HeadSHA:  trialSHA,
		PRNum:    prNum,
		Members:  memberNumbers(survivors),
		OpenedAt: now,
		Deadline: now.Add(e.ciBackstopTimeout()),
	}
	r.ci = trialCIState{}
	r.mu.Unlock()
	r.state.mu.Lock()
	r.state.trialName = trialName
	r.state.prNum = prNum
	r.state.assembling = false
	r.state.mu.Unlock()
	return survivors, nil, true
}

// recordTrialIfCounts is the runaway-guard accounting assembleAndValidate does after a
// trial: every trial except a green one, a CI-infrastructure abandon, or a cancelled
// one counts as "no progress" (#1528, #2052, #2046).
func (e *Engine) recordTrialIfCounts(ctx context.Context, p trialParams, result TrainCIResult, err error) {
	if result != TrainCIGreen && result != TrainCIInfra && !trainCancelled(ctx, err) {
		e.recordTrial(p.trainKey)
	}
}

// stepForm is the top of the re-form loop (formerly the head of runMergeTrainWorker's for
// loop): nothing left → done; a lone member may skip the trial; otherwise form and open
// the batch's trial.
func (e *Engine) stepForm(ctx context.Context, r *trainRun) stepOut {
	p, state, ep := r.p, r.state, r.ep
	repoKey := p.repoKey()
	trainKey := p.trainKey
	current := r.current()

	if len(current) == 0 {
		e.logfRepo(repoKey, "merge-train", "no survivors remaining for %s — train complete with nothing to land\n", trainKey)
		ep.noteDissolved()
		r.finish()
		return outDone
	}

	// #1644: a length-1 batch may be landable directly from its own PR, skipping the
	// trial entirely — checked on every iteration (not just the first) so a batch that
	// bisects down to a single clean survivor and re-forms is also eligible.
	if len(current) == 1 {
		// Apply any pending review-finding eject signal (#1208) before considering the
		// fast path: the fast path never calls assembleAndValidate, so without this it
		// would silently bypass every one of applyPendingReviewEjects' checkpoints.
		if remaining, ejectedCount := e.applyPendingReviewEjectsEp(ep, state.projectID, repoKey, current); ejectedCount > 0 {
			e.logfRepo(repoKey, "merge-train", "%d member(s) ejected for unresolved review findings or unprocessed comments before the singleton fast path — re-forming for %s\n", ejectedCount, trainKey)
			r.setCurrent(remaining)
			r.persist()
			return outContinue
		}
		if e.trySingletonFastPath(ctx, state, p, current[0]) {
			r.finish()
			return outDone
		}
		// #2044: a singleton that is merely BEHIND the pinned base is caught up on its own
		// branch and landed through the fast path rather than via a trial.
		caughtUp, decided := e.trySingletonCatchUp(ctx, state, p, current[0])
		if decided {
			r.finish()
			return outDone
		}
		current[0] = caughtUp
		r.setCurrent(current)
	}

	trialName := p.nextTrialName()
	state.mu.Lock()
	state.trialName = trialName
	state.assembling = true
	state.mu.Unlock()

	e.noteTrainPhase(ep, repoKey, nil, phaseAssembling())
	survivors, aerr, ok := e.openAndRecord(ctx, r, p, "main", current, trialName)
	if !ok {
		return e.mainOutcome(ctx, r, trialName, survivors, TrainCIPending, nil, aerr)
	}
	r.setStep(stepMain)
	r.persist()
	return outAwait
}

// stepMainVerdict consumes the verdict of the batch's trial.
func (e *Engine) stepMainVerdict(ctx context.Context, r *trainRun, v trialVerdict) stepOut {
	t := r.trial()
	if t == nil {
		r.setStep(stepForm)
		return outContinue
	}
	r.state.mu.Lock()
	r.state.trialName = t.Name
	r.state.prNum = t.PRNum
	r.state.mu.Unlock()
	return e.mainOutcome(ctx, r, t.Name, r.memberList(t.Members), v.result, v.diag, nil)
}

// mainOutcome is what the old re-form loop did after assembleAndValidate returned.
func (e *Engine) mainOutcome(ctx context.Context, r *trainRun, trialName string, survivors []trainMember, result TrainCIResult, diag *trainCIDiagnostic, aerr error) stepOut {
	p, state, ep := r.p, r.state, r.ep
	repoKey := p.repoKey()
	trainKey := p.trainKey
	current := r.current()
	partitionBase := r.rec.PartitionBase

	e.recordTrialIfCounts(ctx, p, result, aerr)
	r.clearTrial()

	if aerr != nil {
		e.logfRepo(repoKey, "merge-train", "assemble/validate failed for %s: %v\n", trainKey, aerr)
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		ep.noteAbandoned("trial could not be assembled")
		e.noteTrainPhase(ep, repoKey, current, phaseReleased())
		r.finish()
		return outDone
	}
	if len(survivors) == 0 {
		// Every member was ejected during assembly (unresolvable conflicts).
		e.logfRepo(repoKey, "merge-train", "entire batch ejected during assembly for %s\n", trainKey)
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		r.finish()
		return outDone
	}

	// Hook 1: check runaway guard after the initial re-form trial (ADR-059 D8).
	// partitionBase (the sentinel-aware value), not p.baseBranch (the real resolved branch
	// name, never empty) — the latter would desync this call's trainKey from the trainKey
	// used for isRunawayTripped (found in review, #1648).
	if count, tripped := e.isRunawayTripped(trainKey); tripped {
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		ep.noteAbandoned("runaway guard fired")
		e.fireRunawayGuard(ctx, p.owner, p.repo, partitionBase, membersToItems(current), count)
		r.finish()
		return outDone
	}

	// Hook 2: apply any pending review-finding ejects flagged externally while this trial
	// was assembling/CI-polling (#1208). A flagged member's trial is always discarded here,
	// regardless of its own CI result: a green trial containing a flagged member must never
	// reach landGreenBatch.
	if remaining, ejectedCount := e.applyPendingReviewEjectsEp(ep, state.projectID, repoKey, survivors); ejectedCount > 0 {
		e.logfRepo(repoKey, "merge-train", "%d member(s) ejected for unresolved review findings or unprocessed comments mid-trial — discarding trial and re-forming for %s\n", ejectedCount, trainKey)
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		r.setCurrent(remaining)
		r.setStep(stepForm)
		r.persist()
		return outContinue
	}

	state.mu.Lock()
	state.CIResult = result
	state.mu.Unlock()

	switch result {
	case TrainCIGreen:
		// D-d hard invariant: a green batch lands immediately, zero bisection. landGreenBatch
		// adds the D5 main-moved landing gate around landMergeTrainBatch. The landing phase
		// is recorded first (visibility + ownership); a restart during it drops the record
		// and the durable reconstructTrainState routes finish the landing.
		e.logfRepo(repoKey, "merge-train", "combined Validate green for %s (%d survivor(s)) — landing\n", trainKey, len(survivors))
		r.setStep(stepLanding)
		r.persist()
		e.landGreenBatch(ctx, state, p, survivors)
		r.finish()
		return outDone
	case TrainCIPending:
		e.logfRepo(repoKey, "merge-train", "combined Validate pending/timed out for %s — will retry next poll\n", trainKey)
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		ep.noteAbandoned("trial CI still pending at timeout")
		e.noteTrainPhase(ep, repoKey, survivors, phaseReleased())
		r.finish()
		return outDone
	case TrainCIInfra:
		// #2052: CI never started and retriggering did not help. Not the members' fault: no
		// bisection, ejection, pause or counter — they stay Queued and a fresh trial forms
		// once the cooldown lapses.
		e.logfRepo(repoKey, "merge-train", "combined Validate abandoned for %s — CI infrastructure failure (%s); %d member(s) left in Queued, nothing charged\n", trainKey, infraNote(diag), len(survivors))
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		e.markInfraAbandon(trainKey)
		ep.noteAbandoned("CI never started (infrastructure failure)")
		e.noteTrainPhase(ep, repoKey, survivors, phaseReleased())
		r.finish()
		return outDone
	default: // TrainCIRed
		if len(survivors) == 1 {
			// #1440 R1: a red batch of exactly one member has no poisoner to isolate —
			// short-circuit straight to the dedicated singleton disposition.
			e.logf(survivors[0].item.Number, "merge-train", "combined Validate RED for %s with a single member (#%d) — no poisoner to isolate; disposing as a red singleton\n", trainKey, survivors[0].item.Number)
			e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
			e.ejectRedSingleton(state.projectID, p.owner, p.repo, survivors[0], p, diag)
			r.finish()
			return outDone
		}
		e.logfRepo(repoKey, "merge-train", "combined Validate RED for %s (%d member(s)) — bisecting to isolate the poisoner\n", trainKey, len(survivors))
		// The red trial's artifacts are unneeded; bisection sub-trials build fresh.
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		state.mu.Lock()
		state.bisecting = true
		state.mu.Unlock()
		if e.trainRedBatchHook != nil {
			e.trainRedBatchHook()
		}
		r.remember(survivors...)
		r.mu.Lock()
		r.rec.Bisect = newBisectState(memberNumbers(survivors), diag, e.effectiveBisectCap())
		r.mu.Unlock()
		r.setStep(stepBisect)
		r.persist()
		return outContinue
	}
}

// bisectOrigin returns the batch the bisection episode started from.
func (r *trainRun) bisectOrigin() []trainMember {
	r.mu.Lock()
	var nums []int
	if r.rec.Bisect != nil {
		nums = append(nums, r.rec.Bisect.Origin...)
	}
	r.mu.Unlock()
	return r.memberList(nums)
}

// stepBisectNext opens the next half-trial, isolates the poisoner, or degrades to the
// one-at-a-time fallback — the loop body of the old recursive bisect plus handleRedBatch.
func (e *Engine) stepBisectNext(ctx context.Context, r *trainRun) stepOut {
	p, state, ep := r.p, r.state, r.ep
	repoKey := p.repoKey()

	r.mu.Lock()
	b := r.rec.Bisect
	if b == nil {
		r.mu.Unlock()
		r.setStep(stepForm)
		return outContinue
	}
	// A poisoner marked by a previous incarnation but not yet ejected (write-ahead).
	pendingPoisoner := r.rec.Poisoner
	act, half := b.next()
	halfNums := append([]int(nil), half...)
	used, costCap := b.Used, b.Cap
	r.mu.Unlock()

	if pendingPoisoner != 0 {
		act = bisectIsolated
	}

	switch act {
	case bisectIsolated:
		return e.ejectPoisoner(ctx, r)
	case bisectFallbackCap:
		e.logfRepo(repoKey, "merge-train", "bisection cost cap (%d validations) reached — degrading to one-at-a-time fallback\n", costCap)
		return e.startOneAtATime(ctx, r)
	case bisectFallbackSplit:
		// Both halves green: the redness spans the split — a non-isolable interaction (D-e).
		return e.startOneAtATime(ctx, r)
	}

	// The step is the validation about to run (the episode's initial red validation counts
	// as the first); the ceiling is the cost cap, not a prediction — the number of remaining
	// halves is not known up front.
	halfMembers := r.memberList(halfNums)
	e.noteTrainPhase(ep, repoKey, halfMembers, phaseBisecting(used+1, costCap))
	trialName := p.nextTrialName()
	sub := p
	sub.quietTrialLines = true // this by-value copy: the "bisecting" line above owns the status line (#2048)
	state.mu.Lock()
	state.trialName = trialName
	state.mu.Unlock()
	survivors, aerr, ok := e.openAndRecord(ctx, r, sub, "half", halfMembers, trialName)
	if !ok {
		return e.halfOutcome(ctx, r, trialName, survivors, TrainCIPending, nil, aerr)
	}
	r.setStep(stepHalf)
	r.persist()
	return outAwait
}

func (e *Engine) stepHalfVerdict(ctx context.Context, r *trainRun, v trialVerdict) stepOut {
	t := r.trial()
	if t == nil {
		r.setStep(stepBisect)
		return outContinue
	}
	return e.halfOutcome(ctx, r, t.Name, r.memberList(t.Members), v.result, v.diag, nil)
}

// halfOutcome folds one bisection half-trial's outcome into the bisection state, in the
// order the old code checked it: spend, cleanup, cancel, assembly error, runaway, infra,
// then red/not-red.
func (e *Engine) halfOutcome(ctx context.Context, r *trainRun, trialName string, survivors []trainMember, result TrainCIResult, diag *trainCIDiagnostic, err error) stepOut {
	p := r.p
	repoKey := p.repoKey()
	trainKey := p.trainKey

	e.recordTrialIfCounts(ctx, p, result, err)
	r.clearTrial()
	r.mu.Lock()
	if r.rec.Bisect != nil {
		r.rec.Bisect.spent()
	}
	r.mu.Unlock()
	e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)

	if trainCancelled(ctx, err) {
		// #2046: a cancelled sub-trial is neither red nor green — abort the episode
		// (members stay Queued) instead of degrading to a fallback that would start more work.
		e.logfRepo(repoKey, "merge-train", "bisection cancelled: %v — leaving members in Queued\n", err)
		return e.abortBisection(ctx, r, bisectAbortInfra)
	}
	if err != nil {
		e.logfRepo(repoKey, "merge-train", "bisection trial failed to assemble: %v — degrading to one-at-a-time fallback\n", err)
		if _, tripped := e.isRunawayTripped(trainKey); tripped {
			return e.abortBisection(ctx, r, bisectAbortRunaway)
		}
		return e.startOneAtATime(ctx, r)
	}
	if _, tripped := e.isRunawayTripped(trainKey); tripped {
		return e.abortBisection(ctx, r, bisectAbortRunaway)
	}
	if result == TrainCIInfra {
		return e.abortBisection(ctx, r, bisectAbortInfra)
	}

	r.mu.Lock()
	if r.rec.Bisect != nil {
		r.rec.Bisect.apply(result == TrainCIRed && len(survivors) > 0, memberNumbers(survivors), diag)
	}
	r.mu.Unlock()
	r.remember(survivors...)
	r.setStep(stepBisect)
	r.persist()
	return outContinue
}

type bisectAbort int

const (
	bisectAbortInfra bisectAbort = iota
	bisectAbortRunaway
)

// abortBisection ends the episode when a sub-trial was cancelled, hit a CI infrastructure
// failure (#2052) or tripped the runaway guard.
func (e *Engine) abortBisection(ctx context.Context, r *trainRun, why bisectAbort) stepOut {
	p, ep := r.p, r.ep
	repoKey := p.repoKey()
	trainKey := p.trainKey
	origin := r.bisectOrigin()
	switch why {
	case bisectAbortRunaway:
		// Runaway guard fired inside bisect. partitionBase (sentinel-aware), not
		// p.baseBranch — see mainOutcome's Hook 1.
		count, _ := e.isRunawayTripped(trainKey)
		ep.noteAbandoned("runaway guard fired")
		e.fireRunawayGuard(ctx, p.owner, p.repo, r.rec.PartitionBase, membersToItems(origin), count)
	default:
		if ctx.Err() != nil {
			// #2046: bisect reuses the infra abort shape for a cancelled sub-trial. A cancel is
			// not a CI infrastructure failure, so don't log it as one or start the infra cooldown.
			e.logfRepo(repoKey, "merge-train", "bisection for %s cancelled; %d member(s) left in Queued, nothing charged\n", trainKey, len(origin))
			ep.noteAbandoned("cancelled during bisection")
			e.noteTrainPhase(ep, repoKey, origin, phaseReleased())
		} else {
			// #2052: a bisection sub-trial hit a CI infrastructure failure. Abort the whole
			// episode — nothing was ejected yet (ejection follows bisection), every member stays
			// Queued, nothing is charged for the abandoned trial.
			e.logfRepo(repoKey, "merge-train", "bisection for %s aborted — CI infrastructure failure on a sub-trial; %d member(s) left in Queued, nothing charged\n", trainKey, len(origin))
			e.markInfraAbandon(trainKey)
			ep.noteAbandoned("CI never started on a bisection trial (infrastructure failure)")
			e.noteTrainPhase(ep, repoKey, origin, phaseReleased())
		}
	}
	r.state.mu.Lock()
	r.state.bisecting = false
	r.state.mu.Unlock()
	r.finish()
	return outDone
}

// ejectPoisoner is handleRedBatch's isolated-poisoner tail: eject, forget its rerere
// resolutions, and re-form the survivors for the main loop (FR-3). The mark is persisted
// BEFORE the ejection, so a restart between the mark and the end finishes it once.
func (e *Engine) ejectPoisoner(ctx context.Context, r *trainRun) stepOut {
	p, state, ep := r.p, r.state, r.ep
	repoKey := p.repoKey()

	r.mu.Lock()
	b := r.rec.Bisect
	num := r.rec.Poisoner
	if num == 0 && b != nil {
		num = b.poisoner()
		r.rec.Poisoner = num
	}
	var diag *trainCIDiagnostic
	var origin []int
	if b != nil {
		diag = b.RedDiag
		origin = append([]int(nil), b.Origin...)
	}
	r.mu.Unlock()
	r.persist()

	red := r.memberList(origin)
	var poisoner *trainMember
	for i := range red {
		if red[i].item.Number == num {
			poisoner = &red[i]
		}
	}
	if poisoner != nil {
		// Eject the isolated poisoner (D-a shared counter, D-c comment, cap→pause reuse). red —
		// the full batch at the start of this episode — is passed as the R4 batch context: the
		// isolating run itself always validates the poisoner alone, so "the other batch
		// members" means who else rode in this train attempt.
		e.logf(poisoner.item.Number, "merge-train", "bisection isolated #%d as the batch poisoner — ejecting\n", poisoner.item.Number)
		e.ejectMember(p.owner, p.repo, poisoner.item,
			fmt.Sprintf("ejected from merge-train — the combined Validate fails whenever #%d is in the batch (isolated by halving bisection). It will be retried in a future train with a different composition.", poisoner.item.Number),
			diag, red, true)
		ep.notePoisoner(poisoner.item.Number)
		e.noteTrainEjected(ep, repoKey, poisoner.item.Number, "poisoner", false)
		e.forgetPoisonerResolutions(ctx, p, red, *poisoner)
	}

	var survivors []trainMember
	for i := range red {
		if red[i].item.Number != num {
			survivors = append(survivors, red[i])
		}
	}
	state.mu.Lock()
	state.bisecting = false
	state.mu.Unlock()
	r.mu.Lock()
	r.rec.Bisect = nil
	r.rec.Poisoner = 0
	r.mu.Unlock()
	r.setCurrent(survivors)
	r.setStep(stepForm)
	r.persist()
	return outContinue
}

// startOneAtATime is handleRedBatch's fallback branch (FR-5): the bisection could not
// isolate a single poisoner, so every member of the episode's batch is validated and
// landed as its own singleton.
func (e *Engine) startOneAtATime(ctx context.Context, r *trainRun) stepOut {
	p, ep := r.p, r.ep
	repoKey := p.repoKey()
	r.mu.Lock()
	var origin []int
	var used, costCap int
	if r.rec.Bisect != nil {
		origin = append(origin, r.rec.Bisect.Origin...)
		used, costCap = r.rec.Bisect.Used, r.rec.Bisect.Cap
	}
	r.mu.Unlock()
	members := r.memberList(origin)
	e.logfRepo(repoKey, "merge-train", "could not isolate a single poisoner for %s/%s (%d/%d validations used) — degrading to one-at-a-time landing of %d member(s)\n", p.owner, p.repo, used, costCap, len(members))
	ep.noteOneAtATime()
	return e.beginOneAtATime(r, members)
}

// beginOneAtATime positions the run at the first member of the one-at-a-time fallback.
func (e *Engine) beginOneAtATime(r *trainRun, members []trainMember) stepOut {
	p, ep := r.p, r.ep
	repoKey := p.owner + "/" + p.repo
	e.logfRepo(repoKey, "merge-train", "one-at-a-time fallback: processing %d member(s) as singleton batches\n", len(members))
	ep.noteOneAtATime()
	e.noteTrainPhase(ep, repoKey, members, phaseOneAtATime())
	r.remember(members...)
	r.mu.Lock()
	r.rec.OAT = &runOATRecord{Members: memberNumbers(members)}
	r.rec.Bisect = nil
	r.mu.Unlock()
	r.setStep(stepOAT)
	r.persist()
	return outContinue
}

// oatMember returns the member at the one-at-a-time cursor (ok=false past the end).
func (r *trainRun) oatMember() (trainMember, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rec.OAT == nil || r.rec.OAT.Index >= len(r.rec.OAT.Members) {
		return trainMember{}, false
	}
	m, ok := r.members[r.rec.OAT.Members[r.rec.OAT.Index]]
	return m, ok
}

func (r *trainRun) oatAdvance() {
	r.mu.Lock()
	if r.rec.OAT != nil {
		r.rec.OAT.Index++
	}
	r.mu.Unlock()
}

// oatDone ends the fallback: it landed/ejected/left-queued every member itself.
func (e *Engine) oatDone(r *trainRun) stepOut {
	r.state.mu.Lock()
	r.state.bisecting = false
	r.state.mu.Unlock()
	r.finish()
	return outDone
}

// oatRunaway ends the fallback because the runaway guard tripped. The detached wrapper
// (landOneAtATime) reports it to its caller instead, exactly as before.
func (e *Engine) oatRunaway(ctx context.Context, r *trainRun) stepOut {
	if r.detached {
		r.sawRunaway = true
		return outDone
	}
	p, ep := r.p, r.ep
	count, _ := e.isRunawayTripped(p.trainKey)
	ep.noteAbandoned("runaway guard fired")
	e.fireRunawayGuard(ctx, p.owner, p.repo, r.rec.PartitionBase, membersToItems(r.bisectOriginOrOAT()), count)
	r.state.mu.Lock()
	r.state.bisecting = false
	r.state.mu.Unlock()
	r.finish()
	return outDone
}

// bisectOriginOrOAT is the episode's batch for runaway reporting: the bisection origin, or
// the fallback list when bisection was already cleared.
func (r *trainRun) bisectOriginOrOAT() []trainMember {
	r.mu.Lock()
	var nums []int
	if r.rec.Bisect != nil {
		nums = append(nums, r.rec.Bisect.Origin...)
	} else if r.rec.OAT != nil {
		nums = append(nums, r.rec.OAT.Members...)
	}
	r.mu.Unlock()
	return r.memberList(nums)
}

// stepOATNext opens the next singleton's trial (the loop head of the old landOneAtATime).
func (e *Engine) stepOATNext(ctx context.Context, r *trainRun) stepOut {
	p := r.p
	m, ok := r.oatMember()
	if !ok {
		return e.oatDone(r)
	}
	if e.trainValidateFn == nil {
		// Re-pin the base to current origin/<base> so a prior singleton's land is seen.
		// FetchOrigin (not a raw exec.Command) — serialized under wm.mu, since this
		// WorktreeManager is shared by every base partition of this repo (found in review,
		// #1648).
		p.wm.FetchOrigin() // best-effort
		if sha, rerr := gitRevParse(p.wm.baseDir, "refs/remotes/origin/"+p.baseBranch); rerr == nil {
			p.baseSHA = sha
			r.p.baseSHA = sha
		}
	}
	trialName := p.nextTrialName()
	survivors, aerr, opened := e.openAndRecord(ctx, r, p, "single", []trainMember{m}, trialName)
	if !opened {
		return e.singleOutcome(ctx, r, m, trialName, survivors, TrainCIPending, nil, aerr)
	}
	r.setStep(stepSingle)
	r.persist()
	return outAwait
}

func (e *Engine) stepSingleVerdict(ctx context.Context, r *trainRun, v trialVerdict) stepOut {
	t := r.trial()
	m, ok := r.oatMember()
	if t == nil || !ok {
		r.setStep(stepOAT)
		return outContinue
	}
	return e.singleOutcome(ctx, r, m, t.Name, r.memberList(t.Members), v.result, v.diag, nil)
}

// singleOutcome is the body of the old landOneAtATime loop after its trial returned.
func (e *Engine) singleOutcome(ctx context.Context, r *trainRun, m trainMember, trialName string, survivors []trainMember, result TrainCIResult, diag *trainCIDiagnostic, err error) stepOut {
	p, state := r.p, r.state
	repoKey := p.owner + "/" + p.repo
	trainKey := p.trainKey

	e.recordTrialIfCounts(ctx, p, result, err)
	r.clearTrial()
	if trainCancelled(ctx, err) {
		// #2046: cancelled, not a verdict — stop without touching the rest.
		e.logf(m.item.Number, "merge-train", "one-at-a-time landing cancelled: %v — leaving remaining members in Queued\n", err)
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		return e.oatDone(r)
	}
	if err != nil || len(survivors) == 0 {
		e.logf(m.item.Number, "merge-train", "could not assemble #%d in isolation: %v — leaving in Queued\n", m.item.Number, err)
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		if _, tripped := e.isRunawayTripped(trainKey); tripped {
			return e.oatRunaway(ctx, r)
		}
		return e.oatNext(r)
	}
	if _, tripped := e.isRunawayTripped(trainKey); tripped {
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		return e.oatRunaway(ctx, r)
	}

	// Hook 2: apply any pending review-finding eject flagged externally while this singleton
	// trial was assembling/CI-polling (#1208).
	if _, ejectedCount := e.applyPendingReviewEjects(state.projectID, repoKey, survivors); ejectedCount > 0 {
		e.logf(m.item.Number, "merge-train", "pending review-finding or unprocessed-comment eject flagged for singleton #%d — discarding trial\n", m.item.Number)
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		return e.oatNext(r)
	}

	switch result {
	case TrainCIGreen:
		e.landSingleton(ctx, state, p, m, trialName)
	case TrainCIRed:
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		e.logf(m.item.Number, "merge-train", "#%d fails combined Validate even in isolation — disposing as a red singleton\n", m.item.Number)
		// #1440: this validates m completely alone — structurally the same true-singleton
		// scenario the top-level arity guard targets, just reached via the one-at-a-time
		// fallback. It gets the same disposition rather than ejectMember's multi-member wording.
		e.ejectRedSingleton(state.projectID, p.owner, p.repo, m, p, diag)
	case TrainCIInfra:
		// #2052: CI never started for this singleton. Every later member would hit the same
		// provider failure, so stop the whole fallback; all stay Queued, nothing charged.
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		e.logf(m.item.Number, "merge-train", "combined Validate for singleton #%d abandoned — CI infrastructure failure (%s); leaving the remaining member(s) in Queued\n", m.item.Number, infraNote(diag))
		e.markInfraAbandon(trainKey)
		return e.oatDone(r)
	default: // TrainCIPending
		e.cleanupTrialArtifacts(p.repoKey(), p.wm, trialName)
		e.logf(m.item.Number, "merge-train", "combined Validate pending for singleton #%d — leaving in Queued\n", m.item.Number)
	}
	return e.oatNext(r)
}

func (e *Engine) oatNext(r *trainRun) stepOut {
	r.oatAdvance()
	r.setStep(stepOAT)
	r.persist()
	return outContinue
}

// ─── drivers ─────────────────────────────────────────────────────────────────────────

// awaitTrialVerdict blocks for the open trial's verdict (the synchronous driver).
func (e *Engine) awaitTrialVerdict(ctx context.Context, r *trainRun) trialVerdict {
	t := r.trial()
	if t == nil {
		return trialVerdict{result: TrainCIPending}
	}
	if e.trainValidateFn != nil {
		res, diag := e.trainValidateFn(ctx, r.memberList(t.Members))
		return trialVerdict{result: res, diag: diag}
	}
	res, diag := e.pollTrainCI(ctx, r.p.owner, r.p.repo, t.PRNum, t.HeadSHA)
	return trialVerdict{result: res, diag: diag}
}

// driveSync runs the whole episode on the calling goroutine.
func (e *Engine) driveSync(ctx context.Context, r *trainRun) {
	awaiting := e.advance(ctx, r, nil)
	for awaiting && !r.finished.Load() {
		v := e.awaitTrialVerdict(ctx, r)
		awaiting = e.advance(ctx, r, &v)
	}
}

// stepAsync runs one step goroutine's worth of work on the asynchronous driver. The
// caller has claimed r.stepping and marked the liveness; both are released here.
func (e *Engine) stepAsync(ctx context.Context, r *trainRun, v *trialVerdict) {
	defer r.stepping.Store(false)
	e.advance(ctx, r, v)
	r.leaveStep()
}

// landOneAtATime is the FR-5 fallback entry used by callers that already hold a batch
// and its trial parameters (the prefix-reuse integration test, for one): it runs the
// one-at-a-time state machine on a detached, unpersisted run and returns true if the
// runaway guard fired during processing. Production reaches the same machine through
// bisection (startOneAtATime), where it is part of the persisted run.
func (e *Engine) landOneAtATime(ctx context.Context, state *mergeTrainWorkerState, p trialParams, members []trainMember) bool {
	r := &trainRun{
		e:        e,
		trainKey: p.trainKey,
		p:        p,
		state:    state,
		ep:       p.episode,
		members:  map[int]trainMember{},
		detached: true,
		rec:      trainRunRecord{TrainKey: p.trainKey, Owner: p.owner, Repo: p.repo},
	}
	if r.p.namer == nil {
		r.p.namer = &trialNamer{base: "merge-train-detached"}
	}
	e.beginOneAtATime(r, members)
	e.driveSync(ctx, r)
	return r.sawRunaway
}
