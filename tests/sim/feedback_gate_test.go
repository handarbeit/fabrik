package sim

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #1953 sim scenarios: never advance or land while review feedback is
// unprocessed. Reproduces the #616 timeline — a body-only COMMENTED bot review
// lands while Validate (wait_for_ci) is running, CI later goes green, and the
// engine must dispatch the review body as a reinvoke and must not merge before
// it has been processed.
//
// Assertions are scoped to engine decisions (worker invocations, the mutation
// log, reactions), not GitHub merge semantics — see tests/sim/README.md.

const (
	feedbackGateCheck  = "slow-gate"
	feedbackReviewID   = 5356295491
	feedbackReviewBody = "lookup_key masking in resolve_entities_by_name_in_kinds"
)

// feedbackGateStages: Validate carries wait_for_ci only, as in #616.
func feedbackGateStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Review", Order: 2},
		{Name: "Validate", Order: 3, WaitForCI: &tr},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}
}

func newFeedbackGateEnv(t *testing.T, cfgFn func(*engine.Config)) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:    feedbackGateStages(),
		Yolo:      boolPtr(false),
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxReviewCycles = 5
			if cfgFn != nil {
				cfgFn(cfg)
			}
		},
	})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{feedbackGateCheck})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return env
}

// feedbackProbe records what each review-reinvoke saw and pushes a real rework
// commit, as a real comment worker does.
type feedbackProbe struct {
	mu       sync.Mutex
	calls    int
	merges   []int
	bodyIDs  [][]string
	noCommit bool
	// signalComplete makes a no-commit invocation still report
	// FABRIK_STAGE_COMPLETE — the shape that runs handleStageComplete (and its
	// resetCommentBreaker) after every reinvoke.
	signalComplete bool
	// afterCall, when non-nil, runs after each invocation (used by the R4 test
	// to make the reviewer produce a fresh body every cycle).
	afterCall func(n int)
}

func (p *feedbackProbe) script(env *Env) simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		p.mu.Lock()
		p.calls++
		n := p.calls
		p.merges = append(p.merges, len(env.Sim.Log().ByMethod("MergePR")))
		var ids []string
		for _, c := range comments {
			ids = append(ids, c.ID)
		}
		p.bodyIDs = append(p.bodyIDs, ids)
		p.mu.Unlock()

		if p.noCommit {
			if p.afterCall != nil {
				p.afterCall(n)
			}
			if p.signalComplete {
				return "nothing actionable\nFABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{InputTokens: 100, OutputTokens: 20, TurnsUsed: 1, MaxTurns: 15}, nil
			}
			return "nothing actionable\n", false, engine.TokenUsage{InputTokens: 100, OutputTokens: 20, TurnsUsed: 1, MaxTurns: 15}, nil
		}
		out, completed, usage, err := simclaude.DefaultCommentScript(ctx, stage, issue, comments, workDir, opts)
		if err == nil {
			if pushOut, perr := exec.Command("git", "-C", workDir, "push", "origin", "HEAD").CombinedOutput(); perr != nil {
				return "", false, engine.TokenUsage{}, fmt.Errorf("push rework: %v\n%s", perr, pushOut)
			}
		}
		if p.afterCall != nil {
			p.afterCall(n)
		}
		return out, completed, usage, err
	}
}

func (p *feedbackProbe) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestFeedbackGate_MidRunReviewBodyIsReinvokedBeforeLanding is R7: review body
// lands mid-run of a wait_for_ci stage, CI goes green afterwards — the body must
// be dispatched as a reinvoke, and the PR must not merge before it has been.
func TestFeedbackGate_MidRunReviewBodyIsReinvokedBeforeLanding(t *testing.T) {
	t.Parallel()
	env := newFeedbackGateEnv(t, nil)
	probe := &feedbackProbe{}
	env.Claude.ForStageComments("Validate", probe.script(env))

	// The review lands *during* Validate's own invocation — the #616 window.
	// Before the run reports completion, so the pre-run snapshot never sees it.
	env.Claude.ForStage("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		prs, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, issue.Number)
		if prs != nil {
			env.Sim.Sim().SeedReview(env.OwnerRepo, prs.Number, gh.PRReview{
				Author: "handarbeit-pruefer", State: "COMMENTED", Body: feedbackReviewBody, DatabaseID: feedbackReviewID,
			})
		}
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})

	num := FileIssue(t, env, "sim feedback gate (#616 repro)", "body", "Implement", "fabrik:yolo")
	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

	// The review body must be dispatched as a reinvoke while CI is still
	// pending — no CI check has been seeded yet.
	AdvanceUntil(t, env, func(*Env) bool { return probe.callCount() >= 1 }, 120)
	probe.mu.Lock()
	first := append([]string(nil), probe.bodyIDs[0]...)
	mergesAtDispatch := probe.merges[0]
	probe.mu.Unlock()
	wantID := fmt.Sprintf("review-body:%d", feedbackReviewID)
	if len(first) != 1 || first[0] != wantID {
		t.Fatalf("reinvoke comments = %v, want exactly [%s]", first, wantID)
	}
	if mergesAtDispatch != 0 {
		t.Fatalf("PR merged (%d MergePR) before the review body was dispatched", mergesAtDispatch)
	}
	if linkedPRMerged(env, num) {
		t.Fatal("PR merged before the review body was processed")
	}

	// Now CI goes green on the head the reinvoke pushed: the item may land.
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: feedbackGateCheck, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}
	WaitForIssueClosed(t, env, num, 120)
	if !linkedPRMerged(env, num) {
		t.Fatal("issue closed but the PR is not merged")
	}

	// R8: the review body carries 👀 and 🚀, the human-visible signal.
	react := env.Sim.Sim().ReviewReactions(env.OwnerRepo, pr.Number, feedbackReviewID)
	if react["eyes"] != 1 || react["rocket"] != 1 {
		t.Errorf("review reactions = %v, want eyes and rocket", react)
	}
}

// TestFeedbackGate_ChatterConvergesToPause is R4: a reviewer that produces a
// fresh non-actionable body after every reinvoke must still converge to a pause,
// not an infinite loop. The convergence route is the success-agnostic no-op
// breaker (#1555), since the gate itself is clear and the reinvoke lands no
// commit (ReviewCycles is refunded).
func TestFeedbackGate_ChatterConvergesToPause(t *testing.T) {
	t.Parallel()
	chatterConvergesToPause(t, false)
}

// The same bound must hold when every no-commit reinvoke still reports
// FABRIK_STAGE_COMPLETE, which runs handleStageComplete and its
// resetCommentBreaker each cycle — a reset there must not defeat R4.
func TestFeedbackGate_ChatterWithCompletionSignalConvergesToPause(t *testing.T) {
	t.Parallel()
	chatterConvergesToPause(t, true)
}

func chatterConvergesToPause(t *testing.T, signalComplete bool) {
	t.Helper()
	env := newFeedbackGateEnv(t, func(cfg *engine.Config) {
		cfg.MaxNoOpCommentCycles = 3
	})
	probe := &feedbackProbe{noCommit: true, signalComplete: signalComplete}
	var reviewIDs = feedbackReviewID
	var num int
	seed := func() {
		reviewIDs++
		pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
		if err == nil && pr != nil {
			env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{
				Author: "handarbeit-pruefer", State: "COMMENTED", Body: fmt.Sprintf("chatter %d", reviewIDs), DatabaseID: reviewIDs,
			})
		}
	}
	probe.afterCall = func(int) { seed() }
	env.Claude.ForStageComments("Validate", probe.script(env))

	num = FileIssue(t, env, "sim feedback gate chatter", "body", "Implement", "fabrik:yolo")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	seed()

	AdvanceUntil(t, env, func(*Env) bool {
		return hasLabel(IssueLabels(t, env, num), "fabrik:paused")
	}, 200)

	if n := probe.callCount(); n > 12 {
		t.Errorf("chatter reviewer drove %d reinvokes — the bound did not hold", n)
	}
	if linkedPRMerged(env, num) {
		t.Error("PR merged despite endless unprocessed review feedback")
	}
	t.Logf("chatter converged to fabrik:paused after %d reinvoke(s)", probe.callCount())
}
