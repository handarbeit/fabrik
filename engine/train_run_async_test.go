package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ── harness for the asynchronous merge-train driver (#2051) ──
//
// The trainValidateFn seam answers every trial at once, so the tests drive one verdict per
// "round": hold released → settleTrainRuns → wait for the step goroutine → hold re-armed.
// Two engines sharing one state directory model a daemon restart.

// asyncPartition is a non-default partition ("main"), the shape the pre-existing merge-train
// tests use: a default-base partition additionally runs the live base-label contradiction
// check at landing (#1773), which these tests have no reason to script.
const asyncPartition = "main"

var asyncTrainKey = mergeTrainKey("owner/repo", asyncPartition)

type asyncTrain struct {
	t      *testing.T
	eng    *Engine
	client *mockGitHubClient
	sv     *scriptedValidator
	hold   atomic.Bool
	board  *gh.ProjectBoard
	dir    string
	wm     *WorktreeManager
	batch  []gh.ProjectItem
}

func newAsyncTrain(t *testing.T, n int, verdict refVerdict) *asyncTrain {
	t.Helper()
	_, _, _, wm := setupTrainRepo(t)
	a := &asyncTrain{t: t, dir: t.TempDir(), wm: wm}
	a.sv = &scriptedValidator{verdict: verdict}
	a.batch = makeSeamBatch(n)
	a.board = &gh.ProjectBoard{ProjectID: "PVT_test", Items: a.batch}
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	a.client = client
	a.eng = a.configure(eng)
	return a
}

func (a *asyncTrain) configure(eng *Engine) *Engine {
	eng.trainValidateFn = a.sv.fn
	eng.cfg.CIBackstopTimeout = time.Hour
	eng.EnableTrainRunsForTest(a.dir)
	eng.SetTrainValidateHoldForTest(a.hold.Load)
	return eng
}

// restart replaces the engine with a fresh one over the same client, worktree manager and
// state directory — everything in memory is lost, as in a daemon restart.
func (a *asyncTrain) restart() {
	a.t.Helper()
	a.eng.wg.Wait()
	eng := trainTestEngine(a.t, a.client, &mockClaudeInvoker{}, a.wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = a.wm
	eng.mu.Unlock()
	a.eng = a.configure(eng)
}

func (a *asyncTrain) dispatch() {
	a.t.Helper()
	a.hold.Store(true)
	a.eng.dispatchMergeTrainWorker(context.Background(), a.batch, "PVT_test", asyncPartition)
	a.eng.wg.Wait()
}

// round releases the hold for exactly one settle pass and waits for any step it launched.
func (a *asyncTrain) round() {
	a.t.Helper()
	a.hold.Store(false)
	a.eng.settleTrainRuns(context.Background(), a.board)
	a.eng.wg.Wait()
	a.hold.Store(true)
}

func (a *asyncTrain) settleHeld() {
	a.t.Helper()
	a.hold.Store(true)
	a.eng.settleTrainRuns(context.Background(), a.board)
	a.eng.wg.Wait()
}

func (a *asyncTrain) open() bool {
	return len(a.eng.TrainRunPhases()) > 0
}

func (a *asyncTrain) calls() [][]int {
	a.sv.mu.Lock()
	defer a.sv.mu.Unlock()
	return append([][]int(nil), a.sv.calls...)
}

func (a *asyncTrain) merges() int {
	a.client.mu.Lock()
	defer a.client.mu.Unlock()
	return len(a.client.mergePRCalls)
}

func (a *asyncTrain) disposed() []int {
	var out []int
	for i := 1; i <= len(a.batch); i++ {
		if parityDisposed(a.client, i) {
			out = append(out, i)
		}
	}
	return out
}

func (a *asyncTrain) runToEnd(maxRounds int) int {
	a.t.Helper()
	for i := 0; i < maxRounds; i++ {
		if !a.open() {
			return i
		}
		a.round()
	}
	a.t.Fatalf("run still open after %d rounds: %+v", maxRounds, a.eng.TrainRunPhases())
	return maxRounds
}

func poisonedBy(poison ...int) refVerdict {
	return func(set []int) TrainCIResult {
		for _, p := range poison {
			if refHas(set, p) {
				return TrainCIRed
			}
		}
		return TrainCIGreen
	}
}

// syncReference runs the same scenario on the synchronous driver and returns what it did.
func syncReference(t *testing.T, n int, verdict refVerdict) refOutcome {
	t.Helper()
	got, _ := runParityScenario(t, n, verdict, 0)
	return got
}

// R2/FR-003/FR-006: the worker exits once the trial is open and recorded; a poll evaluates
// the trial's CI without any goroutine or slot being held, and lands it on green.
func TestTrainRunAsync_WorkerExitsAfterOpenThenPollLands(t *testing.T) {
	a := newAsyncTrain(t, 3, poisonedBy())
	a.dispatch()

	if a.eng.HasInFlightWorker() {
		t.Fatal("no worker goroutine may be live while the trial waits on CI")
	}
	phases := a.eng.TrainRunPhases()
	if len(phases) != 1 || phases[0].Phase != runPhaseTrialCI {
		t.Fatalf("want one open run in trial-ci, got %+v", phases)
	}
	if _, owned := a.eng.mergeTrainInFlight.Load(asyncTrainKey); !owned {
		t.Fatal("the open run must keep the partition claim (dispatch guard, ADR-1208)")
	}
	if a.merges() != 0 {
		t.Fatal("nothing may land before a verdict")
	}

	// A second dispatch for the same partition must not start another train.
	a.eng.dispatchMergeTrainWorker(context.Background(), a.batch, "PVT_test", asyncPartition)
	a.eng.wg.Wait()
	if n := len(a.calls()); n != 0 {
		t.Fatalf("duplicate dispatch evaluated a trial (%d validations)", n)
	}

	a.settleHeld() // no verdict yet: the trial is left alone
	if !a.open() || a.merges() != 0 {
		t.Fatal("a pending trial must be left alone")
	}

	a.round()
	if a.open() {
		t.Fatalf("green verdict must finish the run: %+v", a.eng.TrainRunPhases())
	}
	if a.merges() != 1 {
		t.Fatalf("merges = %d, want the batch landed once", a.merges())
	}
	if _, owned := a.eng.mergeTrainInFlight.Load(asyncTrainKey); owned {
		t.Fatal("claim must be cleared when the run finishes")
	}
	if got := a.calls(); len(got) != 1 {
		t.Fatalf("validations = %v, want exactly one (green common path)", got)
	}
}

// R4/SC-004: driven one verdict per poll, the async driver validates exactly the member
// sets the synchronous driver (and so the reference) does, and reaches the same outcome.
func TestTrainRunAsync_MatchesSynchronousDriver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		n      int
		poison []int
	}{
		{"single_poisoner", 6, []int{4}},
		{"first_member", 5, []int{1}},
		{"two_poisoners", 8, []int{2, 7}},
		{"interaction_fallback", 4, nil}, // replaced below with a pairwise verdict
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			verdict := poisonedBy(tc.poison...)
			if tc.name == "interaction_fallback" {
				verdict = func(set []int) TrainCIResult {
					if refHas(set, 1) && refHas(set, 4) {
						return TrainCIRed
					}
					return TrainCIGreen
				}
			}
			want := syncReference(t, tc.n, verdict)

			a := newAsyncTrain(t, tc.n, verdict)
			a.dispatch()
			a.runToEnd(100)
			assertParity(t, refOutcome{calls: a.calls(), ejected: a.disposed(), landed: a.merges()}, want)
		})
	}
}

// R3/FR-008/SC-001: a restart at ANY point of a run — mid-trial, mid-bisection, mid
// one-at-a-time — resumes at the same step: the validations still to run, and the
// final landed/ejected sets, equal an uninterrupted run's.
func TestTrainRunAsync_RestartAtEveryPointResumesSameRun(t *testing.T) {
	scenarios := []struct {
		name    string
		n       int
		verdict refVerdict
	}{
		{"green", 3, poisonedBy()},
		{"bisect_single_poisoner", 6, poisonedBy(5)},
		{"bisect_two_poisoners", 7, poisonedBy(2, 6)},
		{"one_at_a_time_interaction", 4, func(set []int) TrainCIResult {
			if refHas(set, 1) && refHas(set, 4) {
				return TrainCIRed
			}
			return TrainCIGreen
		}},
	}
	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			ref := newAsyncTrain(t, sc.n, sc.verdict)
			ref.dispatch()
			total := ref.runToEnd(100)
			want := refOutcome{calls: ref.calls(), ejected: ref.disposed(), landed: ref.merges()}

			for restartAfter := 0; restartAfter <= total; restartAfter++ {
				restartAfter := restartAfter
				t.Run(fmt.Sprintf("restart_after_%d", restartAfter), func(t *testing.T) {
					a := newAsyncTrain(t, sc.n, sc.verdict)
					a.dispatch()
					for i := 0; i < restartAfter && a.open(); i++ {
						a.round()
					}
					if a.open() {
						a.restart() // the old engine and everything in its memory is gone
						// The restarted engine must adopt the SAME run, not form a new train:
						// settle once with the hold on and check nothing was validated or built.
						before := len(a.calls())
						a.settleHeld()
						if len(a.calls()) != before {
							t.Fatalf("adoption evaluated a trial it should have left pending")
						}
						if !a.open() {
							t.Fatalf("restarted engine did not adopt the persisted run")
						}
					}
					a.runToEnd(100)
					assertParity(t, refOutcome{calls: a.calls(), ejected: a.disposed(), landed: a.merges()}, want)
				})
			}
		})
	}
}

// R2/FR-010/SC-003: a trial with no CI progress is surfaced by the poll within one poll of
// its deadline and handled as a timed-out trial (members stay Queued, nothing landed).
func TestTrainRunAsync_StuckTrialSurfacedWithinOnePollOfDeadline(t *testing.T) {
	a := newAsyncTrain(t, 3, poisonedBy())
	clk := newAdvClock()
	a.eng.SetClock(clk)
	a.dispatch()
	if !a.open() {
		t.Fatal("trial should be open")
	}

	backstop := a.eng.ciBackstopTimeout()
	clk.Advance(backstop - time.Second)
	a.settleHeld()
	if !a.open() {
		t.Fatal("a trial still inside its deadline must be left alone")
	}

	clk.Advance(2 * time.Second) // now past the deadline
	a.settleHeld()               // ONE poll
	if a.open() {
		t.Fatalf("the poll after the deadline must surface the stuck trial: %+v", a.eng.TrainRunPhases())
	}
	if a.merges() != 0 || len(a.calls()) != 0 {
		t.Fatalf("a timed-out trial lands nothing and validates nothing: merges=%d calls=%v", a.merges(), a.calls())
	}
	if _, owned := a.eng.mergeTrainInFlight.Load(asyncTrainKey); owned {
		t.Fatal("the partition must be released after the timeout")
	}
}

// R4/FR-007: the bisection cost cap holds across a restart — trials already spent still
// count, so an interrupted episode degrades to one-at-a-time exactly when an uninterrupted
// one does.
func TestTrainRunAsync_CostCapSurvivesRestart(t *testing.T) {
	verdict := poisonedBy(6)
	ref := newAsyncTrain(t, 6, verdict)
	ref.eng.cfg.MaxBisectValidations = 2
	ref.dispatch()
	ref.runToEnd(100)
	want := refOutcome{calls: ref.calls(), ejected: ref.disposed(), landed: ref.merges()}

	a := newAsyncTrain(t, 6, verdict)
	a.eng.cfg.MaxBisectValidations = 2
	a.dispatch()
	a.round() // initial red
	a.restart()
	a.eng.cfg.MaxBisectValidations = 99 // a config change after the restart must not move the cap
	a.settleHeld()                      // adopt
	if !a.open() {
		t.Fatal("the restarted engine did not adopt the persisted run")
	}
	a.runToEnd(100)
	assertParity(t, refOutcome{calls: a.calls(), ejected: a.disposed(), landed: a.merges()}, want)
}

// FR-011: concurrent polls never advance one run twice.
func TestTrainRunAsync_ConcurrentPollsAdvanceOnce(t *testing.T) {
	verdict := poisonedBy(3)
	want := syncReference(t, 5, verdict)

	a := newAsyncTrain(t, 5, verdict)
	a.dispatch()
	a.hold.Store(false)
	for i := 0; i < 60 && a.open(); i++ {
		var wg sync.WaitGroup
		for g := 0; g < 6; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a.eng.settleTrainRuns(context.Background(), a.board)
			}()
		}
		wg.Wait()
		a.eng.wg.Wait()
	}
	if a.open() {
		t.Fatal("run did not finish")
	}
	assertParity(t, refOutcome{calls: a.calls(), ejected: a.disposed(), landed: a.merges()}, want)
}

// FR-012/R5: the phase, position and phase start of every open run are readable from
// engine state, with no worker goroutine.
func TestTrainRunAsync_PhaseSourceReportsPhasePositionAndSince(t *testing.T) {
	a := newAsyncTrain(t, 4, poisonedBy(3))
	clk := newAdvClock()
	a.eng.SetClock(clk)

	if got := a.eng.TrainRunPhases(); len(got) != 0 {
		t.Fatalf("no run open yet, got %+v", got)
	}
	a.dispatch()
	ph := a.eng.TrainRunPhases()
	if len(ph) != 1 || ph[0].Phase != runPhaseTrialCI || ph[0].TrialPR != 900 || ph[0].Since.IsZero() {
		// The seam opens no PR, so TrialPR is 0 there; the phase and clock are what matter.
		if len(ph) != 1 || ph[0].Phase != runPhaseTrialCI || ph[0].Since.IsZero() {
			t.Fatalf("trial-ci phase = %+v", ph)
		}
	}
	started := ph[0].Since

	clk.Advance(time.Minute)
	a.round() // red → bisecting; first half {1,2}
	ph = a.eng.TrainRunPhases()
	if len(ph) != 1 || ph[0].Phase != runPhaseBisect || ph[0].Position != 2 || ph[0].Of != a.eng.effectiveBisectCap() {
		t.Fatalf("bisect phase = %+v, want position 2 of the cost cap", ph)
	}
	if !ph[0].Since.After(started) {
		t.Fatalf("phase start must move on a phase change: %v !> %v", ph[0].Since, started)
	}
	since := ph[0].Since

	clk.Advance(time.Minute)
	a.restart()
	a.settleHeld()
	ph = a.eng.TrainRunPhases()
	if len(ph) != 1 || ph[0].Phase != runPhaseBisect || !ph[0].Since.Equal(since) {
		t.Fatalf("after a restart the phase start must be the persisted one: %+v (want since %v)", ph, since)
	}
}

// ── validity (FR-009): anything unusable degrades to a fresh formation, never a wedge ──

func TestTrainRunAsync_StaleOrInvalidRecordFallsBackToFreshFormation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(a *asyncTrain)
	}{
		{"member_paused_while_down", func(a *asyncTrain) { a.board.Items[1].Labels = append(a.board.Items[1].Labels, "fabrik:paused") }},
		{"member_left_queued", func(a *asyncTrain) { a.board.Items[0].Status = "Implement" }},
		{"member_closed", func(a *asyncTrain) { a.board.Items[2].IsClosed = true }},
		{"member_vanished", func(a *asyncTrain) { a.board.Items = a.board.Items[:2] }},
		{"member_pr_head_moved", func(a *asyncTrain) { a.board.Items[0].LinkedPRHeadSHA = "deadbeefcafe" }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a := newAsyncTrain(t, 3, poisonedBy())
			a.dispatch()
			if !a.open() {
				t.Fatal("trial should be open")
			}
			files, _ := filepath.Glob(filepath.Join(a.dir, "*.json"))
			if len(files) != 1 {
				t.Fatalf("want one persisted record, got %v", files)
			}
			a.restart()
			tc.mutate(a)
			a.settleHeld()
			if a.open() {
				t.Fatal("an invalid record must not be adopted")
			}
			if _, owned := a.eng.mergeTrainInFlight.Load(asyncTrainKey); owned {
				t.Fatal("a discarded record must not leave the partition claimed")
			}
			if left, _ := filepath.Glob(filepath.Join(a.dir, "*.json")); len(left) != 0 {
				t.Fatalf("a discarded record must be removed from disk, left %v", left)
			}
			if a.merges() != 0 {
				t.Fatal("nothing may be landed from stale state")
			}
		})
	}
}

func TestTrainRunAsync_CorruptOrUnknownVersionRecordIsQuarantined(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":         `{"version": 1, "train_key": "owner/repo:main", "step": `,
		"unknown_version": `{"version": 99, "train_key": "owner/repo:main", "step": "trial-ci"}`,
		"no_train_key":    `{"version": 1}`,
	} {
		content := content
		t.Run(name, func(t *testing.T) {
			a := newAsyncTrain(t, 3, poisonedBy())
			path := trainRunFile(a.dir, asyncTrainKey)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			a.settleHeld()
			if a.eng.trainRuns.hasPending(asyncTrainKey) || a.open() {
				t.Fatal("an unusable record must be treated as absent")
			}
			if _, err := os.Stat(path + ".corrupt"); err != nil {
				t.Fatalf("an unusable record must be quarantined: %v", err)
			}
			// The partition forms fresh as it does today.
			a.dispatch()
			if !a.open() {
				t.Fatal("a fresh train must be able to start after a quarantined record")
			}
		})
	}
}

// ── the store ──

func TestTrainRunStore_RoundTripAndMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	s := newTrainRunStore(dir)
	rec := &trainRunRecord{
		Version: trainRunVersion, TrainKey: "o/r:main", Owner: "o", Repo: "r", PartitionBase: "main",
		Step: stepHalf, Original: []int{1, 2, 3, 4}, Current: []int{1, 2, 3, 4},
		Members: []runMemberRecord{{Number: 1, PRNum: 101, HeadSHA: "abc"}},
		Trial:   &runTrialRecord{Kind: "half", Name: "merge-train-main-1-t1", PRNum: 77, Members: []int{1, 2}, Deadline: time.Unix(1_800_000_000, 0).UTC()},
		Bisect:  newBisectState([]int{1, 2, 3, 4}, &trainCIDiagnostic{Note: "n", PRNum: 9}, 5),
	}
	rec.Trial.CI = (&trialCIState{rr: rerunState{done: true, runIDs: []int64{3}, firstFailedIDs: map[int64]bool{11: true}}}).record()
	if err := s.write(rec); err != nil {
		t.Fatal(err)
	}
	s2 := newTrainRunStore(dir)
	if notes := s2.loadDir(); len(notes) != 0 {
		t.Fatalf("unexpected load notes: %v", notes)
	}
	got := s2.pending["o/r:main"]
	if got == nil {
		t.Fatal("record not loaded")
	}
	got.WrittenAt = rec.WrittenAt
	if !reflect.DeepEqual(got, rec) {
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(rec)
		t.Fatalf("round trip differs:\n got %s\nwant %s", a, b)
	}
	var st trialCIState
	st.restore(got.Trial.CI)
	if !st.rr.done || !st.rr.firstFailedIDs[11] || len(st.rr.runIDs) != 1 {
		t.Fatalf("CI state did not survive: %+v", st.rr)
	}

	mem := newTrainRunStore("")
	if err := mem.write(rec); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("a directory-less store must not touch disk, found %d entries", len(entries))
	}
	s.remove("o/r:main")
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("remove must delete the file, left %d", len(entries))
	}
}

func TestTrainRunStore_ConcurrentWritesAreSafe(t *testing.T) {
	s := newTrainRunStore(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				rec := &trainRunRecord{Version: trainRunVersion, TrainKey: fmt.Sprintf("o/r:%d", i%3), Step: stepForm}
				if err := s.write(rec); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
	s2 := newTrainRunStore(s.dir)
	if notes := s2.loadDir(); len(notes) != 0 {
		t.Fatalf("concurrent writes left an unreadable file: %v", notes)
	}
	if len(s2.pending) != 3 {
		t.Fatalf("want 3 records, got %d", len(s2.pending))
	}
}

// A step cancelled by shutdown leaves the record exactly as it was: the verdict is simply
// consumed by the next incarnation (or the next poll), and nothing lands or is ejected.
func TestTrainRunAsync_CancelledStepLeavesRecordIntact(t *testing.T) {
	a := newAsyncTrain(t, 3, poisonedBy(2))
	a.dispatch()
	before, err := os.ReadFile(trainRunFile(a.dir, asyncTrainKey))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.hold.Store(false)
	a.eng.settleTrainRuns(ctx, a.board) // a verdict exists, but the daemon is shutting down
	a.eng.wg.Wait()
	a.hold.Store(true)

	after, err := os.ReadFile(trainRunFile(a.dir, asyncTrainKey))
	if err != nil {
		t.Fatalf("record removed by a cancelled step: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("a cancelled step rewrote the record:\nbefore: %s\nafter:  %s", before, after)
	}
	if !a.open() || a.merges() != 0 || len(a.disposed()) != 0 {
		t.Fatalf("a cancelled step must change nothing: open=%v merges=%d disposed=%v", a.open(), a.merges(), a.disposed())
	}
	if a.eng.HasInFlightWorker() {
		t.Fatal("a cancelled step must release its liveness marker")
	}
	a.runToEnd(100) // and the next poll with a live context finishes the run
}

// Ownership (ADR-1208 / FR-011): while a trial waits on CI with no goroutine, the partition
// is still owned — the claim, the batch numbers (pending-eject routing) and the repo-level
// liveness answer used by the closed-item rescue all say so, while the auto-upgrade idle
// guard (HasInFlightWorker) does not.
func TestTrainRunAsync_OpenRunKeepsOwnershipWithoutALiveWorker(t *testing.T) {
	a := newAsyncTrain(t, 3, poisonedBy())
	a.dispatch()

	members, owned := a.eng.mergeTrainBatchMembers(asyncTrainKey)
	if !owned || !members[1] || !members[2] || !members[3] {
		t.Fatalf("pending-eject routing must still see the run's members: %v owned=%v", members, owned)
	}
	if !a.eng.mergeTrainWorkerActiveForRepo("owner/repo") {
		t.Fatal("an open run must count as live for the closed-item rescue")
	}
	if a.eng.HasInFlightWorker() {
		t.Fatal("a trial only waiting on CI must not block an auto-upgrade restart")
	}
	a.runToEnd(10)
	if a.eng.mergeTrainWorkerActiveForRepo("owner/repo") {
		t.Fatal("a finished run must not count as live")
	}
}

// A pending (unadopted) record holds the partition off: no fresh train is formed over it.
func TestTrainRunAsync_PendingRecordBlocksFreshDispatch(t *testing.T) {
	a := newAsyncTrain(t, 3, poisonedBy())
	a.dispatch()
	a.restart()
	// Loaded but not yet adopted (no settle yet).
	a.eng.trainRuns.loadDir()
	if !a.eng.trainRuns.hasPending(asyncTrainKey) {
		t.Fatal("the persisted record should be pending adoption")
	}
	a.eng.dispatchMergeTrainWorker(context.Background(), a.batch, "PVT_test", asyncPartition)
	a.eng.wg.Wait()
	if _, owned := a.eng.mergeTrainInFlight.Load(asyncTrainKey); owned {
		t.Fatal("dispatch must not form a train over a record awaiting adoption")
	}
}
