package sim

import (
	"context"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #2552 sim twin: a conversation comment from a bot that is not opted in
// (a deploy-preview notice such as vercel[bot]) is not feedback. It starts no
// comment-review worker, holds no feedback gate and leaves no reaction or
// processed marker behind. A bot opted in through feedback_bots or the stage's
// expected_reviewers still does. Each scenario runs the rule neutralised
// (SetBotCommentRuleDisabledForTest) as the non-vacuity control.

const (
	previewBot     = "vercel[bot]"
	previewBotBody = "Deployment ready — preview: https://fabrik-git-fabrik-issue.vercel.app"
	otherBot       = "snyk-bot"
	otherBotBody   = "Snyk scan: no new issues"
)

func botCommentStages(feedbackBots []string, reviewers *[]string) []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Review", Order: 2},
		{Name: "Validate", Order: 3, WaitForCI: &tr, ExpectedReviewers: reviewers, FeedbackBots: feedbackBots},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}
}

type botCommentRun struct {
	env   *Env
	probe *feedbackProbe
	num   int
}

// startBotComment files an item and has each given bot comment on the PR in the
// middle of Validate's own run, then drives it to the CI wait and a generous
// number of polls so a dispatch, if one is coming, has happened.
func startBotComment(t *testing.T, stgs []*stages.Stage, comments map[string]string, neutralised bool) *botCommentRun {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:    stgs,
		Yolo:      boolPtr(false),
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxReviewCycles = 5
		},
	})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{feedbackGateCheck})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	env.Engine.SetBotCommentRuleDisabledForTest(neutralised)
	probe := &feedbackProbe{}
	for _, name := range []string{"Implement", "Review", "Validate"} {
		env.Claude.ForStageComments(name, probe.script(env))
	}
	env.Claude.ForStage("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if prs, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, issue.Number); prs != nil {
			for author, body := range comments {
				env.Sim.Sim().SeedPRComment(env.OwnerRepo, prs.Number, author, body)
			}
		}
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})
	num := FileIssue(t, env, "sim bot comment", "body", "Implement", "fabrik:yolo")
	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	RunPolls(t, env, 25)
	return &botCommentRun{env: env, probe: probe, num: num}
}

// land seeds CI green on the PR's current head and waits for the item to merge —
// proof that no feedback gate is holding it.
func (r *botCommentRun) land(t *testing.T) {
	t.Helper()
	pr, err := r.env.Sim.FetchLinkedPR(r.env.Owner, r.env.Repo, r.num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	r.env.Sim.Sim().SeedCheckRun(r.env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: feedbackGateCheck, Status: "completed", Conclusion: "success"})
	if err := r.env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}
	WaitForIssueClosed(t, r.env, r.num, 150)
	if !linkedPRMerged(r.env, r.num) {
		t.Fatal("issue closed but the PR is not merged")
	}
}

// botCommentReactions counts reactions the engine wrote on PR conversation
// comments authored by author.
func (r *botCommentRun) reactionsOn(t *testing.T, author string) int {
	t.Helper()
	pr, err := r.env.Sim.FetchLinkedPR(r.env.Owner, r.env.Repo, r.num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	comments, err := r.env.Sim.FetchIssueComments(r.env.Owner, r.env.Repo, pr.Number)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	n := 0
	for _, c := range comments {
		if c.Author != author {
			continue
		}
		for _, e := range r.env.Sim.Log().ByMethod("AddCommentReaction") {
			if e.Args.Number == c.DatabaseID && !e.Failed() {
				n++
			}
		}
	}
	return n
}

func TestBotComment_UndeclaredStartsNoWorkerAndHoldsNoGate(t *testing.T) {
	t.Parallel()
	t.Run("as shipped", func(t *testing.T) {
		t.Parallel()
		r := startBotComment(t, botCommentStages(nil, nil), map[string]string{previewBot: previewBotBody}, false)
		if n := r.probe.callCount(); n != 0 {
			t.Fatalf("an undeclared bot's PR comment dispatched %d comment-review worker(s), want 0", n)
		}
		if n := r.reactionsOn(t, previewBot); n != 0 {
			t.Fatalf("the engine wrote %d reaction(s) on a skipped bot comment, want 0 (derived state)", n)
		}
		r.land(t)
		if n := r.probe.callCount(); n != 0 {
			t.Fatalf("landing dispatched %d comment-review worker(s), want 0", n)
		}
		if n := r.reactionsOn(t, previewBot); n != 0 {
			t.Fatalf("landing left %d reaction(s) on a skipped bot comment, want 0", n)
		}
	})
	t.Run("neutralised control dispatches", func(t *testing.T) {
		t.Parallel()
		r := startBotComment(t, botCommentStages(nil, nil), map[string]string{previewBot: previewBotBody}, true)
		if n := r.probe.callCount(); n < 1 {
			t.Fatalf("with the rule neutralised the bot comment must dispatch a worker, got %d", n)
		}
	})
}

func TestBotComment_OptedInBotStillDispatches(t *testing.T) {
	t.Parallel()
	reviewers := []string{"vercel"}
	cases := []struct {
		name string
		stgs []*stages.Stage
	}{
		{"feedback_bots", botCommentStages([]string{"vercel"}, nil)},
		{"expected_reviewers", botCommentStages(nil, &reviewers)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := startBotComment(t, c.stgs, map[string]string{previewBot: previewBotBody}, false)
			if n := r.probe.callCount(); n < 1 {
				t.Fatalf("an opted-in bot's PR comment must dispatch a worker, got %d", n)
			}
		})
	}
}

// Of two bots on one PR only the opted-in one counts.
func TestBotComment_OnlyTheDeclaredBotCounts(t *testing.T) {
	t.Parallel()
	r := startBotComment(t, botCommentStages([]string{"vercel"}, nil),
		map[string]string{previewBot: previewBotBody, otherBot: otherBotBody}, false)
	if n := r.probe.callCount(); n < 1 {
		t.Fatalf("the declared bot's comment must dispatch a worker, got %d", n)
	}
	r.probe.mu.Lock()
	defer r.probe.mu.Unlock()
	if len(r.probe.bodyIDs) == 0 || len(r.probe.bodyIDs[0]) != 1 {
		t.Fatalf("the worker must be handed exactly the declared bot's comment, got %v", r.probe.bodyIDs)
	}
}
