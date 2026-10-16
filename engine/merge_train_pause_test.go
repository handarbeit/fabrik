package engine

import (
	"context"
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// R3 (#2341, ADR 2341): the merge-train landing paths re-check the pause per member,
// live, strictly before the irreversible merge. A paused member refuses the whole
// landing: every member stays in Queued and nothing is ejected, counted or paused.

func pausedMemberLabels(paused ...int) func(owner, repo string, n int) ([]string, error) {
	set := map[int]bool{}
	for _, n := range paused {
		set[n] = true
	}
	return func(owner, repo string, n int) ([]string, error) {
		if set[n] {
			return []string{"fabrik:paused", "fabrik:awaiting-input"}, nil
		}
		return nil, nil
	}
}

func TestPausedTrainMembers_LiveReadNotSnapshot(t *testing.T) {
	client := &mockGitHubClient{fetchLabelsFn: pausedMemberLabels(2)}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	members := []trainMember{makeQueuedMember(1, 10, "One"), makeQueuedMember(2, 11, "Two")}
	// The batch-formation snapshots carry no pause; only the live read does.
	got, unread := eng.pausedTrainMembers("owner", "repo", members)
	if len(got) != 1 || got[0] != 2 || len(unread) != 0 {
		t.Fatalf("paused = %v unread = %v, want [2] and none", got, unread)
	}
}

// A failed live read is "unknown", never "not paused": the batch-formation snapshot
// cannot carry fabrik:paused (paused members are excluded at formation), so it must
// not be consulted as a fallback.
func TestPausedTrainMembers_ReadErrorIsUnread(t *testing.T) {
	client := &mockGitHubClient{fetchLabelsFn: func(owner, repo string, n int) ([]string, error) {
		if n == 2 {
			return nil, errors.New("rate limited")
		}
		return nil, nil
	}}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	members := []trainMember{makeQueuedMember(1, 10, "One"), makeQueuedMember(2, 11, "Two")}

	paused, unread := eng.pausedTrainMembers("owner", "repo", members)
	if len(paused) != 0 || len(unread) != 1 || unread[0] != 2 {
		t.Fatalf("paused = %v unread = %v, want none and [2]", paused, unread)
	}
	if !eng.refuseIfMembersPaused("owner", "repo", members) {
		t.Fatal("an unreadable member must refuse the landing")
	}
}

// A FetchLabels error for one member refuses the whole batch landing: nothing is
// opened, merged, moved, closed or labelled, and every member stays in Queued.
func TestLandMergeTrainBatch_UnreadableMember_RefusesWholeBatch(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(1, 10, "One"), makeQueuedMember(2, 11, "Two")}
	eng, client, createPR, mergePR := batchPauseFixture(t)
	client.fetchLabelsFn = func(owner, repo string, n int) ([]string, error) {
		if n == 2 {
			return nil, errors.New("rate limited")
		}
		return nil, nil
	}
	wm := eng.worktreesFor("owner/repo")
	trainKey := defaultTrainKey("owner", "repo")
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(trainKey, state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", trainKey, survivors, wm)

	if *createPR || *mergePR {
		t.Errorf("a batch with an unreadable member must not open or merge a PR (create=%v merge=%v)", *createPR, *mergePR)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 0 || len(client.closeIssueCalls) != 0 ||
		len(client.addCommentCalls) != 0 || len(client.addLabelCalls) != 0 {
		t.Error("a refusal must leave every member untouched")
	}
}

func TestTrySingletonFastPath_UnreadableLabels_NoMerge(t *testing.T) {
	eng, client, state, p, m, merges := fastPathPauseFixture(t, false)
	client.fetchLabelsFn = func(owner, repo string, n int) ([]string, error) {
		return nil, errors.New("rate limited")
	}
	if !eng.trySingletonFastPath(context.Background(), state, p, m) {
		t.Fatal("an unreadable member must decide the disposition (true) so no trial is built")
	}
	if *merges != 0 {
		t.Errorf("a member whose labels cannot be read must not be merged, got %d", *merges)
	}
}

func batchPauseFixture(t *testing.T, paused ...int) (*Engine, *mockGitHubClient, *bool, *bool) {
	t.Helper()
	createPRCalled, mergePRCalled := false, false
	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			createPRCalled = true
			return 100, nil
		},
		fetchLabelsFn: pausedMemberLabels(paused...),
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			mergePRCalled = true
			return nil
		},
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	return eng, client, &createPRCalled, &mergePRCalled
}

func TestLandMergeTrainBatch_PausedMember_RefusesWholeBatch(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(1, 10, "One"), makeQueuedMember(2, 11, "Two")}
	eng, client, createPR, mergePR := batchPauseFixture(t, 2)
	wm := eng.worktreesFor("owner/repo")
	trainKey := defaultTrainKey("owner", "repo")
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(trainKey, state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", trainKey, survivors, wm)

	if *createPR || *mergePR {
		t.Errorf("a batch with a paused member must not open or merge a PR (create=%v merge=%v)", *createPR, *mergePR)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if n := len(client.updateStatusCalls); n != 0 {
		t.Errorf("members must stay in Queued, got %d status update(s)", n)
	}
	if n := len(client.closeIssueCalls); n != 0 {
		t.Errorf("nothing may be closed, got %d", n)
	}
	if n := len(client.addCommentCalls); n != 0 {
		t.Errorf("a refusal posts no comment, got %d", n)
	}
	if n := len(client.addLabelCalls); n != 0 {
		t.Errorf("a refusal adds no label (no pause, no eject), got %d", n)
	}
}

func TestLandMergeTrainBatch_PauseLifted_Proceeds(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(1, 10, "One")}
	eng, _, createPR, _ := batchPauseFixture(t /* nobody paused */)
	wm := eng.worktreesFor("owner/repo")
	trainKey := defaultTrainKey("owner", "repo")
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(trainKey, state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", trainKey, survivors, wm)

	if !*createPR {
		t.Error("with no member paused the landing must proceed")
	}
}

// An already-merged integration PR is a resumed landing: its Done move must finish
// even though a member is paused.
func TestLandMergeTrainBatch_PausedMember_AlreadyMergedPR_Exempt(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(1, 10, "One")}
	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) {
			return []gh.PRDetails{
				{Number: 300, State: "closed", Merged: true, HeadRefName: trainBranchPrefix + "merge-train-main-12345", Body: mergeTrainBatchMarker + " #1"},
			}, nil
		},
		fetchLabelsFn: pausedMemberLabels(1),
		addCommentFn:  func(owner, repo string, n int, body string) (int, error) { return 1, nil },
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	trainKey := defaultTrainKey("owner", "repo")
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(trainKey, state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", trainKey, survivors, wm)

	client.mu.Lock()
	defer client.mu.Unlock()
	if n := len(client.updateStatusCalls); n != 1 {
		t.Errorf("an already-merged batch must still advance to Done, got %d status update(s)", n)
	}
}

func TestLandSingleton_PausedMember_Refuses(t *testing.T) {
	m := makeQueuedMember(9, 90, "Issue Nine")
	eng, client, createPR, mergePR := batchPauseFixture(t, 9)
	wm := eng.worktreesFor("owner/repo")
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{
		owner: "owner", repo: "repo", baseBranch: "main",
		trainKey: defaultTrainKey("owner", "repo"),
		wm:       wm, holdingStg: holdingStage(eng.cfg),
	}

	eng.landSingleton(context.Background(), state, p, m, "merge-train-singleton-paused")

	if *createPR || *mergePR {
		t.Errorf("a paused singleton must not be landed (create=%v merge=%v)", *createPR, *mergePR)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 0 || len(client.closeIssueCalls) != 0 || len(client.addLabelCalls) != 0 {
		t.Error("a refusal must leave the member untouched")
	}
}

func fastPathPauseFixture(t *testing.T, paused bool) (*Engine, *mockGitHubClient, *mergeTrainWorkerState, trialParams, trainMember, *int) {
	t.Helper()
	merges := 0
	var pausedNums []int
	if paused {
		pausedNums = []int{9}
	}
	client := &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, HeadSHA: "head-sha", MergeableState: "clean"}, nil
		},
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) { return 0, nil },
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{{ID: 1, Name: "test", Status: "completed", Conclusion: "success"}}, nil
		},
		fetchLabelsFn: pausedMemberLabels(pausedNums...),
		mergePRAtHeadSHAFn: func(owner, repo string, prNumber int, sha string) error {
			merges++
			return nil
		},
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", baseSHA: "base-sha", wm: wm, holdingStg: holdingStage(eng.cfg)}
	m := trainMember{item: gh.ProjectItem{Number: 9, Title: "Issue Nine", ItemID: "item-9", Repo: "owner/repo", Status: "Queued"}, prNum: 90, headSHA: "head-sha"}
	return eng, client, state, p, m, &merges
}

func TestTrySingletonFastPath_PausedMember_NoMergeNoTrial(t *testing.T) {
	eng, client, state, p, m, merges := fastPathPauseFixture(t, true)

	if !eng.trySingletonFastPath(context.Background(), state, p, m) {
		t.Fatal("a paused member must decide the disposition (true) so no trial is built for it")
	}
	if *merges != 0 {
		t.Errorf("a paused member must not be merged, got %d MergePRAtHeadSHA call(s)", *merges)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updateStatusCalls) != 0 || len(client.addLabelCalls) != 0 {
		t.Error("a refusal must leave the member untouched")
	}
}

func TestTrySingletonFastPath_PauseLifted_Merges(t *testing.T) {
	eng, _, state, p, m, merges := fastPathPauseFixture(t, false)
	if !eng.trySingletonFastPath(context.Background(), state, p, m) {
		t.Fatal("expected the fast path to be taken")
	}
	if *merges != 1 {
		t.Errorf("an unpaused member must be merged once, got %d", *merges)
	}
}

func TestTrainPauseGuard_NonVacuity(t *testing.T) {
	eng, _, state, p, m, merges := fastPathPauseFixture(t, true)
	eng.SetPauseGuardDisabledForTest(true)
	eng.trySingletonFastPath(context.Background(), state, p, m)
	if *merges != 1 {
		t.Errorf("with the guard disabled the paused member is merged; got %d", *merges)
	}
}
