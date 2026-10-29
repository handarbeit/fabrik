package sim

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #2490 (ADR 2490): review, CI-fix and rebase rounds have an optional reserved
// worker slot on top of max_concurrent, so stage workers holding the whole general
// pool can no longer queue a short fix round behind long stage runs. Stage runs
// never use it. Neutralising the reservation (SC-005) makes the same scenario wait.

type fixSlotReading struct{ fixInUse, fixMax, general int }

// fixRoundSlotScenario reaches a Validate item with fabrik:awaiting-ci, then holds
// every general slot and seeds a failing check so the engine dispatches a CI-fix
// reinvoke. It returns the env, the issue number, the recorded slot readings at the
// instant the reinvoke's Claude call ran, and the release of the held general pool.
func fixRoundSlotScenario(t *testing.T, neutralise bool) (env *Env, num int, readings func() []fixSlotReading, release func()) {
	t.Helper()
	env = NewEnv(t, EnvOptions{
		Stages:    ciFixStages(),
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = 5
			cfg.FixRoundSlots = 1
		},
	})
	// Production (New) builds the reserved slots from cfg.FixRoundSlots;
	// NewWithDeps does not by default, so enable them to match.
	env.Engine.EnableFixRoundSlotsForTest(1)
	env.Engine.SetFixRoundReservationDisabledForTest(neutralise)

	var mu sync.Mutex
	var seen []fixSlotReading
	fix := ciFixCommentScript(env.Sim, env.OwnerRepo, "success")
	env.Claude.ForStageComments("Validate", simclaude.CommentScript(func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		in, max := env.Engine.FixRoundSlotUsageForTest()
		g, _, _ := env.Engine.WorkerSlotUsageForTest()
		mu.Lock()
		seen = append(seen, fixSlotReading{in, max, g})
		mu.Unlock()
		return fix(ctx, stage, issue, comments, workDir, opts)
	}))
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	num = FileIssue(t, env, "sim fix-round slot", "A CI-fix round while stage workers hold every slot.", "Implement")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil || pr.Number == 0 {
		t.Fatalf("expected a linked PR: %v", err)
	}

	// The engine's PR took the next number of the shared issue/PR sequence; step
	// the Env's issue counter past it so a later FileIssue does not collide.
	env.issueSeqMu.Lock()
	if env.issueSeqNext < pr.Number {
		env.issueSeqNext = pr.Number
	}
	env.issueSeqMu.Unlock()

	// Every general slot is now held, as a saturated pool of long Implement runs.
	release = env.Engine.HoldGeneralWorkerSlotsForTest()
	t.Cleanup(release)
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "failure", OutputSummary: "fix-round-slot failure"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCheckRun: %v", err)
	}
	readings = func() []fixSlotReading {
		mu.Lock()
		defer mu.Unlock()
		return append([]fixSlotReading(nil), seen...)
	}
	return env, num, readings, release
}

// With every general slot held, a CI-fix round starts at once on the reserved slot;
// a stage run dispatched in the same state does not start.
func TestFixRoundSlot_CIFixRoundRunsWhileGeneralPoolSaturated(t *testing.T) {
	t.Parallel()
	env, _, readings, release := fixRoundSlotScenario(t, false)
	base := env.Claude.CommentCallCount("Validate")

	// A stage run for a new item in the same saturated state (the Implement stage
	// has no slot to take, and the reserved one is fix-round-only).
	stageNum := FileIssue(t, env, "sim fix-round slot stage run", "A stage run must not use the reserved slot.", "Implement")
	implBase := env.Claude.StageCallCount("Implement")

	AdvanceUntil(t, env, func(env *Env) bool {
		return env.Claude.CommentCallCount("Validate") > base
	}, 40)

	got := readings()
	if len(got) == 0 {
		t.Fatal("the CI-fix round never ran while the general pool was held")
	}
	for _, r := range got {
		if r.fixMax != 1 || r.fixInUse != 1 {
			t.Errorf("during the CI-fix call: reserved slot %d/%d, want 1/1", r.fixInUse, r.fixMax)
		}
	}
	if n := env.Claude.StageCallCount("Implement"); n != implBase {
		t.Errorf("a stage run started on #%d while the general pool was held (Implement calls %d -> %d)", stageNum, implBase, n)
	}

	// Releasing the general pool lets the stage run proceed, and the reserved slot
	// is free again once the round is done.
	release()
	AdvanceUntil(t, env, func(env *Env) bool {
		in, _ := env.Engine.FixRoundSlotUsageForTest()
		return env.Claude.StageCallCount("Implement") > implBase && in == 0
	}, 80)
}

// Neutralising the reservation (SC-005): the same scenario shows the CI-fix round
// waiting behind the full general pool, and starting only once a slot is released.
func TestFixRoundSlot_NeutralisedReservationMakesTheRoundWait(t *testing.T) {
	t.Parallel()
	env, _, readings, release := fixRoundSlotScenario(t, true)
	base := env.Claude.CommentCallCount("Validate")

	// PollOnce directly, not RunPoll: RunPoll waits for every dispatched worker to
	// finish, and the queued round is exactly the in-flight worker that cannot.
	for i := 0; i < 12; i++ {
		env.Clock.Advance(env.PollInterval)
		if err := env.Engine.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !env.Engine.HasInFlightWorker() {
		t.Fatal("expected the CI-fix round to be dispatched and queued for a slot")
	}
	if n := env.Claude.CommentCallCount("Validate"); n != base {
		t.Fatalf("CI-fix round started (%d -> %d) with the general pool held and the reservation neutralised", base, n)
	}
	if len(readings()) != 0 {
		t.Fatal("the CI-fix script ran while the round should still be queued")
	}

	release()
	AdvanceUntil(t, env, func(env *Env) bool {
		return env.Claude.CommentCallCount("Validate") > base
	}, 40)
}
