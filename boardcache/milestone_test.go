package boardcache

import (
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

func milestonePayload(action, repo string, number int, milestoneJSON string) []byte {
	return []byte(fmt.Sprintf(
		`{"action":%q,"issue":{"number":%d,"node_id":"I_001","title":"t","milestone":%s},"repository":{"full_name":%q}}`,
		action, number, milestoneJSON, repo))
}

func TestIssuesMilestonedAndDemilestoned(t *testing.T) {
	c := seedCache(t)

	s := testGetState(t, c, "owner/repo", 1)
	if s.MilestoneKnown {
		t.Fatalf("probe-seeded item must start with unknown milestone, got known=%v ms=%+v", s.MilestoneKnown, s.Milestone)
	}

	c.ApplyDelta("issues", milestonePayload("milestoned", "owner/repo", 1, `{"title":"v2","number":9}`))
	s = testGetState(t, c, "owner/repo", 1)
	if !s.MilestoneKnown || s.Milestone == nil || s.Milestone.Title != "v2" || s.Milestone.Number != 9 {
		t.Fatalf("after milestoned: known=%v ms=%+v, want v2 #9", s.MilestoneKnown, s.Milestone)
	}

	c.ApplyDelta("issues", milestonePayload("demilestoned", "owner/repo", 1, `null`))
	s = testGetState(t, c, "owner/repo", 1)
	if !s.MilestoneKnown || s.Milestone != nil {
		t.Fatalf("after demilestoned: known=%v ms=%+v, want known-none", s.MilestoneKnown, s.Milestone)
	}
}

func TestIssuesMilestonedWithoutReadableMilestoneIsIgnored(t *testing.T) {
	c := seedCache(t)
	c.ApplyDelta("issues", []byte(`{"action":"milestoned","issue":{"number":1,"node_id":"I_001"},"repository":{"full_name":"owner/repo"}}`))
	s := testGetState(t, c, "owner/repo", 1)
	if s.MilestoneKnown {
		t.Fatalf("payload without issue.milestone must not record 'none': known=%v", s.MilestoneKnown)
	}
}

func TestIssuesOpenedCarriesMilestone(t *testing.T) {
	c := seedCache(t)
	c.ApplyDelta("issues", milestonePayload("opened", "owner/repo", 7, `{"title":"v3","number":4}`))
	s := testGetState(t, c, "owner/repo", 7)
	if !s.MilestoneKnown || s.Milestone == nil || s.Milestone.Number != 4 {
		t.Fatalf("opened: known=%v ms=%+v, want #4", s.MilestoneKnown, s.Milestone)
	}
}

func TestReconcileCapturesMilestoneAndDeepFetchDoesNotWipeIt(t *testing.T) {
	c := seedCache(t)
	c.Reconcile(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research",
				Milestone: &gh.Milestone{Title: "v1", Number: 1}, MilestoneKnown: true},
			{ID: "I_002", ItemID: "PVTI_002", Number: 2, Repo: "owner/repo", Status: "Plan", MilestoneKnown: true},
		},
	})
	s1 := testGetState(t, c, "owner/repo", 1)
	if !s1.MilestoneKnown || s1.Milestone == nil || s1.Milestone.Title != "v1" {
		t.Fatalf("reconcile did not capture milestone: known=%v ms=%+v", s1.MilestoneKnown, s1.Milestone)
	}
	s2 := testGetState(t, c, "owner/repo", 2)
	if !s2.MilestoneKnown || s2.Milestone != nil {
		t.Fatalf("no-milestone item: known=%v ms=%+v, want known-none", s2.MilestoneKnown, s2.Milestone)
	}

	// A deep fetch whose item does not carry the milestone (MilestoneKnown
	// false) must not wipe the captured value.
	c.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{
		ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research",
		Body: "deep",
	}})
	s1 = testGetState(t, c, "owner/repo", 1)
	if !s1.MilestoneKnown || s1.Milestone == nil || s1.Milestone.Title != "v1" {
		t.Fatalf("deep fetch without milestone wiped it: known=%v ms=%+v", s1.MilestoneKnown, s1.Milestone)
	}

	// And the board read path carries it back out.
	pi := snapshotToProjectItem(mustGet(t, c, 1))
	if !pi.MilestoneKnown || pi.Milestone == nil || pi.Milestone.Number != 1 {
		t.Fatalf("snapshotToProjectItem lost milestone: %+v", pi)
	}
}

func mustGet(t *testing.T, c *CacheImpl, n int) itemstate.Snapshot {
	t.Helper()
	snap, err := c.store.Get("owner/repo", n)
	if err != nil {
		t.Fatalf("store.Get(%d): %v", n, err)
	}
	return snap
}

// milestoned/demilestoned for an item the store does not hold must not fetch it
// from GitHub (R10 rules out new API calls); the next reconcile captures it.
func TestIssuesMilestonedUncachedItemMakesNoGitHubCall(t *testing.T) {
	mc := &mockClient{projectItemResult: &gh.ProjectItem{ID: "I_9", ItemID: "PVTI_9", Number: 9, Repo: "owner/repo", Status: "Plan"}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	c.BootstrapFromProbe([]gh.BoardProbeItem{{ContentID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research"}}, "PID")

	c.ApplyDelta("issues", milestonePayload("milestoned", "owner/repo", 9, `{"title":"v2","number":9}`))
	c.ApplyDelta("issues", milestonePayload("demilestoned", "owner/repo", 9, `null`))

	if mc.fetchProjectItemCount != 0 {
		t.Fatalf("FetchProjectItem called %d time(s) for an uncached item; R10 allows none", mc.fetchProjectItemCount)
	}
	if _, ok := c.store.Peek("owner/repo", 9); ok {
		t.Fatal("an uncached item must not be added to the store by a milestone event")
	}
}

func TestApplyBoardMilestonesOnlyTouchesCachedKnownItems(t *testing.T) {
	c := seedCache(t)
	c.ApplyBoardMilestones(&gh.ProjectBoard{Items: []gh.ProjectItem{
		{Number: 1, Repo: "owner/repo", Milestone: &gh.Milestone{Title: "v1", Number: 1}, MilestoneKnown: true},
		{Number: 2, Repo: "owner/repo"}, // milestone not captured on this item: stays unknown
		{Number: 7, Repo: "owner/repo", MilestoneKnown: true},
	}})
	if s := testGetState(t, c, "owner/repo", 1); !s.MilestoneKnown || s.Milestone == nil || s.Milestone.Title != "v1" {
		t.Fatalf("#1: known=%v ms=%+v", s.MilestoneKnown, s.Milestone)
	}
	if s := testGetState(t, c, "owner/repo", 2); s.MilestoneKnown {
		t.Fatalf("#2 must stay unknown, got known=%v", s.MilestoneKnown)
	}
	if _, ok := c.store.Peek("owner/repo", 7); ok {
		t.Fatal("an item absent from the store must not be created")
	}
	c.ApplyBoardMilestones(nil) // must not panic
}
