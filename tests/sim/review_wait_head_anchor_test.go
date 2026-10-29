package sim

import (
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
)

// Sim twin of the review-wait head anchor (#2467, ADR 2467). The wait is
// measured from the later of fabrik:awaiting-review's applied time and the
// arrival (committer date) of the PR's current head.
//
// The gate compares GitHub-sourced times with the host clock (see
// engine/review_wait_anchor.go), so — like timeout_test.go — the Env's StartTime
// is backdated 24 real hours: the label is ancient, and a scenario decides
// whether the head is recent (a push just happened) by seeding its commit time
// relative to the real now. Every scenario whose outcome the anchor changes runs
// again with engine.SetReviewWaitHeadAnchorDisabledForTest(true) and must flip.

func newHeadAnchorEnv(t *testing.T, maxCycles int) *Env {
	t.Helper()
	return NewEnv(t, EnvOptions{
		Stages:    reviewAuthorityStages(),
		StartTime: time.Now().Add(-24 * time.Hour),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.ReviewWaitTimeout = 15 * time.Minute
			cfg.MaxReviewCycles = maxCycles
		},
	})
}

// seedWaitingOnHuman puts an item in Review with a human reviewer requested.
func seedWaitingOnHuman(t *testing.T, env *Env, marker string, extraLabels ...string) (num, prNum int) {
	t.Helper()
	num, prNum = seedReviewGateItemDraft(t, env, "Review", marker, extraLabels...)
	env.Sim.Sim().SeedReviewRequest(env.OwnerRepo, prNum, gh.ReviewRequest{Login: "alice-human", IsBot: false})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedReviewRequest: %v", err)
	}
	return num, prNum
}

func seedHeadTime(t *testing.T, env *Env, sha string, at time.Time) {
	t.Helper()
	env.Sim.Sim().SeedCommitTime(env.OwnerRepo, sha, at)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCommitTime: %v", err)
	}
}

func anchorPaused(t *testing.T, env *Env, num int) bool {
	t.Helper()
	return hasLabel(IssueLabels(t, env, num), "fabrik:paused")
}

// The reported shape: the label is far older than the window, but the last fix
// was pushed seconds ago. The wait restarts; the control (anchor off) pauses.
func TestReviewWaitHeadAnchor_PushRestartsTheWait(t *testing.T) {
	t.Parallel()
	for _, anchorOff := range []bool{false, true} {
		anchorOff := anchorOff
		name := "anchored"
		if anchorOff {
			name = "neutralised-control"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newHeadAnchorEnv(t, 5)
			env.Engine.SetReviewWaitHeadAnchorDisabledForTest(anchorOff)
			num, prNum := seedWaitingOnHuman(t, env, "head-anchor-push-"+name)
			seedHeadTime(t, env, headOf(t, env, prNum), time.Now().Add(-30*time.Second))

			if anchorOff {
				WaitForIssueLabel(t, env, num, "fabrik:paused", 80)
				return
			}
			WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
			RunPolls(t, env, 30)
			if anchorPaused(t, env, num) {
				t.Fatal("review-wait timeout fired 30s after a push; the wait must restart on the new head")
			}
			if !hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-review") {
				t.Fatal("fabrik:awaiting-review must stay on while the reviewer has the new head")
			}
		})
	}
}

// No push (the head predates the label) and a full silent window after the push
// both time out, as today.
func TestReviewWaitHeadAnchor_TimeoutStillFires(t *testing.T) {
	t.Parallel()
	for name, headAge := range map[string]time.Duration{
		"head-predates-label":  48 * time.Hour,
		"full-window-after-it": 20 * time.Minute,
	} {
		name, headAge := name, headAge
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newHeadAnchorEnv(t, 5)
			num, prNum := seedWaitingOnHuman(t, env, "head-anchor-fires-"+name)
			seedHeadTime(t, env, headOf(t, env, prNum), time.Now().Add(-headAge))
			WaitForIssueLabel(t, env, num, "fabrik:paused", 80)
		})
	}
}

// A head whose commit time GitHub cannot answer falls back to the label time:
// no earlier timeout than before, and no pause of its own.
func TestReviewWaitHeadAnchor_UnknownHeadTimeFallsBackToLabel(t *testing.T) {
	t.Parallel()
	env := newHeadAnchorEnv(t, 5)
	num, _ := seedWaitingOnHuman(t, env, "head-anchor-unknown")
	// No SeedCommitTime: FetchCommitTime answers not found. The label is ancient,
	// so today's timeout fires.
	WaitForIssueLabel(t, env, num, "fabrik:paused", 80)
}

// A restart mid-wait keeps the anchor (R2): it is re-read from GitHub, not held
// in the engine's memory.
func TestReviewWaitHeadAnchor_RestartKeepsTheAnchor(t *testing.T) {
	t.Parallel()
	env := newHeadAnchorEnv(t, 5)
	num, prNum := seedWaitingOnHuman(t, env, "head-anchor-restart")
	seedHeadTime(t, env, headOf(t, env, prNum), time.Now().Add(-30*time.Second))
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	RunPolls(t, env, 10)
	if anchorPaused(t, env, num) {
		t.Fatal("paused before the restart")
	}

	env = RestartEnv(t, env)
	RunPolls(t, env, 30)
	if anchorPaused(t, env, num) {
		t.Fatal("a restart must not shorten the wait")
	}
}

// R4: a push after the bot re-prompt ends the ladder cycle, so Phase 2 does not
// pause on the old clock. The control (anchor off) pauses at Phase 2.
func TestReviewWaitHeadAnchor_LadderResetsOnPush(t *testing.T) {
	t.Parallel()
	for _, anchorOff := range []bool{false, true} {
		anchorOff := anchorOff
		name := "anchored"
		if anchorOff {
			name = "neutralised-control"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newExpectedReviewersTimeoutEnv(t)
			env.Engine.SetReviewWaitHeadAnchorDisabledForTest(anchorOff)
			num, prNum := seedReviewGateItemDraft(t, env, "Review", "head-anchor-ladder-"+name, "expected-reviewers:declared")
			first := headOf(t, env, prNum)
			seedHeadTime(t, env, first, time.Now().Add(-48*time.Hour))

			// Phase 1 fires on the old head.
			WaitForIssueLabel(t, env, num, "fabrik:bot-reprompted", 80)

			// The fix is pushed: the head is new, and was committed seconds ago.
			second := pushFix(t, env, num, prNum)
			seedHeadTime(t, env, second, time.Now().Add(-10*time.Second))

			if anchorOff {
				WaitForIssueLabel(t, env, num, "fabrik:paused", 80)
				return
			}
			AdvanceUntil(t, env, func(env *Env) bool {
				return !hasLabel(IssueLabels(t, env, num), "fabrik:bot-reprompted")
			}, 80)
			RunPolls(t, env, 20)
			if anchorPaused(t, env, num) {
				t.Fatal("Phase 2 paused on the old clock after a push")
			}
			if !hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-review") {
				t.Fatal("fabrik:awaiting-review must stay on across the ladder reset")
			}
		})
	}
}

// R3 / FR-005: a loop in which every round pushes a new head still ends at
// MaxReviewCycles; the review-wait timeout (restarted by each push) does not.
func TestReviewWaitHeadAnchor_PushLoopEndsAtMaxReviewCycles(t *testing.T) {
	t.Parallel()
	const maxCycles = 2
	env := newHeadAnchorEnv(t, maxCycles)
	num, prNum := seedReviewGateItem(t, env, "Review", "head-anchor-loop", "review-authority:authoritative")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)

	round := func(i int) {
		// A fresh push is modelled by re-dating the current head rather than by
		// SeedCommit: the mock reinvoke commits in the item's worktree, and a second
		// commit placed on the remote branch would diverge from it (branch-sync,
		// #2295) and pause the item for an unrelated reason.
		head := headOf(t, env, prNum)
		seedHeadTime(t, env, head, time.Now().Add(-5*time.Second))
		seedActionableReview(t, env, prNum, "reviewer-human", "CHANGES_REQUESTED", fmt.Sprintf("round %d: still not right", i))
	}
	for i := 0; i < maxCycles; i++ {
		before := env.Claude.CommentCallCount("Review")
		round(i + 1)
		AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.CommentCallCount("Review") > before }, 80)
		if anchorPaused(t, env, num) {
			t.Fatalf("paused after round %d, before MaxReviewCycles", i+1)
		}
	}
	round(maxCycles + 1)
	WaitForIssueLabel(t, env, num, "fabrik:paused", 80)
	if !hasCommentWithPrefix(projectItem(t, env, num).Comments, reviewCycleLimitCommentPrefix) {
		t.Fatalf("the push loop must end through the review cycle limit (prefix %q)", reviewCycleLimitCommentPrefix)
	}
	if got := env.Claude.CommentCallCount("Review"); got != maxCycles {
		t.Fatalf("expected exactly %d reinvokes before the cycle-limit pause, got %d", maxCycles, got)
	}
}
