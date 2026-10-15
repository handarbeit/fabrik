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

// This file is the sim twin of #2343 (ADR-2343): the Validate completion SHA is engine
// memory, so a restart used to wipe it and a later push to the PR no longer invalidated
// stage:Validate:complete. The engine now rebuilds it from the Validate stage comment's
// commit: header before the landing decision.
//
// The Validate completion is driven through the real addCompleteLabelAndRemoveCI path
// (not seeded), so the stage comment's commit: header is genuine; RestartEnv then gives
// the engine an empty Store over unchanged GitHub state.
//
// Neutralisation: Engine.SetValidateSHARebuildDisabledForTest restores the old FR-5
// "empty SHA means do nothing" behaviour — PushAfterRestartInvalidatesValidate fails
// with it (TestValidateSHARestart_NonVacuity asserts exactly that).

func validateSHARestartEnv(t *testing.T) (env *Env, num int, firstHead string) {
	t.Helper()
	env = NewEnv(t, EnvOptions{
		Stages:    ciFixStages(),
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = 5
			// simgh posts as simgh-bot; the rebuild trusts only the engine's own comments.
			cfg.User = "simgh-bot"
		},
	})
	// A real Validate run always reports; the default script emits only the marker, which
	// the engine would not post as a stage comment.
	env.Claude.ForStage("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		out, ok, usage, err := simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
		return "Validation report: all requirements met.\n\n" + out, ok, usage, err
	})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	num = FileIssue(t, env, "validate sha restart", "Prove a push after a restart still invalidates Validate.", "Implement", "fabrik:cruise")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	firstHead = pr.HeadSHA
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, firstHead, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}
	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 80)
	if got := env.Claude.StageCallCount("Validate"); got != 1 {
		t.Fatalf("StageCallCount(Validate) = %d before restart, want 1", got)
	}
	return env, num, firstHead
}

// pushToPRBranch moves the PR branch head, as an external push would.
func pushToPRBranch(t *testing.T, env *Env, num int) string {
	t.Helper()
	branch := memberBranch(num)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, branch, map[string]string{"external.txt": "pushed after restart\n"}, "external push after restart")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("pushing to %s: %v", branch, err)
	}
	head, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	return head
}

func TestValidateSHARestart_PushAfterRestartInvalidatesValidate(t *testing.T) {
	t.Parallel()
	env, num, firstHead := validateSHARestartEnv(t)

	restarted := RestartEnv(t, env)
	newHead := pushToPRBranch(t, restarted, num)
	if newHead == firstHead {
		t.Fatal("the push did not move the head")
	}

	for i := 0; i < 3; i++ {
		RunPoll(t, restarted)
	}
	if hasLabel(IssueLabels(t, restarted, num), validateCompleteLabel) {
		t.Fatalf("%s survived a post-restart push to the PR branch\n\n%s", validateCompleteLabel, diagnostics(restarted))
	}
	// ... and Validate runs again on the new head.
	for i := 0; i < 40 && restarted.Claude.StageCallCount("Validate") < 2; i++ {
		RunPoll(t, restarted)
	}
	if got := restarted.Claude.StageCallCount("Validate"); got < 2 {
		t.Errorf("StageCallCount(Validate) = %d after a post-restart push, want Validate re-dispatched (>=2)\n\n%s", got, diagnostics(restarted))
	}
}

// With the rebuild disabled the same scenario must not invalidate: this is what makes
// PushAfterRestartInvalidatesValidate non-vacuous.
func TestValidateSHARestart_NonVacuity(t *testing.T) {
	t.Parallel()
	env, num, _ := validateSHARestartEnv(t)

	restarted := RestartEnv(t, env)
	defer restarted.Engine.SetValidateSHARebuildDisabledForTest()()
	pushToPRBranch(t, restarted, num)

	for i := 0; i < 3; i++ {
		RunPoll(t, restarted)
	}
	if got := restarted.Claude.StageCallCount("Validate"); got != 1 {
		t.Errorf("StageCallCount(Validate) = %d with the rebuild disabled, want 1 (old behaviour: push goes unnoticed)", got)
	}
	if !hasLabel(IssueLabels(t, restarted, num), "stage:Validate:complete") {
		t.Error("stage:Validate:complete was removed with the rebuild disabled — the twin is vacuous")
	}
}

// An unchanged head after a restart must keep Validate complete and not re-run it.
func TestValidateSHARestart_UnchangedHeadKeepsValidate(t *testing.T) {
	t.Parallel()
	env, num, _ := validateSHARestartEnv(t)

	restarted := RestartEnv(t, env)
	for i := 0; i < 3; i++ {
		RunPoll(t, restarted)
	}
	if got := restarted.Claude.StageCallCount("Validate"); got != 1 {
		t.Errorf("StageCallCount(Validate) = %d after a restart with no push, want 1", got)
	}
	if !hasLabel(IssueLabels(t, restarted, num), "stage:Validate:complete") {
		t.Errorf("stage:Validate:complete lost on an unchanged head\n\n%s", diagnostics(restarted))
	}
}
