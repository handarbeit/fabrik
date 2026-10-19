package sim

import (
	"errors"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/stages"
)

// #2396: a start whose first probe fails but whose board fetch succeeds must not
// leave the board cache serving an empty project ID. Before the fix every later
// advance sent UpdateProjectItemStatus with "" and failed until a restart.

func projectIDStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Plan", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

// statusWriteIDs returns the project ID of every UpdateProjectItemStatus call
// the engine made, in order.
func statusWriteIDs(env *Env) []string {
	var ids []string
	for _, e := range env.Sim.Log().ByMethod("UpdateProjectItemStatus") {
		ids = append(ids, e.Args.ID)
	}
	return ids
}

// runDegradedStart drives the incident: first probe fails, the board fetch
// succeeds and the deep fetch fills the store, then later polls advance the item.
func runDegradedStart(t *testing.T, neutralise bool) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:                    projectIDStages(),
		Yolo:                      boolPtr(true),
		StartTime:                 time.Now(),
		BoardCache:                true,
		AllowEmptyProjectIDWrites: neutralise,
	})
	if neutralise {
		env.BoardCache.SetProjectIDRecordingDisabledForTest(true)
		env.BoardCache.SetEmptyIDBoardGuardDisabledForTest(true)
		env.Engine.SetProjectIDWriteGuardDisabledForTest(true)
	}
	t.Cleanup(env.Engine.RegisterObservers())
	env.Sim.Faults().FailOnce("ProbeProjectBoard", errors.New("host unreachable"))
	FileIssue(t, env, "degraded start (#2396)", "body", "Specify")
	RunPolls(t, env, 8)
	return env
}

func TestProjectID_DegradedStartStillAdvancesWithRealID(t *testing.T) {
	t.Parallel()
	env := runDegradedStart(t, false)
	ids := statusWriteIDs(env)
	if len(ids) == 0 {
		t.Fatalf("no Status update was made: the scenario no longer reaches an advance, so a pass would be vacuous\n\n%s", diagnostics(env))
	}
	for i, id := range ids {
		if id == "" {
			t.Errorf("Status update %d carried an empty project ID", i)
		}
	}
	if env.BoardCache.ProjectID() == "" {
		t.Error("the cache never learned the project ID")
	}
}

// Non-vacuity: with the fix neutralised the same scenario sends the empty ID.
func TestProjectID_DegradedStartReproducesEmptyWhenFixNeutralised(t *testing.T) {
	t.Parallel()
	env := runDegradedStart(t, true)
	empty := 0
	for _, id := range statusWriteIDs(env) {
		if id == "" {
			empty++
		}
	}
	if empty == 0 {
		t.Fatalf("neutralised run made no empty-ID write: the twin cannot tell the fix from the old behaviour\n\n%s", diagnostics(env))
	}
}
