package sim

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// This file is the sim twin of #2045 / ADR-2045 (red-singleton auto-repair): a lone Queued
// member whose combined Validate is red because of itself is no longer paused for a human;
// the engine re-enters Validate itself with the train's diagnostic, and once that Validate
// completes the member re-queues and lands.

// redOnceVerdict reds the first trial PR it is asked about and greens every later one — the
// base moved, the member was repaired, and the retried trial passes.
func redOnceVerdict() func(members []int) []gh.CheckRun {
	var n atomic.Int32
	return func(members []int) []gh.CheckRun {
		if n.Add(1) == 1 {
			return []gh.CheckRun{redCheckRun("ci/build")}
		}
		return []gh.CheckRun{greenCheckRun("")}
	}
}

func autoRepairEnv(t *testing.T, capN int) *Env {
	t.Helper()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{
		// wait_for_ci on Validate is the production shape: the repaired member's Validate
		// completion is landed by the catch-up loop (advance to Queued) rather than falling
		// through to advanceToNextStage. The clock starts at real now so the awaiting-ci
		// backstop does not fire on the first settle pass.
		ValidateWaitForCI: true,
		StartTime:         time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxTrainAutoRepairAttempts = capN
			for _, s := range cfg.Stages {
				if s.Name == "Validate" {
					s.Prompt = "validate"
				}
			}
		},
	})
	// Stands in for the cache write-through and its reactive observers, neither of which
	// tests/sim wires in — see cacheWriteThrough.
	t.Cleanup(env.Engine.RegisterObservers())
	return env
}

// seedValidatedMember queues a member as the pipeline would have left it: Validate complete,
// yolo (so a repaired Validate completion advances to Queued again).
func seedValidatedMember(t *testing.T, env *Env, marker string) int {
	t.Helper()
	num, _ := QueueMember(t, env, marker, map[string]string{marker + ".txt": marker + "\n"})
	for _, l := range []string{"stage:Specify:complete", "stage:Research:complete", "stage:Plan:complete", "stage:Implement:complete", "stage:Review:complete", "stage:Validate:complete", "fabrik:yolo"} {
		if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, l); err != nil {
			t.Fatalf("AddLabelToIssue(%s): %v", l, err)
		}
	}
	return num
}

// recordRepairContext scripts Validate: it records the repair context file the dispatch
// received (if any) and then behaves like the default script.
func recordRepairContext(env *Env) (get func() (string, bool)) {
	var (
		mu      sync.Mutex
		content string
		present bool
	)
	env.Claude.ForStage("Validate", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		data, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", "merge-train-repair.md"))
		mu.Lock()
		content, present = string(data), err == nil
		mu.Unlock()
		return simclaude.DefaultScript(ctx, stage, issue, newComments, resume, workDir, opts)
	})
	return func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return content, present
	}
}

func TestMergeTrainAutoRepair_RedSingletonRepairsThenLands(t *testing.T) {
	t.Parallel()
	env := autoRepairEnv(t, 1)
	num := seedValidatedMember(t, env, "autorepair")
	repairContext := recordRepairContext(env)
	startTrialVerdictSeeder(t, env, redOnceVerdict())

	RunPoll(t, env)

	// The red trial re-entered Validate (repair) instead of pausing. The production cache's
	// status write-through is what admits the rerouted member to the next poll.
	if got := projectItem(t, env, num).Status; got != "Validate" {
		t.Fatalf("expected #%d rerouted to Validate by the auto-repair, got %q", num, got)
	}
	cacheWriteThrough(env, num, "Validate")

	// The repaired member's Validate completes, it re-queues, and it lands.
	WaitForProjectStatus(t, env, num, "Done", 120)
	WaitForIssueClosed(t, env, num, 20)

	if got := env.Claude.StageCallCount("Validate"); got != 1 {
		t.Errorf("expected exactly one repair Validate dispatch, got %d", got)
	}
	content, present := repairContext()
	if !present {
		t.Fatal("the repair's Validate dispatch must receive .fabrik-context/merge-train-repair.md")
	}
	for _, want := range []string{"Merge-train repair", "ci/build", "Never revert"} {
		if !strings.Contains(content, want) {
			t.Errorf("repair context missing %q:\n%s", want, content)
		}
	}
	if !hasCommentContaining(t, env, num, "auto-repair started") {
		t.Error("expected the auto-repair comment")
	}
	// Never paused for a human along the way.
	for _, e := range env.Sim.Log().ByMethod("AddLabelToIssue") {
		if e.Args.Number == num && len(e.Args.Values) > 0 && e.Args.Values[0] == "fabrik:paused" {
			t.Errorf("auto-repair must not pause the member, got %+v", e)
		}
	}
}

// Neutralisation: with the cap at 0 the same scenario pauses exactly as ADR-1545 did and
// Validate is never dispatched.
func TestMergeTrainAutoRepair_DisabledPausesInstead(t *testing.T) {
	t.Parallel()
	env := autoRepairEnv(t, 0)
	num := seedValidatedMember(t, env, "autorepair-off")
	recordRepairContext(env)
	startTrialVerdictSeeder(t, env, redOnceVerdict())

	RunPoll(t, env)

	WaitForProjectStatus(t, env, num, "Validate", 20)
	WaitForIssueLabel(t, env, num, "fabrik:paused", 5)
	RunPolls(t, env, 3)
	if got := env.Claude.StageCallCount("Validate"); got != 0 {
		t.Errorf("with auto-repair disabled Validate must not be dispatched, got %d", got)
	}
	if hasCommentContaining(t, env, num, "auto-repair started") {
		t.Error("no auto-repair comment when disabled")
	}
}
