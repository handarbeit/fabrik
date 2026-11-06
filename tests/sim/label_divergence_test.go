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
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Sim twins for #2551 (ADR 2551): an engine label write that GitHub reported as
// successful but did not keep (simgh.DropLabelWrite). The cache-divergence
// detector (ADR 2170) sees the contradiction when a later listing is applied; the
// engine now confirms it with one live read and re-applies once (R1), and no
// advance trusts a completion label the engine withdrew for a rework (R2).
//
// Both twins act from inside the worker script, which runs synchronously on the
// worker goroutine, so the sequence is deterministic: the listing that contradicts
// the engine's write is a board reconcile (reconcileBoard), applied while the worker
// is in flight.

const simBotLock = "fabrik:locked:fabrik-sim-bot"

func labelDivergenceEnv(t *testing.T) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:     smokeStages(),
		Yolo:       boolPtr(false),
		StartTime:  time.Now(),
		BoardCache: true,
	})
	t.Cleanup(env.Engine.RegisterObservers())
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Engine.StartChannelEventsForTest(t.TempDir())
	t.Cleanup(env.Engine.StopChannelEventsForTest)
	return env
}

type lockProbe struct {
	mu                       sync.Mutex
	runs                     int
	before, after            []string
	liveReads, lockAdds      int
	repairDrained            bool
	lockAddsBefore, readsPre int
}

func lockAddCount(env *Env, num int) int {
	n := 0
	for _, en := range env.Sim.Log().ByMethod("AddLabelToIssue") {
		if en.Args.Number == num && en.Args.Label == simBotLock {
			n++
		}
	}
	return n
}

// runLockNotHeld files an item, drops the engine's lock-label add and, from inside
// the worker, applies a listing that contradicts the engine's write.
func runLockNotHeld(t *testing.T, repairOff bool) (*Env, *lockProbe, int) {
	t.Helper()
	env := labelDivergenceEnv(t)
	env.Engine.SetLabelRepairDisabledForTest(repairOff)
	env.Sim.Sim().DropLabelWrite(simgh.LabelEventLabeled, simBotLock, 1)
	probe := &lockProbe{}
	var num int
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		probe.runs++
		probe.before, _ = env.Sim.Sim().FetchLabels(env.Owner, env.Repo, issue.Number)
		readsBefore := env.Sim.Faults().CallCount("FetchLabels")
		addsBefore := lockAddCount(env, issue.Number)
		reconcileBoard(t, env) // the contradicting listing
		probe.repairDrained = env.Engine.DrainLabelRepairForTest(5 * time.Second)
		probe.after, _ = env.Sim.Sim().FetchLabels(env.Owner, env.Repo, issue.Number)
		probe.liveReads = env.Sim.Faults().CallCount("FetchLabels") - readsBefore
		probe.lockAdds = lockAddCount(env, issue.Number) - addsBefore
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})
	num = FileIssue(t, env, "lock add not held", "body", "Specify")
	AdvanceUntil(t, env, func(env *Env) bool {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		return probe.runs >= 1
	}, 40)
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
	return env, probe, num
}

// R1 / SC-001: the lock add did not stick; the engine confirms with one live read,
// re-applies once, and the worker runs with its lock.
func TestLabelDivergence_LockAddNotHeldIsReapplied(t *testing.T) {
	t.Parallel()
	_, probe, _ := runLockNotHeld(t, false)
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if hasLabel(probe.before, simBotLock) {
		t.Fatalf("the scenario did not drop the lock add: %v", probe.before)
	}
	if !probe.repairDrained {
		t.Fatal("the label repair did not drain")
	}
	if !hasLabel(probe.after, simBotLock) {
		t.Errorf("lock label not restored while the worker ran: %v", probe.after)
	}
	if probe.liveReads != 1 || probe.lockAdds != 1 {
		t.Errorf("repair made %d live read(s) and %d lock add(s), want exactly 1 and 1", probe.liveReads, probe.lockAdds)
	}
}

// Non-vacuity: with the repair neutralised the lock stays absent.
func TestLabelDivergence_LockAddNotHeld_NonVacuous(t *testing.T) {
	t.Parallel()
	_, probe, _ := runLockNotHeld(t, true)
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if hasLabel(probe.after, simBotLock) {
		t.Fatalf("lock restored with the repair neutralised — the twin proves nothing: %v", probe.after)
	}
	if probe.liveReads != 0 || probe.lockAdds != 0 {
		t.Errorf("neutralised repair still read %d / wrote %d", probe.liveReads, probe.lockAdds)
	}
}

// ---- complete-label removal not held during a rework ----

type reworkProbe struct {
	mu           sync.Mutex
	runs         int
	statusMid    string
	labelsMid    []string
	liveReads    int
	completeAdds int
}

// runCompleteRemovalNotHeld parks an item at Research with stage:Research:complete,
// then posts a human comment. The engine removes the completion label for the
// rework but GitHub keeps it (DropLabelWrite). From inside the comment worker the
// item is made auto-advancing (cruise), a contradicting listing is applied and a
// poll is run, exactly the window in which a stale completion label could advance
// the item.
func runCompleteRemovalNotHeld(t *testing.T, repairOff, trustOff bool) (*Env, *reworkProbe, int) {
	t.Helper()
	env := labelDivergenceEnv(t)
	env.Engine.SetLabelRepairDisabledForTest(repairOff)
	env.Engine.SetLabelTrustGateDisabledForTest(trustOff)
	complete := "stage:" + reworkStage + ":complete"
	num := FileIssue(t, env, "complete removal not held", "body", reworkStage, "stage:Specify:complete")
	WaitForIssueLabel(t, env, num, complete, 40)
	RunPolls(t, env, 3)

	probe := &reworkProbe{}
	env.Claude.ForStageComments(reworkStage, func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		probe.runs++
		if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, issue.Number, "fabrik:cruise"); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		// The feedback gate would hold any advance while the human comment is unprocessed,
		// which would hide the label problem. A rework with nothing pending (a CI-fix or
		// rebase reinvoke) has no such cover, so the comment is marked processed first.
		for _, c := range comments {
			if err := env.Sim.Sim().AddCommentReaction(env.Owner, env.Repo, c.DatabaseID, "ROCKET"); err != nil {
				return "", false, engine.TokenUsage{}, err
			}
		}
		readsBefore := env.Sim.Faults().CallCount("FetchLabels")
		reconcileBoard(t, env) // the listing still shows the completion label
		env.Engine.DrainLabelRepairForTest(5 * time.Second)
		if err := env.Engine.PollOnce(context.Background()); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		probe.statusMid = projectItem(t, env, issue.Number).Status
		probe.labelsMid, _ = env.Sim.Sim().FetchLabels(env.Owner, env.Repo, issue.Number)
		probe.liveReads = env.Sim.Faults().CallCount("FetchLabels") - readsBefore
		return simclaude.NoOpCommentReview()(ctx, stage, issue, comments, workDir, opts)
	})

	// The engine's removal of the completion label for the rework is the one write
	// GitHub does not keep.
	env.Sim.Sim().DropLabelWrite(simgh.LabelEventUnlabeled, complete, 1)
	env.Clock.Advance(time.Second)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", "please tighten the research findings")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	AdvanceUntil(t, env, func(env *Env) bool {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		return probe.runs >= 1
	}, 40)
	// Let the worker finish.
	RunPolls(t, env, 4)
	return env, probe, num
}

// R2 / SC-002: with the completion label still visible during the rework, the item
// is not advanced on it. With the repair active the removal is also restored.
func TestLabelDivergence_StaleCompleteLabelDoesNotAdvanceDuringRework(t *testing.T) {
	t.Parallel()
	_, probe, num := runCompleteRemovalNotHeld(t, true, false) // repair off: only the gate stands
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.runs != 1 {
		t.Fatalf("comment worker ran %d time(s), want 1", probe.runs)
	}
	if probe.statusMid != reworkStage {
		t.Errorf("#%d advanced to %q on a completion label the engine had withdrawn for the rework", num, probe.statusMid)
	}
}

// Non-vacuity: with the gate neutralised the item advances mid-rework.
func TestLabelDivergence_StaleCompleteLabel_NonVacuous(t *testing.T) {
	t.Parallel()
	env, probe, _ := runCompleteRemovalNotHeld(t, true, true)
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.statusMid == reworkStage {
		t.Fatalf("item did not advance with the gate neutralised — the twin proves nothing (labels %v)\n\n%s", probe.labelsMid, diagnostics(env))
	}
}

// R1 on the second shape: the engine confirms with one live read and re-applies the
// removal, so GitHub matches the rework.
func TestLabelDivergence_CompleteRemovalIsReappliedDuringRework(t *testing.T) {
	t.Parallel()
	_, probe, _ := runCompleteRemovalNotHeld(t, false, false)
	probe.mu.Lock()
	defer probe.mu.Unlock()
	complete := "stage:" + reworkStage + ":complete"
	if hasLabel(probe.labelsMid, complete) {
		t.Errorf("completion label still present mid-rework after the repair: %v", probe.labelsMid)
	}
	if probe.liveReads < 1 {
		t.Errorf("no live label read was made to confirm the divergence")
	}
}
