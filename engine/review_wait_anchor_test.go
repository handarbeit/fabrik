package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

var errTestRead = errors.New("read failed")

// anchorFixture builds the chronicle#529 / PR #534 shape (#2467): the item waits
// on a human reviewer, fabrik:awaiting-review was applied labelAge ago, and the
// PR's current head carries a committer date headAge ago.
type anchorFixture struct {
	client *mockGitHubClient
	eng    *Engine
	item   gh.ProjectItem
	stage  *stages.Stage
	board  *gh.ProjectBoard
	now    time.Time
}

func newAnchorFixture(t *testing.T, labelAge, headAge time.Duration) *anchorFixture {
	t.Helper()
	now := time.Now()
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 15 * time.Minute
	eng.SetClock(stubClock{t: now})
	labelAt := now.Add(-labelAge)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return labelAt, nil
		}
		return time.Time{}, nil
	}
	headAt := now.Add(-headAge)
	client.fetchCommitTimeFn = func(owner, repo, sha string) (time.Time, error) {
		return headAt, nil
	}
	return &anchorFixture{
		client: client,
		eng:    eng,
		board:  &gh.ProjectBoard{ProjectID: "PVT_1"},
		stage:  &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)},
		now:    now,
		item: gh.ProjectItem{
			Number:                 529,
			Repo:                   "owner/repo",
			Labels:                 []string{"fabrik:awaiting-review"},
			LinkedPRNumber:         534,
			LinkedPRHeadSHA:        "b15bf0d7",
			LinkedPRReviewRequests: []gh.ReviewRequest{{Login: "alice"}}, // a human: the pause path
		},
	}
}

// The reported defect: label 17.5 minutes old, last fix pushed 30 seconds ago.
// The wait must be measured from the push, so the 15-minute timeout has not fired.
func TestReviewWaitAnchor_PushAfterLabelRestartsTheWait(t *testing.T) {
	f := newAnchorFixture(t, 17*time.Minute+30*time.Second, 30*time.Second)
	blocked, timedOut, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage)
	if timedOut {
		t.Fatal("review-wait timeout fired 30s after a push; the wait must restart on the new head (#2467)")
	}
	if !blocked {
		t.Error("expected the gate to keep blocking while the reviewer has the new head")
	}
	if len(f.client.removeLabelCalls) != 0 {
		t.Errorf("fabrik:awaiting-review must stay on, got removals %+v", f.client.removeLabelCalls)
	}
}

// No push after the label (the head predates it): the timeout fires exactly as
// before this change (FR-004).
func TestReviewWaitAnchor_NoPushFiresAsToday(t *testing.T) {
	f := newAnchorFixture(t, 16*time.Minute, 40*time.Minute)
	_, timedOut, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage)
	if !timedOut {
		t.Fatal("timeout must fire when the head predates the label and the window elapsed")
	}
}

// A full window of reviewer silence on the new head fires the timeout.
func TestReviewWaitAnchor_FullWindowAfterPushFires(t *testing.T) {
	f := newAnchorFixture(t, 40*time.Minute, 16*time.Minute)
	_, timedOut, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage)
	if !timedOut {
		t.Fatal("timeout must fire a full window after the push")
	}
}

// An unreadable head time falls back to the label time and never produces an
// earlier timeout than before (FR-003, FR-007), nor a pause by itself.
func TestReviewWaitAnchor_ReadFailureFallsBackToLabel(t *testing.T) {
	f := newAnchorFixture(t, 10*time.Minute, 30*time.Second)
	f.client.fetchCommitTimeFn = func(owner, repo, sha string) (time.Time, error) {
		return time.Time{}, errTestRead
	}
	if _, timedOut, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage); timedOut {
		t.Fatal("a failed head read must not time the wait out before the label window")
	}
	f2 := newAnchorFixture(t, 16*time.Minute, 30*time.Second)
	f2.client.fetchCommitTimeFn = f.client.fetchCommitTimeFn
	if _, timedOut, _, _ := f2.eng.checkReviewGate(f2.board, f2.item, f2.stage); !timedOut {
		t.Fatal("a failed head read must fall back to today's label-only timeout")
	}
}

// A commit dated far in the future is ignored (label anchor); one inside the
// skew allowance is clamped to now so the wait cannot slide forward.
func TestReviewWaitAnchor_FutureDatedHeadIsBounded(t *testing.T) {
	far := newAnchorFixture(t, 16*time.Minute, -time.Hour)
	if _, timedOut, _, _ := far.eng.checkReviewGate(far.board, far.item, far.stage); !timedOut {
		t.Fatal("a head dated an hour ahead must be ignored, not extend the wait")
	}
	near := newAnchorFixture(t, 16*time.Minute, -time.Minute)
	if _, timedOut, _, _ := near.eng.checkReviewGate(near.board, near.item, near.stage); timedOut {
		t.Fatal("a head within the skew allowance counts as arriving now")
	}
}

// The head SHA is resolved live when the snapshot has none, and the commit time
// is memoised per SHA.
func TestReviewWaitAnchor_ResolvesHeadLiveAndCachesPerSHA(t *testing.T) {
	f := newAnchorFixture(t, 17*time.Minute, 30*time.Second)
	f.item.LinkedPRHeadSHA = ""
	f.client.fetchPRDetailsFn = func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
		return &gh.PRDetails{Number: prNumber, State: "open", HeadSHA: "livehead"}, nil
	}
	var reads int
	inner := f.client.fetchCommitTimeFn
	f.client.fetchCommitTimeFn = func(owner, repo, sha string) (time.Time, error) {
		reads++
		if sha != "livehead" {
			t.Errorf("read commit %q, want the live head", sha)
		}
		return inner(owner, repo, sha)
	}
	for i := 0; i < 3; i++ {
		if _, timedOut, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage); timedOut {
			t.Fatal("timed out")
		}
	}
	if reads != 1 {
		t.Errorf("commit time read %d times, want 1 (memoised per SHA)", reads)
	}
}

// A restart (fresh Engine, empty cache) reaches the same verdict: the anchor is
// re-read from GitHub, not held in memory (R2, AC3) — inside the window and past it.
func TestReviewWaitAnchor_RestartKeepsTheAnchor(t *testing.T) {
	for _, tc := range []struct {
		name         string
		headAge      time.Duration
		wantTimedOut bool
	}{
		{"inside the window", 30 * time.Second, false},
		{"past the window", 16 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAnchorFixture(t, 40*time.Minute, tc.headAge)
			_, before, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage)
			restarted := reviewTestEngine(t, f.client)
			restarted.cfg.ReviewWaitTimeout = f.eng.cfg.ReviewWaitTimeout
			_, after, _, _ := restarted.checkReviewGate(f.board, f.item, f.stage)
			if before != tc.wantTimedOut || after != tc.wantTimedOut {
				t.Errorf("timedOut before restart=%v after restart=%v, want %v for both", before, after, tc.wantTimedOut)
			}
		})
	}
}

// Non-vacuity: with the anchor neutralised the reported scenario times out.
func TestReviewWaitAnchor_NonVacuity(t *testing.T) {
	f := newAnchorFixture(t, 17*time.Minute+30*time.Second, 30*time.Second)
	f.eng.SetReviewWaitHeadAnchorDisabledForTest(true)
	if _, timedOut, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage); !timedOut {
		t.Fatal("with the head anchor neutralised the label-only timeout must fire; the test would otherwise prove nothing")
	}
}

// --- ladder (R4/FR-006) -----------------------------------------------------

func newLadderFixture(t *testing.T, repromptedAge, headAge time.Duration) *anchorFixture {
	t.Helper()
	f := newAnchorFixture(t, 40*time.Minute, headAge)
	repromptedAt := f.now.Add(-repromptedAge)
	awaitingAt := f.now.Add(-40 * time.Minute)
	f.client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		switch labelName {
		case "fabrik:awaiting-review":
			return awaitingAt, nil
		case "fabrik:bot-reprompted":
			return repromptedAt, nil
		}
		return time.Time{}, nil
	}
	f.item.Labels = []string{"fabrik:awaiting-review", "fabrik:bot-reprompted"}
	f.item.LinkedPRReviewRequests = []gh.ReviewRequest{{Login: "copilot-pull-request-reviewer", IsBot: true}}
	return f
}

func anchorRemovedLabels(c *mockGitHubClient) []string {
	var out []string
	for _, r := range c.removeLabelCalls {
		out = append(out, r.labelName)
	}
	return out
}

// A push after the re-prompt ends the ladder cycle: Phase 2 does not fire on the
// old clock, only the re-prompt label is removed, and nothing is re-prompted
// until a full window after the new head.
func TestReviewWaitAnchor_LadderResetsOnNewHead(t *testing.T) {
	f := newLadderFixture(t, 16*time.Minute, time.Minute)
	blocked, timedOut, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage)
	if timedOut || !blocked {
		t.Fatalf("expected still blocked after a new head, got blocked=%v timedOut=%v", blocked, timedOut)
	}
	got := anchorRemovedLabels(f.client)
	if len(got) != 1 || got[0] != "fabrik:bot-reprompted" {
		t.Errorf("only fabrik:bot-reprompted may be removed, got %v", got)
	}
	if len(f.client.addCommentCalls) != 0 {
		t.Errorf("no re-prompt before a full window on the new head, got %d comments", len(f.client.addCommentCalls))
	}
}

// A full window after the new head, Phase 1 fires again for the new head.
func TestReviewWaitAnchor_LadderRefiresPhase1AfterWindowOnNewHead(t *testing.T) {
	f := newLadderFixture(t, 40*time.Minute, 16*time.Minute)
	blocked, timedOut, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage)
	if timedOut || !blocked {
		t.Fatalf("expected Phase 1 to re-fire (blocked), got blocked=%v timedOut=%v", blocked, timedOut)
	}
	if len(f.client.addCommentCalls) == 0 {
		t.Error("expected a fresh re-prompt comment for the new head")
	}
}

// No head change after the re-prompt: Phase 2 fires as today.
func TestReviewWaitAnchor_LadderPhase2UnchangedWithoutPush(t *testing.T) {
	f := newLadderFixture(t, 16*time.Minute, 60*time.Minute)
	_, timedOut, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage)
	if !timedOut {
		t.Fatal("Phase 2 must fire when no head arrived after the re-prompt")
	}
}

// Non-vacuity for the ladder reset.
func TestReviewWaitAnchor_LadderNonVacuity(t *testing.T) {
	f := newLadderFixture(t, 16*time.Minute, time.Minute)
	f.eng.SetReviewWaitHeadAnchorDisabledForTest(true)
	if _, timedOut, _, _ := f.eng.checkReviewGate(f.board, f.item, f.stage); !timedOut {
		t.Fatal("with the anchor neutralised Phase 2 fires on the old clock; the ladder test would otherwise prove nothing")
	}
}

// R3 / FR-005 (#2467): with the head anchor restarting the wait on every push,
// the review-wait timeout can no longer end a loop in which every round pushes
// a new head. MaxReviewCycles still does — through ReviewBlockedCycles, which is
// charged while the gate is blocked and never refunded for a no-op.
func TestReviewWaitAnchor_PushLoopStillEndsAtMaxReviewCycles(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "Addressed it.", false, TokenUsage{}, nil
		},
	}
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)
	eng.cfg.MaxReviewCycles = 3
	eng.cfg.ReviewWaitTimeout = 15 * time.Minute
	now := time.Now()
	eng.SetClock(stubClock{t: now})
	// The label is far older than the window, so without the anchor every round
	// would be a timeout; every round's head is a fresh push.
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return now.Add(-3 * time.Hour), nil
	}
	client.fetchCommitTimeFn = func(owner, repo, sha string) (time.Time, error) {
		return now.Add(-20 * time.Second), nil
	}
	if _, err := eng.worktreesFor("owner/repo").EnsureWorktree(33, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	baseItem := gh.ProjectItem{
		Number:          33,
		Repo:            "owner/repo",
		Labels:          []string{"stage:Implement:complete", "fabrik:awaiting-review"},
		LinkedPRNumber:  102,
		LinkedPRHeadSHA: "head-1",
	}
	round := func(id int) bool {
		item := baseItem
		item.LinkedPRReviews = []gh.PRReview{{Author: "alice", State: "CHANGES_REQUESTED", Body: fmt.Sprintf("finding %d", id), DatabaseID: id}}
		remirrorLiveReviews(client, item)
		advanced := make(map[string]bool)
		pctx := &phase1Ctx{ctx: context.Background(), board: board, item: item, stage: stgs[0], hasComplete: true, advancedItems: advanced}
		if !eng.handleReviewGate(pctx) {
			t.Fatalf("round %d: handleReviewGate did not claim the item", id)
		}
		eng.wg.Wait()
		return advanced["owner/repo#33"]
	}

	for i := 0; i < eng.cfg.MaxReviewCycles; i++ {
		if !round(4000 + i) {
			t.Fatalf("round %d: expected a reinvoke dispatch", i+1)
		}
	}
	for _, name := range anchorRemovedLabels(client) {
		if name == "fabrik:awaiting-review" {
			t.Fatal("the review-wait timeout fired during the push loop; the anchor should have restarted the wait each round")
		}
	}
	if round(4999) {
		t.Fatal("expected no further reinvoke once MaxReviewCycles was reached")
	}
	paused := false
	client.mu.Lock()
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			paused = true
		}
	}
	client.mu.Unlock()
	if !paused {
		t.Error("expected fabrik:paused via pauseForReviewCycleLimit at MaxReviewCycles")
	}
}
