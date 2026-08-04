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

// R1/ADR-1387: a closed item is never dispatched to a Claude stage invocation.
// Before ADR-1387, a closed item at the gate-checked Validate stage lacking
// stage:Validate:complete was admitted here regardless of which gate label (or
// none) it carried, on the theory that admission was the only way for the
// settle-owner (runValidatePRTerminalAdvance) to observe and heal it. That
// admission was also, indistinguishably, an admission to real dispatch — which
// produced an unbounded post-close Claude-invocation loop when the completing
// label was deferred (wait_for_ci) and a defensive sweep stripped the
// suppressing gate label from the closed issue every poll (#617 x conjunctive
// gate). The settle-owner now has its own board.Items-sourced feed
// (settleClosedValidateAdvance), so these items must no longer be admitted.

func TestItemMayNeedWork_ClosedAtValidate_AwaitingReview_NotAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   1,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:awaiting-review", "fabrik:paused"},
	}
	if eng.itemMayNeedWork(item) {
		t.Error("closed Validate item with fabrik:awaiting-review must NOT be admitted — healing is settleClosedValidateAdvance's job, not dispatch's")
	}
}

func TestItemMayNeedWork_ClosedAtValidate_PausedOnly_NotAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   2,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:paused", "fabrik:awaiting-input"},
	}
	if eng.itemMayNeedWork(item) {
		t.Error("closed Validate item with only fabrik:paused must NOT be admitted")
	}
}

func TestItemMayNeedWork_ClosedAtValidate_NoGateLabel_NotAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   3,
		Status:   "Validate",
		IsClosed: true,
		Labels:   nil,
	}
	if eng.itemMayNeedWork(item) {
		t.Error("closed Validate item with no gate label must NOT be admitted")
	}
}

func TestItemMayNeedWork_ClosedAtValidate_AwaitingCI_NoLongerAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   4,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:awaiting-ci"},
	}
	if eng.itemMayNeedWork(item) {
		t.Error("closed Validate item with fabrik:awaiting-ci must NOT be admitted (this is exactly the loop-producing state; see #1387)")
	}
}

// Regression guard: the removed widenings never applied to non-gate-checked
// stages in the first place. A closed item at a non-gate-checked stage with no
// complete/gate label must still be dropped, so we don't start deep-fetching
// every closed mid-pipeline issue.
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

// itemMayNeedWork mirror: a closed item carrying stage:<X>:complete is still
// admitted (unchanged by R3) so the catch-up loop / Phase 1 handlers can
// process it (e.g. drain a lingering review reinvoke).
func TestItemMayNeedWork_ClosedAtValidate_StageComplete_StillAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   6,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"stage:Validate:complete"},
	}
	if !eng.itemMayNeedWork(item) {
		t.Error("closed Validate item carrying stage:Validate:complete must remain admitted — R3 only removed the gate-checked/awaiting-ci/auto-merge-enabled widenings, not the stage:complete admission")
	}
}

// itemNeedsWork mirrors of the itemMayNeedWork closed-issue gate above — the
// admission block is duplicated between the two functions (Risks section,
// #1387), so both must be independently verified.

func TestItemNeedsWork_ClosedAtValidate_AwaitingReview_NotAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   11,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:awaiting-review", "fabrik:paused"},
	}
	if eng.itemNeedsWork(item) {
		t.Error("closed Validate item with fabrik:awaiting-review must NOT pass itemNeedsWork")
	}
}

func TestItemNeedsWork_ClosedAtValidate_PausedOnly_NotAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   12,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:paused", "fabrik:awaiting-input"},
	}
	if eng.itemNeedsWork(item) {
		t.Error("closed Validate item with only fabrik:paused must NOT pass itemNeedsWork")
	}
}

func TestItemNeedsWork_ClosedAtValidate_NoGateLabel_NotAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   13,
		Status:   "Validate",
		IsClosed: true,
		Labels:   nil,
	}
	if eng.itemNeedsWork(item) {
		t.Error("closed Validate item with no gate label must NOT pass itemNeedsWork")
	}
}

func TestItemNeedsWork_ClosedAtValidate_AwaitingCI_NotAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   14,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:awaiting-ci"},
	}
	if eng.itemNeedsWork(item) {
		t.Error("closed Validate item with fabrik:awaiting-ci must NOT pass itemNeedsWork")
	}
}

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

// R1/ADR-1387, a second instance of the same class of bug (Pruefer, PR
// #1388): before this fix, itemNeedsWork's "new comments are always worth
// processing" fast path had no IsClosed check, so a closed item admitted via
// the retained stage:complete exception (see
// TestItemMayNeedWork_ClosedAtValidate_StageComplete_StillAdmitted above)
// could still reach a real Claude invocation via comment processing — a
// narrower, pre-existing variant of the same "closed item reaches dispatch"
// bug this issue fixes, just through the comment path rather than the
// CI-gate/awaiting-ci loop. A closed item must never be dispatched (R1),
// regardless of new comments, except at a cleanup stage.
func TestItemNeedsWork_ClosedAtValidate_StageComplete_WithNewComment_NotAdmitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	item := gh.ProjectItem{
		Number:   12,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"stage:Validate:complete"},
		Comments: []gh.Comment{
			{ID: "C1", Author: "someone", Body: "any follow-up comment"},
		},
	}
	if eng.itemNeedsWork(item) {
		t.Error("closed item with stage:Validate:complete must NOT pass itemNeedsWork even with a new comment (R1, ADR-1387) — comment processing is still a real Claude invocation")
	}
}

// R1's stated exception: a closed item at a cleanup_worktree stage still
// reaches the dispatch path, so worktree reaping can run.
func TestItemMayNeedWork_ClosedAtCleanupStage_Admitted(t *testing.T) {
	eng := gateCheckedEngine(t)
	eng.cfg.Stages = append(eng.cfg.Stages, &stages.Stage{Name: "Done", Order: 3, CleanupWorktree: true})
	item := gh.ProjectItem{
		Number:   8,
		Status:   "Done",
		IsClosed: true,
		Labels:   nil,
	}
	if !eng.itemMayNeedWork(item) {
		t.Error("closed item at a cleanup_worktree stage must remain admitted so cleanup can reap the worktree (R1's stated exception)")
	}
}

// R7: open items at every representative stage kind are unaffected by the
// closed-issue admission simplification, since that block is entirely inside
// `if item.IsClosed`. Table-driven per the issue's explicit Risk callout, as a
// durable regression guard rather than an inline assertion.
func TestItemMayNeedWork_OpenItems_UnaffectedByClosedIssueGuard(t *testing.T) {
	tr := true
	openStages := []*stages.Stage{
		{Name: "Specify", Order: 1, Prompt: "specify"},
		{Name: "Research", Order: 2, Prompt: "research"},
		{Name: "Plan", Order: 3, Prompt: "plan"},
		{Name: "Implement", Order: 4, Prompt: "implement"},
		{Name: "Review", Order: 5, Prompt: "review"},
		{Name: "Validate", Order: 6, Prompt: "validate", WaitForCI: &tr},
		{Name: "Done", Order: 7, CleanupWorktree: true},
		{Name: "Backlog", Order: 8, Unmanaged: true},
		{Name: "Queued", Order: 9, HoldingStage: true},
	}
	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: openStages},
		&mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)

	cases := []struct {
		name   string
		status string
		want   bool // expected itemMayNeedWork result for an open item with no other gating labels
	}{
		{"Specify", "Specify", true},
		{"Research", "Research", true},
		{"Plan", "Plan", true},
		{"Implement", "Implement", true},
		{"Review", "Review", true},
		{"Validate", "Validate", true},
		{"CleanupWorktree_Done", "Done", false},  // no worktree on disk, not closed -> false
		{"Unmanaged_Backlog", "Backlog", false},  // Unmanaged stages are never dispatched
		{"HoldingStage_Queued", "Queued", false}, // Holding stages are batch-scoped, never per-item
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := gh.ProjectItem{
				Number:   100,
				Status:   tc.status,
				IsClosed: false,
			}
			got := eng.itemMayNeedWork(item)
			if got != tc.want {
				t.Errorf("itemMayNeedWork(open item at %s) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}
