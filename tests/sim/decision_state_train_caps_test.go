package sim

import (
	"testing"

	"github.com/handarbeit/fabrik/engine"
)

// Restart coverage for the merge-train ejection cap (#2349, ADR 2349). The count used to
// reset on every restart, so a frequently restarted daemon never reached the cap that pauses
// a member that keeps getting ejected.

// TestMergeTrainEjection_CapHoldsAcrossRestart is TestMergeTrainEjection_MaxEjectionsPausesMember
// with a daemon restart between the two ejections: the second ejection must still trip the
// cap (MaxMergeTrainEjections=2).
func TestMergeTrainEjection_CapHoldsAcrossRestart(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{
		ConfigureCfg: func(cfg *engine.Config) { cfg.MaxMergeTrainEjections = 2 },
	})
	EnableDecisionState(t, env)

	numPoison, _ := QueueMember(t, env, "ejectrestart-poison", map[string]string{"poison.txt": "p\n"})
	numA, _ := QueueMember(t, env, "ejectrestart-a", map[string]string{"a.txt": "a\n"})
	stop := startTrialVerdictSeeder(t, env, poisonVerdict(numPoison))

	RunPoll(t, env)
	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numPoison, "Queued", 10)
	if hasLabel(IssueLabels(t, env, numPoison), "fabrik:paused") {
		t.Fatalf("#%d paused after only 1 of 2 ejections", numPoison)
	}
	stop()

	// The restart: in-memory state is gone; the count must come back from disk.
	env2 := restartMergeTrainEnv(t, env)
	resyncIssueSeqAfterEngineActivity(t, env2)
	numB, _ := QueueMember(t, env2, "ejectrestart-b", map[string]string{"b.txt": "b\n"})
	startTrialVerdictSeeder(t, env2, poisonVerdict(numPoison))

	RunPoll(t, env2)
	WaitForProjectStatus(t, env2, numB, "Done", 20)
	WaitForIssueLabel(t, env2, numPoison, "fabrik:paused", 10)
	if !hasCommentContaining(t, env2, numPoison, "ejected from the merge-train 2 consecutive times") {
		t.Errorf("expected the pause comment naming 2 ejections after the restart, got: %v", commentsOn(t, env2, numPoison))
	}
}
