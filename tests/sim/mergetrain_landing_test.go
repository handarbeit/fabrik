package sim

import (
	"errors"
	"testing"

	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// This file covers R6: both merge-train landing paths (landOneAtATime /
// landSingleton, and landMergeTrainBatch), plus the known asymmetry between
// them — landSingleton's failed member-issue close gets a durable
// fabrik:awaiting-member-close retry/escalation arc (ADR-061); landMergeTrainBatch's
// identically-shaped failure does not (ADR-061's own deferred-follow-up note,
// confirmed directly against engine/merge_train.go: landMergeTrainBatch's
// final CloseIssue call only logs a warning — it never calls
// markMergeTrainMemberCloseOutstanding).

var errInjectedLandingFault = errors.New("simgh: injected landing fault")

// TestMergeTrainLanding_OneAtATimeViaInteractionOnlyPoison exercises both
// landOneAtATime and landSingleton on the real path: a two-member batch
// where the poison exists only in combination (mirrors
// TestMergeTrainAssembly_PoisonMatrix's "member poisonous only in
// combination" subtest, which already proves the fallback triggers — this
// scenario is the R6-focused counterpart, asserting the landing mechanics
// specifically: each member gets its own dedicated singleton landing PR,
// and landSingleton's marker-free PR body means neither collides with
// findIntegrationPR — both members land independently in the same worker
// dispatch).
//
// landSingleton's own fabrik:awaiting-member-close retry/escalation arc
// (ADR-061) is covered by the existing unit-adjacent sim test
// TestSettleScan_AwaitingMemberClose (settle_scan_escalation_test.go) — not
// duplicated here; this file only proves the real landOneAtATime/
// landSingleton mechanics run correctly end to end against real git.
func TestMergeTrainLanding_OneAtATimeViaInteractionOnlyPoison(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	numA, _ := QueueMember(t, env, "landing-interaction-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "landing-interaction-b", map[string]string{"b.txt": "b\n"})
	startTrialVerdictSeeder(t, env, interactionPoisonVerdict(numA, numB))

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 10)
	WaitForIssueClosed(t, env, numA, 5)
	WaitForIssueClosed(t, env, numB, 5)

	// Each landed via its own singleton PR, not a shared combined-batch
	// landing PR — landSingleton's whole reason for a marker-free PR body.
	if got := len(env.Sim.Log().ByMethod("CreatePR")); got < 2 {
		t.Errorf("expected at least 2 singleton landing PRs (one per member), got %d CreatePR call(s)", got)
	}
}

// TestMergeTrainLanding_BatchCloseAsymmetryPin is R6's second half: pinning
// the as-found asymmetry, not fixing it. A real green two-member batch
// lands via landMergeTrainBatch; the final member-issue CloseIssue call for
// one member is made to fail. Unlike landSingleton's equivalent failure
// (fabrik:awaiting-member-close, retried every poll, eventually escalating),
// landMergeTrainBatch's own final close has no settle-scan-backed retry at
// all (ADR-061's own deferred-follow-up note) — the member reaches Done on
// the board but its issue is left open indefinitely, with only a warning in
// the engine's own log (unobservable to a sim scenario, so this test's
// proof is the *absence* of any retry marker or a later automatic close,
// not a log assertion).
func TestMergeTrainLanding_BatchCloseAsymmetryPin(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	numA, _ := QueueMember(t, env, "batchclose-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "batchclose-b", map[string]string{"b.txt": "b\n"})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	// Fail the member-issue close (not the PR close) for #A specifically —
	// landMergeTrainBatch calls CloseIssue twice per member (PR, then
	// issue); narrowing on the issue number isolates the second call.
	env.Sim.Faults().FailWhen("CloseIssue",
		func(a simgh.Args) bool { return a.Number == numA },
		1, errInjectedLandingFault)

	RunPoll(t, env)

	// The board advance and PR close still happen — only the final issue
	// close is affected, and it's warn-and-continue: no fabrik:paused, no
	// stall.
	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 10)
	WaitForIssueClosed(t, env, numB, 5) // B's close never faulted — lands clean

	// The asymmetry itself: A's issue is left open, with no durable retry
	// marker recorded anywhere a settle scan could pick up (landSingleton's
	// equivalent failure would apply fabrik:awaiting-member-close here).
	if projectItem(t, env, numA).IsClosed {
		t.Fatalf("expected #%d's issue to remain open after its landMergeTrainBatch close failed once — the fault should have fired", numA)
	}
	if hasLabel(IssueLabels(t, env, numA), "fabrik:awaiting-member-close") {
		t.Errorf("landMergeTrainBatch has no settle-scan-backed retry for its own member-issue close (ADR-061 deferred follow-up) — fabrik:awaiting-member-close must never appear here")
	}

	// No automatic retry ever closes it: run several more polls (nothing
	// left in Queued to dispatch a fresh train, so this only exercises
	// settle scans, none of which own this failure) and confirm it's still
	// open — this is the asymmetry's actual, observable consequence, not
	// just an absent label.
	RunPolls(t, env, 3)
	if projectItem(t, env, numA).IsClosed {
		t.Errorf("expected #%d to remain open indefinitely — landMergeTrainBatch's close failure has no retry path to eventually succeed on its own", numA)
	}
}
