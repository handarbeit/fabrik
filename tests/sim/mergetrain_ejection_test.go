package sim

import (
	"testing"

	"github.com/handarbeit/fabrik/engine"
)

// This file covers R5's ejection half: comment wording per cause, the
// MaxMergeTrainEjections pause-after-N guard, and the "no scenario ends
// with a member in Queued without a comment" invariant. The runaway-guard
// half of R5 is mergetrain_runaway_test.go.

// TestMergeTrainEjection_MaxEjectionsPausesMember exercises ejectMember's
// shared mergeTrainEjectionCounts guard (D-a) across two successive,
// independently-formed trains — not two bisection rounds within one worker
// dispatch (the re-form loop lands survivors and completes in a single poll
// once isolated, so a batch can only re-eject the same member across
// separate poll cycles once fresh Queued partners exist for it to be batched
// with again; a lone re-Queued poisoner would instead hit the red-singleton
// disposition, ejectRedSingleton, which never touches this counter at all —
// see mergetrain_redsingleton_test.go). MaxMergeTrainEjections is configured
// to 2 so the guard trips on the second ejection rather than the production
// default of 3, keeping this test to two polls.
func TestMergeTrainEjection_MaxEjectionsPausesMember(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{
		ConfigureCfg: func(cfg *engine.Config) { cfg.MaxMergeTrainEjections = 2 },
	})

	numPoison, _ := QueueMember(t, env, "ejectcount-poison", map[string]string{"poison.txt": "p\n"})
	numA, _ := QueueMember(t, env, "ejectcount-a", map[string]string{"a.txt": "a\n"})
	startTrialVerdictSeeder(t, env, poisonVerdict(numPoison))

	RunPoll(t, env)

	// First ejection: not yet at the cap, so the member stays Queued,
	// eligible for a future train, with the plain per-cause ejection
	// comment — no pause yet.
	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numPoison, "Queued", 10)
	if !hasCommentContaining(t, env, numPoison, "ejected from merge-train") {
		t.Fatalf("expected an ejection comment on #%d after the first ejection", numPoison)
	}
	if hasLabel(IssueLabels(t, env, numPoison), "fabrik:paused") {
		t.Fatalf("expected #%d not yet paused after only 1 of 2 ejections", numPoison)
	}

	// A fresh Queued partner for the second round — without one, the
	// poisoner would be alone and hit the red-singleton disposition instead
	// (a different code path, not this guard). resyncIssueSeqAfterEngineActivity
	// is required here: round 1's own trial/landing PRs auto-assigned numbers
	// the test's own reservation counter has no visibility into.
	resyncIssueSeqAfterEngineActivity(t, env)
	numB, _ := QueueMember(t, env, "ejectcount-b", map[string]string{"b.txt": "b\n"})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numB, "Done", 20)
	WaitForIssueLabel(t, env, numPoison, "fabrik:paused", 10)
	WaitForIssueLabel(t, env, numPoison, "fabrik:awaiting-input", 5)
	if !hasCommentContaining(t, env, numPoison, "ejected from the merge-train 2 consecutive times") {
		t.Errorf("expected the pause-after-N comment to name the ejection count, got comments: %v", commentsOn(t, env, numPoison))
	}
	if !hasCommentContaining(t, env, numPoison, "Manual intervention is required") {
		t.Errorf("expected the pause comment to explain manual intervention is required")
	}

	// Non-vacuity (AC10): a broken guard that never actually compares against
	// MaxMergeTrainEjections (e.g. always incrementing but never checking, or
	// checking against the wrong repo/issue key) would leave #%d ejected-but-
	// unpaused here even after 2 ejections — the label assertions above catch
	// that directly.
}

// TestMergeTrainEjection_NoQueuedMemberWithoutComment is R5's "never
// silently dropped" invariant, asserted directly rather than only implied by
// other scenarios' individual comment checks: after a batch with one
// isolable poisoner settles, every member left in the Queued column — not
// just the specific one a given scenario happened to check — carries at
// least one explanatory comment. This mirrors the poison-matrix scenarios'
// own per-member checks (mergetrain_assembly_test.go) but sweeps the whole
// board rather than one named issue, so a regression that silently drops the
// comment for a *different* ejection cause than the ones already covered
// there would still be caught here.
func TestMergeTrainEjection_NoQueuedMemberWithoutComment(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	numA, _ := QueueMember(t, env, "sweep-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "sweep-b", map[string]string{"b.txt": "b\n"})
	numC, _ := QueueMember(t, env, "sweep-c", map[string]string{"c.txt": "c\n"})
	startTrialVerdictSeeder(t, env, poisonVerdict(numB))

	RunPoll(t, env)
	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numC, "Done", 10)
	WaitForProjectStatus(t, env, numB, "Queued", 10)

	for _, n := range []int{numA, numB, numC} {
		item := projectItem(t, env, n)
		if item.Status != "Queued" {
			continue // landed members are out of scope for this invariant
		}
		if len(commentsOn(t, env, n)) == 0 {
			t.Errorf("#%d is left in Queued with zero comments — no explanation for why it's still there", n)
		}
	}
}
