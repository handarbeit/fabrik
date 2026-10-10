package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// The stage wait_for_ci gate re-runs a confirmed check-run failure's failed jobs
// once before dispatching a CI-fix worker (#2072, ADR 2072). These scenarios
// are the stage-gate twin of mergetrain_ci_infra_test.go's flake re-run.

// flakeRerunEnv builds the Validate-gated pipeline of TestCIFixReinvoke and
// seeds a flaky red on the PR head once the first Validate dispatch has pushed:
// a finished, failed Actions run whose re-run turns the sentinel check green.
// It returns the env, the issue number and the head SHA.
func flakeRerunEnv(t *testing.T, neutralise bool) (*Env, int, string) {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:       ciFixStages(),
		StartTime:    time.Now(), // see TestCIFixReinvoke's StartTime comment
		ConfigureCfg: func(cfg *engine.Config) { cfg.MaxCiFixCycles = 5 },
	})
	env.Engine.SetCIFlakeRerunDisabledForTest(neutralise)
	// If a CI-fix worker is ever dispatched it pushes a fix that goes green, so
	// the neutralised twin still terminates.
	env.Claude.ForStageComments("Validate", ciFixCommentScript(env.Sim, env.OwnerRepo, "success"))
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	num := FileIssue(t, env, "sim flake re-run", "A flaky red clears by re-run, with no CI-fix worker.", "Implement")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v (pr=%v)", err, pr)
	}
	sha := pr.HeadSHA

	s := env.Sim.Sim()
	s.SeedWorkflowRun(env.OwnerRepo, sha, gh.WorkflowRun{ID: 7001, Name: "CI", Status: "completed", Conclusion: "failure", JobCount: 1})
	s.SeedRerunStep(env.OwnerRepo, 7001, simgh.RerunStep{
		SHA:       sha,
		CheckRuns: []gh.CheckRun{{Name: ciFixSentinel, Status: "completed", Conclusion: "success", DetailsURL: actionsJobURL(7001, 1)}},
	})
	s.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "failure", DetailsURL: actionsJobURL(7001, 1)})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding the flaky red: %v", err)
	}
	return env, num, sha
}

// Flaky red → re-run → green → the gate clears without a CI-fix worker.
func TestCIFlakeRerun_FlakyRedClearsWithoutCIFixWorker(t *testing.T) {
	t.Parallel()
	env, num, _ := flakeRerunEnv(t, false)

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 80)
	WaitForIssueClosed(t, env, num, 80)
	WaitForProjectStatus(t, env, num, "Done", 80)

	if n := len(env.Sim.Log().ByMethod("RerunFailedJobs")); n != 1 {
		t.Errorf("RerunFailedJobs calls = %d, want exactly 1", n)
	}
	if n := env.Claude.CommentCallCount("Validate"); n != 0 {
		t.Errorf("CI-fix worker invocations = %d, want 0 — the re-run should have cleared the flake", n)
	}
}

// Neutralised twin: with the re-run off, the same flaky red dispatches a CI-fix
// worker and re-runs nothing, so the scenario above discriminates.
func TestCIFlakeRerun_Neutralised_DispatchesCIFixWorker(t *testing.T) {
	t.Parallel()
	env, num, _ := flakeRerunEnv(t, true)

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 80)
	WaitForIssueClosed(t, env, num, 80)

	if n := len(env.Sim.Log().ByMethod("RerunFailedJobs")); n != 0 {
		t.Errorf("RerunFailedJobs calls = %d, want 0 when neutralised", n)
	}
	if n := env.Claude.CommentCallCount("Validate"); n < 1 {
		t.Errorf("CI-fix worker invocations = %d, want >= 1 when neutralised", n)
	}
}
