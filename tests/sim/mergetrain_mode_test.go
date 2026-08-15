package sim

import "testing"

// This file covers R8/AC8: both FABRIK_MERGE_TRAIN modes run in the same
// suite invocation. In sim, cfg.MergeTrain is construction-time
// configuration (mergeTrainEnvOptions.Mode, plumbed straight through to
// engine.Config — see mergeTrainEnv), not a bed restart, so there is no
// scripts/e2e/run.sh-style two-pass dance to reproduce: TestMergeTrainMode_On
// and TestMergeTrainMode_Off below run as ordinary parallel subtests of one
// `go test` invocation, each with its own independent *Env/*Engine.
//
// TestMergeTrainMode_On is a short positive control: without it,
// TestMergeTrainMode_Off "passing" because merge-train is broken entirely
// would be indistinguishable from it passing because MergeTrain:"off"
// correctly disables it — the exact non-vacuity shape AC10 asks every
// scenario to demonstrate.

// TestMergeTrainMode_Off proves handleMergeTrainBatch's own
// `e.cfg.MergeTrain == "on"` gate (engine/poll.go): a member placed at
// Queued is never drained — no draft CI PR opens, no board-status move ever
// happens, across several polls — because the merge-train worker is never
// dispatched for this engine at all.
func TestMergeTrainMode_Off(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{Mode: "off"})
	startTrialVerdictSeeder(t, env, allGreenVerdict) // never consulted — no trial PR is ever opened for it to react to

	num, _ := QueueMember(t, env, "mode-off", map[string]string{"off.txt": "off\n"})

	RunPolls(t, env, 5)

	if got := projectItem(t, env, num).Status; got != "Queued" {
		t.Errorf("status = %q after 5 polls with merge_train off, want Queued (undrained)", got)
	}
	if got := draftPRCount(env); got != 0 {
		t.Errorf("CreateDraftPR called %d time(s) with merge_train off, want 0 — no trial should ever be assembled", got)
	}
	if projectItem(t, env, num).IsClosed {
		t.Error("member unexpectedly closed with merge_train off")
	}
}

// TestMergeTrainMode_On is the positive control for TestMergeTrainMode_Off's
// non-vacuity: the identical single-member setup, but with merge_train on,
// lands normally within the same poll — proving the "off" scenario's
// undrained-Queued result is actually the mode gate at work, not some
// unrelated breakage that would leave a member stuck in Queued regardless of
// the setting.
func TestMergeTrainMode_On(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{Mode: "on"})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	num, _ := QueueMember(t, env, "mode-on", map[string]string{"on.txt": "on\n"})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, num, "Done", 20)
	WaitForIssueClosed(t, env, num, 5)
}
