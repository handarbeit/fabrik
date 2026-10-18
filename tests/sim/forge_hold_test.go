package sim

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
	"github.com/handarbeit/fabrik/warnings"
)

// Sim twin of #2393 (ADR 2393): the engine must not hand a ready item to the
// model when the lock cannot be written (R1) or while GitHub is unreachable (R3).
//
// The seed is #2388, where an unreachable GitHub Enterprise host made one
// Research stage run about every eight minutes for ~21 hours: each run's lock
// write timed out, the worker spawned unlocked, and its results could not be
// recorded, so the next poll saw an untouched item.
//
// Not parallel: it redirects the process-wide warnings file to read the
// operator-warning panel back (see display_field_warnings_test.go).

func forgeHoldStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

func forgeHoldEnv(t *testing.T, cache bool) *Env {
	t.Helper()
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	return NewEnv(t, EnvOptions{
		Stages:     forgeHoldStages(),
		ForgeHold:  true,
		BoardCache: cache,
		StartTime:  time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.RetryBackoff = time.Millisecond // let a (wrongly) unlocked run re-dispatch quickly
		},
	})
}

// pollTolerant is RunPoll for an outage: the poll may legitimately report an
// error when its own reads fail, which is not what these scenarios assert on.
func pollTolerant(t *testing.T, env *Env, advance time.Duration) {
	t.Helper()
	env.Clock.Advance(advance)
	_ = env.Engine.PollOnce(context.Background())
	if env.Engine.HasInFlightWorker() {
		waitForWorkerQuiescence(t, env)
	} else {
		time.Sleep(workerYield)
	}
}

func panelHas(t *testing.T, key string) bool {
	t.Helper()
	es, err := warnings.Load()
	if err != nil {
		t.Fatalf("warnings.Load: %v", err)
	}
	for _, e := range es {
		if e.Key == key {
			return true
		}
	}
	return false
}

// runLockWriteOutage fails every write with the #2388 error while reads work and
// reports how many times the model was handed the ready item. neutralise
// restores the pre-#2393 behaviour of spawning the worker unlocked.
func runLockWriteOutage(t *testing.T, neutralise bool) (calls, lockAttempts int, env *Env, num int) {
	t.Helper()
	env = forgeHoldEnv(t, false)
	if neutralise {
		env.Engine.SetLockWriteRefusalDisabledForTest(true)
		env.Engine.SetForgeHoldDisabledForTest(true)
	}
	num = FileIssue(t, env, "lock write outage (#2393)", "body", "Specify")
	env.Sim.Faults().FailAllWrites(simgh.ErrDeadlineExceeded())

	for i := 0; i < 12; i++ {
		pollTolerant(t, env, 2*time.Second)
	}
	return env.Claude.StageCallCount("Specify"), env.Sim.Faults().CallCount("AddLabelToIssue"), env, num
}

// R1/R2 + acceptance: with every write failing, the model is never handed a ready
// item, however long it stays ready; the item is retried (so the scenario is not
// vacuous), and dispatch resumes once the fault clears.
func TestForgeHold_Sim_FailedLockWriteSpawnsNoWorker(t *testing.T) {
	calls, lockAttempts, env, num := runLockWriteOutage(t, false)
	if calls != 0 {
		t.Fatalf("the model was handed the item %d time(s) while every write failed, want 0\n\n%s", calls, diagnostics(env))
	}
	if lockAttempts < 3 {
		t.Fatalf("lock write attempted %d time(s); the item must be retried by its admission cooldown (scenario would be vacuous)", lockAttempts)
	}
	if env.Engine.ForgeHeldForTest() {
		t.Error("writes-only failure must not engage the forge hold: reads still answer")
	}
	labels, err := env.Sim.Sim().FetchLabels(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	for _, l := range labels {
		if l == "stage:Specify:in_progress" || l == "stage:Specify:failed" {
			t.Errorf("unexpected label %q: a refused dispatch must record nothing", l)
		}
	}

	env.Sim.Faults().ClearBlanket()
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 40)
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
}

// Neutralising R1 and R3 re-creates #2388: the unlocked worker runs again and
// again although nothing it does can be recorded.
func TestForgeHold_Sim_NeutralisedRefusalRedispatches(t *testing.T) {
	calls, _, env, _ := runLockWriteOutage(t, true)
	if calls < 2 {
		t.Fatalf("with R1 and R3 neutralised the item was handed to the model %d time(s); want a re-dispatch (>= 2) — the scenario no longer reproduces #2388\n\n%s", calls, diagnostics(env))
	}
}

// runForgeOutage lets the first run start normally and takes GitHub away while it
// works (the model API stays up), so nothing the run produces can be recorded.
// R1 is neutralised so that only the account-wide hold (R3) stands between the
// item and a re-dispatch. Returns the model calls made before and after the hold
// engaged (or, neutralised, after the same number of polls).
func runForgeOutage(t *testing.T, neutraliseHold bool) (before, after int, env *Env) {
	t.Helper()
	env = forgeHoldEnv(t, true)
	env.Engine.SetLockWriteRefusalDisabledForTest(true)
	if neutraliseHold {
		env.Engine.SetForgeHoldDisabledForTest(true)
	}
	var down atomic.Bool
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, c []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if down.CompareAndSwap(false, true) {
			// The forge goes dark after the lock was taken: this run's output
			// cannot be posted and its completion label cannot be written.
			env.Sim.Faults().FailAll(simgh.ErrDeadlineExceeded())
		}
		return simclaude.DefaultScript(ctx, stage, issue, c, resume, workDir, opts)
	})
	FileIssue(t, env, "forge outage (#2393)", "body", "Specify")

	for i := 0; i < 40 && !down.Load(); i++ {
		RunPoll(t, env)
	}
	if !down.Load() {
		t.Fatalf("setup: the first run never started\n\n%s", diagnostics(env))
	}
	before = env.Claude.StageCallCount("Specify")

	// Several polls spanning well over the hold window, all failing on transport errors.
	for i := 0; i < 12; i++ {
		pollTolerant(t, env, 30*time.Second)
	}
	return before, env.Claude.StageCallCount("Specify") - before, env
}

// R3 + R4: a sustained transport outage engages the account-wide hold; no model
// call starts after it, the operator warning and health block appear once, and
// everything resolves when the forge answers again.
func TestForgeHold_Sim_HoldStopsRedispatchAndResumes(t *testing.T) {
	before, after, env := runForgeOutage(t, false)
	if before != 1 {
		t.Fatalf("setup: %d model call(s) before the outage, want 1", before)
	}
	if !env.Engine.ForgeHeldForTest() {
		t.Fatalf("the forge hold never engaged after a sustained outage\n\n%s", diagnostics(env))
	}
	if after != 0 {
		t.Fatalf("%d model call(s) started after the outage began, want 0 — the hold must stop re-dispatch\n\n%s", after, diagnostics(env))
	}
	if !panelHas(t, "forge_unreachable:github") {
		t.Error("no forge_unreachable operator warning recorded")
	}

	env.Sim.Faults().ClearBlanket()
	AdvanceUntil(t, env, func(env *Env) bool { return !env.Engine.ForgeHeldForTest() }, 10)
	if panelHas(t, "forge_unreachable:github") {
		t.Error("forge_unreachable warning not resolved after the forge answered")
	}
}

// Neutralised, the same outage re-dispatches (the #2388 loop).
func TestForgeHold_Sim_NeutralisedHoldRedispatches(t *testing.T) {
	_, after, env := runForgeOutage(t, true)
	if after < 1 {
		t.Fatalf("with the hold neutralised no re-dispatch happened during the outage — the scenario does not reproduce #2388\n\n%s", diagnostics(env))
	}
}
