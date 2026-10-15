package sim

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
	"github.com/handarbeit/fabrik/tests/sim/simgh/ghfault"
)

// #2296: a cancelled required check on a Validate PR head is not a pass. The
// stage-gate twin of mergetrain_ci_cancelled_test.go.

type cancelledGateOpts struct {
	rerunConclusion string // conclusion of the check run the scripted re-run adds
	rerunForbidden  bool   // the re-run endpoint is refused (no `actions` permission)
}

// cancelledGateEnv is flakeRerunEnv with a CANCELLED sentinel check instead of a
// failed one. It returns the env, the issue number, the head SHA, and a getter
// for the bodies of every CI-fix comment the Validate worker was handed.
func cancelledGateEnv(t *testing.T, o cancelledGateOpts) (*Env, int, string, func() []string) {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:       ciFixStages(),
		StartTime:    time.Now(),
		ConfigureCfg: func(cfg *engine.Config) { cfg.MaxCiFixCycles = 5 },
	})

	var mu sync.Mutex
	var bodies []string
	inner := ciFixCommentScript(env.Sim, env.OwnerRepo, "success")
	env.Claude.ForStageComments("Validate", simclaude.CommentScript(func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		mu.Lock()
		for _, c := range comments {
			bodies = append(bodies, c.Body)
		}
		mu.Unlock()
		return inner(ctx, stage, issue, comments, workDir, opts)
	}))
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	num := FileIssue(t, env, "sim cancelled gate", "A cancelled required check is not a pass.", "Implement")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("FetchLinkedPR: %v (pr=%v)", err, pr)
	}
	sha := pr.HeadSHA

	if o.rerunForbidden {
		env.Sim.Faults().FailAlways("RerunFailedJobs", ghfault.Wrap(gh.ErrForbidden, "Resource not accessible by integration"))
	}
	s := env.Sim.Sim()
	s.SeedWorkflowRun(env.OwnerRepo, sha, gh.WorkflowRun{ID: 7101, Name: "CI", Status: "completed", Conclusion: "cancelled", JobCount: 1})
	s.SeedRerunStep(env.OwnerRepo, 7101, simgh.RerunStep{
		SHA:        sha,
		Conclusion: o.rerunConclusion,
		CheckRuns:  []gh.CheckRun{{Name: ciFixSentinel, Status: "completed", Conclusion: o.rerunConclusion, DetailsURL: actionsJobURL(7101, 1)}},
	})
	s.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "cancelled", DetailsURL: actionsJobURL(7101, 1)})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding the cancelled check: %v", err)
	}
	return env, num, sha, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// Cancelled → one re-run → success: the gate clears, with no CI-fix worker, and
// the stage did not complete on the cancelled run itself.
func TestCICancelled_RerunSuccessClearsGateWithoutCIFixWorker(t *testing.T) {
	t.Parallel()
	env, num, _, bodies := cancelledGateEnv(t, cancelledGateOpts{rerunConclusion: "success"})

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 80)
	WaitForIssueClosed(t, env, num, 80)

	if n := len(env.Sim.Log().ByMethod("RerunFailedJobs")); n != 1 {
		t.Errorf("RerunFailedJobs calls = %d, want exactly 1", n)
	}
	if n := env.Claude.CommentCallCount("Validate"); n != 0 {
		t.Errorf("CI-fix worker invocations = %d, want 0 (bodies: %v)", n, bodies())
	}
}

// Cancelled → re-run → cancelled again: a CI-fix reinvoke whose context says the
// check was cancelled (not failed) and that a fresh run is needed.
func TestCICancelled_RerunCancelledAgainDispatchesCIFixNamingCancellation(t *testing.T) {
	t.Parallel()
	env, num, _, bodies := cancelledGateEnv(t, cancelledGateOpts{rerunConclusion: "cancelled"})

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 120)
	WaitForIssueClosed(t, env, num, 120)

	if n := len(env.Sim.Log().ByMethod("RerunFailedJobs")); n != 1 {
		t.Errorf("RerunFailedJobs calls = %d, want exactly 1 (the budget is not re-spent)", n)
	}
	got := bodies()
	if len(got) < 1 {
		t.Fatal("no CI-fix worker was dispatched after the re-run was cancelled again")
	}
	for _, want := range []string{"cancelled", "fresh run is needed"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("CI-fix context missing %q:\n%s", want, got[0])
		}
	}
	for _, bad := range []string{"NEW REGRESSION", "Failed checks on PR branch"} {
		if strings.Contains(got[0], bad) {
			t.Errorf("CI-fix context must not frame a cancellation as a failure (%q):\n%s", bad, got[0])
		}
	}
}

// A refused re-run (no `actions` permission) never waits silently for
// CIBackstopTimeout: the CI-fix reinvoke dispatches at once.
func TestCICancelled_RerunForbiddenDispatchesCIFixImmediately(t *testing.T) {
	t.Parallel()
	env, num, _, bodies := cancelledGateEnv(t, cancelledGateOpts{rerunConclusion: "success", rerunForbidden: true})

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 120)
	WaitForIssueClosed(t, env, num, 120)

	got := bodies()
	if len(got) < 1 {
		t.Fatal("a forbidden re-run on a cancelled check must dispatch a CI-fix worker, not wait")
	}
	if !strings.Contains(got[0], "cancelled") {
		t.Errorf("CI-fix context should name the cancellation:\n%s", got[0])
	}
}

// Neutralised twin: with the re-run disabled, the cancelled head also goes
// straight to the CI-fix worker and re-runs nothing — proving the first test
// above owes its worker-free pass to the re-run reaching the cancelled check.
func TestCICancelled_Neutralised_DispatchesCIFixWorker(t *testing.T) {
	t.Parallel()
	env, num, _, bodies := cancelledGateEnv(t, cancelledGateOpts{rerunConclusion: "success"})
	env.Engine.SetCIFlakeRerunDisabledForTest(true)

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 120)
	WaitForIssueClosed(t, env, num, 120)

	if n := len(env.Sim.Log().ByMethod("RerunFailedJobs")); n != 0 {
		t.Errorf("RerunFailedJobs calls = %d, want 0 when neutralised", n)
	}
	if len(bodies()) < 1 {
		t.Error("CI-fix worker invocations = 0, want >= 1 when neutralised")
	}
}
