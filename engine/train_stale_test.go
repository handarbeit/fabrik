package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ─── evalTrialCI: the dirty classification (#2298 R1) ─────────────────────────────────

func dirtyEvalEngine(t *testing.T, behind func() (int, error)) (*Engine, *mockGitHubClient) {
	t.Helper()
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return nil, "dirty", nil
		},
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) { return behind() },
	}
	return trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir())), client
}

var staleProbe = trialBaseProbe{baseBranch: "main", trialBranch: trainBranchPrefix + "t1"}

func TestEvalTrialCI_DirtyOnMovedBaseIsStale(t *testing.T) {
	eng, client := dirtyEvalEngine(t, func() (int, error) { return 2, nil })
	st := trialCIState{probe: staleProbe}
	res, diag, decided := eng.evalTrialCI(context.Background(), "owner", "repo", 42, "sha123", &st)
	if !decided || res != TrainCIStale {
		t.Fatalf("dirty + base moved = (%v, decided=%v), want stale", res, decided)
	}
	if diag == nil || !diag.BaseMoveObserved || diag.PRNum != 42 {
		t.Errorf("diagnostic = %+v, want BaseMoveObserved on PR 42", diag)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.fetchCommitsBehindCalls) != 1 {
		t.Fatalf("FetchCommitsBehind calls = %d, want 1", len(client.fetchCommitsBehindCalls))
	}
	c := client.fetchCommitsBehindCalls[0]
	if c.base != "main" || c.head != trainBranchPrefix+"t1" {
		t.Errorf("compared %s...%s, want main...%st1", c.base, c.head, trainBranchPrefix)
	}
}

func TestEvalTrialCI_DirtyWithUnreadableBaseFailsTowardStale(t *testing.T) {
	eng, _ := dirtyEvalEngine(t, func() (int, error) { return 0, errors.New("boom") })
	st := trialCIState{probe: staleProbe}
	res, diag, decided := eng.evalTrialCI(context.Background(), "owner", "repo", 42, "sha123", &st)
	if !decided || res != TrainCIStale {
		t.Fatalf("dirty + unreadable base = (%v, decided=%v), want stale", res, decided)
	}
	if diag == nil || diag.BaseMoveObserved {
		t.Errorf("diagnostic = %+v, want BaseMoveObserved false (a move was not confirmed)", diag)
	}
}

func TestEvalTrialCI_DirtyOnUnmovedBaseStaysRed(t *testing.T) {
	eng, _ := dirtyEvalEngine(t, func() (int, error) { return 0, nil })
	st := trialCIState{probe: staleProbe}
	res, diag, decided := eng.evalTrialCI(context.Background(), "owner", "repo", 42, "sha123", &st)
	if !decided || res != TrainCIRed {
		t.Fatalf("dirty + base unmoved = (%v, decided=%v), want red", res, decided)
	}
	if diag == nil || !strings.Contains(diag.Note, "has not moved") {
		t.Errorf("diagnostic note = %+v, want it to say the base has not moved", diag)
	}
}

func TestEvalTrialCI_DirtyWithoutProbeOrWithSeamIsRed(t *testing.T) {
	eng, client := dirtyEvalEngine(t, func() (int, error) { return 5, nil })
	// No probe: the pre-#2298 reading, no compare read.
	var st trialCIState
	if res, _, _ := eng.evalTrialCI(context.Background(), "owner", "repo", 42, "sha123", &st); res != TrainCIRed {
		t.Errorf("dirty without a probe = %v, want red", res)
	}
	if n := len(client.fetchCommitsBehindCalls); n != 0 {
		t.Errorf("FetchCommitsBehind calls = %d, want 0 without a probe", n)
	}
	// Neutralisation seam: dirty is red unconditionally.
	eng.SetTrainStaleAsRedForTest(true)
	st = trialCIState{probe: staleProbe}
	if res, _, _ := eng.evalTrialCI(context.Background(), "owner", "repo", 42, "sha123", &st); res != TrainCIRed {
		t.Errorf("dirty under SetTrainStaleAsRedForTest = %v, want red", res)
	}
}

func TestRecordTrialIfCounts_StaleIsNotCharged(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	p := trialParams{trainKey: "owner/repo:main"}
	eng.recordTrialIfCounts(context.Background(), p, nil, TrainCIStale, nil)
	if got := trialCount(eng, p.trainKey); got != 0 {
		t.Errorf("stale charged the runaway guard %d time(s), want 0", got)
	}
	eng.recordTrialIfCounts(context.Background(), p, nil, TrainCIRed, nil)
	if got := trialCount(eng, p.trainKey); got != 1 {
		t.Errorf("red charged %d, want 1 (control)", got)
	}
}

// ─── the state machine under the synchronous driver ───────────────────────────────────

// staleSeamValidator scripts one verdict per call (the last repeats) and records the
// member numbers of every trial.
type staleSeamValidator struct {
	mu     sync.Mutex
	script []TrainCIResult
	calls  [][]int
}

func (v *staleSeamValidator) fn(_ context.Context, members []trainMember) (TrainCIResult, *trainCIDiagnostic) {
	v.mu.Lock()
	defer v.mu.Unlock()
	nums := make([]int, len(members))
	for i, m := range members {
		nums[i] = m.item.Number
	}
	i := len(v.calls)
	v.calls = append(v.calls, nums)
	if i >= len(v.script) {
		i = len(v.script) - 1
	}
	switch res := v.script[i]; res {
	case TrainCIRed:
		return res, &trainCIDiagnostic{FailedChecks: []gh.CheckRun{{Name: "ci/test", Status: "completed", Conclusion: "failure"}}, PRNum: 900, TrialSHA: "seam"}
	case TrainCIStale:
		return res, &trainCIDiagnostic{Note: "the base moved under the trial", PRNum: 900, TrialSHA: "seam", BaseMoveObserved: true}
	default:
		return res, nil
	}
}

func (v *staleSeamValidator) seen() [][]int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([][]int(nil), v.calls...)
}

type staleRun struct {
	eng    *Engine
	client *mockGitHubClient
	v      *staleSeamValidator
}

// runStaleWorker runs a whole sync-driver episode of n members under the scripted verdicts.
func runStaleWorker(t *testing.T, n int, neutralise bool, tune func(*Engine), script ...TrainCIResult) staleRun {
	t.Helper()
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	v := &staleSeamValidator{script: script}
	eng.trainValidateFn = v.fn
	eng.SetTrainStaleAsRedForTest(neutralise)
	if tune != nil {
		tune(eng)
	}
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", "main"), state)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", "main", makeSeamBatch(n))
	return staleRun{eng: eng, client: client, v: v}
}

// uncharged lists every way a member was charged or touched: ejection, pause, reroute
// off Queued, the "own combined Validate is failing" wording, a runaway-guard trial. An
// empty result means the stale verdict was handled without charging anyone.
func (s staleRun) unchargedProblems(n int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		if c := ejectionCommentCount(s.client, i); c != 0 {
			out = append(out, fmt.Sprintf("member #%d got %d ejection comment(s)", i, c))
		}
		s.eng.mergeTrainEjectionsMu.Lock()
		cnt := s.eng.mergeTrainEjectionCounts[fmt.Sprintf("owner/repo#%d", i)]
		s.eng.mergeTrainEjectionsMu.Unlock()
		if cnt != 0 {
			out = append(out, fmt.Sprintf("member #%d counted %d toward MaxMergeTrainEjections", i, cnt))
		}
	}
	s.client.mu.Lock()
	defer s.client.mu.Unlock()
	for _, c := range s.client.addLabelCalls {
		if strings.Contains(c.labelName, "paused") || strings.Contains(c.labelName, "revalidate") {
			out = append(out, fmt.Sprintf("label %q applied", c.labelName))
		}
	}
	for _, c := range s.client.addCommentCalls {
		if strings.Contains(c.body, "combined Validate is failing") || strings.Contains(c.body, "auto-repair") {
			out = append(out, fmt.Sprintf("comment blames the member: %q", c.body))
		}
	}
	return out
}

// checkRebuilt asserts: the stale trial was followed by a trial of the same member set,
// nobody was charged, the runaway guard recorded nothing and the batch then landed.
func checkRebuilt(t *testing.T, s staleRun, members []int) []string {
	t.Helper()
	probs := s.unchargedProblems(len(members))
	calls := s.v.seen()
	if len(calls) != 2 || !sameInts(calls[0], members) || !sameInts(calls[1], members) {
		probs = append(probs, fmt.Sprintf("trials = %v, want the same set %v twice (stale, then rebuilt)", calls, members))
	}
	if got := trialCount(s.eng, mergeTrainKey("owner/repo", "main")); got != 0 {
		probs = append(probs, fmt.Sprintf("runaway guard recorded %d trial(s)", got))
	}
	s.client.mu.Lock()
	merges := len(s.client.mergePRCalls)
	s.client.mu.Unlock()
	if merges == 0 {
		probs = append(probs, "nothing landed after the rebuild")
	}
	return probs
}

func TestStaleTrial_SingletonIsRebuiltNotRepaired(t *testing.T) {
	for _, neutralise := range []bool{false, true} {
		s := runStaleWorker(t, 1, neutralise, nil, TrainCIStale, TrainCIGreen)
		probs := checkRebuilt(t, s, []int{1})
		if neutralise {
			if len(probs) == 0 {
				t.Error("non-vacuity: with dirty mapped back to red the singleton scenario must fail, but it passed")
			}
			continue
		}
		if len(probs) > 0 {
			t.Errorf("singleton stale rebuild: %s", strings.Join(probs, "; "))
		}
	}
}

func TestStaleTrial_BatchIsRebuiltWithoutBisection(t *testing.T) {
	for _, neutralise := range []bool{false, true} {
		s := runStaleWorker(t, 3, neutralise, nil, TrainCIStale, TrainCIGreen)
		probs := checkRebuilt(t, s, []int{1, 2, 3})
		if neutralise {
			if len(probs) == 0 {
				t.Error("non-vacuity: with dirty mapped back to red the batch scenario must fail, but it passed")
			}
			continue
		}
		if len(probs) > 0 {
			t.Errorf("batch stale rebuild: %s", strings.Join(probs, "; "))
		}
	}
}

// A stale bisection half ends the episode without isolating anyone: the origin batch is
// re-formed and validated whole on the new base (R4).
func TestStaleTrial_BisectionHalfEndsEpisodeWithoutEjection(t *testing.T) {
	for _, neutralise := range []bool{false, true} {
		s := runStaleWorker(t, 4, neutralise, nil, TrainCIRed, TrainCIStale, TrainCIGreen)
		calls := s.v.seen()
		var probs []string
		if len(calls) != 3 || len(calls[0]) != 4 || len(calls[1]) != 2 || !sameInts(calls[2], []int{1, 2, 3, 4}) {
			probs = append(probs, fmt.Sprintf("trials = %v, want full, first half (stale), full again", calls))
		}
		probs = append(probs, s.unchargedProblems(4)...)
		if neutralise {
			if len(probs) == 0 {
				t.Error("non-vacuity: with dirty mapped back to red the bisection scenario must fail, but it passed")
			}
			continue
		}
		if len(probs) > 0 {
			t.Errorf("stale bisection half: %s", strings.Join(probs, "; "))
		}
	}
}

// A stale one-at-a-time trial retries the same member on the new base; the cursor does
// not advance and nobody is ejected.
func TestStaleTrial_OneAtATimeRetriesSameMember(t *testing.T) {
	for _, neutralise := range []bool{false, true} {
		s := runStaleWorker(t, 2, neutralise, func(e *Engine) { e.cfg.MaxBisectValidations = 1 },
			TrainCIRed, TrainCIStale, TrainCIGreen, TrainCIGreen)
		calls := s.v.seen()
		var probs []string
		if len(calls) != 4 || !sameInts(calls[1], []int{1}) || !sameInts(calls[2], []int{1}) || !sameInts(calls[3], []int{2}) {
			probs = append(probs, fmt.Sprintf("trials = %v, want full(red), #1 (stale), #1 again, #2", calls))
		}
		probs = append(probs, s.unchargedProblems(2)...)
		if neutralise {
			if len(probs) == 0 {
				t.Error("non-vacuity: with dirty mapped back to red the one-at-a-time scenario must fail, but it passed")
			}
			continue
		}
		if len(probs) > 0 {
			t.Errorf("stale one-at-a-time trial: %s", strings.Join(probs, "; "))
		}
	}
}

// Past max_train_rebase_cycles the batch is dissolved back to Queued, uncharged, naming the
// base moving under the trial; no pause, no ejection.
func TestStaleTrial_BudgetExhaustedDissolvesUncharged(t *testing.T) {
	for _, neutralise := range []bool{false, true} {
		s := runStaleWorker(t, 2, neutralise, func(e *Engine) { e.cfg.MaxTrainRebaseCycles = 2 }, TrainCIStale)
		calls := s.v.seen()
		var probs []string
		if len(calls) != 3 {
			probs = append(probs, fmt.Sprintf("trials = %d, want 3 (initial + 2 rebuilds, the third stale one dissolves)", len(calls)))
		}
		probs = append(probs, s.unchargedProblems(2)...)
		dissolved := 0
		s.client.mu.Lock()
		for _, c := range s.client.addCommentCalls {
			if strings.Contains(c.body, "batch dissolved") && strings.Contains(c.body, "kept moving under the trial") {
				dissolved++
			}
		}
		s.client.mu.Unlock()
		if dissolved != 2 {
			probs = append(probs, fmt.Sprintf("dissolve comments = %d, want 2 (one per member)", dissolved))
		}
		if got := trialCount(s.eng, mergeTrainKey("owner/repo", "main")); got != 0 {
			probs = append(probs, fmt.Sprintf("runaway guard recorded %d trial(s)", got))
		}
		if neutralise {
			if len(probs) == 0 {
				t.Error("non-vacuity: with dirty mapped back to red the exhaustion scenario must fail, but it passed")
			}
			continue
		}
		if len(probs) > 0 {
			t.Errorf("stale budget exhaustion: %s", strings.Join(probs, "; "))
		}
	}
}

// ─── persistence and restart-safety (R2, R7) ──────────────────────────────────────────

func TestTrainRunRecord_RebuildFieldsRoundTripAndOldFilesDecodeToZero(t *testing.T) {
	dir := t.TempDir()
	store := newTrainRunStore(dir)
	rec := &trainRunRecord{Version: trainRunVersion, TrainKey: "owner/repo:main", Owner: "owner", Repo: "repo",
		Step: stepForm, Current: []int{1}, RebaseCycles: 2, StaleTrial: "t1", Rebuild: true}
	if err := store.write(rec); err != nil {
		t.Fatal(err)
	}
	reload := newTrainRunStore(dir)
	if notes := reload.loadDir(); len(notes) != 0 {
		t.Fatalf("load notes: %v", notes)
	}
	got := reload.pending["owner/repo:main"]
	if got == nil || got.RebaseCycles != 2 || got.StaleTrial != "t1" || !got.Rebuild {
		t.Fatalf("reloaded record = %+v, want RebaseCycles 2, StaleTrial t1, Rebuild", got)
	}

	old := &trainRunRecord{Version: trainRunVersion, TrainKey: "owner/repo:other", Owner: "owner", Repo: "repo", Step: stepForm, Current: []int{1}}
	if err := store.write(old); err != nil {
		t.Fatal(err)
	}
	reload = newTrainRunStore(dir)
	reload.loadDir()
	if o := reload.pending["owner/repo:other"]; o == nil || o.RebaseCycles != 0 || o.StaleTrial != "" || o.Rebuild {
		t.Errorf("a record without the new fields must decode to zero values, got %+v", o)
	}
}

func staleReplayRun(t *testing.T, replay bool) (*Engine, *trainRun) {
	t.Helper()
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, _, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	namer := &trialNamer{base: "merge-train-test"}
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", trainKey: mergeTrainKey("owner/repo", "main"),
		wm: wm, nextTrialName: namer.next, namer: namer}
	members := map[int]trainMember{}
	for _, it := range makeSeamBatch(2) {
		members[it.Number] = trainMember{item: it, prNum: 100 + it.Number, headSHA: fmt.Sprintf("sha-%d", it.Number)}
	}
	rec := trainRunRecord{TrainKey: p.trainKey, Owner: "owner", Repo: "repo", Step: stepMain, Current: []int{1, 2},
		RebaseCycles: 1, Trial: &runTrialRecord{Kind: "main", Name: "t1", PRNum: 900, Members: []int{1, 2}}}
	if replay {
		rec.StaleTrial = "t1"
	}
	return eng, &trainRun{e: eng, trainKey: p.trainKey, p: p, state: state, members: members, rec: rec, detached: true}
}

// Replaying the stale verdict for a trial whose cycle was already counted (a restart between
// the write-ahead and the end of the rebuild) finishes the rebuild without a second count.
func TestStaleTrial_ReplayDoesNotCountTwice(t *testing.T) {
	eng, r := staleReplayRun(t, true)
	tr := r.trial()
	if out := eng.handleStaleTrial(context.Background(), r, tr, nil); out != outContinue {
		t.Fatalf("outcome = %v, want continue", out)
	}
	if r.rec.RebaseCycles != 1 {
		t.Errorf("RebaseCycles = %d after a replay, want it still 1", r.rec.RebaseCycles)
	}
	if r.rec.Trial != nil || r.rec.Step != stepForm {
		t.Errorf("replay left trial=%v step=%q, want no trial at %q", r.rec.Trial, r.rec.Step, stepForm)
	}
}

// A first verdict counts exactly one cycle, keys it to the trial and sets the re-form state.
func TestStaleTrial_FirstVerdictCountsOneCycleWriteAhead(t *testing.T) {
	eng, r := staleReplayRun(t, false)
	tr := r.trial()
	if out := eng.handleStaleTrial(context.Background(), r, tr, nil); out != outContinue {
		t.Fatalf("outcome = %v, want continue", out)
	}
	if r.rec.RebaseCycles != 2 || r.rec.StaleTrial != "t1" || !r.rec.Rebuild {
		t.Errorf("record = cycles %d stale %q rebuild %v, want 2 / t1 / true", r.rec.RebaseCycles, r.rec.StaleTrial, r.rec.Rebuild)
	}
	if !sameInts(r.rec.Current, []int{1, 2}) {
		t.Errorf("Current = %v, want the trial's members [1 2]", r.rec.Current)
	}
}

// The rebuild budget is shared with the landing rebase: cycles already spent on stale
// rebuilds leave only the remainder at landing, and landing spends from the same counter.
func TestLandGreenBatchRun_SharesTheRunsRebuildBudget(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	client.fetchCommitsBehindFn = func(_, _, _, _ string) (int, error) { return 1, nil } // never catches up
	eng.cfg.MaxTrainRebaseCycles = 3
	state := &mergeTrainWorkerState{projectID: "PVT_test", trialName: "t1", prNum: 900}
	namer := &trialNamer{base: "merge-train-test"}
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", trainKey: mergeTrainKey("owner/repo", "main"),
		wm: wm, nextTrialName: namer.next, namer: namer}
	r := &trainRun{e: eng, trainKey: p.trainKey, p: p, state: state, members: map[int]trainMember{}, detached: true,
		rec: trainRunRecord{RebaseCycles: 2}} // two cycles already spent on stale rebuilds

	var survivors []trainMember
	for _, it := range makeSeamBatch(2) {
		survivors = append(survivors, trainMember{item: it, prNum: 100 + it.Number, headSHA: "sha"})
	}
	eng.landGreenBatchRun(context.Background(), r, state, p, survivors)
	if r.rec.RebaseCycles != 3 {
		t.Errorf("RebaseCycles = %d, want 3: landing may spend only the one remaining cycle, then dissolve", r.rec.RebaseCycles)
	}
	dissolved := 0
	client.mu.Lock()
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "batch dissolved") {
			dissolved++
		}
	}
	client.mu.Unlock()
	if dissolved != 2 {
		t.Errorf("dissolve comments = %d, want 2", dissolved)
	}
}
