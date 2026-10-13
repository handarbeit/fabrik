package sim

import (
	"context"
	"sync"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #2115 (ADR 2115): the merge train has one worker slot of its own on top of
// max_concurrent, so stage workers holding the whole general pool can no longer starve
// a train conflict resolution.

type slotReading struct{ general, train, trainMax int }

// recordingResolver wraps claudeResolveConflict, recording worker-slot occupancy at the
// instant the train's Claude call is running.
func recordingResolver(env *Env, path, content string) (simclaude.CommentScript, func() []slotReading) {
	var mu sync.Mutex
	var seen []slotReading
	inner := claudeResolveConflict(path, content)
	script := func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		g, tr, trMax := env.Engine.WorkerSlotUsageForTest()
		mu.Lock()
		seen = append(seen, slotReading{g, tr, trMax})
		mu.Unlock()
		return inner(ctx, stage, issue, comments, workDir, opts)
	}
	return script, func() []slotReading {
		mu.Lock()
		defer mu.Unlock()
		return append([]slotReading(nil), seen...)
	}
}

// With every general slot held by stage workers, a conflicting member is resolved by
// the train in the reserved slot and the batch lands without waiting for any stage
// worker to finish.
func TestMergeTrainReservedSlot_ConflictResolvesWhileGeneralPoolSaturated(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	startTrialVerdictSeeder(t, env, allGreenVerdict)
	script, readings := recordingResolver(env, "shared-slot.txt", "resolved\n")
	env.Claude.ForStageComments("Queued", script)

	numA, _ := QueueMember(t, env, "slot-a", map[string]string{"shared-slot.txt": "content-from-a\n"})
	numB, _ := QueueMember(t, env, "slot-b", map[string]string{"shared-slot.txt": "content-from-b\n"})

	release := env.Engine.HoldGeneralWorkerSlotsForTest()
	defer release()

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 20)

	got := readings()
	if len(got) == 0 {
		t.Fatal("the train's conflict resolution never ran while the general pool was held")
	}
	for _, r := range got {
		if r.trainMax != 1 || r.train != 1 {
			t.Errorf("during the train's Claude call: reserved slot %d/%d, want 1/1", r.train, r.trainMax)
		}
	}
	if g, tr, _ := env.Engine.WorkerSlotUsageForTest(); tr != 0 {
		t.Errorf("reserved slot still held after the episode (train=%d general=%d)", tr, g)
	}
}

// With merge_train off there is no reserved slot: the pool is exactly max_concurrent.
func TestMergeTrainReservedSlot_AbsentWhenMergeTrainOff(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{Mode: "off"})
	if _, _, trainMax := env.Engine.WorkerSlotUsageForTest(); trainMax != 0 {
		t.Errorf("reserved slot capacity = %d with merge_train off, want 0", trainMax)
	}
}
