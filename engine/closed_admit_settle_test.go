package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// gateCheckedStages returns a pipeline whose Validate stage is gate-checked
// (wait_for_ci) and an Implement stage that is not — used to verify the
// closed-issue admit gate in itemMayNeedWork / itemNeedsWork.
func gateCheckedStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Validate", Order: 2, Prompt: "validate", WaitForCI: &tr},
	}
}

func gateCheckedEngine(t *testing.T) *Engine {
	t.Helper()
	return NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: gateCheckedStages()},
		&mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
}

// A merged PR closes the issue while it sits at the gate-checked Validate stage
// carrying any gate label (or none). The settle-owner (runValidatePRTerminalAdvance)
// reads only items admitted by itemMayNeedWork, so the admit gate must let these
// through regardless of which gate label is present — otherwise the #874-class
// merge is stranded (the ADR-056 D2 gap this guards).

func TestItemMayNeedWork_ClosedAtValidate_AwaitingReview_Admitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   1,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:awaiting-review", "fabrik:paused"},
	}
	if !eng.itemMayNeedWork(item) {
		t.Error("closed Validate item with fabrik:awaiting-review must be admitted so the settle-owner can heal it")
	}
}

func TestItemMayNeedWork_ClosedAtValidate_PausedOnly_Admitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   2,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:paused", "fabrik:awaiting-input"},
	}
	if !eng.itemMayNeedWork(item) {
		t.Error("closed Validate item with only fabrik:paused must be admitted (no-gate-label / awaiting-review merges)")
	}
}

func TestItemMayNeedWork_ClosedAtValidate_NoGateLabel_Admitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   3,
		Status:   "Validate",
		IsClosed: true,
		Labels:   nil,
	}
	if !eng.itemMayNeedWork(item) {
		t.Error("closed Validate item with no gate label must be admitted (gate-label-agnostic settle owner)")
	}
}

func TestItemMayNeedWork_ClosedAtValidate_AwaitingCI_StillAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   4,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:awaiting-ci"},
	}
	if !eng.itemMayNeedWork(item) {
		t.Error("closed Validate item with fabrik:awaiting-ci must remain admitted (pre-existing behavior)")
	}
}

// Regression guard: the fix is scoped to gate-checked stages. A closed item at a
// non-gate-checked stage with no complete/gate label must still be dropped, so we
// don't start deep-fetching every closed mid-pipeline issue.
func TestItemMayNeedWork_ClosedAtNonGateStage_NotAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   5,
		Status:   "Implement", // not gate-checked
		IsClosed: true,
		Labels:   []string{"fabrik:paused"},
	}
	if eng.itemMayNeedWork(item) {
		t.Error("closed item at a non-gate-checked stage with no complete/gate label must NOT be admitted")
	}
}

// itemNeedsWork mirror: the closed-issue admit gate keeps the same shape as
// itemMayNeedWork (the awaiting-ci/auto-merge allowlist lives in both), so the
// non-gate-stage drop must hold here too. (The full itemNeedsWork still returns
// false for a closed paused item with no pending work via downstream guards —
// healing is the settle-owner's job, fed by itemMayNeedWork, not itemNeedsWork.)
func TestItemNeedsWork_ClosedAtNonGateStage_NotAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   7,
		Status:   "Implement",
		IsClosed: true,
		Labels:   []string{"fabrik:paused", "fabrik:awaiting-input"},
	}
	if eng.itemNeedsWork(item) {
		t.Error("closed item at a non-gate-checked stage must NOT pass itemNeedsWork")
	}
}
