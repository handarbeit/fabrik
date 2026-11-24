package engine

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Neutralisation (#2044). Each test below fails with the branch it names removed:
//   - LandsViaFastPath*            : remove the trySingletonCatchUp call in runMergeTrainWorker.
//   - Off*                         : remove the singletonCatchUpEnabled() guard.
//   - NotBehind*                   : remove the `behind <= 0` guard.
//   - Deferred* (session/editing/labels) : remove the catchUpBusyReason check.
//   - GitDefer*, PushRejected*     : make the catchUpDefer / catchUpFallback arms land or eject.
//   - AttemptCap*                  : remove the attempt-cap guard.
//   - Conflict*                    : drop the ejectMember call in the catchUpConflict arm.
//   - Red*                         : drop the memberCIRed arm / the failed-job re-run.
//   - NoCI*, Timeout*, Moved*      : drop the matching waitMemberCI branch.
//   - TOCTOU*                      : drop the live-head comparison in singletonFastPathEligible.
//   - Marker*                      : drop postCatchUpMarker (MarkerRetries*: post it once, no retry).

const (
	cuMemberHead = "head-sha"
	cuCaughtHead = "caught-head"
)

// catchUpWorld is a behind singleton plus mocks wired for the catch-up decision logic.
type catchUpWorld struct {
	t        *testing.T
	eng      *Engine
	client   *mockGitHubClient
	state    *mergeTrainWorkerState
	p        trialParams
	m        trainMember
	gitCalls atomic.Int32
	gitOut   catchUpGitOutcome
	labels   []string
	prHead   atomic.Value // string: what FetchPRDetails reports as the PR head
	checks   atomic.Value // []gh.CheckRun for the caught-up head
}

func newCatchUpWorld(t *testing.T) *catchUpWorld {
	t.Helper()
	w := &catchUpWorld{t: t}
	w.prHead.Store(cuCaughtHead)
	w.checks.Store([]gh.CheckRun{{ID: 1, Name: "build", Status: "completed", Conclusion: "success"}})
	w.gitOut = catchUpGitOutcome{kind: catchUpPushed, newHead: cuCaughtHead, pure: true}
	w.client = &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, n int) ([]string, error) { return w.labels, nil },
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) {
			if head == cuMemberHead {
				return 3, nil
			}
			return 0, nil
		},
		fetchPRDetailsFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: n, HeadSHA: w.prHead.Load().(string), MergeableState: "clean"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			if sha == cuCaughtHead {
				return w.checks.Load().([]gh.CheckRun), nil
			}
			return nil, nil
		},
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	wm := NewWorktreeManager(t.TempDir())
	w.eng = trainTestEngine(t, w.client, &mockClaudeInvoker{}, wm)
	// The trainValidateFn seam keeps the catch-up's git half out of reach; trainCatchUpGitFn
	// stands in for it. A trial being built would call this and fail the test.
	w.eng.trainValidateFn = func(ctx context.Context, members []trainMember) (TrainCIResult, *trainCIDiagnostic) {
		t.Error("a trial was assembled")
		return TrainCIRed, nil
	}
	w.eng.trainCatchUpGitFn = func(ctx context.Context, p trialParams, m trainMember) catchUpGitOutcome {
		w.gitCalls.Add(1)
		return w.gitOut
	}
	w.eng.SetTrainCIPollIntervalForTest(time.Millisecond)
	w.eng.SetCatchUpTimingForTest(50*time.Millisecond, 50*time.Millisecond)
	w.eng.SetCIInfraTimingForTest(time.Millisecond, time.Millisecond, time.Millisecond, -1)
	w.state = &mergeTrainWorkerState{projectID: "PVT_test"}
	w.p = trialParams{owner: "owner", repo: "repo", baseBranch: "main", baseSHA: "base-sha", trainKey: "owner/repo", wm: wm, holdingStg: holdingStage(w.eng.cfg)}
	w.m = trainMember{item: gh.ProjectItem{Number: 9, Title: "Issue Nine", ItemID: "item-9", Repo: "owner/repo", Status: "Queued"}, prNum: 90, headSHA: cuMemberHead}
	return w
}

func (w *catchUpWorld) run() (trainMember, bool) {
	return w.eng.trySingletonCatchUp(context.Background(), w.state, w.p, w.m)
}

func (w *catchUpWorld) comments() []string {
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	var out []string
	for _, c := range w.client.addCommentCalls {
		out = append(out, c.body)
	}
	return out
}

func (w *catchUpWorld) assertNothingLandedOrCharged() {
	w.t.Helper()
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	if len(w.client.mergePRAtHeadSHACalls) != 0 || len(w.client.mergePRCalls) != 0 {
		w.t.Errorf("a merge was attempted: %+v %+v", w.client.mergePRAtHeadSHACalls, w.client.mergePRCalls)
	}
	if len(w.client.updateStatusCalls) != 0 {
		w.t.Errorf("a board status change happened: %+v", w.client.updateStatusCalls)
	}
	for _, c := range w.client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			w.t.Error("the member was paused")
		}
	}
	w.eng.mergeTrainEjectionsMu.Lock()
	defer w.eng.mergeTrainEjectionsMu.Unlock()
	if n := w.eng.mergeTrainEjectionCounts["owner/repo#9"]; n != 0 {
		w.t.Errorf("ejection counter charged: %d", n)
	}
	w.eng.mergeTrainTrialsMu.Lock()
	defer w.eng.mergeTrainTrialsMu.Unlock()
	if n := len(w.eng.mergeTrainTrials["owner/repo"]); n != 0 {
		w.t.Errorf("runaway-guard trial counter charged: %d", n)
	}
}

func TestSingletonCatchUp_LandsViaFastPathWithNoTrialOrDraftPR(t *testing.T) {
	w := newCatchUpWorld(t)

	got, decided := w.run()
	if !decided {
		t.Fatal("expected the disposition to be decided (landed)")
	}
	if got.headSHA != cuCaughtHead || got.caughtUpFrom != "base-sha" {
		t.Errorf("returned member = %+v, want head %s caught up from base-sha", got, cuCaughtHead)
	}
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	if len(w.client.mergePRAtHeadSHACalls) != 1 || w.client.mergePRAtHeadSHACalls[0].prNumber != 90 || w.client.mergePRAtHeadSHACalls[0].expectedHeadSHA != cuCaughtHead {
		t.Fatalf("want one MergePRAtHeadSHA on PR #90 pinned to the caught-up head, got %+v", w.client.mergePRAtHeadSHACalls)
	}
	if len(w.client.createDraftPRCalls) != 0 || len(w.client.createPRCalls) != 0 {
		t.Errorf("no draft or landing PR may be created: %d / %d", len(w.client.createDraftPRCalls), len(w.client.createPRCalls))
	}
	var sawMarker, sawLanded bool
	for _, c := range w.client.addCommentCalls {
		if strings.Contains(c.body, formatCatchUpMarker(catchUpMarker{Head: cuCaughtHead, Base: "base-sha", Pure: true})) && c.issueNumber == 90 {
			sawMarker = true
		}
		if strings.Contains(c.body, "after a catch-up") {
			sawLanded = true
		}
	}
	if !sawMarker {
		t.Error("the catch-up marker comment was not posted on the member's PR")
	}
	if !sawLanded {
		t.Error("the landed comment does not mention the catch-up")
	}
	if w.eng.catchUpAttemptCount("owner/repo", 9) != 0 {
		t.Error("the attempt counter must reset on landing")
	}
}

func TestSingletonCatchUp_OffKeepsTodaysTrialPath(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.cfg.SingletonCatchUp = "off"

	got, decided := w.run()
	if decided || got.headSHA != cuMemberHead {
		t.Fatalf("off must hand the untouched member to the trial path, got (%+v, %v)", got, decided)
	}
	if w.gitCalls.Load() != 0 {
		t.Error("the catch-up's git half ran with singleton_catch_up: off")
	}
}

func TestSingletonCatchUp_NotBehindIsLeftToTheTrialPath(t *testing.T) {
	w := newCatchUpWorld(t)
	w.client.fetchCommitsBehindFn = func(owner, repo, base, head string) (int, error) { return 0, nil }
	if _, decided := w.run(); decided || w.gitCalls.Load() != 0 {
		t.Fatalf("a member that is not behind must not be caught up (decided=%v, git calls=%d)", decided, w.gitCalls.Load())
	}
}

func TestSingletonCatchUp_BehindReadErrorFailsClosedToTheTrial(t *testing.T) {
	w := newCatchUpWorld(t)
	w.client.fetchCommitsBehindFn = func(owner, repo, base, head string) (int, error) { return 0, context.DeadlineExceeded }
	if _, decided := w.run(); decided || w.gitCalls.Load() != 0 {
		t.Fatalf("an unreadable ancestry must not trigger a push (decided=%v, git calls=%d)", decided, w.gitCalls.Load())
	}
}

func TestSingletonCatchUp_DeferredWhileAStageSessionIsActive(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 9, StageName: "Validate", StartedAt: time.Now()})

	_, decided := w.run()
	if !decided {
		t.Fatal("an active session must defer the member (decided, stays Queued)")
	}
	if w.gitCalls.Load() != 0 {
		t.Error("the branch was touched while a session was active")
	}
	w.assertNothingLandedOrCharged()
}

func TestSingletonCatchUp_DeferredWhileFabrikEditingIsSet(t *testing.T) {
	w := newCatchUpWorld(t)
	w.labels = []string{"fabrik:editing"}

	if _, decided := w.run(); !decided || w.gitCalls.Load() != 0 {
		t.Fatalf("fabrik:editing must defer (decided=%v, git calls=%d)", decided, w.gitCalls.Load())
	}
	w.assertNothingLandedOrCharged()
}

func TestSingletonCatchUp_DeferredWhenLabelsCannotBeRead(t *testing.T) {
	w := newCatchUpWorld(t)
	w.client.fetchLabelsFn = func(owner, repo string, n int) ([]string, error) { return nil, context.DeadlineExceeded }

	if _, decided := w.run(); !decided || w.gitCalls.Load() != 0 {
		t.Fatalf("an unreadable label set must defer, fail closed (decided=%v, git calls=%d)", decided, w.gitCalls.Load())
	}
}

func TestSingletonCatchUp_GitDeferLeavesMemberQueuedUncharged(t *testing.T) {
	w := newCatchUpWorld(t)
	w.gitOut = catchUpGitOutcome{kind: catchUpDefer, reason: "worktree has uncommitted changes"}

	if _, decided := w.run(); !decided {
		t.Fatal("a deferred catch-up must decide the poll without a trial")
	}
	w.assertNothingLandedOrCharged()
	if len(w.comments()) != 0 {
		t.Errorf("no comment is expected for a defer, got %v", w.comments())
	}
}

func TestSingletonCatchUp_PushRejectedFallsBackToTheTrialUncharged(t *testing.T) {
	w := newCatchUpWorld(t)
	w.gitOut = catchUpGitOutcome{kind: catchUpFallback, reason: "push rejected by branch protection"}

	got, decided := w.run()
	if decided || got.headSHA != cuMemberHead {
		t.Fatalf("a rejected push must hand the untouched member to the trial path, got (%+v, %v)", got, decided)
	}
	w.assertNothingLandedOrCharged()
	if w.eng.catchUpAttemptCount("owner/repo", 9) != 0 {
		t.Error("a push that never happened must not count as an attempt")
	}
}

func TestSingletonCatchUp_AttemptCapFallsBackToTheTrial(t *testing.T) {
	w := newCatchUpWorld(t)
	for i := 0; i < w.eng.effectiveMaxTrainRebaseCycles(); i++ {
		w.eng.recordCatchUpAttempt("owner/repo", 9)
	}
	if _, decided := w.run(); decided || w.gitCalls.Load() != 0 {
		t.Fatalf("past the attempt cap the trial must take over (decided=%v, git calls=%d)", decided, w.gitCalls.Load())
	}
}

func TestSingletonCatchUp_UnresolvableConflictEjectsTheMember(t *testing.T) {
	w := newCatchUpWorld(t)
	w.gitOut = catchUpGitOutcome{kind: catchUpConflict, reason: "ejected from merge-train — catching this branch up with the base hit a conflict that could not be resolved: x"}

	if _, decided := w.run(); !decided {
		t.Fatal("an unresolvable conflict decides the poll")
	}
	var ejected bool
	for _, c := range w.comments() {
		if strings.Contains(c, "could not be resolved") {
			ejected = true
		}
	}
	if !ejected {
		t.Errorf("no ejection comment posted; comments: %v", w.comments())
	}
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	if len(w.client.mergePRAtHeadSHACalls) != 0 {
		t.Error("an ejected member must not be merged")
	}
}

func TestSingletonCatchUp_RedAfterCatchUpReRunsOnceThenPauses(t *testing.T) {
	w := newCatchUpWorld(t)
	failed := []gh.CheckRun{{ID: 7, Name: "build", Status: "completed", Conclusion: "failure", DetailsURL: "https://github.com/owner/repo/actions/runs/555/job/1"}}
	w.checks.Store(failed)
	var reruns atomic.Int32
	w.client.rerunFailedJobsFn = func(owner, repo string, runID int64) error { reruns.Add(1); return nil }

	if _, decided := w.run(); !decided {
		t.Fatal("a red catch-up decides the poll")
	}
	if reruns.Load() != 1 {
		t.Errorf("failed jobs were re-run %d time(s), want exactly once before judging red", reruns.Load())
	}
	var sawRed bool
	for _, c := range w.comments() {
		if strings.Contains(c, "validation failed") && strings.Contains(c, "caught the branch up") {
			sawRed = true
		}
	}
	if !sawRed {
		t.Errorf("no red catch-up comment; comments: %v", w.comments())
	}
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	if len(w.client.mergePRAtHeadSHACalls) != 0 {
		t.Error("a red member must not be merged")
	}
	var paused bool
	for _, c := range w.client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			paused = true
		}
	}
	if !paused {
		t.Error("the red singleton path pauses the member")
	}
}

func TestSingletonCatchUp_NoCheckRunsFallsBackToTheTrialWithTheCaughtUpHead(t *testing.T) {
	w := newCatchUpWorld(t)
	w.checks.Store([]gh.CheckRun(nil))

	got, decided := w.run()
	if decided {
		t.Fatal("CI that never starts must fall back to the trial, not decide the poll")
	}
	if got.headSHA != cuCaughtHead {
		t.Errorf("the trial must be built from the caught-up head, got %s", got.headSHA)
	}
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	if len(w.client.mergePRAtHeadSHACalls) != 0 {
		t.Error("nothing may land without CI evidence")
	}
}

func TestSingletonCatchUp_BackstopTimeoutLeavesMemberQueuedUncharged(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.cfg.CIBackstopTimeout = 30 * time.Millisecond
	w.checks.Store([]gh.CheckRun{{ID: 1, Name: "build", Status: "in_progress"}})

	if _, decided := w.run(); !decided {
		t.Fatal("a timed-out wait decides the poll (member stays Queued)")
	}
	w.assertNothingLandedOrCharged()
}

func TestSingletonCatchUp_PRHeadMovedDuringTheWaitLeavesTheMember(t *testing.T) {
	w := newCatchUpWorld(t)
	w.prHead.Store("someone-pushed")

	if _, decided := w.run(); !decided {
		t.Fatal("a moved head decides the poll")
	}
	w.assertNothingLandedOrCharged()
}

func TestSingletonCatchUp_TOCTOU_HeadChangesBetweenGreenAndLanding(t *testing.T) {
	w := newCatchUpWorld(t)
	var calls atomic.Int32
	w.client.fetchPRDetailsFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
		head := cuCaughtHead
		// waitMemberCI reads the PR once and sees the caught-up head; the fast path's own
		// read, a moment later, sees a push that landed in between.
		if calls.Add(1) > 1 {
			head = "pushed-after-green"
		}
		return &gh.PRDetails{Number: n, HeadSHA: head, MergeableState: "clean"}, nil
	}

	got, decided := w.run()
	if decided {
		t.Fatal("the fast path must decline and hand over to the trial path")
	}
	if got.headSHA != cuCaughtHead {
		t.Errorf("member head = %s", got.headSHA)
	}
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	if len(w.client.mergePRAtHeadSHACalls) != 0 {
		t.Error("a head that changed after CI validated another must never be merged")
	}
}

func TestSingletonCatchUp_PendingReviewEjectIsHonouredDuringTheWait(t *testing.T) {
	w := newCatchUpWorld(t)
	w.checks.Store([]gh.CheckRun{{ID: 1, Name: "build", Status: "in_progress"}})
	w.eng.markPendingReviewEject("owner/repo", 9, 1, w.state)

	if _, decided := w.run(); !decided {
		t.Fatal("an applied eject decides the poll")
	}
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	if len(w.client.mergePRAtHeadSHACalls) != 0 {
		t.Error("an ejected member must not be merged")
	}
	if len(w.client.updateStatusCalls) != 1 {
		t.Errorf("want the member rerouted off Queued once, got %d status updates", len(w.client.updateStatusCalls))
	}
}

func TestSingletonCatchUp_ConflictEditedCatchUpPostsAnActionableMarker(t *testing.T) {
	w := newCatchUpWorld(t)
	w.gitOut = catchUpGitOutcome{kind: catchUpPushed, newHead: cuCaughtHead, pure: false}

	w.run()
	var sawImpure bool
	for _, c := range w.comments() {
		if strings.Contains(c, "pure=false") {
			sawImpure = true
		}
		if strings.Contains(c, "pure=true") {
			t.Error("a conflict-edited catch-up must not be marked pure")
		}
	}
	if !sawImpure {
		t.Errorf("no pure=false marker posted; comments: %v", w.comments())
	}
}

// A transient AddComment failure must not lose the marker: without it every bot review of
// the pushed head is actionable. Neutralisation: make postCatchUpMarker post once (no retry)
// and the test sees no marker.
func TestSingletonCatchUp_MarkerRetriesTransientPostFailure(t *testing.T) {
	orig := landedCommentRetryDelay
	landedCommentRetryDelay = 0
	t.Cleanup(func() { landedCommentRetryDelay = orig })
	w := newCatchUpWorld(t)
	var attempts atomic.Int32
	var markerPosted atomic.Bool
	w.client.addCommentFn = func(owner, repo string, n int, body string) (int, error) {
		if strings.Contains(body, "fabrik:train-catch-up") {
			if attempts.Add(1) < 3 {
				return 0, fmt.Errorf("executing request: %w", &net.OpError{Op: "read", Net: "tcp"})
			}
			markerPosted.Store(true)
		}
		return 1, nil
	}

	w.run()
	if !markerPosted.Load() || attempts.Load() != 3 {
		t.Errorf("marker posted=%v after %d attempts; want posted on the third", markerPosted.Load(), attempts.Load())
	}
}

// #2296: a caught-up head whose own latest run stays cancelled after the one
// re-run falls through to the trial, uncharged — it never reaches the
// red-singleton ejection.
func TestSingletonCatchUp_CancelledAfterReRunFallsBackToTheTrialUncharged(t *testing.T) {
	w := newCatchUpWorld(t)
	w.checks.Store([]gh.CheckRun{{ID: 7, Name: "build", Status: "completed", Conclusion: "cancelled", DetailsURL: "https://github.com/owner/repo/actions/runs/555/job/1"}})
	var reruns atomic.Int32
	w.client.rerunFailedJobsFn = func(owner, repo string, runID int64) error { reruns.Add(1); return nil }

	got, decided := w.run()
	if decided {
		t.Fatal("a still-cancelled catch-up must fall back to the trial, not decide the poll")
	}
	if got.headSHA != cuCaughtHead {
		t.Errorf("the trial must be built from the caught-up head, got %s", got.headSHA)
	}
	if reruns.Load() != 1 {
		t.Errorf("cancelled jobs were re-run %d time(s), want exactly once", reruns.Load())
	}
	w.assertNothingLandedOrCharged()
	for _, c := range w.comments() {
		if strings.Contains(c, "validation failed") {
			t.Errorf("a cancellation must not post a red comment: %s", c)
		}
	}
}

// Neutralised twin: with the pre-fix charge-as-red reading the same setup pauses.
func TestSingletonCatchUp_CancelledNeutralisedPauses(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.SetTrainNotPassedAsRedForTest(true)
	w.checks.Store([]gh.CheckRun{{ID: 7, Name: "build", Status: "completed", Conclusion: "cancelled", DetailsURL: "https://github.com/owner/repo/actions/runs/555/job/1"}})
	w.client.rerunFailedJobsFn = func(owner, repo string, runID int64) error { return nil }
	if _, decided := w.run(); !decided {
		t.Fatal("with the seam on a cancelled catch-up reads red and decides the poll")
	}
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	var paused bool
	for _, c := range w.client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			paused = true
		}
	}
	if !paused {
		t.Error("expected the red-singleton pause under the neutralising seam")
	}
}

// ---- #2615: bounded deferral of a worktree-busy member ----
//
// Neutralisation: each test below fails with SetCatchUpBusyBoundDisabledForTest(true)
// (the member stays Queued) except the ones that assert the bound does NOT fire.

func (w *catchUpWorld) busyOutcome(reason string) {
	w.gitOut = catchUpGitOutcome{kind: catchUpDefer, reason: ErrCatchUpBusy.Error() + ": " + reason, worktreeBusy: true}
}

func (w *catchUpWorld) pausedAndRerouted() (paused bool, moves int) {
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	for _, c := range w.client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			paused = true
		}
	}
	return paused, len(w.client.updateStatusCalls)
}

func TestSingletonCatchUp_BusyDeferralIsBoundedToN(t *testing.T) {
	w := newCatchUpWorld(t)
	w.busyOutcome("REBASE_HEAD present")

	for i := 1; i < catchUpBusyBound; i++ {
		if _, decided := w.run(); !decided {
			t.Fatal("a deferral decides the poll")
		}
		if paused, moves := w.pausedAndRerouted(); paused || moves != 0 {
			t.Fatalf("formation %d: member moved before the bound (paused=%v moves=%d)", i, paused, moves)
		}
	}
	// The Nth consecutive deferral stops the waiting.
	if _, decided := w.run(); !decided {
		t.Fatal("the bound decides the poll")
	}
	paused, moves := w.pausedAndRerouted()
	if !paused || moves != 1 {
		t.Fatalf("after %d busy deferrals the member must be rerouted and paused (paused=%v moves=%d)", catchUpBusyBound, paused, moves)
	}
	var named int
	for _, c := range w.comments() {
		if strings.Contains(c, catchUpStuckFragment) && strings.Contains(c, "REBASE_HEAD present") {
			named++
		}
	}
	if named != 1 {
		t.Errorf("want exactly one comment naming the reason, got %d: %v", named, w.comments())
	}
	// Uncharged: no ejection count, no trial counter, no failure label.
	w.eng.mergeTrainEjectionsMu.Lock()
	if n := w.eng.mergeTrainEjectionCounts["owner/repo#9"]; n != 0 {
		t.Errorf("ejection counter charged: %d", n)
	}
	w.eng.mergeTrainEjectionsMu.Unlock()
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	for _, c := range w.client.addLabelCalls {
		if strings.HasSuffix(c.labelName, ":failed") {
			t.Errorf("failure label applied: %s", c.labelName)
		}
	}
	if len(w.client.mergePRAtHeadSHACalls) != 0 {
		t.Error("nothing may be merged")
	}
}

func TestSingletonCatchUp_BusyBoundNeutralised_MemberStaysQueued(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.SetCatchUpBusyBoundDisabledForTest(true)
	w.busyOutcome("REBASE_HEAD present")
	for i := 0; i < catchUpBusyBound+3; i++ {
		w.run()
	}
	w.assertNothingLandedOrCharged()
}

func TestSingletonCatchUp_BusyBoundDoesNotFireWithAWorker(t *testing.T) {
	w := newCatchUpWorld(t)
	w.busyOutcome("worktree has uncommitted changes")
	w.run() // count 1
	// A worker enters: the engine-side check defers first and the run starts over.
	w.eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 9, StageName: "Validate", StartedAt: time.Now()})
	for i := 0; i < catchUpBusyBound+2; i++ {
		w.run()
	}
	if paused, moves := w.pausedAndRerouted(); paused || moves != 0 {
		t.Fatalf("a member with a worker must keep today's deferral (paused=%v moves=%d)", paused, moves)
	}
	if _, _, ok := w.eng.catchUpDeferralPeek("owner/repo", func(int) bool { return true }); ok {
		t.Error("the deferral run must be cleared once a worker was seen")
	}
}

func TestSingletonCatchUp_BusyBoundDoesNotFireWithEditingLabel(t *testing.T) {
	w := newCatchUpWorld(t)
	w.busyOutcome("worktree has uncommitted changes")
	w.run()
	w.labels = []string{"fabrik:editing"}
	for i := 0; i < catchUpBusyBound+2; i++ {
		w.run()
	}
	if paused, moves := w.pausedAndRerouted(); paused || moves != 0 {
		t.Fatalf("fabrik:editing keeps today's deferral (paused=%v moves=%d)", paused, moves)
	}
}

func TestSingletonCatchUp_RemoteMovedDeferralDoesNotCount(t *testing.T) {
	w := newCatchUpWorld(t)
	w.gitOut = catchUpGitOutcome{kind: catchUpDefer, reason: "remote moved", worktreeBusy: false}
	for i := 0; i < catchUpBusyBound+2; i++ {
		w.run()
	}
	w.assertNothingLandedOrCharged()
}

func TestSingletonCatchUp_ProgressResetsTheBusyCount(t *testing.T) {
	w := newCatchUpWorld(t)
	w.busyOutcome("worktree has uncommitted changes")
	w.run() // count 1
	// The worktree is clean next time: catch-up proceeds (here: falls back to the trial).
	w.gitOut = catchUpGitOutcome{kind: catchUpFallback, reason: "push rejected"}
	if _, decided := w.run(); decided {
		t.Fatal("fallback hands the member to the trial")
	}
	w.busyOutcome("worktree has uncommitted changes")
	w.run() // count 1 again, not 2
	if paused, moves := w.pausedAndRerouted(); paused || moves != 0 {
		t.Fatalf("the count must start over after progress (paused=%v moves=%d)", paused, moves)
	}
}

func TestSingletonCatchUp_BusyBoundRerouteFailureWritesNothingAndKeepsCount(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.statusField = nil // rerouteQueuedMemberOffHolding cannot resolve the board option
	w.busyOutcome("REBASE_HEAD present")
	for i := 0; i < catchUpBusyBound+1; i++ {
		if _, decided := w.run(); !decided {
			t.Fatal("still deferred")
		}
	}
	if paused, _ := w.pausedAndRerouted(); paused {
		t.Fatal("a failed reroute must not pause")
	}
	if len(w.comments()) != 0 {
		t.Errorf("a failed reroute must post nothing: %v", w.comments())
	}
	if _, _, ok := w.eng.catchUpDeferralPeek("owner/repo", func(int) bool { return true }); !ok {
		t.Error("the deferral must still be recorded so the next formation retries")
	}
}

// A rebase, merge or cherry-pick really in progress is never counted toward the bound
// (FR-007), but it is named in the partition's idle_reason so a rebase directory nobody
// finishes is visible instead of `unknown` (#2615 R4, SC-004).
func TestSingletonCatchUp_OperationDeferralIsVisibleButNeverBounded(t *testing.T) {
	w := newCatchUpWorld(t)
	w.gitOut = catchUpGitOutcome{kind: catchUpDefer, reason: ErrCatchUpBusy.Error() + ": rebase-merge/ present", operation: true}
	for i := 0; i < catchUpBusyBound+3; i++ {
		w.run()
	}
	if paused, moves := w.pausedAndRerouted(); paused || moves != 0 {
		t.Fatalf("a real operation must never trigger the bound (paused=%v moves=%d)", paused, moves)
	}
	w.assertNothingLandedOrCharged()
	issue, reason, ok := w.eng.catchUpDeferralPeek("owner/repo", func(int) bool { return true })
	if !ok || issue != 9 || !strings.Contains(reason, "rebase-merge/ present") {
		t.Fatalf("operation deferral must be named for idle_reason: ok=%v issue=%d reason=%q", ok, issue, reason)
	}
	// Once the operation is gone and the worktree is merely busy, counting starts fresh.
	w.busyOutcome("worktree has uncommitted changes")
	w.run()
	if paused, _ := w.pausedAndRerouted(); paused {
		t.Fatal("the first worktree-busy deferral after an operation must not bound")
	}
}

// A counted deferral records no admission cooldown of its own, so on a quiet webhook-healthy
// board the idle backoff (about 10 x poll + poll) used to space two deferrals further apart
// than the run's gap: the run restarted every time and catchUpBusyBound was never reached
// (#2615 R3, review finding). Each counted deferral now records a short cooldown, so the poll
// comes back within about half a poll interval.
const cuDevPollSeconds = 180 // the dev daemon: poll 180 s, tailscale (webhook-healthy)

func nextPollAfterDeferral(w *catchUpWorld) (next, idle time.Duration) {
	idle = computeEffectiveInterval(cuDevPollSeconds*time.Second, time.Hour, 1.0, true)
	now := w.eng.now()
	return capIntervalToDue(idle, w.eng.earliestShortCooldown(now), now, minDueWakeInterval), idle
}

func TestSingletonCatchUp_CountedDeferralKeepsThePollComingBack(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.cfg.PollSeconds = cuDevPollSeconds
	w.busyOutcome("worktree has uncommitted changes")
	w.run()
	next, idle := nextPollAfterDeferral(w)
	if want := time.Duration(cuDevPollSeconds) * time.Second / 2; next > want {
		t.Fatalf("after a counted deferral the next poll is %v away (idle cadence %v); want at most %v", next, idle, want)
	}
}

func TestSingletonCatchUp_DeferralCooldownNeutralised_PollSleepsAtTheIdleCadence(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.cfg.PollSeconds = cuDevPollSeconds
	w.eng.SetCatchUpDeferralCooldownDisabledForTest(true)
	w.busyOutcome("worktree has uncommitted changes")
	w.run()
	if next, idle := nextPollAfterDeferral(w); next != idle {
		t.Fatalf("with the cooldown neutralised the poll must sleep at the idle cadence %v, got %v", idle, next)
	}
}

// The bound still fires when the deferrals are spaced at the real idle cadence of the
// dev daemon (poll 180 s, webhook-healthy, no other cooldown): the gap between the two
// deferrals is the interval PollWithBackoff would actually sleep.
func TestSingletonCatchUp_BoundFiresWhenDeferralsAreSpacedAtTheRealCadence(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.cfg.PollSeconds = cuDevPollSeconds
	clk := &fakeClock{t: time.Now()}
	w.eng.SetClock(clk)
	w.busyOutcome("REBASE_HEAD present")
	for i := 0; i < catchUpBusyBound; i++ {
		w.run()
		next, _ := nextPollAfterDeferral(w)
		clk.t = clk.t.Add(next)
	}
	if paused, moves := w.pausedAndRerouted(); !paused || moves != 1 {
		t.Fatalf("the member must be rerouted and paused after %d deferrals at the real cadence (paused=%v moves=%d)", catchUpBusyBound, paused, moves)
	}
}

// Even when the poll cap is skipped (GraphQL rate-limit backoff), the run's gap must be
// wider than the webhook-healthy idle cadence, or the count restarts between deferrals.
func TestCatchUpDeferralGap_CoversTheIdleCadence(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.cfg.PollSeconds = cuDevPollSeconds
	idle := computeEffectiveInterval(cuDevPollSeconds*time.Second, time.Hour, 1.0, true)
	if gap := w.eng.catchUpDeferralGap(); gap <= idle {
		t.Fatalf("deferral gap %v does not cover the idle cadence %v", gap, idle)
	}
}

// An operation really in progress is named in idle_reason until the next formation, however
// long the poll sleeps: its record does not age out (nothing re-polls it on a cadence).
func TestSingletonCatchUp_OperationRecordDoesNotAgeOut(t *testing.T) {
	w := newCatchUpWorld(t)
	w.eng.cfg.PollSeconds = cuDevPollSeconds
	clk := &fakeClock{t: time.Now()}
	w.eng.SetClock(clk)
	w.gitOut = catchUpGitOutcome{kind: catchUpDefer, reason: ErrCatchUpBusy.Error() + ": rebase-merge/ present", operation: true}
	w.run()
	clk.t = clk.t.Add(24 * time.Hour)
	if _, reason, ok := w.eng.catchUpDeferralPeek("owner/repo", func(int) bool { return true }); !ok || !strings.Contains(reason, "rebase-merge/ present") {
		t.Fatalf("an operation record must stay named (ok=%v reason=%q)", ok, reason)
	}
}
