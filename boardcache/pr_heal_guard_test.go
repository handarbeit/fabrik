package boardcache

import (
	"encoding/json"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// seedMembers returns a cache holding hydrated, unlinked items 1..n.
func seedMembers(t *testing.T, mc *mockClient, n int) *CacheImpl {
	t.Helper()
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	var items []gh.ProjectItem
	for i := 1; i <= n; i++ {
		items = append(items, gh.ProjectItem{ID: "I", ItemID: "PVTI", Number: i, Repo: "owner/repo", Status: "Queued"})
	}
	testBootstrapFromBoard(c, &gh.ProjectBoard{ProjectID: "PID", Items: items})
	for i := 1; i <= n; i++ {
		testSetDeepFetched(c, "owner/repo", i)
	}
	return c
}

func trainPRPayload(action string, prNum int, ref string) []byte {
	p := pullRequestPayload{Action: action}
	p.Repository.FullName = "owner/repo"
	p.PullRequest.Number = prNum
	p.PullRequest.State = "open"
	p.PullRequest.Head.SHA = "sha-train"
	p.PullRequest.Head.Ref = ref
	b, _ := json.Marshal(p)
	return b
}

func assertMembersUntouched(t *testing.T, c *CacheImpl, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		s := testGetState(t, c, "owner/repo", i)
		if s.LinkedPR != nil && (s.LinkedPR.Number != 0 || s.LinkedPR.HeadSHA != "") {
			t.Errorf("member #%d: linked PR changed: %+v", i, s.LinkedPR)
		}
		if !c.IsItemDeepFetched("owner/repo", i) {
			t.Errorf("member #%d: deep cache invalidated", i)
		}
	}
}

// A multi-member PR is attached to none of its closing issues, whatever the
// order the body names them in (#2483, FR-003, FR-005).
func TestResolvePRLinkage_RefusesMultipleCachedClosingIssues(t *testing.T) {
	for _, order := range [][]int{{1, 2, 3}, {3, 2, 1}} {
		for _, disabled := range []bool{false, true} {
			order, disabled := order, disabled
			mc := &mockClient{fetchPRClosingIssuesFn: func(owner, repo string, n int) ([]int, error) { return order, nil }}
			c := seedMembers(t, mc, 3)
			c.SetPRHealGuardDisabledForTest(disabled)
			_, issNum, found, healed, err := c.resolvePRLinkage("owner", "repo", 99)
			if err != nil {
				t.Fatal(err)
			}
			if disabled {
				// Neutralised: the first-named issue wins, as before the fix.
				if !found || !healed || issNum != order[0] {
					t.Errorf("order=%v disabled: want first-named %d, got found=%v issue=%d", order, order[0], found, issNum)
				}
				continue
			}
			if found || healed {
				t.Errorf("order=%v: want refusal, got found=%v healed=%v issue=%d", order, found, healed, issNum)
			}
		}
	}
}

func TestResolvePRLinkage_SingleCachedClosingIssueStillHeals(t *testing.T) {
	// One cached issue (#1) and one uncached (#500): behaves as before.
	mc := &mockClient{fetchPRClosingIssuesFn: func(owner, repo string, n int) ([]int, error) { return []int{500, 1}, nil }}
	c := seedMembers(t, mc, 1)
	_, issNum, found, healed, err := c.resolvePRLinkage("owner", "repo", 99)
	if err != nil || !found || !healed || issNum != 1 {
		t.Errorf("want heal onto #1, got issue=%d found=%v healed=%v err=%v", issNum, found, healed, err)
	}
}

func TestResolvePRLinkage_AlreadyLinkedUnaffectedByMultiIssue(t *testing.T) {
	mc := &mockClient{fetchPRClosingIssuesFn: func(owner, repo string, n int) ([]int, error) { return []int{1, 2}, nil }}
	c := seedMembers(t, mc, 2)
	c.RecordPRLinkage("owner/repo", 99, 2)
	_, issNum, found, healed, _ := c.resolvePRLinkage("owner", "repo", 99)
	if !found || healed || issNum != 2 || mc.fetchPRClosingIssuesCount != 0 {
		t.Errorf("want authoritative hit on #2, got issue=%d found=%v healed=%v calls=%d", issNum, found, healed, mc.fetchPRClosingIssuesCount)
	}
}

func TestTrainPR_PullRequestDeliveryLeavesMembersUntouched(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		// Head-branch gate alone: the body names exactly one cached issue, so the
		// multi-issue refusal cannot be what protects it (a singleton train PR).
		mc := &mockClient{fetchPRClosingIssuesFn: func(owner, repo string, n int) ([]int, error) { return []int{1}, nil }}
		c := seedMembers(t, mc, 3)
		c.SetPRHealGuardDisabledForTest(disabled)
		c.ApplyDelta("pull_request", trainPRPayload("opened", 99, "fabrik/merge-train/trial-1"))
		if disabled {
			if s := testGetState(t, c, "owner/repo", 1); s.LinkedPR == nil || s.LinkedPR.Number != 99 {
				t.Errorf("neutralised: want the old heal onto #1, got %+v", s.LinkedPR)
			}
			continue
		}
		if mc.fetchPRClosingIssuesCount != 0 {
			t.Errorf("want 0 FetchPRClosingIssues calls, got %d", mc.fetchPRClosingIssuesCount)
		}
		assertMembersUntouched(t, c, 3)
	}
}

func TestTrainPR_AlreadyPoisonedIndexIsNotWrittenThrough(t *testing.T) {
	mc := &mockClient{}
	c := seedMembers(t, mc, 2)
	c.RecordPRLinkage("owner/repo", 99, 1) // earlier mis-attach
	before := testGetState(t, c, "owner/repo", 1).LinkedPR
	c.ApplyDelta("pull_request", trainPRPayload("synchronize", 99, "fabrik/merge-train/trial-1"))
	after := testGetState(t, c, "owner/repo", 1).LinkedPR
	if (before == nil) != (after == nil) || (before != nil && before.HeadSHA != after.HeadSHA) {
		t.Errorf("poisoned member rewritten: before=%+v after=%+v", before, after)
	}
	if !c.IsItemDeepFetched("owner/repo", 1) || !c.IsItemDeepFetched("owner/repo", 2) {
		t.Error("deep cache invalidated")
	}
}

func TestTrainPR_MemberOwnPRStillHeals(t *testing.T) {
	mc := &mockClient{fetchPRClosingIssuesFn: func(owner, repo string, n int) ([]int, error) { return []int{1}, nil }}
	c := seedMembers(t, mc, 2)
	c.ApplyDelta("pull_request", trainPRPayload("opened", 42, "fabrik/issue-1"))
	if s := testGetState(t, c, "owner/repo", 1); s.LinkedPR == nil || s.LinkedPR.Number != 42 {
		t.Errorf("want member's own PR healed onto #1, got %+v", s.LinkedPR)
	}
}

func TestTrainPR_ReviewDeliveriesIgnored(t *testing.T) {
	mc := &mockClient{fetchPRClosingIssuesFn: func(owner, repo string, n int) ([]int, error) { return []int{1}, nil }}
	c := seedMembers(t, mc, 2)

	rv := pullRequestReviewPayload{Action: "submitted"}
	rv.Repository.FullName = "owner/repo"
	rv.PullRequest.Number = 99
	rv.PullRequest.Head.Ref = "fabrik/merge-train/trial-1"
	rv.Review.DatabaseID = 7
	rv.Review.State = "commented"
	b, _ := json.Marshal(rv)
	c.ApplyDelta("pull_request_review", b)

	rc := pullRequestReviewCommentPayload{Action: "created"}
	rc.Repository.FullName = "owner/repo"
	rc.PullRequest.Number = 99
	rc.PullRequest.Head.Ref = "fabrik/merge-train/trial-1"
	rc.Comment.DatabaseID = 8
	b, _ = json.Marshal(rc)
	c.ApplyDelta("pull_request_review_comment", b)

	if mc.fetchPRClosingIssuesCount != 0 {
		t.Errorf("want 0 FetchPRClosingIssues calls, got %d", mc.fetchPRClosingIssuesCount)
	}
	assertMembersUntouched(t, c, 2)
}

func trainCheckRunPayload(sha, suiteBranch, prRef string) []byte {
	p := checkRunPayload{Action: "completed"}
	p.Repository.FullName = "owner/repo"
	p.CheckRun.ID = 5
	p.CheckRun.Name = "build"
	p.CheckRun.Status = "completed"
	p.CheckRun.Conclusion = "success"
	p.CheckRun.HeadSHA = sha
	p.CheckRun.CheckSuite.HeadBranch = suiteBranch
	if prRef != "" {
		p.CheckRun.PullRequests = append(p.CheckRun.PullRequests, struct {
			Number int `json:"number"`
			Head   struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}{Number: 99, Head: struct {
			Ref string `json:"ref"`
		}{Ref: prRef}})
	}
	b, _ := json.Marshal(p)
	return b
}

func TestTrainPR_CheckRunDroppedWithoutBuffering(t *testing.T) {
	cases := map[string][]byte{
		"suite head_branch": trainCheckRunPayload("sha-train", "fabrik/merge-train/trial-1", ""),
		"pull_requests ref": trainCheckRunPayload("sha-train", "", "fabrik/merge-train/trial-1"),
	}
	for name, payload := range cases {
		mc := &mockClient{
			fetchPRsForSHAFn: func(owner, repo, sha string) ([]int, error) { return []int{99}, nil },
			fetchPRClosingIssuesFn: func(owner, repo string, n int) ([]int, error) {
				return []int{1}, nil
			},
		}
		c := seedMembers(t, mc, 2)
		c.ApplyDelta("check_run", payload)
		if got := c.store.CheckRunsBySHA("sha-train"); len(got) != 0 {
			t.Errorf("%s: want nothing buffered, got %+v", name, got)
		}
		if mc.fetchPRClosingIssuesCount != 0 {
			t.Errorf("%s: want 0 FetchPRClosingIssues calls, got %d", name, mc.fetchPRClosingIssuesCount)
		}
		assertMembersUntouched(t, c, 2)
	}
}

// Without branch fields the multi-closing-issue refusal is the backstop.
func TestTrainPR_CheckRunWithoutBranchFallsBackToRefusal(t *testing.T) {
	mc := &mockClient{
		fetchPRsForSHAFn:       func(owner, repo, sha string) ([]int, error) { return []int{99}, nil },
		fetchPRClosingIssuesFn: func(owner, repo string, n int) ([]int, error) { return []int{1, 2, 3}, nil },
	}
	c := seedMembers(t, mc, 3)
	c.ApplyDelta("check_run", trainCheckRunPayload("sha-train", "", ""))
	assertMembersUntouched(t, c, 3)
}

func TestIsTrainBranch(t *testing.T) {
	for ref, want := range map[string]bool{
		"fabrik/merge-train/trial-1": true,
		"fabrik/issue-12":            false,
		"main":                       false,
		"":                           false,
	} {
		if got := isTrainBranch(ref); got != want {
			t.Errorf("isTrainBranch(%q) = %v, want %v", ref, got, want)
		}
	}
}
