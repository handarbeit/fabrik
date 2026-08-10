package sim

import (
	"time"

	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// reviewTimeoutStages: Implement creates the draft PR the review gate needs
// something to attach reviewer state to; Review is the wait_for_reviews
// stage under test.
func reviewTimeoutStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, CreateDraftPR: true},
		{Name: "Review", Order: 2, WaitForReviews: &tr},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

// TestReviewWaitTimeout_FiresWithZeroRealElapsedTime is AC6: advancing past
// FABRIK_REVIEW_WAIT_TIMEOUT fires the review-wait timeout without the test
// itself waiting out real minutes.
//
// Per R3/the spec review's Finding 3, this gate is GitHub-anchored, not
// engine-local: checkAwaitingReviewTimeout (engine/reviews.go) compares
// real time.Since(appliedAt) — appliedAt read live via FetchLabelAppliedAt
// — against cfg.ReviewWaitTimeout. No engine Clock seam is involved at all.
// What makes the timeout fire immediately is that appliedAt itself is a
// simgh-controlled timestamp: this Env's Clock starts 24 real hours in the
// past (see StartTime below) and only ever creeps forward by tiny
// RunPoll-driven increments, so by the time fabrik:awaiting-review is
// applied, its recorded timestamp is already far older than any reasonable
// ReviewWaitTimeout — a backdated FetchLabelAppliedAt response, exactly as
// the spec review's Finding 3 prescribed, with no production clock change
// required.
func TestReviewWaitTimeout_FiresWithZeroRealElapsedTime(t *testing.T) {
	t.Parallel()
	// Captured after t.Parallel() so the elapsed-time assertion below measures
	// only this test's own execution, not time spent queued behind serial tests.
	testStart := time.Now()

	env := NewEnv(t, EnvOptions{
		Stages:    reviewTimeoutStages(),
		StartTime: time.Now().Add(-24 * time.Hour),
	})

	num := FileIssue(t, env, "Review timeout", "body", "Implement")

	WaitForProjectStatus(t, env, num, "Review", 80)

	// Seed a pure-human outstanding reviewer against the real draft PR
	// Implement just created, so the gate has an unambiguous "waiting on a
	// human" reason rather than routing into the bot re-prompt ladder.
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatal("expected a linked PR after Implement")
	}
	env.Sim.Sim().SeedReviewRequest(env.OwnerRepo, pr.Number, gh.ReviewRequest{Login: "alice-human", IsBot: false})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedReviewRequest: %v", err)
	}

	// Deliberately does NOT also wait for fabrik:awaiting-review first: this
	// Env's Clock starts 24 real hours in the past (see StartTime above), so
	// the review-wait timeout can already be past due on the very same poll
	// that applies the label — the label's own application and its
	// supersession by fabrik:paused can land in the same or adjacent poll
	// cycles, with nothing guaranteeing a poll boundary falls between them.
	// A test asserting the transient intermediate state would be racy by
	// construction; fabrik:paused is the one label that must eventually be
	// stable, so it's the only one asserted here.
	WaitForIssueLabel(t, env, num, "fabrik:paused", 80)

	elapsed := time.Since(testStart)
	const budget = 30 * time.Second // generously above the workerYield-paced poll cost; nowhere near cfg.ReviewWaitTimeout's real 15m default
	if elapsed > budget {
		t.Errorf("test took %v of real wall-clock time, want under %v — the review-wait timeout should have fired immediately off the backdated label timestamp, not waited out any real duration", elapsed, budget)
	}
	t.Logf("review-wait timeout fired after %v of real wall-clock time (cfg.ReviewWaitTimeout default is 15m)", elapsed)
}
