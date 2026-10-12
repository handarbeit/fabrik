package engine

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
)

func workerEngine(t *testing.T) *Engine {
	t.Helper()
	return apiEngine(t, 0) // NewWithDeps registered owner/repo's worktree manager
}

func health(t *testing.T, e *Engine) *localapi.HealthResult {
	t.Helper()
	res, err := e.LocalAPIBackend().Health(localapi.HealthParams{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	return res
}

func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// F16 (#2121 R2): a reinvoke dispatched while every slot is held is QUEUED, not in
// flight; health never reports more slots in use than the maximum and counts it
// as queued; once it acquires the slot it is in flight and started_at is the
// acquisition time.
func TestReinvokeDispatchedWithAllSlotsHeldIsQueuedThenInFlight(t *testing.T) {
	e := workerEngine(t)
	max := cap(e.sem)

	// max-1 other items run stage workers; the remaining slot-holder is a
	// merge-train style holder with no item worker. Every slot is held.
	for i := 0; i < max; i++ {
		e.seedAPIItem(t, 10+i, "Implement")
		if i < max-1 {
			e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 10 + i, StageName: "Implement",
				StartedAt: time.Now(), Kind: itemstate.WorkerKindStageRun})
		}
	}
	saturateSem(e)
	e.seedAPIItem(t, 5, "Implement", "fabrik:editing")

	t0 := time.Now().Truncate(time.Second)
	e.SetClock(fixedClock{t0})

	built := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	item := gh.ProjectItem{Number: 5, Repo: "owner/repo", Status: "Implement"}
	stage := &stages.Stage{Name: "Implement", Order: 2, MaxTurns: 50}
	e.dispatchReinvoke(ctx, &gh.ProjectBoard{}, item, stage, reinvokeOpts{
		tag:  "review-reinvoke",
		kind: itemstate.WorkerKindReviewReinvoke,
		build: func(string) []gh.Comment {
			close(built)
			<-release
			return nil // nothing to process: processComments returns before any side effect
		},
	})

	// Queued: holds no slot, so it is not in flight and not counted as in use.
	st := status(t, e, "5")
	if st.Worker.InFlight || !st.Worker.Queued || st.Worker.QueuedSince == nil || !st.Worker.QueuedSince.V.Equal(t0) {
		t.Fatalf("worker while all slots are held = %+v (queued_since %+v), want queued since %v and not in flight", st.Worker, st.Worker.QueuedSince, t0)
	}
	if st.Worker.StartedAt.Valid {
		t.Errorf("started_at = %+v for a worker that holds no slot, want unknown", st.Worker.StartedAt)
	}
	if st.Worker.Kind != itemstate.WorkerKindReviewReinvoke {
		t.Errorf("kind = %q", st.Worker.Kind)
	}
	if st.Attention.State != "waiting" || st.Attention.Code != attention.CodeQueuedForSlot || st.Attention.Cause != attention.CauseSlotStarved {
		t.Errorf("attention while queued = %+v", st.Attention)
	}
	if !strings.Contains(st.Attention.Summary, "queued: waiting for a worker slot (since ") {
		t.Errorf("summary = %q", st.Attention.Summary)
	}
	h := health(t, e)
	if h.Workers.InUse > max || h.Workers.InUse != max-1 || h.Workers.Queued != 1 {
		t.Errorf("health workers = %+v, want in_use %d (<= %d) and queued 1", h.Workers, max-1, max)
	}

	// A slot frees up and the reinvoke takes it: it is now running.
	t1 := t0.Add(2 * time.Minute)
	e.SetClock(fixedClock{t1})
	e.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 10})
	<-e.sem
	select {
	case <-built:
	case <-time.After(10 * time.Second):
		t.Fatal("the reinvoke never acquired the freed slot")
	}
	st = status(t, e, "5")
	if !st.Worker.InFlight || st.Worker.Queued || st.Worker.QueuedSince != nil {
		t.Fatalf("worker after the slot was acquired = %+v, want in flight", st.Worker)
	}
	if !st.Worker.StartedAt.Valid || !st.Worker.StartedAt.V.Equal(t1) {
		t.Errorf("started_at = %+v, want the slot-acquired time %v", st.Worker.StartedAt, t1)
	}
	if st.Attention.State != "working" || !strings.Contains(st.Attention.Summary, "a review reinvoke is running") {
		t.Errorf("attention after acquire = %+v", st.Attention)
	}
	h = health(t, e)
	if h.Workers.InUse > max || h.Workers.Queued != 0 {
		t.Errorf("health workers = %+v, want in_use <= %d and queued 0", h.Workers, max)
	}

	close(release)
	waitForCond(t, "the reinvoke worker to exit", func() bool {
		snap, _ := e.store.Peek("owner/repo", 5)
		return snap.Worker() == nil
	})
	e.wg.Wait()
	drainSem(e)
}

// F16's other half: the stored record can transiently exceed the maximum (a
// worker that just exited and one that just entered both visible); health must
// never report that.
func TestHealthNeverReportsMoreSlotsInUseThanTheMaximum(t *testing.T) {
	e := workerEngine(t)
	max := cap(e.sem)
	for i := 0; i < max+2; i++ {
		e.seedAPIItem(t, 20+i, "Implement")
		e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 20 + i, StageName: "Implement", StartedAt: time.Now()})
	}
	if got := health(t, e).Workers.InUse; got != max {
		t.Errorf("in_use = %d with %d recorded workers, want it clamped to %d", got, max+2, max)
	}
}

// F10 (#2121 R3): a cruise item with an incomplete stage and every slot busy is
// waiting for dispatch, naming the slot count — never idle.
func TestCruiseItemWithAllSlotsBusyIsWaitingForDispatch(t *testing.T) {
	e := workerEngine(t)
	e.seedAPIItem(t, 7, "Specify", "fabrik:cruise")
	saturateSem(e)
	defer drainSem(e)

	st := status(t, e, "7")
	if st.Attention.State == "idle" {
		t.Fatalf("a dispatchable item is never idle: %+v", st.Attention)
	}
	if st.Attention.State != "waiting" || st.Attention.Code != attention.CodeWaitingForDispatch {
		t.Fatalf("attention = %+v", st.Attention)
	}
	want := "all 3 of 3 worker slots are busy"
	if !strings.Contains(st.Attention.Summary, want) {
		t.Errorf("summary %q should say %q", st.Attention.Summary, want)
	}
	if st.Attention.Cause != attention.CauseSlotStarved {
		t.Errorf("cause = %q", st.Attention.Cause)
	}

	// With a free slot the reason is the next poll.
	<-e.sem
	st = status(t, e, "7")
	if st.Attention.Code != attention.CodeWaitingForDispatch || !strings.Contains(st.Attention.Summary, "next poll") || st.Attention.Cause != "" {
		t.Errorf("attention with a free slot = %+v", st.Attention)
	}
}

// An item that something gates is not waiting for dispatch.
func TestGatedItemsAreNotWaitingForDispatch(t *testing.T) {
	e := workerEngine(t)
	for i, labels := range [][]string{
		{"fabrik:blocked"}, {"stage:Implement:complete"}, {"fabrik:awaiting-ci"}, {"fabrik:paused"},
		{"fabrik:locked:someone"}, {"stage:Implement:failed"},
	} {
		n := 30 + i
		e.seedAPIItem(t, n, "Implement", labels...)
		if code := status(t, e, strconv.Itoa(n)).Attention.Code; code == attention.CodeWaitingForDispatch {
			t.Errorf("item with %v reported as waiting for dispatch", labels)
		}
	}
	// Closed, unmanaged and holding-stage items likewise.
	e.seedAPIItem(t, 40, "Backlog")
	e.seedAPIItem(t, 41, "Queued")
	for _, n := range []int{40, 41} {
		if code := status(t, e, strconv.Itoa(n)).Attention.Code; code == attention.CodeWaitingForDispatch {
			t.Errorf("#%d reported as waiting for dispatch", n)
		}
	}
}

// Worker kinds and a comment-review trigger are reported by status.
func TestStatusReportsWorkerKindTriggerAndTurns(t *testing.T) {
	e := workerEngine(t)
	e.seedAPIItem(t, 8, "Implement", "fabrik:awaiting-ci", "fabrik:editing")
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 8, StageName: "Implement", StartedAt: time.Now(),
		Kind: itemstate.WorkerKindCommentReview, Trigger: "C_12"})
	e.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 8, Reason: "feedback-pending", Until: time.Now().Add(time.Hour)})

	st := status(t, e, "8")
	if st.Worker.Kind != itemstate.WorkerKindCommentReview || st.Worker.Trigger != "C_12" || !st.Worker.InFlight {
		t.Fatalf("worker = %+v", st.Worker)
	}
	if st.Worker.TurnsUsed.Valid {
		t.Errorf("turns_used = %+v before any turn is recorded, want unknown", st.Worker.TurnsUsed)
	}
	if st.Attention.State != "working" || !strings.Contains(st.Attention.Summary, "a comment review is running, addressing C_12") {
		t.Errorf("attention = %+v (F4: the awaiting-ci label must not win)", st.Attention)
	}
	for _, r := range st.Attention.Reasons {
		if strings.HasPrefix(r.Code, "cooldown:") {
			t.Errorf("reason %q reported while a worker runs (F15)", r.Code)
		}
	}
	e.store.Apply(itemstate.WorkerTurnsRecorded{Repo: "owner/repo", Number: 8, Turns: 12})
	st = status(t, e, "8")
	if !st.Worker.TurnsUsed.Valid || st.Worker.TurnsUsed.V != 12 || !strings.Contains(st.Attention.Summary, "(turn 12 of 50)") {
		t.Errorf("turns = %+v summary %q", st.Worker.TurnsUsed, st.Attention.Summary)
	}
}

// The attention package mirrors itemstate's worker-kind strings (it cannot import
// it); they must stay equal.
func TestAttentionWorkerKindsMirrorItemstate(t *testing.T) {
	for _, p := range [][2]string{
		{attention.WorkerKindStageRun, itemstate.WorkerKindStageRun},
		{attention.WorkerKindCommentReview, itemstate.WorkerKindCommentReview},
		{attention.WorkerKindReviewReinvoke, itemstate.WorkerKindReviewReinvoke},
		{attention.WorkerKindCIFixReinvoke, itemstate.WorkerKindCIFixReinvoke},
		{attention.WorkerKindRebaseReinvoke, itemstate.WorkerKindRebaseReinvoke},
		{attention.WorkerKindTrainConflict, itemstate.WorkerKindTrainConflict},
	} {
		if p[0] != p[1] {
			t.Errorf("attention kind %q != itemstate kind %q", p[0], p[1])
		}
	}
}

// A merge-train conflict resolution is reported on the member, queued then running.
func TestStatusReportsMergeTrainConflictResolution(t *testing.T) {
	e := workerEngine(t)
	e.seedAPIItem(t, 9, "Queued")
	e.store.Apply(itemstate.ConflictResolutionSet{Repo: "owner/repo", Number: 9, Active: true, Queued: true, Since: time.Now()})
	st := status(t, e, "9")
	if !st.Worker.Queued || st.Worker.InFlight || st.Worker.Kind != itemstate.WorkerKindTrainConflict {
		t.Fatalf("worker = %+v", st.Worker)
	}
	if st.Attention.Code != attention.CodeQueuedForSlot {
		t.Errorf("attention = %+v", st.Attention)
	}
	if h := health(t, e); h.Workers.Queued != 1 {
		t.Errorf("queued = %d, a queued conflict resolution waits for a slot", h.Workers.Queued)
	}
	e.store.Apply(itemstate.ConflictResolutionSet{Repo: "owner/repo", Number: 9, Active: true, Since: time.Now()})
	st = status(t, e, "9")
	if !st.Worker.InFlight || st.Worker.Queued || st.Attention.State != "working" {
		t.Errorf("running conflict resolution: worker=%+v attention=%+v", st.Worker, st.Attention)
	}
}

// FR-014/FR-016/R-cause through the real engine inputs.
func TestStatusExplainsRetryGateHoldAndCause(t *testing.T) {
	e := workerEngine(t)
	// Killed by the wall-time limit, retry due, every slot busy.
	e.seedAPIItem(t, 50, "Implement", "stage:Implement:in_progress", "fabrik:locked:fabrik-dev")
	e.emitRetryScheduled(gh.ProjectItem{Number: 50, Repo: "owner/repo"}, &stages.Stage{Name: "Implement"}, "wall-time-kill", e.now().Add(-time.Minute))
	saturateSem(e)
	defer drainSem(e)
	st := status(t, e, "50")
	if st.Attention.State != "waiting" || st.Attention.Code != attention.CodeRetryWaitingForSlot || st.Attention.Cause != attention.CauseWallTimeKill {
		t.Fatalf("attention = %+v", st.Attention)
	}
	if !strings.Contains(st.Attention.Summary, "wall-time limit") {
		t.Errorf("summary %q", st.Attention.Summary)
	}

	// Feedback gate hold on a stage-complete item: the reason is the hold.
	e.seedAPIItem(t, 51, "Implement", "stage:Implement:complete", "fabrik:cruise")
	e.noteGateHold(gh.ProjectItem{Number: 51, Repo: "owner/repo"}, gateFeedback, "holding advance — 1 unprocessed review body", []string{"review-body:5483772616"})
	st = status(t, e, "51")
	if st.Attention.State == "stalled" || st.Attention.Code != attention.CodeGateHold || st.Attention.Cause != "gate-hold:feedback" ||
		!strings.Contains(st.Attention.Summary, "review-body:5483772616") {
		t.Errorf("attention = %+v", st.Attention)
	}
	// A hold the gate stopped confirming has ended.
	e.SetClock(fixedClock{e.now().Add(e.wedgeLapseFor(gateFeedback) + time.Minute)})
	if st = status(t, e, "51"); st.Attention.Code == attention.CodeGateHold {
		t.Errorf("a lapsed hold is still reported: %+v", st.Attention)
	}
}

// daemon-stop comes only from the explicit marker on the pause comment.
func TestStatusDaemonStopCauseNeedsTheMarker(t *testing.T) {
	e := workerEngine(t)
	e.seedAPIItem(t, 60, "Implement", "fabrik:paused", "stage:Implement:in_progress")
	if c := status(t, e, "60").Attention.Cause; c != "" {
		t.Fatalf("a paused item with no evidence has cause %q", c)
	}
	e.store.Apply(itemstate.IssueCommentCreated{Repo: "owner/repo", Number: 60, Comment: gh.Comment{
		DatabaseID: 1, Body: "🏭 **Fabrik — paused by a daemon clean stop**\n\nRemove `fabrik:paused` to resume.\n\n" + pauseCauseDaemonStopMarker,
	}})
	if c := status(t, e, "60").Attention.Cause; c != attention.CauseDaemonStop {
		t.Errorf("cause = %q, want daemon-stop", c)
	}
	// A different, later pause supersedes the stop.
	e.store.Apply(itemstate.IssueCommentCreated{Repo: "owner/repo", Number: 60, Comment: gh.Comment{
		DatabaseID: 2, Body: "🏭 **Fabrik — push refused**\n\nRemove `fabrik:paused` to resume.",
	}})
	if c := status(t, e, "60").Attention.Cause; c != "" {
		t.Errorf("a later pause comment must supersede the stop marker, cause %q", c)
	}
}

// R6: a live worker's turns reach status through the invocation's OnTurn hook
// (turnCountingWriter → liveTurnRecorder → WorkerTurnsRecorded), throttled, and
// stay unknown until the first turn.
func TestLiveTurnsReachStatusThroughOnTurn(t *testing.T) {
	e := workerEngine(t)
	e.seedAPIItem(t, 9, "Implement")
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 9, StageName: "Implement", StartedAt: time.Now(),
		Kind: itemstate.WorkerKindStageRun})
	if st := status(t, e, "9"); st.Worker.TurnsUsed.Valid {
		t.Fatalf("turns_used = %+v before any turn, want unknown", st.Worker.TurnsUsed)
	}

	var sink strings.Builder
	w := &turnCountingWriter{inner: &sink, issueNumber: 9, maxTurns: 50, onTurn: e.liveTurnRecorder("owner/repo", 9)}
	line := "{\"type\":\"user\"}\n"
	for i := 0; i < 2; i++ { // turn 1 recorded, turn 2 throttled
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if st := status(t, e, "9"); !st.Worker.TurnsUsed.Valid || st.Worker.TurnsUsed.V != 1 {
		t.Errorf("after 2 turns turns_used = %+v, want the throttled 1", st.Worker.TurnsUsed)
	}
	if _, err := w.Write([]byte(line)); err != nil { // turn 3 recorded
		t.Fatal(err)
	}
	if st := status(t, e, "9"); !st.Worker.TurnsUsed.Valid || st.Worker.TurnsUsed.V != 3 {
		t.Errorf("after 3 turns turns_used = %+v, want 3", st.Worker.TurnsUsed)
	}
}
