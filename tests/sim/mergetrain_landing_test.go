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
// the as-found asymmetry, not fixing it. A real green two-member batch lands
// via landMergeTrainBatch; the final member-issue CloseIssue call for one
// member is made to fail.
//
// This is *not* the "member left open" scenario R6's original design
// expected — that shape turns out to be unreachable on the path this test
// actually exercises, and the reason is itself the interesting finding.
// landMergeTrainBatch's integration PR body unconditionally carries a
// "Closes #N" line per survivor (assembleTrialBranch's closesLines, R2's own
// verdict-seeder parses the same lines). On the default branch, GitHub's own
// merge-time auto-close (reproduced faithfully by simgh's MergePR, gated on
// base == the repo's default branch — see simgh/prs.go) fires the instant
// the integration PR merges, closing every member issue *before*
// landMergeTrainBatch's own explicit member-issue CloseIssue call ever runs.
// That explicit call is therefore redundant on this path — its failure has
// no observable consequence, because the issue is already closed by the
// time it's attempted.
//
// This is the actual, structural reason landMergeTrainBatch's close failure
// has never needed a settle-scan-backed retry the way landSingleton's does
// (ADR-061's deferred-follow-up note): landSingleton's own landing PR body
// deliberately carries no "Closes #N" line (see its doc comment — a
// marker-free body to dodge findIntegrationPR collisions across sequential
// singleton lands), so auto-close never fires there and its own explicit
// CloseIssue is the *only* closer, which is exactly why its failure is
// consequential enough to need ADR-061's retry/escalation arc. This test
// proves that asymmetry precisely: it asserts MergePR precedes the (faulted)
// CloseIssue call, that the issue is closed anyway despite the fault, and
// that no fabrik:awaiting-member-close marker is ever applied — because
// nothing here needs retrying.
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

	// The board advance, PR close, and issue close all still happen — the
	// injected fault on #A's explicit CloseIssue is genuinely inconsequential
	// here (see doc comment above).
	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 10)
	WaitForIssueClosed(t, env, numA, 5) // closed via MergePR's own auto-close, not the faulted call
	WaitForIssueClosed(t, env, numB, 5) // B's close never faulted — lands clean regardless

	// The ordering that explains why: the integration PR's merge (which
	// carries #A's "Closes #%d" line) happens before landMergeTrainBatch's
	// own explicit, faulted CloseIssue(#A) attempt.
	mergePRPred := simgh.MethodIs("MergePR")
	closeAPred := simgh.And(simgh.MethodIs("CloseIssue"), simgh.OnIssue(numA), simgh.WasInjected())
	precedes, err := env.Sim.Log().Precedes(mergePRPred, closeAPred)
	if err != nil {
		t.Fatalf("Precedes(MergePR, injected CloseIssue(#%d)): %v", numA, err)
	}
	if !precedes {
		t.Errorf("expected the integration PR's MergePR to precede the faulted CloseIssue(#%d) — landMergeTrainBatch's own close attempt should come after the auto-close-triggering merge", numA)
	}

	// No retry marker is ever applied — there is nothing to retry, unlike
	// landSingleton's equivalent failure (fabrik:awaiting-member-close).
	if hasLabel(IssueLabels(t, env, numA), "fabrik:awaiting-member-close") {
		t.Errorf("landMergeTrainBatch has no settle-scan-backed retry for its own member-issue close (ADR-061 deferred follow-up), and none is needed here — fabrik:awaiting-member-close must never appear")
	}
}
