package sim

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #2160 sim scenario: the feedback gate reads reviews live while the dispatcher
// reads the board cache. After an engine self-write the cache can judge itself
// fresh against a baseline that post-dates a review, so the dispatcher never sees
// it while the gate does — the item was held for ever, until a restart cleared
// the cache. The #2125 shape: a second bot's review body arrives while the first
// is being addressed. It must be dispatched too, with no restart.
//
// The mask is modelled with Engine.MaskReviewFromCacheForTest (it lifts only when
// the engine invalidates the item's deep cache) because the sim's own timestamps
// cannot hold a wall-clock baseline in place deterministically. BoardCache is
// required: without it the sim's readClient is the live adapter and the
// disagreement cannot occur.
func TestFeedbackGate_SecondReviewBodyBehindStaleCacheIsDispatched(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{
		Stages:     feedbackGateStages(),
		Yolo:       boolPtr(false),
		StartTime:  time.Now(),
		BoardCache: true,
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxReviewCycles = 5
		},
	})
	// The cache path admits items through the reactive mayNeedWork observer.
	t.Cleanup(env.Engine.RegisterObservers())
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{feedbackGateCheck})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	const secondReviewID = 5356295999
	env.Engine.MaskReviewFromCacheForTest(secondReviewID)

	probe := &feedbackProbe{}
	// During the first reinvoke a second bot posts its own review body.
	probe.afterCall = func(n int) {
		if n != 1 {
			return
		}
		if pr, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, 1); pr != nil {
			env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{
				Author: "other-review-bot", State: "COMMENTED", Body: "second finding", DatabaseID: secondReviewID,
			})
		}
	}
	env.Claude.ForStageComments("Validate", probe.script(env))
	env.Claude.ForStage("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if prs, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, issue.Number); prs != nil {
			env.Sim.Sim().SeedReview(env.OwnerRepo, prs.Number, gh.PRReview{
				Author: "handarbeit-pruefer", State: "COMMENTED", Body: feedbackReviewBody, DatabaseID: feedbackReviewID,
			})
		}
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})

	num := FileIssue(t, env, "sim feedback gate (#2160 repro)", "body", "Implement", "fabrik:yolo")
	if num != 1 {
		t.Fatalf("issue number = %d; the probe hook assumes #1", num)
	}
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

	// The first body is dispatched while CI is pending; CI then goes green on the
	// head that reinvoke pushed, so the gate (landing decision) sees both reviews.
	AdvanceUntil(t, env, func(*Env) bool { return probe.callCount() >= 1 }, 120)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: feedbackGateCheck, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}

	// Both bodies must be dispatched, without a restart.
	AdvanceUntil(t, env, func(*Env) bool { return probe.callCount() >= 2 }, 60)

	probe.mu.Lock()
	defer probe.mu.Unlock()
	got := map[string]bool{}
	for _, ids := range probe.bodyIDs {
		for _, id := range ids {
			got[id] = true
		}
	}
	for _, want := range []string{fmt.Sprintf("review-body:%d", feedbackReviewID), fmt.Sprintf("review-body:%d", secondReviewID)} {
		if !got[want] {
			t.Errorf("review body %s was never dispatched; reinvokes saw %v", want, probe.bodyIDs)
		}
	}
}
