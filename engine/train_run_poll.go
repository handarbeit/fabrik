package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tui"
)

// ─── Per-poll evaluation and restart adoption of persisted merge-train runs (#2051) ───
//
// settleTrainRuns is the merge-train's counterpart of settleAwaitingCIScan (ADR-1270): a
// per-poll scan, independent of Queued routing, that owns every open trial. For each run it
//
//   - adopts records loaded from disk after a restart, once the board snapshot can
//     validate them (FR-008/FR-009);
//   - evaluates the open trial's CI with the same evalTrialCI the blocking pollTrainCI
//     uses — API reads only, no goroutine and no worker slot (FR-004, FR-006);
//   - surfaces a trial with no verdict by its persisted deadline, compared to e.now(), so a
//     wedged trial is found within one poll of it (FR-010);
//   - launches a short step goroutine only once a verdict exists, or when a restored run is
//     positioned between trials and simply needs its next step.
//
// It runs before Queued routing so a restarted partition's record is adopted (and its claim
// re-registered) before routeQueuedGroup could form a fresh train over it.

// settleTrainRuns is called once per poll when merge_train is on.
func (e *Engine) settleTrainRuns(ctx context.Context, board *gh.ProjectBoard) {
	if e.trainRuns == nil || board == nil {
		return
	}
	e.trainRunsLoadOnce.Do(func() {
		for _, note := range e.trainRuns.loadDir() {
			e.logf(0, "merge-train", "train run state: %s\n", note)
		}
	})

	e.trainRuns.mu.Lock()
	pending := make([]*trainRunRecord, 0, len(e.trainRuns.pending))
	for _, rec := range e.trainRuns.pending {
		pending = append(pending, rec)
	}
	e.trainRuns.mu.Unlock()
	for _, rec := range pending {
		e.adoptTrainRun(ctx, board, rec)
	}

	for _, r := range e.trainRuns.liveRuns() {
		e.pollTrainRun(ctx, board, r)
	}
}

// pollTrainRun evaluates one run for this poll.
func (e *Engine) pollTrainRun(ctx context.Context, board *gh.ProjectBoard, r *trainRun) {
	if r.finished.Load() || !r.stepping.CompareAndSwap(false, true) {
		return // finished, or a step goroutine / another evaluation owns it (FR-011)
	}
	r.refreshItems(board)

	t := r.trial()
	if t == nil {
		// Positioned between trials (an adopted run): it only needs its next step.
		e.launchStep(ctx, r, nil)
		return
	}

	var (
		v       trialVerdict
		decided bool
	)
	// The CI is always read first, even past the deadline: a restart that outlasted the
	// remaining backstop must not abandon a trial whose CI already finished.
	switch {
	case e.trainValidateFn != nil:
		if e.trainValidateHoldForTest == nil || !e.trainValidateHoldForTest() {
			v.result, v.diag = e.trainValidateFn(ctx, r.memberList(t.Members))
			decided = true
		}
	default:
		before := r.ci.record()
		res, diag, dec := e.evalTrialCI(ctx, r.p.owner, r.p.repo, t.PRNum, t.HeadSHA, &r.ci)
		v, decided = trialVerdict{result: res, diag: diag}, dec
		if !reflect.DeepEqual(before, r.ci.record()) {
			r.persist() // a retrigger / re-run budget spent must survive a restart
		}
	}
	if !decided && !e.now().Before(t.Deadline) {
		// FR-010: no verdict by the deadline. The poll itself surfaces the stuck trial and
		// hands it to the train's existing timed-out-trial handling (pending: clean up,
		// members stay Queued).
		e.logfRepo(r.repoKey(), "merge-train", "trial %s (PR #%d) for %s has no CI verdict %s after it opened (%s) — treating it as timed out\n",
			t.Name, t.PRNum, r.trainKey, e.now().Sub(t.OpenedAt).Round(time.Second), t.OpenedAt.Format(time.RFC3339))
		v = trialVerdict{result: TrainCIPending}
		decided = true
	}
	if !decided {
		r.stepping.Store(false)
		return
	}
	e.launchStep(ctx, r, &v)
}

// launchStep starts a step goroutine for r; the caller holds r.stepping.
func (e *Engine) launchStep(ctx context.Context, r *trainRun, v *trialVerdict) {
	e.store.EnterRepoWorker(r.trainKey)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.stepAsync(ctx, r, v)
	}()
}

// refreshItems re-attaches the live board items to the run's members, so a step never
// acts on a stale title, label set or item ID. The caller owns the run (stepping).
func (r *trainRun) refreshItems(board *gh.ProjectBoard) {
	for _, it := range board.Items {
		if m, ok := r.members[it.Number]; ok {
			if owner, repo := itemOwnerRepo(it, r.e.defaultRepo()); owner != r.p.owner || repo != r.p.repo {
				continue
			}
			m.item = it
			r.members[it.Number] = m
		}
	}
}

// ── adoption ──

// adoptTrainRun validates a loaded record against GitHub and either resumes it as a live run
// or discards it. A discard falls back to today's behaviour — the partition forms fresh and
// reconstructTrainState finishes or dissolves whatever the previous incarnation left — so a
// bad record can never wedge a partition or double-land a member.
func (e *Engine) adoptTrainRun(ctx context.Context, board *gh.ProjectBoard, rec *trainRunRecord) {
	repoKey := rec.Owner + "/" + rec.Repo
	if rec.Step == stepLanding {
		// A landing was executing. Its effects are not recorded here; the durable
		// reconstructTrainState routes finish a merged landing idempotently (ADR-1871).
		e.discardTrainRunMode(rec, nil, "a landing was in progress — it is completed from GitHub's state", discardKeepTrial)
		return
	}
	// A step that awaits a trial must have one; otherwise it is positioned between trials.
	if rec.Trial == nil {
		switch rec.Step {
		case stepMain:
			rec.Step = stepForm
		case stepHalf:
			rec.Step = stepBisect
		case stepSingle:
			rec.Step = stepOAT
		}
	}
	hs := holdingStage(e.cfg)
	if hs == nil {
		e.discardTrainRun(rec, nil, "no holding stage is configured")
		return
	}
	byNum := map[int]gh.ProjectItem{}
	for _, it := range board.Items {
		if o, rp := itemOwnerRepo(it, e.defaultRepo()); o == rec.Owner && rp == rec.Repo {
			byNum[it.Number] = it
		}
	}

	recMembers := map[int]runMemberRecord{}
	for _, m := range rec.Members {
		recMembers[m.Number] = m
	}

	// The members whose continued presence in Queued the record depends on.
	need := map[int]bool{}
	add := func(ns []int) {
		for _, n := range ns {
			need[n] = true
		}
	}
	// In the one-at-a-time fallback Current is still the pre-bisection batch: nothing
	// narrows it as the cursor advances, so it names members already landed or ejected.
	// The cursor and the open trial say who is still live.
	if rec.OAT == nil {
		add(rec.Current)
	}
	if rec.Trial != nil {
		add(rec.Trial.Members)
	}
	if rec.Bisect != nil {
		add(rec.Bisect.Origin)
	}
	if rec.OAT != nil {
		if rec.OAT.Index < len(rec.OAT.Members) {
			add(rec.OAT.Members[rec.OAT.Index:])
		}
	}

	// Members that assembly ejected are still listed in the pre-assembly sets; they left
	// Queued on purpose, so they neither keep the run alive nor re-enter a later trial.
	for _, n := range rec.Ejected {
		if it, ok := byNum[n]; ok && it.Status == hs.Name {
			continue
		}
		delete(need, n)
		kept := rec.Current[:0:0]
		for _, c := range rec.Current {
			if c != n {
				kept = append(kept, c)
			}
		}
		rec.Current = kept
	}

	// A poisoner marked (write-ahead) but possibly ejected before the restart: if it already
	// left Queued the ejection completed — drop it from the record and carry on re-forming
	// the survivors; if it is still Queued the ejection is simply finished once.
	if rec.Poisoner != 0 {
		it, ok := byNum[rec.Poisoner]
		if !ok || it.Status != hs.Name {
			p := rec.Poisoner
			rec.Poisoner = 0
			delete(need, p)
			if rec.Bisect != nil {
				var surv []int
				for _, n := range rec.Bisect.Origin {
					if n != p {
						surv = append(surv, n)
					}
				}
				rec.Current, rec.Bisect, rec.Trial, rec.Step = surv, nil, nil, stepForm
				need = map[int]bool{}
				add(surv)
			}
		}
	}

	// R1: every live member is still Queued, open, unpaused, and its PR head unchanged.
	var firstItem gh.ProjectItem
	var items []gh.ProjectItem
	for n := range need {
		it, ok := byNum[n]
		switch {
		case !ok:
			e.discardTrainRun(rec, nil, fmt.Sprintf("member #%d is no longer on the board", n))
			return
		case it.Status != hs.Name:
			e.discardTrainRun(rec, nil, fmt.Sprintf("member #%d is no longer in %s (now %q)", n, hs.Name, it.Status))
			return
		case it.IsClosed:
			e.discardTrainRun(rec, nil, fmt.Sprintf("member #%d was closed", n))
			return
		case hasLabel(it.Labels, "fabrik:paused"):
			e.discardTrainRun(rec, nil, fmt.Sprintf("member #%d was paused", n))
			return
		}
		if m, ok := recMembers[n]; ok && it.LinkedPRHeadSHA != "" && m.HeadSHA != "" && it.LinkedPRHeadSHA != m.HeadSHA {
			e.discardTrainRun(rec, nil, fmt.Sprintf("member #%d's PR head moved (%s → %s)", n, short(m.HeadSHA), short(it.LinkedPRHeadSHA)))
			return
		}
		if len(items) == 0 {
			firstItem = it
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		e.discardTrainRun(rec, nil, "the record references no members")
		return
	}

	// The repository must be ready before anything git-shaped; a transient failure just
	// defers adoption to the next poll (the partition stays held off meanwhile).
	if err := e.ensureRepoReady(ctx, firstItem); err != nil {
		if errors.Is(err, ErrSkipItem) {
			e.logfRepo(repoKey, "merge-train", "train run for %s: repo %s not ready yet — adoption deferred to the next poll\n", rec.TrainKey, repoKey)
		} else {
			e.logfRepo(repoKey, "merge-train", "train run for %s: ensureRepoReady failed: %v — adoption deferred\n", rec.TrainKey, err)
		}
		return
	}
	wm := e.worktreesFor(repoKey)

	// R2: the open trial PR is still open at the recorded head SHA. A merged one is left to
	// reconstructTrainState's Route 1, which completes the landing idempotently (ADR-1871).
	if rec.Trial != nil && e.trainValidateFn == nil {
		pr, err := e.client.FetchPRDetails(rec.Owner, rec.Repo, rec.Trial.PRNum)
		if err != nil {
			e.logfRepo(repoKey, "merge-train", "train run for %s: cannot read trial PR #%d: %v — adoption deferred to the next poll\n", rec.TrainKey, rec.Trial.PRNum, err)
			return
		}
		switch {
		case pr == nil || strings.EqualFold(pr.State, "closed") && !pr.Merged:
			e.discardTrainRunMode(rec, wm, fmt.Sprintf("trial PR #%d is closed", rec.Trial.PRNum), discardCleanOnly)
			return
		case pr.Merged:
			e.discardTrainRunMode(rec, wm, fmt.Sprintf("trial PR #%d already merged — the landing is completed from the merged PR", rec.Trial.PRNum), discardCleanOnly)
			return
		case pr.HeadSHA != "" && rec.Trial.HeadSHA != "" && pr.HeadSHA != rec.Trial.HeadSHA:
			e.discardTrainRun(rec, wm, fmt.Sprintf("trial PR #%d head moved (%s → %s)", rec.Trial.PRNum, short(rec.Trial.HeadSHA), short(pr.HeadSHA)))
			return
		}
	}

	// R3: the pinned base is still resolvable in the bare clone. (It having MOVED is not a
	// failure: landGreenBatch's trialBehind / rebase cycle handles a moved base at landing.)
	if rec.BaseSHA != "" && e.trainValidateFn == nil {
		if _, err := gitRevParse(wm.baseDir, rec.BaseSHA+"^{commit}"); err != nil {
			e.discardTrainRun(rec, wm, fmt.Sprintf("pinned base %s is no longer in the clone", short(rec.BaseSHA)))
			return
		}
	}

	// Claim the partition (ADR-1208): from here on the dispatch guard, the pending-eject
	// routing and the invalidation gate all see this run exactly as they saw its worker.
	batchNumbers := map[int]bool{}
	for _, n := range rec.Original {
		batchNumbers[n] = true
	}
	for n := range need {
		batchNumbers[n] = true
	}
	ep := newTrainEpisode(rec.PartitionBase, rec.Original)
	state := &mergeTrainWorkerState{projectID: rec.ProjectID, batchNumbers: batchNumbers, episode: ep}
	if _, loaded := e.mergeTrainInFlight.LoadOrStore(rec.TrainKey, state); loaded {
		e.logfRepo(repoKey, "merge-train", "persisted train run for %s ignored: another worker already owns the partition\n", rec.TrainKey)
		e.trainRuns.dropPending(rec.TrainKey)
		return
	}

	// Rebuild the run. Live Status of each member is re-read (ADR-1871): a member whose live
	// Status shows the landing completed after the board snapshot must not be re-landed.
	holdingStg := hs
	extendTurns := false
	for _, it := range items {
		if hasLabel(it.Labels, "fabrik:extend-turns") {
			extendTurns = true
		}
	}
	namer := &trialNamer{base: rec.BaseTrialName, seq: rec.TrialSeq}
	p := trialParams{
		owner:            rec.Owner,
		repo:             rec.Repo,
		baseBranch:       rec.BaseBranch,
		trainKey:         rec.TrainKey,
		baseSHA:          rec.BaseSHA,
		wm:               wm,
		holdingStg:       holdingStg,
		maxTurnsOverride: mergeTrainMaxTurnsOverride(holdingStg, extendTurns),
		nextTrialName:    namer.next,
		namer:            namer,
		episode:          ep,
	}
	for _, it := range items {
		switch e.liveLandingState(state, p, it) {
		case liveMoved:
			e.mergeTrainInFlight.Delete(rec.TrainKey)
			e.discardTrainRun(rec, wm, fmt.Sprintf("member #%d already moved off %s (live status)", it.Number, hs.Name))
			return
		case liveReadFailed:
			e.mergeTrainInFlight.Delete(rec.TrainKey)
			return // a live read failed: try again next poll
		}
	}
	if e.trainValidateFn == nil {
		p.prefixCache = newTrainPrefixCache(rec.TrainKey, wm.BaseDir(), e.mergeTrainPrefixReuseDisabledForTest)
		p.prefixCache.sweepStaleRefs()
	}

	r := &trainRun{
		e:        e,
		trainKey: rec.TrainKey,
		store:    e.trainRuns,
		rec:      *rec,
		p:        p,
		state:    state,
		ep:       ep,
		members:  map[int]trainMember{},
		async:    true,
		started:  time.Now(),
	}
	for _, m := range rec.Members {
		tm := trainMember{prNum: m.PRNum, headSHA: m.HeadSHA, caughtUpFrom: m.CaughtUpFrom}
		if it, ok := byNum[m.Number]; ok {
			tm.item = it
		} else {
			tm.item = gh.ProjectItem{Number: m.Number, Repo: repoKey}
		}
		r.members[m.Number] = tm
	}
	if rec.Trial != nil {
		r.ci.restore(rec.Trial.CI)
		state.trialName = rec.Trial.Name
		state.prNum = rec.Trial.PRNum
	}
	state.bisecting = rec.Bisect != nil || rec.OAT != nil
	r.rec.Version = trainRunVersion
	if rec.OAT != nil {
		ep.setActive(r.memberList(liveOATMembers(rec.OAT)))
	} else {
		ep.setActive(r.memberList(rec.Current))
	}
	if rec.OAT != nil {
		ep.noteOneAtATime()
	}

	e.emitStructural(tui.JobStartedEvent{IssueNumber: 0, Repo: repoKey, Title: ep.title(), StageName: "Merge Train", StartedAt: r.started})
	e.trainRuns.register(r)
	e.syncRunPhase(r)
	e.logfRepo(repoKey, "merge-train", "resumed train run for %s after a restart: step %s, %d member(s)%s\n",
		rec.TrainKey, rec.Step, len(items), resumedTrialNote(rec.Trial))
}

// liveOATMembers is the part of the one-at-a-time fallback still to be processed.
func liveOATMembers(o *runOATRecord) []int {
	if o.Index >= len(o.Members) {
		return nil
	}
	return o.Members[o.Index:]
}

func resumedTrialNote(t *runTrialRecord) string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf(", trial %s (PR #%d)", t.Name, t.PRNum)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// discardMode says what a discard does to the trial the record described.
type discardMode int

const (
	// discardCloseTrial closes the trial PR and deletes its branch: the trial can no longer
	// be trusted (a member left, a head moved), and reconstructTrainState's Route 2 must not
	// find an open PR whose branch still carries the departed member's commits and land it.
	discardCloseTrial discardMode = iota
	// discardCleanOnly deletes the branch but does not close the PR (already merged/closed).
	discardCleanOnly
	// discardKeepTrial leaves the trial PR and branch alone: a landing was in progress and
	// the durable reconstruction routes finish it from them.
	discardKeepTrial
)

// discardTrainRun drops a record that cannot be resumed and closes the trial it described;
// the partition then forms fresh and reconstructTrainState sweeps any remnant.
func (e *Engine) discardTrainRun(rec *trainRunRecord, wm *WorktreeManager, why string) {
	e.discardTrainRunMode(rec, wm, why, discardCloseTrial)
}

// discardTrainRunMode is discardTrainRun with an explicit treatment of the trial. A nil wm
// is replaced by the repo's registered manager when there is one (a discard can happen
// before ensureRepoReady ran); with none, the PR is still closed and the orphaned branch is
// left to reconstructTrainState's Route 3 sweep.
func (e *Engine) discardTrainRunMode(rec *trainRunRecord, wm *WorktreeManager, why string, mode discardMode) {
	repoKey := rec.Owner + "/" + rec.Repo
	e.logfRepo(repoKey, "merge-train", "not resuming the persisted train run for %s: %s — forming fresh\n", rec.TrainKey, why)
	if t := rec.Trial; t != nil && mode != discardKeepTrial {
		if mode == discardCloseTrial && t.PRNum != 0 {
			if err := e.client.CloseIssue(rec.Owner, rec.Repo, t.PRNum); err != nil {
				e.logfRepo(repoKey, "merge-train", "warn: could not close discarded trial PR #%d: %v\n", t.PRNum, err)
			}
		}
		if wm == nil {
			e.mu.Lock()
			wm = e.worktreeManagers[repoKey]
			e.mu.Unlock()
		}
		if wm != nil && t.Name != "" {
			e.cleanupTrialArtifacts(repoKey, wm, t.Name)
		}
	}
	e.trainRuns.unregister(rec.TrainKey)
}

// ── phase source (R5) ──

// syncRunPhase derives the train's phase from the run's persisted position and feeds it to
// the one phase hook, restoring the persisted phase start. Used when a run is adopted; every
// live transition site already calls the same hook with the same phase builders.
func (e *Engine) syncRunPhase(r *trainRun) {
	r.mu.Lock()
	step, since := r.rec.Step, r.rec.PhaseStartedAt
	var b *bisectState
	if r.rec.Bisect != nil {
		cp := *r.rec.Bisect
		b = &cp
	}
	var tr *runTrialRecord
	if r.rec.Trial != nil {
		cp := *r.rec.Trial
		tr = &cp
	}
	cur := append([]int(nil), r.rec.Current...)
	r.mu.Unlock()

	var ph trainPhase
	var nums []int
	switch step {
	case stepMain:
		ph = phaseTrialCI(tr.PRNum)
		nums = tr.Members
	case stepHalf, stepBisect:
		used, cap := 1, 1
		if b != nil {
			used, cap = b.Used, b.Cap
		}
		ph = phaseBisecting(used+1, cap)
		if tr != nil {
			nums = tr.Members
		} else if b != nil {
			nums = b.Red
		}
	case stepOAT, stepSingle:
		ph = phaseOneAtATime()
	case stepLanding:
		ph = phaseLanding()
		nums = cur
	default:
		ph = phaseAssembling()
	}
	if since.IsZero() {
		since = e.now()
	}
	var members []trainMember
	if nums != nil {
		members = r.memberList(nums)
	}
	e.noteTrainPhaseAt(r.ep, r.repoKey(), members, ph, since)
}

// TrainRunPhase is the engine-state view of one open merge-train run: what its trial is
// doing, where it is in a bisection or the one-at-a-time fallback, and since when. It is the
// single source the TUI and status-line phase hook reads from (#2051 R5) — it needs no
// worker goroutine.
type TrainRunPhase struct {
	TrainKey string
	Phase    string // trial-ci | bisect | one-at-a-time | landing
	Step     string // the precise state-machine step
	Position int    // bisect: validation about to run (1-based); one-at-a-time: member index (1-based); else 0
	Of       int    // bisect: the cost cap; one-at-a-time: members in the fallback; else 0
	Since    time.Time
	TrialPR  int // the open trial's draft CI PR, 0 when none is open
	Members  []int
}

// TrainRunPhases reports every open run, ordered by train key. No open trial for a partition
// means no entry.
func (e *Engine) TrainRunPhases() []TrainRunPhase {
	var out []TrainRunPhase
	for _, r := range e.trainRuns.liveRuns() {
		if r.finished.Load() {
			continue
		}
		r.mu.Lock()
		ph := TrainRunPhase{
			TrainKey: r.trainKey,
			Phase:    runPhaseOf(r.rec.Step),
			Step:     r.rec.Step,
			Since:    r.rec.PhaseStartedAt,
			Members:  append([]int(nil), r.rec.Current...),
		}
		if r.rec.Trial != nil {
			ph.TrialPR = r.rec.Trial.PRNum
			ph.Members = append([]int(nil), r.rec.Trial.Members...)
		}
		if b := r.rec.Bisect; b != nil && ph.Phase == runPhaseBisect {
			ph.Position, ph.Of = b.Used+1, b.Cap
		}
		if o := r.rec.OAT; o != nil && ph.Phase == runPhaseOneAtATime {
			ph.Position, ph.Of = o.Index+1, len(o.Members)
		}
		r.mu.Unlock()
		out = append(out, ph)
	}
	return out
}

// trainRunOpenForRepo reports whether any open run exists for repoKey in any base partition.
func (e *Engine) trainRunOpenForRepo(repoKey string) bool {
	for _, r := range e.trainRuns.liveRuns() {
		if !r.finished.Load() && r.repoKey() == repoKey {
			return true
		}
	}
	return false
}

// ── engine wiring and test seams ──

// EnableTrainRunsForTest switches the engine to the asynchronous merge-train driver with run
// records persisted under dir ("" keeps them in memory). Two engines pointed at the same dir
// model a daemon restart. Test-only; production gets the store from New().
func (e *Engine) EnableTrainRunsForTest(dir string) {
	e.trainRuns = newTrainRunStore(dir)
}

// SetTrainRunDirForTest is EnableTrainRunsForTest for readability at restart sites.
func (e *Engine) SetTrainRunDirForTest(dir string) { e.EnableTrainRunsForTest(dir) }

// SetTrainValidateHoldForTest installs the hold predicate described on
// Engine.trainValidateHoldForTest.
func (e *Engine) SetTrainValidateHoldForTest(hold func() bool) { e.trainValidateHoldForTest = hold }
