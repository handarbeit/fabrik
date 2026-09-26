package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// #1871: the two "resume an interrupted landing" branches must decide from a live
// Status read, not the batch's possibly stale board snapshot.

func landedCommentCount(client *mockGitHubClient) int {
	client.mu.Lock()
	defer client.mu.Unlock()
	n := 0
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "Landed via") {
			n++
		}
	}
	return n
}

func liveGuardFastPathFixture(t *testing.T, status string, statusErr error) (*Engine, *mockGitHubClient, *mergeTrainWorkerState, trialParams, trainMember) {
	t.Helper()
	client := &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, HeadSHA: "head-sha", MergeableState: "clean", Merged: true}, nil
		},
		fetchProjectItemStatusFn: func(string) (string, error) { return status, statusErr },
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", baseSHA: "base-sha", wm: wm, holdingStg: holdingStage(eng.cfg)}
	// Snapshot still says Queued regardless of the live status.
	m := trainMember{item: gh.ProjectItem{Number: 9, Title: "Issue Nine", ItemID: "item-9", Repo: "owner/repo", Status: "Queued"}, prNum: 90, headSHA: "head-sha"}
	return eng, client, state, p, m
}

func TestLiveLandingState_Outcomes(t *testing.T) {
	cases := []struct {
		name   string
		status string
		err    error
		want   landingLiveState
	}{
		{"holding", "Queued", nil, liveHolding},
		{"moved", "Done", nil, liveMoved},
		{"empty", "", nil, liveUnknown},
		{"error", "", errors.New("boom"), liveReadFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, _, state, p, m := liveGuardFastPathFixture(t, tc.status, tc.err)
			if got := eng.liveLandingState(state, p, m.item); got != tc.want {
				t.Errorf("liveLandingState = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLiveLandingState_FallsBackToLookupWhenNoItemID(t *testing.T) {
	eng, client, state, p, m := liveGuardFastPathFixture(t, "Queued", nil)
	client.lookupIssueProjectItemFn = func(projectID, repo string, n int) (string, string, error) {
		return "item-9", "Done", nil
	}
	m.item.ItemID = ""
	if got := eng.liveLandingState(state, p, m.item); got != liveMoved {
		t.Fatalf("liveLandingState = %d, want liveMoved", got)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.lookupIssueProjectItemCalls) != 1 || client.lookupIssueProjectItemCalls[0].projectID != "PVT_test" {
		t.Errorf("expected one lookup against PVT_test, got %+v", client.lookupIssueProjectItemCalls)
	}
	if len(client.fetchProjectItemStatusCalls) != 0 {
		t.Errorf("FetchProjectItemStatus must not be used without an ItemID")
	}
}

func TestTrySingletonFastPath_AlreadyMerged_StaleQueuedButLiveDone_DoesNotRelandNorTrial(t *testing.T) {
	eng, client, state, p, m := liveGuardFastPathFixture(t, "Done", nil)
	if !eng.trySingletonFastPath(context.Background(), state, p, m) {
		t.Fatal("expected disposition decided (true) so no trial is built")
	}
	client.mu.Lock()
	nUpdates := len(client.updateStatusCalls)
	client.mu.Unlock()
	if nUpdates != 0 || landedCommentCount(client) != 0 {
		t.Errorf("a live-Done member must not be re-landed: %d status updates, %d Landed comments", nUpdates, landedCommentCount(client))
	}
}

func TestTrySingletonFastPath_AlreadyMerged_LiveQueued_StillResumes(t *testing.T) {
	eng, client, state, p, m := liveGuardFastPathFixture(t, "Queued", nil)
	if !eng.trySingletonFastPath(context.Background(), state, p, m) {
		t.Fatal("expected handled")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("restart recovery must still move the member to Done once, got %d", len(client.updateStatusCalls))
	}
}

func TestTrySingletonFastPath_AlreadyMerged_ReadError_DefersThenResumes(t *testing.T) {
	eng, client, state, p, m := liveGuardFastPathFixture(t, "", errors.New("boom"))
	if !eng.trySingletonFastPath(context.Background(), state, p, m) {
		t.Fatal("expected disposition decided (defer) on a read error")
	}
	client.mu.Lock()
	if len(client.updateStatusCalls) != 0 {
		t.Fatalf("a deferred resume must write nothing, got %d status updates", len(client.updateStatusCalls))
	}
	client.fetchProjectItemStatusFn = func(string) (string, error) { return "Queued", nil }
	client.mu.Unlock()

	eng.trySingletonFastPath(context.Background(), state, p, m)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 1 {
		t.Errorf("next poll must complete the landing once, got %d status updates", len(client.updateStatusCalls))
	}
}

func TestTrySingletonFastPath_AlreadyMerged_GuardDisabled_Relands(t *testing.T) {
	eng, client, state, p, m := liveGuardFastPathFixture(t, "Done", nil)
	eng.SetMergeTrainLandingGuardDisabledForTest(true)
	eng.trySingletonFastPath(context.Background(), state, p, m)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 1 {
		t.Errorf("with the guard disabled the stale snapshot re-lands (non-vacuity), got %d status updates", len(client.updateStatusCalls))
	}
}

func liveGuardReconstructFixture(t *testing.T, statusFn func(itemID string) (string, error)) (*Engine, *mockGitHubClient, *mergeTrainWorkerState, trialParams) {
	t.Helper()
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	mergedPR := gh.PRDetails{
		Number:      400,
		State:       "closed",
		Merged:      true,
		HeadRefName: "fabrik/merge-train/merge-train-main-1",
		Body:        "batch: #1, #2\n" + mergeTrainBatchMarker,
	}
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	client.listPRsFn = func(owner, repo string) ([]gh.PRDetails, error) { return []gh.PRDetails{mergedPR}, nil }
	client.fetchProjectItemStatusFn = statusFn
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", "main"), state)
	p := reconstructParams(wm)
	p.holdingStg = holdingStage(eng.cfg)
	return eng, client, state, p
}

func TestReconstructTrainState_CompleteDeferredLanding_StaleQueuedButLiveDone_Skips(t *testing.T) {
	eng, client, state, p := liveGuardReconstructFixture(t, func(string) (string, error) { return "Done", nil })
	if !eng.reconstructTrainState(context.Background(), state, p, makeSeamBatch(2)) {
		t.Fatal("reconstructTrainState must still report handled so no fresh batch forms this poll")
	}
	client.mu.Lock()
	n := len(client.updateStatusCalls)
	client.mu.Unlock()
	if n != 0 || landedCommentCount(client) != 0 {
		t.Errorf("live-Done members must not be re-landed: %d status updates, %d Landed comments", n, landedCommentCount(client))
	}
}

func TestReconstructTrainState_CompleteDeferredLanding_MixedBatch_LandsOnlyHolding(t *testing.T) {
	eng, client, state, p := liveGuardReconstructFixture(t, func(itemID string) (string, error) {
		if itemID == "item-1" {
			return "Done", nil
		}
		return "Queued", nil
	})
	if !eng.reconstructTrainState(context.Background(), state, p, makeSeamBatch(2)) {
		t.Fatal("expected handled")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("only the still-Queued member should advance, got %d status updates", len(client.updateStatusCalls))
	}
	if got := client.updateStatusCalls[0].itemID; got != "item-2" {
		t.Errorf("advanced %q, want item-2", got)
	}
}

func TestReconstructTrainState_CompleteDeferredLanding_ReadError_DefersThenResumes(t *testing.T) {
	fail := true
	eng, client, state, p := liveGuardReconstructFixture(t, func(string) (string, error) {
		if fail {
			return "", errors.New("boom")
		}
		return "Queued", nil
	})
	eng.reconstructTrainState(context.Background(), state, p, makeSeamBatch(2))
	client.mu.Lock()
	n := len(client.updateStatusCalls)
	client.mu.Unlock()
	if n != 0 {
		t.Fatalf("a deferred resume must write nothing, got %d status updates", n)
	}
	fail = false
	eng.reconstructTrainState(context.Background(), state, p, makeSeamBatch(2))
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 2 {
		t.Errorf("next poll must land both members, got %d status updates", len(client.updateStatusCalls))
	}
}

func TestReconstructTrainState_CompleteDeferredLanding_GuardDisabled_Relands(t *testing.T) {
	eng, client, state, p := liveGuardReconstructFixture(t, func(string) (string, error) { return "Done", nil })
	eng.SetMergeTrainLandingGuardDisabledForTest(true)
	eng.reconstructTrainState(context.Background(), state, p, makeSeamBatch(2))
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 2 {
		t.Errorf("with the guard disabled the stale snapshot re-lands (non-vacuity), got %d status updates", len(client.updateStatusCalls))
	}
}

// dropLiveLandedMembers is the fresh-formation guard (#1871): a stale snapshot that
// lists just-landed members must not carry them into a fresh batch.
func TestDropLiveLandedMembers_KeepsOnlyStillHolding(t *testing.T) {
	statuses := map[string]string{"item-1": "Done", "item-2": "Queued", "item-3": "", "item-4": "Done"}
	errFor := map[string]error{"item-5": errors.New("boom")}
	client := &mockGitHubClient{
		fetchProjectItemStatusFn: func(id string) (string, error) { return statuses[id], errFor[id] },
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: wm, holdingStg: holdingStage(eng.cfg)}
	mk := func(n int, id string) trainMember {
		return trainMember{item: gh.ProjectItem{Number: n, ItemID: id, Repo: "owner/repo", Status: "Queued"}, prNum: n * 10, headSHA: "sha"}
	}
	in := []trainMember{mk(1, "item-1"), mk(2, "item-2"), mk(3, "item-3"), mk(4, "item-4"), mk(5, "item-5")}

	got := eng.dropLiveLandedMembers(state, p, in)
	var nums []int
	for _, m := range got {
		nums = append(nums, m.item.Number)
	}
	// #1 and #4 are live-Done (dropped), #5 read failed (excluded this batch); the
	// live-Queued #2 and the empty-status #3 (no positive evidence) are kept.
	if len(nums) != 2 || nums[0] != 2 || nums[1] != 3 {
		t.Fatalf("kept members = %v, want [2 3]", nums)
	}
}

func TestDropLiveLandedMembers_GuardDisabledKeepsAll(t *testing.T) {
	client := &mockGitHubClient{fetchProjectItemStatusFn: func(string) (string, error) { return "Done", nil }}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	eng.SetMergeTrainLandingGuardDisabledForTest(true)
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: wm, holdingStg: holdingStage(eng.cfg)}
	in := []trainMember{{item: gh.ProjectItem{Number: 1, ItemID: "item-1", Repo: "owner/repo"}, prNum: 10, headSHA: "sha"}}
	if got := eng.dropLiveLandedMembers(state, p, in); len(got) != 1 {
		t.Fatalf("with the guard disabled every member must be kept (non-vacuity), got %d", len(got))
	}
}
