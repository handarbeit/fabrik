package sim

import (
	"errors"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// errInjectedRerouteFault is this file's own injected-fault sentinel,
// mirroring settle_scan_escalation_test.go's errInjectedSettleFault — a
// plain, unambiguous error distinct from anything the engine itself could
// produce, so a test failure can never be confused with a genuine defect
// surfacing through the fault path by coincidence.
var errInjectedRerouteFault = errors.New("simgh: injected reroute fault")

// This file covers R2's mandatory batch-size-1 red case (the #1440 shape) and
// R9's two reroute-off-holding scenarios for #1545. Both #1440 and #1545 are
// already fixed/shipped on main (confirmed directly against
// engine/merge_train.go's ejectRedSingleton and rerouteQueuedMemberOffHolding)
// — these scenarios pin the already-shipped behavior at the sim's multi-poll,
// real-git layer, mirroring what tests/e2e/mergetrain_redsingleton_test.go's
// own doc comment anticipates ("the equivalent sim-bed regression test for
// the state transition is tracked separately under #1452").

// TestMergeTrainRedSingleton_ReroutesOffQueued is R2's batch-size-1 red case
// and R9 scenario 1: a lone Queued member whose own combined Validate is red
// is never bisected (there is nothing to isolate — bisect's own arity guard,
// engine/merge_train.go:789, short-circuits before ever calling bisect) and
// is instead rerouted off Queued to stageBeforeHolding ("Validate" in this
// file's mergeTrainStages fixture) and paused there — reachable by
// fabrik:revalidate, unlike the pre-#1545 stranding-in-Queued bug.
func TestMergeTrainRedSingleton_ReroutesOffQueued(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, _ := QueueMember(t, env, "redsingleton", map[string]string{"solo.txt": "solo\n"})

	// Reds every trial containing this lone member — a batch of one is
	// always "this member alone". (Only one seeder ever runs here:
	// startTrialVerdictSeeder starts a background goroutine with its own
	// PR-dedup state, so starting a second one concurrently — rather than
	// stopping the first — would race both against the engine's first
	// FetchCheckRuns read and could seed conflicting verdicts on the same
	// trial SHA.)
	startTrialVerdictSeeder(t, env, poisonVerdict(num))

	RunPoll(t, env)

	// R1: left Queued for the reroute target, not bisected-and-ejected.
	WaitForProjectStatus(t, env, num, "Validate", 20)
	if projectItem(t, env, num).IsClosed {
		t.Fatalf("red singleton #%d must never land", num)
	}

	// The member is paused there — reachable, unlike a pre-#1545 pause inside Queued.
	WaitForIssueLabel(t, env, num, "fabrik:paused", 5)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 5)

	if !hasCommentContaining(t, env, num, "own combined Validate is failing") {
		t.Error("expected the disposition comment to name the member's own Validate as the cause, not a batch interaction")
	}
	if !hasCommentContaining(t, env, num, "has left the Queued column for Validate") {
		t.Error("expected the disposition comment to name Validate as the reroute target")
	}
	if !hasCommentContaining(t, env, num, "fabrik:revalidate") {
		t.Error("expected the disposition comment to recommend fabrik:revalidate as the recovery action (target stage is literally named Validate)")
	}

	// Non-vacuity (AC10): a bisection-shaped disposition would instead post an
	// "ejected" comment and leave the member in Queued for a future train —
	// asserting the comment's actual wording (above) and the Status transition
	// together rule that out; a regression that routed this case through
	// ejectMember/bisect instead would fail both.
	if hasCommentContaining(t, env, num, "🏭 **Fabrik merge-train — ejected**") {
		t.Error("a red singleton must never be bisected-and-ejected — that is the #1440 defect this disposition replaces")
	}
}

// TestMergeTrainRedSingleton_Dispatchable is R9 scenario 1 (state-transition
// half): once rerouted, the member sits at stageBeforeHolding — not a dead
// end. Applying fabrik:revalidate (the recovery instruction the disposition
// comment itself names) re-enters Validate and the member is dispatched
// again, proving it is genuinely reachable by the ordinary pipeline rather
// than merely relocated to a different resting place.
func TestMergeTrainRedSingleton_Dispatchable(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, _ := QueueMember(t, env, "redsingleton-dispatch", map[string]string{"solo2.txt": "solo2\n"})
	startTrialVerdictSeeder(t, env, poisonVerdict(num))

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Validate", 20)
	WaitForIssueLabel(t, env, num, "fabrik:paused", 5)

	// The Validate stage in this fixture has no scripted invocation registered
	// (mergeTrainStages() gives it a bare Prompt with no Claude script) — that's
	// fine: the assertion here is only that fabrik:revalidate is honored as a
	// *dispatch* trigger (fabrik:paused/awaiting-input clear and the stage is
	// attempted again), not that the invocation itself completes successfully.
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:revalidate"); err != nil {
		t.Fatalf("AddLabelToIssue(fabrik:revalidate): %v", err)
	}
	RunPoll(t, env)

	WaitForLabelAbsent(t, env, num, "fabrik:paused", 5)
	WaitForLabelAbsent(t, env, num, "fabrik:revalidate", 5)
}

// TestMergeTrainRedSingleton_FailedRerouteNoCommentNoCount is R9 scenario 2:
// when the board-status mutation underlying the reroute fails transiently,
// rerouteQueuedMemberOffHolding returns false with zero side effects
// (engine/merge_train.go) — ejectRedSingleton then posts nothing and pauses
// nobody, leaving the member exactly as it was so the next poll's fresh train
// re-formation re-detects the same red-singleton condition and retries the
// reroute whole, rather than compounding a duplicate comment or a stray
// pause on top of a half-applied state. This mirrors
// ejectQueuedMemberForReviewFindings's identical reroute-before-side-effects
// ordering (ADR-1208) that #1545/ADR-1545 reuses.
func TestMergeTrainRedSingleton_FailedRerouteNoCommentNoCount(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, _ := QueueMember(t, env, "redsingleton-failreroute", map[string]string{"solo3.txt": "solo3\n"})
	startTrialVerdictSeeder(t, env, poisonVerdict(num))

	// Fail the reroute's own board-status mutation for this member exactly
	// once, narrowed on its itemID — UpdateProjectItemStatus carries no issue
	// Number in its Args (project-item calls key off itemID instead; see
	// settle_scan_escalation_test.go's identical narrowing) — so the first
	// reroute attempt fails and a later one (once re-detected) can succeed.
	itemID := projectItem(t, env, num).ItemID
	env.Sim.Faults().FailWhen("UpdateProjectItemStatus",
		func(a simgh.Args) bool { return len(a.Values) > 0 && a.Values[0] == itemID },
		1, errInjectedRerouteFault)

	RunPoll(t, env)

	// Failed reroute: member is still in Queued, not paused, no disposition comment.
	if got := projectItem(t, env, num).Status; got != "Queued" {
		t.Fatalf("expected #%d to remain in Queued after a failed reroute, got status %q", num, got)
	}
	if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Error("expected no fabrik:paused after a failed reroute — nothing should be paused until the reroute actually succeeds")
	}
	if hasCommentContaining(t, env, num, "own combined Validate is failing") {
		t.Error("expected no disposition comment after a failed reroute")
	}

	// Ordering guarantee: no UpdateProjectItemStatus call this run precedes any
	// AddComment/label-add on this member — because none of those later steps
	// ran at all. Assert directly that no comment was posted for this issue,
	// which is the strongest statement available when the "before" event never
	// fired (Precedes itself requires both predicates to match at least once).
	entries := env.Sim.Log().ByMethod("AddComment")
	for _, e := range entries {
		if e.Args.Number == num {
			t.Errorf("expected zero AddComment calls on #%d after a failed reroute, got: %+v", num, e)
		}
	}

	// Re-detected on a later poll: the fault was one-shot, so the next train
	// re-formation's reroute attempt succeeds, and the ordinary disposition
	// (reroute before comment/pause) proceeds — proving the condition is
	// retried whole rather than lost.
	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Validate", 20)
	WaitForIssueLabel(t, env, num, "fabrik:paused", 5)

	// The ordering guarantee proper, once both events exist: the status move
	// off Queued precedes the disposition comment — reroute-before-side-effects,
	// not the other way around. The second predicate must narrow to the
	// red-singleton disposition comment specifically, by body content: the
	// re-formed train's own reconstructTrainState first dissolves the earlier
	// (poll 1) trial's now-orphaned integration PR, which posts its own,
	// unrelated "batch dissolved" comment on #1 before the reroute this
	// assertion cares about even runs — OnIssue(num) alone would match that
	// one instead, since it precedes everything.
	isDispositionComment := func(e simgh.Entry) bool {
		return e.Method == "AddComment" && e.Args.Number == num &&
			strings.Contains(e.Args.Body, "own combined Validate is failing")
	}
	ok, err := env.Sim.Log().Precedes(
		simgh.And(simgh.MethodIs("UpdateProjectItemStatus"), simgh.Succeeded()),
		isDispositionComment,
	)
	if err != nil {
		t.Fatalf("Precedes: %v", err)
	}
	if !ok {
		t.Error("expected the successful reroute's UpdateProjectItemStatus to precede the disposition AddComment")
	}
}
