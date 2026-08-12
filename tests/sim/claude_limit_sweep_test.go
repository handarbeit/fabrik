package sim

import (
	"testing"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// This file is R5's Claude-limit-path coverage beyond what
// failure_shapes_test.go's TestFailureShape_UsageLimitExit already
// establishes (fabrik:claude-limit applied without stage:*:failed/
// fabrik:paused; fabrik:clear-claude-limit lifting the suspension on the
// *same* issue that hit it). Two properties remain unproven there:
//
//  1. settleClaudeLimitLabelSweep (engine/claude_limit_settle.go) —
//     specifically the account-wide sweep, as distinct from a per-issue
//     redispatch that happens to clear its own label. TestClaudeLimitLabelSweep
//     isolates this by observing a *second*, permanently-paused issue that
//     never itself dispatches again.
//  2. The StageAttempted-without-StageRetryIncremented property (AC5):
//     TestClaudeLimitExit_NeverAccumulatesTowardMaxRetries proves repeated
//     usage-limit hits on the same issue never accumulate toward
//     max_retries, however many times it happens — the direct behavioral
//     consequence of handleUsageLimitExit's own doc comment
//     ("StageRetryIncremented is deliberately never called").
//
// Neither test needs the account-wide suspension's real 1-hour fallback
// backoff to actually elapse — activateClaudeSuspension/
// claudeSuspendedUntilTime are anchored to real time.Now(), not the
// injected Clock (see restart_recovery_test.go's sibling comment on the
// same asymmetry; ADR-1449's engine-seam minimalism deliberately did not
// extend the Clock seam to this path), so fast-forwarding it is not
// available to this harness without a new production seam. Both scenarios
// instead use fabrik:clear-claude-limit — the same deterministic,
// real-time-independent lever CLAUDE.md documents as the operator escape
// hatch for exactly this situation — to end the suspension on demand.
//
// The contrasting positive case ("a genuine repeated stage failure DOES
// reach stage:<name>:failed/fabrik:paused at MaxRetries") is not
// re-derived here: engine/mutate_test.go, engine/process_item_test.go,
// engine/slice_retry_test.go, and engine/write_through_test.go already
// exercise escalateFailedStage directly at the engine-unit level — the
// same "engine-level tests already assert the exact comment/edge cases"
// precedent settle_scan_escalation_test.go's own doc comment cites for not
// re-deriving comment literals from scratch at this layer.

// TestClaudeLimitLabelSweep proves settleClaudeLimitLabelSweep — not
// merely a per-issue redispatch that happens to also clear
// fabrik:claude-limit — is what removes the label account-wide once the
// suspension lifts. Two issues: #A actually hits the usage limit (a real
// invocation, a real suspension); #B is seeded directly with
// fabrik:claude-limit + fabrik:paused and never dispatches again (paused
// items are excluded from ordinary dispatch), so the *only* mechanism that
// could ever remove #B's label is the unconditional per-poll sweep — not
// handleUsageLimitExit's own per-issue clear (that fires only on an
// issue's own next non-limit invocation, which #B never has).
func TestClaudeLimitLabelSweep(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	env.Claude.ForStage("Specify", simclaude.UsageLimitExit(), simclaude.DefaultScript)

	numA := FileIssue(t, env, "claude-limit sweep: trigger", "body", "Specify")
	numB := FileIssue(t, env, "claude-limit sweep: bystander", "body", "Specify", "fabrik:paused")

	WaitForIssueLabel(t, env, numA, "fabrik:claude-limit", 80)
	t.Logf("#%d hit the usage limit — account-wide suspension now active", numA)

	// #B's own fabrik:claude-limit is seeded only now, with the suspension
	// already genuinely active — seeding it up front (before any real
	// suspension exists) would race: settleClaudeLimitLabelSweep correctly
	// treats "carries the label but nothing is suspended" as stale state and
	// clears it on the very first poll, before #A ever dispatches, which
	// would make this scenario vacuous (B's label gone for the wrong
	// reason, coincidentally before the assertion below even runs).
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, numB, "fabrik:claude-limit"); err != nil {
		t.Fatalf("seed #B's fabrik:claude-limit: %v", err)
	}
	RunPoll(t, env)

	// #B's label must NOT have cleared while the suspension is still active
	// — the "not one poll early" half of non-vacuity: if the sweep ran
	// unconditionally regardless of suspension state, this would already be
	// false.
	if !hasLabel(IssueLabels(t, env, numB), "fabrik:claude-limit") {
		t.Fatal("#B's fabrik:claude-limit already cleared before the suspension lifted — the sweep must not run while still suspended")
	}

	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, numA, "fabrik:clear-claude-limit"); err != nil {
		t.Fatalf("AddLabelToIssue(fabrik:clear-claude-limit): %v", err)
	}
	t.Logf("cleared the suspension via #%d — settleClaudeLimitLabelSweep should now sweep every open item, including the bystander", numA)

	// #B never dispatches (still fabrik:paused throughout), so its label
	// clearing can only be the unconditional sweep, not a per-issue path.
	WaitForLabelAbsent(t, env, numB, "fabrik:claude-limit", 20)
	if !hasLabel(IssueLabels(t, env, numB), "fabrik:paused") {
		t.Error("#B's unrelated fabrik:paused must remain untouched by the sweep — only fabrik:claude-limit is its concern")
	}
	t.Logf("#%d (bystander, never dispatched) had fabrik:claude-limit swept away — settleClaudeLimitLabelSweep confirmed, not a per-issue clear", numB)
}

// TestClaudeLimitExit_NeverAccumulatesTowardMaxRetries drives two
// consecutive usage-limit exits on the same issue with MaxRetries=1 — the
// smallest budget that would, if StageRetryIncremented were ever called,
// already escalate the issue by the second exit (count 2 >= MaxRetries 1).
// Between the two, the suspension is lifted via fabrik:clear-claude-limit
// (a real hour of wall-clock waiting is not something this scenario can
// afford — see this file's own doc comment) so the second dispatch is
// actually reached, not just theoretically eligible.
func TestClaudeLimitExit_NeverAccumulatesTowardMaxRetries(t *testing.T) {
	t.Parallel() // real-time-bound (retryCooldownPolls) — see failure_shapes_test.go's identical rationale
	env := NewEnv(t, EnvOptions{
		Stages: failureShapeStages(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxRetries = 1
		},
	})
	// Every call gets UsageLimitExit — ForStage/pickStageScript repeats a
	// single registered script for every subsequent invocation once the
	// sequence is exhausted (tests/sim/simclaude/invoker.go), so both hits
	// this scenario needs are genuinely usage-limit exits, not a first hit
	// followed by a normal completion.
	env.Claude.ForStage("Specify", simclaude.UsageLimitExit())

	num := FileIssue(t, env, "claude-limit never accumulates", "body", "Specify")

	WaitForIssueLabel(t, env, num, "fabrik:claude-limit", 80)
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Fatalf("StageCallCount(Specify) after first hit = %d, want 1", got)
	}
	if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Fatal("fabrik:paused already applied after only 1 usage-limit hit (MaxRetries=1) — StageRetryIncremented must never fire for a usage-limit exit")
	}

	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:clear-claude-limit"); err != nil {
		t.Fatalf("AddLabelToIssue(fabrik:clear-claude-limit): %v", err)
	}
	WaitForLabelAbsent(t, env, num, "fabrik:claude-limit", 20)

	// The per-issue dispatch cooldown after the first StageAttempted is
	// real-wall-clock-anchored (see this file's doc comment) — this wait is
	// unavoidable, not a design choice.
	WaitForIssueLabel(t, env, num, "fabrik:claude-limit", retryCooldownPolls)
	if got := env.Claude.StageCallCount("Specify"); got != 2 {
		t.Fatalf("StageCallCount(Specify) after second hit = %d, want 2 — the second dispatch must genuinely have happened for this scenario to prove anything", got)
	}
	t.Logf("#%d: 2 consecutive usage-limit hits with MaxRetries=1 — a real StageRetryIncremented count of 2 would already have escalated here", num)

	// Non-vacuity (AC8): with MaxRetries=1, a genuine stage failure escalates
	// on its 2nd attempt (see engine/mutate_test.go et al., cited in this
	// file's doc comment, for the direct proof of that boundary). This
	// scenario's whole point is that the identical count of *usage-limit*
	// attempts does not.
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "stage:Specify:failed") {
		t.Error("stage:Specify:failed applied after 2 usage-limit hits (MaxRetries=1) — usage-limit exits must never count toward max_retries")
	}
	if hasLabel(labels, "fabrik:paused") {
		t.Error("fabrik:paused applied after 2 usage-limit hits (MaxRetries=1) — usage-limit exits must never count toward max_retries")
	}
	if !hasLabel(labels, "fabrik:claude-limit") {
		t.Error("fabrik:claude-limit missing after the second hit — the second dispatch should have re-applied it")
	}
}
