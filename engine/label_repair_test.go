package engine

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Unit coverage for the confirm-and-re-apply (label_repair.go), the intent record
// (label_intent.go) and the trust gate (label_trust.go), #2551 / ADR 2551. The sim
// twins (tests/sim/label_divergence_test.go) cover the end-to-end shapes; these pin
// each branch of the repair and each refusal of the gate.

type labelRepairFixture struct {
	e      *Engine
	client *mockGitHubClient
	lock   string
	reads  atomic.Int32
	live   atomic.Value // []string returned by FetchLabels
	readFn func() ([]string, error)
}

func newLabelRepairFixture(t *testing.T) *labelRepairFixture {
	t.Helper()
	f := &labelRepairFixture{client: &mockGitHubClient{}}
	f.live.Store([]string{})
	f.client.fetchLabelsFn = func(owner, repo string, n int) ([]string, error) {
		f.reads.Add(1)
		if f.readFn != nil {
			return f.readFn()
		}
		return f.live.Load().([]string), nil
	}
	f.e = testEngine(t, f.client, &mockClaudeInvoker{})
	f.lock = f.e.lockLabel()
	f.e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Implement"}})
	return f
}

func (f *labelRepairFixture) withWorker() {
	f.e.store.Apply(itemstate.LocalLockAcquired{
		Repo: "owner/repo", Number: 1, User: "testuser", AcquiredAt: time.Now(),
		Worker: &itemstate.WorkerHandle{StageName: "Implement", StartedAt: time.Now(), LastSignAt: time.Now()},
	})
}

func (f *labelRepairFixture) divergence(label, written string) labelDivergence {
	return labelDivergence{repo: "owner/repo", number: 1, label: label, written: written, at: f.e.now()}
}

func (f *labelRepairFixture) adds() int {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	return len(f.client.addLabelCalls)
}

func (f *labelRepairFixture) removes() int {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	return len(f.client.removeLabelCalls)
}

func (f *labelRepairFixture) problems() int {
	f.e.labels.mu.Lock()
	defer f.e.labels.mu.Unlock()
	return len(f.e.labels.problems)
}

// SC-001: a lock add that did not stick is re-applied with exactly one live read
// and one write.
func TestLabelRepair_LostLockAddIsReappliedOnce(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.withWorker()
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true)
	f.e.labels.mark("owner/repo#1", f.lock, f.e.now())
	f.e.repairLabel(f.divergence(f.lock, "added"))
	if f.reads.Load() != 1 || f.adds() != 1 {
		t.Fatalf("reads=%d adds=%d, want 1 and 1", f.reads.Load(), f.adds())
	}
	if f.e.labels.marked("owner/repo#1", f.lock) {
		t.Error("the divergence mark was not cleared after the re-apply")
	}
}

// SC-003 / FR-003: the listing lagged, GitHub already matches the intent: nothing written.
func TestLabelRepair_LiveAgreesWritesNothing(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.withWorker()
	f.live.Store([]string{f.lock})
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true)
	f.e.labels.mark("owner/repo#1", f.lock, f.e.now())
	f.e.repairLabel(f.divergence(f.lock, "added"))
	if f.reads.Load() != 1 {
		t.Fatalf("reads=%d, want exactly 1", f.reads.Load())
	}
	if f.adds() != 0 || f.removes() != 0 {
		t.Fatalf("a lagging listing was fought: adds=%d removes=%d", f.adds(), f.removes())
	}
	if f.e.labels.marked("owner/repo#1", f.lock) {
		t.Error("the mark must clear when GitHub matches the intent")
	}
}

// FR-003: the intent has ended (no worker), so the divergence is not acted on and no
// read is made.
func TestLabelRepair_IntentNoLongerCurrent(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true)
	f.e.repairLabel(f.divergence(f.lock, "added"))
	if f.reads.Load() != 0 || f.adds() != 0 {
		t.Fatalf("acted on an ended intent: reads=%d adds=%d", f.reads.Load(), f.adds())
	}
}

// A later engine write in the other direction supersedes the diverged one (the lock
// was released after the add that the listing contradicted).
func TestLabelRepair_SupersededIntent(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.withWorker()
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true)
	f.e.noteLabelIntent("owner/repo", 1, f.lock, false)
	f.e.repairLabel(f.divergence(f.lock, "added"))
	if f.reads.Load() != 0 || f.adds() != 0 {
		t.Fatalf("acted on a superseded intent: reads=%d adds=%d", f.reads.Load(), f.adds())
	}
}

// A release that races the confirming read must not put the lock back.
func TestLabelRepair_ReleaseRacingTheReadWritesNothing(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.withWorker()
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true)
	f.readFn = func() ([]string, error) {
		f.e.noteLabelIntent("owner/repo", 1, f.lock, false) // the worker released while we read
		f.e.store.Apply(itemstate.LocalLockReleased{Repo: "owner/repo", Number: 1})
		return nil, nil
	}
	f.e.repairLabel(f.divergence(f.lock, "added"))
	if f.adds() != 0 {
		t.Fatalf("re-added a lock the worker had just released: adds=%d", f.adds())
	}
}

// R3: a failed confirming read is reported, nothing is written and the label stays marked.
func TestLabelRepair_FailedReadIsReported(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.withWorker()
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true)
	f.e.labels.mark("owner/repo#1", f.lock, f.e.now())
	f.readFn = func() ([]string, error) { return nil, errors.New("boom") }
	f.e.repairLabel(f.divergence(f.lock, "added"))
	if f.adds() != 0 {
		t.Fatal("wrote after a failed read")
	}
	if f.problems() != 1 {
		t.Fatalf("problems=%d, want the failed read reported", f.problems())
	}
	if !f.e.labels.marked("owner/repo#1", f.lock) {
		t.Error("the label must stay untrusted after a failed read")
	}
}

// A problem reported earlier (a failed read) must leave the panel when a later
// divergence on the same label ends without a repair — here the intent is no longer
// current — rather than staying until a restart.
func TestLabelRepair_EarlierProblemClearsWhenTheEpisodeEndsWithoutRepair(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.withWorker()
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true)
	f.e.labels.mark("owner/repo#1", f.lock, f.e.now())
	f.readFn = func() ([]string, error) { return nil, errors.New("boom") }
	f.e.repairLabel(f.divergence(f.lock, "added"))
	if f.problems() != 1 {
		t.Fatalf("problems=%d, want the failed read reported", f.problems())
	}
	// The worker exits: the intent is no longer current.
	f.e.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 1})
	f.e.repairLabel(f.divergence(f.lock, "added"))
	if f.problems() != 0 {
		t.Errorf("problems=%d, want the stale warning cleared once the intent ended", f.problems())
	}
	if f.e.labels.marked("owner/repo#1", f.lock) {
		t.Error("the mark must clear when the intent ended")
	}
}

// R3: a failed re-apply is reported, and not retried.
func TestLabelRepair_FailedReapplyIsReported(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.withWorker()
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true)
	f.client.addLabelToIssueFn = func(owner, repo string, n int, l string) error { return errors.New("denied") }
	f.e.repairLabel(f.divergence(f.lock, "added"))
	if f.adds() != 1 {
		t.Fatalf("adds=%d, want a single attempt", f.adds())
	}
	if f.problems() != 1 {
		t.Fatalf("problems=%d, want the failed re-apply reported", f.problems())
	}
}

// FR-007 / R3: a divergence recurring on the same item and label inside the window is
// reported and not re-applied a second time.
func TestLabelRepair_RecurrenceIsReportedNotReapplied(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.withWorker()
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true)
	f.e.repairLabel(f.divergence(f.lock, "added"))
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true) // the re-apply's own write
	f.e.repairLabel(f.divergence(f.lock, "added"))
	if f.adds() != 1 {
		t.Fatalf("adds=%d, want one re-apply per episode", f.adds())
	}
	if f.problems() != 1 {
		t.Fatalf("problems=%d, want the recurrence reported", f.problems())
	}
}

// The pause pair: a removal after the engine's write is a person lifting it, not a lost write.
func TestLabelRepair_PausePairHumanRemovalIsNotReapplied(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.e.noteLabelIntent("owner/repo", 1, "fabrik:paused", true)
	f.client.fetchLabelRemovedAtFn = func(owner, repo string, n int, l string) (time.Time, error) {
		return time.Now().Add(time.Minute), nil
	}
	f.e.repairLabel(f.divergence("fabrik:paused", "added"))
	if f.adds() != 0 {
		t.Fatal("re-applied a pause a person had lifted")
	}
}

func TestLabelRepair_PausePairLostWriteIsReapplied(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.e.noteLabelIntent("owner/repo", 1, "fabrik:paused", true)
	f.e.repairLabel(f.divergence("fabrik:paused", "added")) // no removal event, GitHub lacks the label
	if f.adds() != 1 {
		t.Fatalf("adds=%d, want the lost pause re-applied once", f.adds())
	}
}

// Complete-label removal: re-applied only while a rework of that stage is open.
func TestLabelRepair_CompleteRemovalOnlyDuringRework(t *testing.T) {
	f := newLabelRepairFixture(t)
	complete := "stage:Implement:complete"
	f.live.Store([]string{complete})
	f.e.noteLabelIntent("owner/repo", 1, complete, false)
	f.e.repairLabel(f.divergence(complete, "removed"))
	if f.removes() != 0 {
		t.Fatal("removed a completion label with no rework open")
	}
	f.e.openRework("owner/repo", 1, "Implement")
	f.e.repairLabel(f.divergence(complete, "removed"))
	if f.removes() != 1 {
		t.Fatalf("removes=%d, want the withdrawn completion label removed once", f.removes())
	}
}

// Only the engine's own lock and the listed labels are in scope.
func TestLabelRepair_OutOfScopeLabelsAreIgnored(t *testing.T) {
	f := newLabelRepairFixture(t)
	for _, l := range []string{"fabrik:locked:someone-else", "fabrik:rebase-needed", "fabrik:awaiting-ci", "fabrik:reworking:Implement", "fabrik:pause:question"} {
		if f.e.labelInScope(l) {
			t.Errorf("%s must be out of scope", l)
		}
	}
	for _, l := range []string{f.lock, "fabrik:editing", "fabrik:paused", "fabrik:awaiting-input", "stage:Plan:complete", "stage:Plan:in_progress"} {
		if !f.e.labelInScope(l) {
			t.Errorf("%s must be in scope", l)
		}
	}
	f.e.handleLabelDivergence([]labelDivergence{f.divergence("fabrik:rebase-needed", "added")})
	if f.e.labels.marked("owner/repo#1", "fabrik:rebase-needed") {
		t.Error("an out-of-scope label was marked")
	}
}

// The hand-off never blocks: a full queue drops the record, the mark is already set.
func TestLabelRepair_QueueOverflowDrops(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.e.labels.queue = make(chan labelDivergence, 1)
	f.e.handleLabelDivergence([]labelDivergence{f.divergence(f.lock, "added"), f.divergence("fabrik:editing", "added")})
	if got := f.e.labels.inFlight.Load(); got != 1 {
		t.Fatalf("inFlight=%d, want 1 (the second record dropped)", got)
	}
	if !f.e.labels.marked("owner/repo#1", "fabrik:editing") {
		t.Error("a dropped record must still leave the label marked")
	}
}

func TestLabelRepair_NeutralisedQueuesNothing(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.e.labels.queue = make(chan labelDivergence, 4)
	f.e.SetLabelRepairDisabledForTest(true)
	f.e.handleLabelDivergence([]labelDivergence{f.divergence(f.lock, "added")})
	if f.e.labels.inFlight.Load() != 0 {
		t.Fatal("queued with the repair neutralised")
	}
	if !f.e.labels.marked("owner/repo#1", f.lock) {
		t.Error("the mark belongs to the trust gate and stays set")
	}
}

func TestLabelRepair_WorkerLifecycle(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.withWorker()
	f.e.noteLabelIntent("owner/repo", 1, f.lock, true)
	f.e.startLabelRepair()
	defer f.e.stopLabelRepair()
	f.e.handleLabelDivergence([]labelDivergence{f.divergence(f.lock, "added")})
	if !f.e.DrainLabelRepairForTest(5 * time.Second) {
		t.Fatal("repair did not drain")
	}
	if f.adds() != 1 {
		t.Fatalf("adds=%d, want 1", f.adds())
	}
	f.e.stopLabelRepair()
	f.e.stopLabelRepair() // idempotent
}

// ---- intent recording ----

func TestNoteLabelIntent_RecordedByTheWriteFunnel(t *testing.T) {
	f := newLabelRepairFixture(t)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	f.e.syncLabelAdd(item, "stage:Implement:in_progress", true)
	in, ok := f.e.labels.intent("owner/repo#1", "stage:Implement:in_progress")
	if !ok || !in.added {
		t.Fatalf("intent after add = %+v, %v", in, ok)
	}
	f.e.syncLabelRemoval(item, "stage:Implement:in_progress", true)
	if in, _ = f.e.labels.intent("owner/repo#1", "stage:Implement:in_progress"); in.added {
		t.Fatal("removal did not supersede the add")
	}
	f.e.syncLabelAdd(item, "fabrik:blocked", true)
	if _, ok := f.e.labels.intent("owner/repo#1", "fabrik:blocked"); ok {
		t.Fatal("recorded an out-of-scope label")
	}
}

func TestEndStageRework_ClosesTheRework(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.e.openRework("owner/repo", 1, "Implement")
	f.e.endStageRework(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, &stages.Stage{Name: "Implement"}, true, true)
	if got := f.e.labels.reworkOpenFor("owner/repo#1"); got != "" {
		t.Fatalf("rework still open for %q after endStageRework", got)
	}
}

// ---- trust gate ----

func gateItem() gh.ProjectItem {
	return gh.ProjectItem{Number: 1, Repo: "owner/repo", Status: "Implement", Labels: []string{"stage:Implement:complete"}}
}

func TestLabelTrustGate_NoOpWithoutMarkOrRework(t *testing.T) {
	f := newLabelRepairFixture(t)
	if f.e.refuseIfLabelsUntrusted(gateItem(), &stages.Stage{Name: "Implement"}, "advancing") {
		t.Fatal("refused an untouched item")
	}
	if f.reads.Load() != 0 {
		t.Fatal("the gate read GitHub for an untouched item")
	}
}

func TestLabelTrustGate_RefusesWhileReworkIsOpen(t *testing.T) {
	f := newLabelRepairFixture(t)
	f.e.openRework("owner/repo", 1, "Implement")
	if !f.e.refuseIfLabelsUntrusted(gateItem(), &stages.Stage{Name: "Implement"}, "advancing") {
		t.Fatal("advanced during a rework")
	}
	f.e.SetLabelTrustGateDisabledForTest(true)
	if f.e.refuseIfLabelsUntrusted(gateItem(), &stages.Stage{Name: "Implement"}, "advancing") {
		t.Fatal("the neutralised gate still refused")
	}
}

func TestLabelTrustGate_MarkedLabel(t *testing.T) {
	stage := &stages.Stage{Name: "Implement"}
	complete := "stage:Implement:complete"
	cases := []struct {
		name       string
		intent     *bool
		live       []string
		readErr    error
		wantRefuse bool
		wantMarked bool
	}{
		{"read failure fails closed", nil, nil, errors.New("down"), true, true},
		{"engine removed it, GitHub still has it", boolp(false), []string{complete}, nil, true, true},
		{"cache claims completion GitHub lacks", boolp(true), nil, nil, true, true},
		{"engine and GitHub agree it is present", boolp(true), []string{complete}, nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newLabelRepairFixture(t)
			if c.intent != nil {
				f.e.noteLabelIntent("owner/repo", 1, complete, *c.intent)
			}
			f.e.labels.mark("owner/repo#1", complete, f.e.now())
			f.readFn = func() ([]string, error) { return c.live, c.readErr }
			if got := f.e.refuseIfLabelsUntrusted(gateItem(), stage, "advancing"); got != c.wantRefuse {
				t.Fatalf("refuse=%v, want %v", got, c.wantRefuse)
			}
			if got := f.e.labels.marked("owner/repo#1", complete); got != c.wantMarked {
				t.Fatalf("marked=%v, want %v", got, c.wantMarked)
			}
		})
	}
}

func boolp(b bool) *bool { return &b }
