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

// #2489 sim twin: a clean review from an expected reviewer (Pruefer's findings
// marker reading zero inline and zero body findings) starts no review-reinvoke
// worker and does not hold the feedback gate; anything not provably clean is
// worked exactly as before. Each scenario also runs the skip neutralised
// (SetCleanReviewSkipDisabledForTest) as the non-vacuity control.

const (
	cleanReviewer     = "handarbeit-pruefer"
	cleanReviewID     = 7700001
	cleanReviewMarker = "<!-- pruefer:findings inline=0 body=0 coverage=complete -->"
	cleanReviewBody   = "Reviewed the change; no issues found.\n\nPRUEFER_COVERAGE: complete\n\n" + cleanReviewMarker
)

func cleanReviewStages() []*stages.Stage {
	tr := true
	exp := []string{cleanReviewer}
	return []*stages.Stage{
		{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Review", Order: 2},
		{Name: "Validate", Order: 3, WaitForCI: &tr, ExpectedReviewers: &exp},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}
}

func newCleanReviewEnv(t *testing.T) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:    cleanReviewStages(),
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
	return env
}

type cleanReviewRun struct {
	env   *Env
	probe *feedbackProbe
	num   int
}

// startCleanReview files an item, lets the review land mid-way through
// Validate's own run (the #616 window) and drives it to the CI wait plus a
// generous number of polls, so a dispatch, if one is coming, has happened.
func startCleanReview(t *testing.T, review gh.PRReview, thread, neutralised bool) *cleanReviewRun {
	t.Helper()
	env := newCleanReviewEnv(t)
	env.Engine.SetCleanReviewSkipDisabledForTest(neutralised)
	probe := &feedbackProbe{}
	env.Claude.ForStageComments("Validate", probe.script(env))
	env.Claude.ForStage("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if prs, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, issue.Number); prs != nil {
			env.Sim.Sim().SeedReview(env.OwnerRepo, prs.Number, review)
			if thread {
				env.Sim.Sim().SeedReviewThreadComment(env.OwnerRepo, prs.Number, cleanReviewer, "please rename this", "README.md", 1)
			}
		}
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})
	num := FileIssue(t, env, "sim clean expected review", "body", "Implement", "fabrik:yolo")
	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	RunPolls(t, env, 25)
	return &cleanReviewRun{env: env, probe: probe, num: num}
}

// land seeds CI green on the PR's current head and waits for the item to merge.
func (r *cleanReviewRun) land(t *testing.T, env *Env) {
	t.Helper()
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, r.num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: feedbackGateCheck, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}
	WaitForIssueClosed(t, env, r.num, 150)
	if !linkedPRMerged(env, r.num) {
		t.Fatal("issue closed but the PR is not merged")
	}
}

func cleanReviewOf(author string) gh.PRReview {
	return gh.PRReview{Author: author, State: "COMMENTED", Body: cleanReviewBody, DatabaseID: cleanReviewID}
}

func TestCleanExpectedReview_NoWorkerAndItemAdvances(t *testing.T) {
	t.Parallel()
	t.Run("as shipped", func(t *testing.T) {
		t.Parallel()
		r := startCleanReview(t, cleanReviewOf(cleanReviewer), false, false)
		if n := r.probe.callCount(); n != 0 {
			t.Fatalf("a clean expected-reviewer review dispatched %d review-reinvoke(s), want 0", n)
		}
		r.land(t, r.env)
		if n := r.probe.callCount(); n != 0 {
			t.Fatalf("landing dispatched %d review-reinvoke(s), want 0", n)
		}
	})
	t.Run("neutralised control dispatches", func(t *testing.T) {
		t.Parallel()
		r := startCleanReview(t, cleanReviewOf(cleanReviewer), false, true)
		if n := r.probe.callCount(); n < 1 {
			t.Fatalf("with the skip neutralised the review must dispatch a worker, got %d", n)
		}
	})
}

func TestCleanExpectedReview_NotProvablyCleanStillDispatches(t *testing.T) {
	t.Parallel()
	mk := func(mut func(*gh.PRReview)) gh.PRReview {
		r := cleanReviewOf(cleanReviewer)
		mut(&r)
		return r
	}
	cases := []struct {
		name   string
		review gh.PRReview
	}{
		{"non-expected author, same body", cleanReviewOf("someone-else")},
		{"non-zero inline", mk(func(r *gh.PRReview) {
			r.Body = "found one\n\n<!-- pruefer:findings inline=1 body=0 coverage=complete -->"
		})},
		{"non-zero body", mk(func(r *gh.PRReview) {
			r.Body = "found one\n\n<!-- pruefer:findings inline=0 body=1 coverage=complete -->"
		})},
		{"no marker", mk(func(r *gh.PRReview) { r.Body = "looks fine to me" })},
		{"unparseable marker", mk(func(r *gh.PRReview) {
			r.Body = "x\n\n<!-- pruefer:findings inline=unknown body=unknown coverage=complete -->"
		})},
		{"duplicated marker", mk(func(r *gh.PRReview) { r.Body = cleanReviewBody + "\n\n" + cleanReviewMarker })},
		{"clean change request", mk(func(r *gh.PRReview) { r.State = "CHANGES_REQUESTED" })},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := startCleanReview(t, c.review, false, false)
			if n := r.probe.callCount(); n != 1 {
				t.Fatalf("dispatched %d review-reinvoke(s), want exactly 1", n)
			}
			r.probe.mu.Lock()
			ids := append([]string(nil), r.probe.bodyIDs[0]...)
			r.probe.mu.Unlock()
			if want := fmt.Sprintf("review-body:%d", cleanReviewID); len(ids) != 1 || ids[0] != want {
				t.Fatalf("reinvoke comments = %v, want [%s]", ids, want)
			}
		})
	}
}

// A clean marker never suppresses an unresolved review thread (FR-009).
func TestCleanExpectedReview_UnresolvedThreadStillDispatches(t *testing.T) {
	t.Parallel()
	r := startCleanReview(t, cleanReviewOf(cleanReviewer), true, false)
	if n := r.probe.callCount(); n < 1 {
		t.Fatalf("an unresolved thread must still dispatch a worker, got %d", n)
	}
	r.probe.mu.Lock()
	defer r.probe.mu.Unlock()
	for _, ids := range r.probe.bodyIDs {
		for _, id := range ids {
			if id == fmt.Sprintf("review-body:%d", cleanReviewID) {
				t.Fatalf("the clean review body was handed to the worker alongside the thread: %v", ids)
			}
		}
	}
}

// SC-004: the skip is derived, so a restart leaves the review excluded.
func TestCleanExpectedReview_StaysSkippedAcrossRestart(t *testing.T) {
	t.Parallel()
	r := startCleanReview(t, cleanReviewOf(cleanReviewer), false, false)
	env2 := RestartEnv(t, r.env)
	RunPolls(t, env2, 15)
	if n := r.probe.callCount(); n != 0 {
		t.Fatalf("after a restart the clean review dispatched %d worker(s), want 0", n)
	}
	r.land(t, env2)
	if n := r.probe.callCount(); n != 0 {
		t.Fatalf("landing after a restart dispatched %d worker(s), want 0", n)
	}
}
