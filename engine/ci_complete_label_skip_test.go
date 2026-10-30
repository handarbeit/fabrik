package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// #2530 (ADR 2530): the CI gate clear does not re-add a completion label the item
// already carries, but keeps clearing fabrik:awaiting-ci and keeps adding the
// label when it is absent.

func completeTestAdded(c *mockGitHubClient) []string {
	var out []string
	for _, a := range c.addLabelCalls {
		out = append(out, a.labelName)
	}
	return out
}

func completeTestRemoved(c *mockGitHubClient, name string) bool {
	for _, r := range c.removeLabelCalls {
		if r.labelName == name {
			return true
		}
	}
	return false
}

func TestAddCompleteLabelAndRemoveCI_AbsentLabel_AddsAndClearsAwaitingCI(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	eng.addCompleteLabelAndRemoveCI("owner", "repo", item, &stages.Stage{Name: "Validate", WaitForCI: &tr})

	if got := completeTestAdded(client); len(got) != 1 || got[0] != "stage:Validate:complete" {
		t.Errorf("add calls = %v, want exactly stage:Validate:complete", got)
	}
	if !completeTestRemoved(client, "fabrik:awaiting-ci") {
		t.Error("fabrik:awaiting-ci must be removed when the gate clears")
	}
}

func TestAddCompleteLabelAndRemoveCI_PresentLabelWithAwaitingCI_SkipsAddStillClears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"stage:Validate:complete", "fabrik:awaiting-ci"}}
	eng.addCompleteLabelAndRemoveCI("owner", "repo", item, &stages.Stage{Name: "Validate", WaitForCI: &tr})

	if got := completeTestAdded(client); len(got) != 0 {
		t.Errorf("add calls = %v, want none for an already-present completion label", got)
	}
	if !completeTestRemoved(client, "fabrik:awaiting-ci") {
		t.Error("fabrik:awaiting-ci must still be removed for the inconsistent complete+awaiting-ci state")
	}
}

func TestAddCompleteLabelAndRemoveCI_PresentLabelNoMarker_WritesNothing(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"stage:Review:complete"}}
	eng.addCompleteLabelAndRemoveCI("owner", "repo", item, &stages.Stage{Name: "Review", WaitForCI: &tr})

	if got := completeTestAdded(client); len(got) != 0 {
		t.Errorf("add calls = %v, want none", got)
	}
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("remove calls = %v, want none", client.removeLabelCalls)
	}
}

func TestAddCompleteLabelAndRemoveCI_SkipNeutralised_AddsAgain(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	defer eng.SetCompleteLabelSkipDisabledForTest()()
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"stage:Validate:complete"}}
	eng.addCompleteLabelAndRemoveCI("owner", "repo", item, &stages.Stage{Name: "Validate", WaitForCI: &tr})

	if got := completeTestAdded(client); len(got) != 1 {
		t.Errorf("non-vacuity: with the skip off the label must be added again, got %v", got)
	}
}

func TestActiveDeepFetches_ExcludesPeriodicOnlyAdmissions(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	if got := eng.activeDeepFetches(3, 3); got != 0 {
		t.Errorf("periodic-only admissions counted as activity: %d", got)
	}
	if got := eng.activeDeepFetches(3, 1); got != 2 {
		t.Errorf("mixed admissions = %d, want 2", got)
	}
	defer eng.SetPeriodicActivityNarrowingDisabledForTest()()
	if got := eng.activeDeepFetches(3, 3); got != 3 {
		t.Errorf("non-vacuity: with the narrowing off all admissions count, got %d", got)
	}
}
