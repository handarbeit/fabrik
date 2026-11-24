package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func rebaseLabelRemovals(c *mockGitHubClient) int {
	n := 0
	for _, r := range c.removeLabelCalls {
		if r.labelName == "fabrik:rebase-needed" {
			n++
		}
	}
	return n
}

// #2613 R1/R3: a computed, non-conflicting pending-CI hold clears the stale label
// and still claims the item without reporting a conflict.
func TestCheckMergeabilityGate_UnsettledComputed_ClearsLabelStillHolds(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeUnsettled, MergeObs: MergeObsComputed, Reason: "CI checks pending"}

	blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
	if !blocked || conflict {
		t.Fatalf("want blocked=true conflict=false, got %v %v", blocked, conflict)
	}
	if got := rebaseLabelRemovals(client); got != 1 {
		t.Errorf("rebase-needed removals = %d, want 1", got)
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("unexpected label adds: %v", client.addLabelCalls)
	}
}

// #2613 R2: unknown or unreadable mergeability never clears the label.
func TestCheckMergeabilityGate_UnsettledUnknownOrNone_KeepsLabel(t *testing.T) {
	tr := true
	for name, obs := range map[string]MergeObservation{"unknown": MergeObsUnknown, "none": MergeObsNone} {
		t.Run(name, func(t *testing.T) {
			client := &mockGitHubClient{}
			eng := testEngineForMerge(t, client)
			item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
			stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
			settle := PRSettleResult{Status: PRMergeUnsettled, MergeObs: obs, Reason: "mergeable=null"}
			blocked, conflict := eng.checkMergeabilityGate(item, stage, settle)
			if !blocked || conflict {
				t.Fatalf("want blocked=true conflict=false, got %v %v", blocked, conflict)
			}
			if got := rebaseLabelRemovals(client); got != 0 {
				t.Errorf("rebase-needed removals = %d, want 0", got)
			}
		})
	}
}

func TestCheckMergeabilityGate_UnsettledComputed_NoLabelNoWrite(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeUnsettled, MergeObs: MergeObsComputed, Reason: "CI checks pending"}
	if blocked, conflict := eng.checkMergeabilityGate(item, stage, settle); !blocked || conflict {
		t.Fatalf("want blocked, no conflict; got %v %v", blocked, conflict)
	}
	if len(client.removeLabelCalls) != 0 || len(client.addLabelCalls) != 0 {
		t.Errorf("unexpected label writes: %v %v", client.removeLabelCalls, client.addLabelCalls)
	}
}

// #2613: the PRMergeQueued claim is out of scope and keeps its label.
func TestCheckMergeabilityGate_Queued_KeepsLabelEvenIfComputed(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeQueued, MergeObs: MergeObsComputed}
	eng.checkMergeabilityGate(item, stage, settle)
	if got := rebaseLabelRemovals(client); got != 0 {
		t.Errorf("removals = %d, want 0", got)
	}
}

// Non-vacuity: with the clearing neutralised the label stays.
func TestCheckMergeabilityGate_UnsettledComputed_Neutralised_KeepsLabel(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	defer eng.SetRebaseLabelClearOnPendingDisabledForTest()()
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeUnsettled, MergeObs: MergeObsComputed, Reason: "CI checks pending"}
	eng.checkMergeabilityGate(item, stage, settle)
	if got := rebaseLabelRemovals(client); got != 0 {
		t.Errorf("removals = %d, want 0 when neutralised", got)
	}
}

// #2613 review: a conflict that returns after a conflict-free hold must drop the
// recorded NoConflict fact, so the attention reason cannot read "rebase pushed".
func TestCheckMergeabilityGate_Conflicting_ClearsStaleNoConflictHold(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	repo := itemOwnerRepoString(item, eng.defaultRepo())
	eng.store.Apply(itemstate.GateHoldRecorded{Repo: repo, Number: 1,
		Hold: itemstate.GateHold{Gate: gateMerge, Reason: "CI checks pending", NoConflict: true}})
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{Status: PRMergeConflicting, PR: &gh.PRDetails{Number: 5}}
	if blocked, conflict := eng.checkMergeabilityGate(item, stage, settle); !blocked || !conflict {
		t.Fatalf("want blocked+conflict, got %v %v", blocked, conflict)
	}
	if snap, ok := eng.store.Peek(repo, 1); ok && snap.State().GateHold != nil {
		t.Errorf("stale merge gate hold survived a conflict: %+v", snap.State().GateHold)
	}
}

// #2613 review: the NoConflict fact recorded on the claim follows the clearing
// rule exactly; a queued PR never reads as a conflict-free pending-CI hold.
func TestMergeHoldNoConflict(t *testing.T) {
	cases := []struct {
		name   string
		settle PRSettleResult
		want   bool
	}{
		{"unsettled computed", PRSettleResult{Status: PRMergeUnsettled, MergeObs: MergeObsComputed}, true},
		{"unsettled unknown", PRSettleResult{Status: PRMergeUnsettled, MergeObs: MergeObsUnknown}, false},
		{"unsettled none", PRSettleResult{Status: PRMergeUnsettled, MergeObs: MergeObsNone}, false},
		{"queued computed", PRSettleResult{Status: PRMergeQueued, MergeObs: MergeObsComputed}, false},
	}
	for _, c := range cases {
		if got := mergeHoldNoConflict(c.settle); got != c.want {
			t.Errorf("%s: mergeHoldNoConflict = %v, want %v", c.name, got, c.want)
		}
	}
}
