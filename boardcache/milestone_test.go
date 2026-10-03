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
