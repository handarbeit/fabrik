package sim

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twin of #2299 (ADR 2299): a Validate worker that is still running when its
// own PR's CI gate clears must not have the PR merged under it, and one that
// returns after the PR merged must take the terminal path instead of arming
// fabrik:awaiting-ci against the merged PR.
//
// The second Validate run is a comment-review worker dispatched by a human
// comment while fabrik:awaiting-ci is armed; the scripted worker blocks inside
// simclaude until the test releases it, which RunPoll cannot drive (it waits for
// worker quiescence), so these scenarios call Engine.PollOnce directly.

type heldValidateWorker struct {
	started chan struct{}
	release chan struct{}
	// afterRelease, when set, runs inside the worker just before it returns.
	afterRelease func(env *Env, num int) error
}

func (h *heldValidateWorker) script(env *Env) simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		select {
		case h.started <- struct{}{}:
		default:
		}
		// The worker has taken its input: mark the human comment processed (the
		// durable 🚀) so the feedback gate does not hold the item for it. What is
		// under test is the merge decision while a worker is in flight, not the
		// feedback gate (which has its own twins).
		for _, c := range comments {
			if c.DatabaseID != 0 {
				if err := env.Sim.AddCommentReaction(env.Owner, env.Repo, c.DatabaseID, "ROCKET"); err != nil {
					return "", false, engine.TokenUsage{}, err
				}
			}
		}
		<-h.release
		if h.afterRelease != nil {
			if err := h.afterRelease(env, issue.Number); err != nil {
				return "", false, engine.TokenUsage{}, err
			}
		}
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{InputTokens: 500, OutputTokens: 100, TurnsUsed: 2, MaxTurns: 50}, nil
	}
}

func pollOnce(t *testing.T, env *Env) {
	t.Helper()
	env.Clock.Advance(env.PollInterval)
	if err := env.Engine.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
}

// armHeldValidate files a yolo item, drives it to fabrik:awaiting-ci at Validate,
// then posts a human comment so a second (held) Validate worker is dispatched.
func armHeldValidate(t *testing.T, env *Env, h *heldValidateWorker) int {
	t.Helper()
	env.Claude.ForStageComments("Validate", h.script(env))
	num := FileIssue(t, env, "validate after merge (#2299)", "body", "Implement", "fabrik:yolo")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 120)

	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "Please double-check the edge case.")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding comment: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		pollOnce(t, env)
		select {
		case <-h.started:
			return num
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second Validate worker was never dispatched\n\n%s", diagnostics(env))
		}
	}
}

func mergeCalls(env *Env) int {
	return len(env.Sim.Log().ByMethod("MergePR")) + len(env.Sim.Log().ByMethod("MergePRAtHeadSHA")) +
		len(env.Sim.Log().ByMethod("EnablePullRequestAutoMerge"))
}

// mergesWhileWorkerHeld drives the first scenario and returns how many merge /
// auto-merge calls the engine made while the second Validate worker was held in
// flight and CI was green. mutate runs on the Env before the scenario starts
// (the non-vacuity test uses it to neutralise the guard).
func mergesWhileWorkerHeld(t *testing.T, mutate func(*Env)) (env *Env, num, merges int) {
	t.Helper()
	env = newFeedbackGateEnv(t, func(cfg *engine.Config) { cfg.MaxCiFixCycles = 5 })
	if mutate != nil {
		mutate(env)
	}
	h := &heldValidateWorker{started: make(chan struct{}, 1), release: make(chan struct{})}
	num = armHeldValidate(t, env, h)
	t.Cleanup(func() { close(h.release) })

	// CI goes green on the PR head while the second worker is running.
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: feedbackGateCheck, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}
	for i := 0; i < 8; i++ {
		pollOnce(t, env)
	}
	return env, num, mergeCalls(env)
}

func TestValidateAfterMerge_NoMergeWhileWorkerInFlight(t *testing.T) {
	t.Parallel()
	env, _, merges := mergesWhileWorkerHeld(t, nil)
	if !env.Engine.HasInFlightWorker() {
		t.Fatal("non-vacuity: the second Validate worker is no longer in flight")
	}
	if merges != 0 {
		t.Fatalf("the PR was merged/auto-merge enabled %d time(s) while a Validate worker was in flight\n\n%s", merges, diagnostics(env))
	}
}

// Non-vacuity: with the in-flight guard neutralised the engine merges under the
// running worker, so the test above bites.
func TestValidateAfterMerge_NoMergeWhileWorkerInFlight_NonVacuity(t *testing.T) {
	t.Parallel()
	_, _, merges := mergesWhileWorkerHeld(t, func(e *Env) { e.Engine.SetMergeInFlightGuardDisabledForTest(true) })
	if merges == 0 {
		t.Fatal("with the guard neutralised the engine must merge while the worker runs (the scenario would be vacuous)")
	}
}

func TestValidateAfterMerge_MergeProceedsAfterWorkerExits(t *testing.T) {
	t.Parallel()
	env := newFeedbackGateEnv(t, func(cfg *engine.Config) { cfg.MaxCiFixCycles = 5 })
	h := &heldValidateWorker{started: make(chan struct{}, 1), release: make(chan struct{})}
	num := armHeldValidate(t, env, h)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: feedbackGateCheck, Status: "completed", Conclusion: "success"})
	for i := 0; i < 4; i++ {
		pollOnce(t, env)
	}
	close(h.release)
	AdvanceUntil(t, env, func(e *Env) bool { return mergeCalls(e) > 0 }, 60)
	WaitForProjectStatus(t, env, num, "Done", 60)
}

// prMergedBeforeWorkerReturns drives the incident sequence: the CI gate clears
// (awaiting-ci removed, stage:Validate:complete added, merge deferred) while the
// second Validate worker runs, the PR then lands out of band, and the worker
// returns into handleStageComplete. It returns how many times fabrik:awaiting-ci
// was added over the whole run — the first arm is expected; a second one is the
// bug (armed against a merged PR).
func prMergedBeforeWorkerReturns(t *testing.T, mutate func(*Env)) (env *Env, num, awaitingCIAdds int) {
	t.Helper()
	env = newFeedbackGateEnv(t, func(cfg *engine.Config) { cfg.MaxCiFixCycles = 5 })
	if mutate != nil {
		mutate(env)
	}
	h := &heldValidateWorker{started: make(chan struct{}, 1), release: make(chan struct{})}
	h.afterRelease = func(e *Env, num int) error {
		pr, err := e.Sim.FetchLinkedPR(e.Owner, e.Repo, num)
		if err != nil || pr == nil {
			return err
		}
		return e.Sim.Sim().MergePR(e.Owner, e.Repo, pr.Number)
	}
	num = armHeldValidate(t, env, h)

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: feedbackGateCheck, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}
	// Poll (without waiting on the held worker) until the CI gate has cleared.
	deadline := time.Now().Add(60 * time.Second)
	for hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-ci") {
		if time.Now().After(deadline) {
			t.Fatalf("the CI gate never cleared while the worker was held\n\n%s", diagnostics(env))
		}
		pollOnce(t, env)
	}
	if !hasLabel(IssueLabels(t, env, num), "stage:Validate:complete") {
		t.Fatal("non-vacuity: the CI gate cleared without stage:Validate:complete")
	}
	close(h.release)

	WaitForProjectStatus(t, env, num, "Done", 80)
	WaitForIssueClosed(t, env, num, 40)
	RunPolls(t, env, 4)
	return env, num, labelAdds(env, num, "fabrik:awaiting-ci")
}

func TestValidateAfterMerge_PRMergedBeforeWorkerReturns_TerminalPath(t *testing.T) {
	t.Parallel()
	env, num, adds := prMergedBeforeWorkerReturns(t, nil)

	if adds != 1 {
		t.Errorf("fabrik:awaiting-ci was added %d time(s); want exactly 1 (the first arm) — a second arm is against a merged PR", adds)
	}
	labels := IssueLabels(t, env, num)
	for _, bad := range []string{"fabrik:awaiting-ci", "fabrik:paused", "fabrik:awaiting-input"} {
		if hasLabel(labels, bad) {
			t.Errorf("%s must not remain on a landed item; labels %v", bad, labels)
		}
	}
	board, err := env.Sim.FetchProjectBoard(env.Owner, env.Repo, env.ProjectNum, "User")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range board.Items {
		if it.Number != num {
			continue
		}
		for _, c := range it.Comments {
			if strings.Contains(c.Body, "CI wait timeout") || strings.Contains(c.Body, "awaiting-ci settle failed") {
				t.Errorf("unexpected CI-timeout/stranded comment on a landed item: %s", c.Body)
			}
		}
	}
}

// Non-vacuity: without the live merged check the worker re-arms awaiting-ci
// against the merged PR, so the assertion above bites.
func TestValidateAfterMerge_PRMergedBeforeWorkerReturns_NonVacuity(t *testing.T) {
	t.Parallel()
	_, _, adds := prMergedBeforeWorkerReturns(t, func(e *Env) { e.Engine.SetCompletionMergedCheckDisabledForTest(true) })
	if adds < 2 {
		t.Errorf("with the merged check neutralised awaiting-ci must be re-armed (adds=%d); the scenario would be vacuous", adds)
	}
}
