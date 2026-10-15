package engine

import (
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func anchorStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Implement", Order: 2},
		{Name: "Validate", Order: 3},
		{Name: "Backlog", Order: 4, Unmanaged: true},
		{Name: "Done", Order: 5, CleanupWorktree: true},
	}
}

func anchorEngine(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	return testEngineWithStages(t, client, anchorStages())
}

func seedAnchorItem(e *Engine, n int, status string, labels ...string) gh.ProjectItem {
	it := gh.ProjectItem{ID: "I", Number: n, Repo: "owner/repo", Status: status, Labels: labels, ItemID: "PVTI"}
	e.store.Apply(itemstate.IssueOpened{Item: it})
	return it
}

func TestSettleDecisionAnchors_OneReadPerItemLabelAndItIsRecorded(t *testing.T) {
	client := &mockGitHubClient{}
	applied := time.Now().Add(-68 * 24 * time.Hour)
	client.fetchLabelAppliedAtFn = func(owner, repo string, n int, label string) (time.Time, error) { return applied, nil }
	e := anchorEngine(t, client)
	it := seedAnchorItem(e, 1, "Specify", "stage:Specify:complete")
	board := &gh.ProjectBoard{Items: []gh.ProjectItem{it}}

	e.settleDecisionAnchors(board)
	e.settleDecisionAnchors(board)

	if n := len(client.fetchLabelAppliedAtCalls); n != 1 {
		t.Fatalf("FetchLabelAppliedAt calls = %d, want 1 (the second pass finds the recorded time)", n)
	}
	if c := client.fetchLabelAppliedAtCalls[0]; c.labelName != "stage:Specify:complete" || c.issueNumber != 1 {
		t.Errorf("call = %+v", c)
	}
	snap, _ := e.store.Peek("owner/repo", 1)
	if got := snap.LabelAppliedAt("stage:Specify:complete"); !got.Equal(applied) {
		t.Errorf("recorded = %v, want %v", got, applied)
	}
}

func TestSettleDecisionAnchors_OnlyItemsThatWouldBeSuspended(t *testing.T) {
	client := &mockGitHubClient{}
	e := anchorEngine(t, client)
	var items []gh.ProjectItem
	items = append(items,
		seedAnchorItem(e, 1, "Specify", "stage:Specify:complete", "fabrik:cruise"), // auto-advances
		seedAnchorItem(e, 2, "Validate", "stage:Validate:complete"),                // Validate is excluded
		seedAnchorItem(e, 3, "Specify"),                                            // not complete
		seedAnchorItem(e, 4, "Specify", "stage:Specify:complete", "fabrik:paused"), // paused
		seedAnchorItem(e, 5, "Backlog", "stage:Specify:complete"),                  // parking column
	)
	e.settleDecisionAnchors(&gh.ProjectBoard{Items: items})
	if n := len(client.fetchLabelAppliedAtCalls); n != 0 {
		t.Fatalf("%d reads spent on items that cannot be suspended", n)
	}
	// Control: the same item without the guard reads.
	it := seedAnchorItem(e, 6, "Specify", "stage:Specify:complete")
	e.settleDecisionAnchors(&gh.ProjectBoard{Items: []gh.ProjectItem{it}})
	if n := len(client.fetchLabelAppliedAtCalls); n != 1 {
		t.Fatalf("control: %d reads, want 1", n)
	}
}

func TestSettleDecisionAnchors_ParkedBlockerReadsFabrikBlocked(t *testing.T) {
	client := &mockGitHubClient{}
	client.fetchLabelAppliedAtFn = func(owner, repo string, n int, label string) (time.Time, error) {
		return time.Now().Add(-time.Hour), nil
	}
	e := anchorEngine(t, client)
	blocked := seedAnchorItem(e, 1, "Specify", "fabrik:blocked")
	seedAnchorItem(e, 2, "Backlog")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{
		ID: "I", Number: 1, Repo: "owner/repo", Status: "Specify", Labels: []string{"fabrik:blocked"},
		BlockedBy: []gh.Dependency{{Number: 2, State: "OPEN"}},
	}})
	e.settleDecisionAnchors(&gh.ProjectBoard{Items: []gh.ProjectItem{blocked}})
	if n := len(client.fetchLabelAppliedAtCalls); n != 1 || client.fetchLabelAppliedAtCalls[0].labelName != "fabrik:blocked" {
		t.Fatalf("calls = %+v", client.fetchLabelAppliedAtCalls)
	}
	// Control: a blocker in an active column is a plain wait; no read.
	client.fetchLabelAppliedAtCalls = nil
	seedAnchorItem(e, 4, "Implement")
	blocked2 := seedAnchorItem(e, 3, "Specify", "fabrik:blocked")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 3, FreshState: gh.ProjectItem{
		ID: "I", Number: 3, Repo: "owner/repo", Status: "Specify", Labels: []string{"fabrik:blocked"},
		BlockedBy: []gh.Dependency{{Number: 4, State: "OPEN"}},
	}})
	e.settleDecisionAnchors(&gh.ProjectBoard{Items: []gh.ProjectItem{blocked2}})
	if n := len(client.fetchLabelAppliedAtCalls); n != 0 {
		t.Fatalf("an active-column blocker spent %d reads", n)
	}
}

func TestSettleDecisionAnchors_CapAndBackoff(t *testing.T) {
	client := &mockGitHubClient{}
	failing := true
	client.fetchLabelAppliedAtFn = func(owner, repo string, n int, label string) (time.Time, error) {
		if failing {
			return time.Time{}, errors.New("boom")
		}
		return time.Now().Add(-time.Hour), nil
	}
	e := anchorEngine(t, client)
	var items []gh.ProjectItem
	for i := 1; i <= 12; i++ {
		items = append(items, seedAnchorItem(e, i, "Specify", "stage:Specify:complete"))
	}
	board := &gh.ProjectBoard{Items: items}

	e.settleDecisionAnchors(board)
	if n := len(client.fetchLabelAppliedAtCalls); n != decisionAnchorPerPoll {
		t.Fatalf("first poll made %d reads, want the cap of %d", n, decisionAnchorPerPoll)
	}
	// The failed ones are backed off; the remaining 4 get their turn.
	e.settleDecisionAnchors(board)
	if n := len(client.fetchLabelAppliedAtCalls); n != 12 {
		t.Fatalf("second poll: total %d reads, want 12 (4 new, none of the backed-off retried)", n)
	}
	// After the retry interval, with the API healthy again, they are retried.
	failing = false
	e.SetClock(fixedClock{time.Now().Add(decisionAnchorRetry + time.Minute)})
	e.settleDecisionAnchors(board)
	if n := len(client.fetchLabelAppliedAtCalls); n != 12+decisionAnchorPerPoll {
		t.Fatalf("after the backoff: total %d reads, want %d", n, 12+decisionAnchorPerPoll)
	}
}

func TestSettleDecisionAnchors_MissIsRemembered(t *testing.T) {
	client := &mockGitHubClient{}
	client.fetchLabelAppliedAtFn = func(owner, repo string, n int, label string) (time.Time, error) {
		return time.Time{}, nil // no label event found
	}
	e := anchorEngine(t, client)
	it := seedAnchorItem(e, 1, "Specify", "stage:Specify:complete")
	board := &gh.ProjectBoard{Items: []gh.ProjectItem{it}}
	e.settleDecisionAnchors(board)
	e.settleDecisionAnchors(board)
	if n := len(client.fetchLabelAppliedAtCalls); n != 1 {
		t.Fatalf("a miss was re-read %d times within the backoff", n)
	}
}
