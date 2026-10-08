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
	w.eng.markPendingReviewEject("owner/repo", 9, 1)

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
