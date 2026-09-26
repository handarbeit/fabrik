package engine

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// queuedCommentItem returns a Queued item carrying the given comments.
func queuedCommentItem(number int, comments ...gh.Comment) gh.ProjectItem {
	return gh.ProjectItem{
		Number:   number,
		ItemID:   "PVTI_" + string(rune('0'+number)),
		Repo:     "owner/repo",
		Status:   "Queued",
		Comments: comments,
	}
}

func queuedHumanComment(id, body string) gh.Comment {
	return gh.Comment{ID: id, DatabaseID: 1, Author: "alice", Body: body}
}

func commentEjectCount(eng *Engine, key string) int {
	eng.mergeTrainEjectionsMu.Lock()
	defer eng.mergeTrainEjectionsMu.Unlock()
	return eng.mergeTrainEjectionCounts[key]
}

func TestPendingCommentEject_OneShotAndPerRepo(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	if eng.takePendingCommentEject("owner/repo", 1) {
		t.Fatal("expected no signal before mark")
	}
	eng.markPendingCommentEject("owner/repo", 1)
	eng.markPendingCommentEject("owner/other", 1)
	if !eng.takePendingCommentEject("owner/repo", 1) {
		t.Fatal("expected pending signal for owner/repo#1")
	}
	if eng.takePendingCommentEject("owner/repo", 1) {
		t.Error("signal must be one-shot")
	}
	if !eng.takePendingCommentEject("owner/other", 1) {
		t.Error("per-repo isolation: owner/other#1 must be independent")
	}

	eng.markPendingCommentEject("owner/repo", 2)
	eng.clearPendingCommentEject("owner/repo", 2)
	if eng.takePendingCommentEject("owner/repo", 2) {
		t.Error("cleared signal must not be taken")
	}
}

func TestEjectQueuedMemberForComments_RerouteSuccess(t *testing.T) {
	client := &mockGitHubClient{}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	item := queuedCommentItem(1, queuedHumanComment("c1", "please change X"))

	eng.ejectQueuedMemberForComments("PVT_1", item)

	if len(client.updateStatusCalls) != 1 || client.updateStatusCalls[0].optionID != "opt-implement" {
		t.Fatalf("expected one reroute to the stage before Queued, got %+v", client.updateStatusCalls)
	}
	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one explanatory comment, got %d", len(calls))
	}
	if !strings.HasPrefix(calls[0].body, "🏭 **Fabrik") {
		t.Errorf("comment must carry the 🏭 **Fabrik prefix, got: %s", calls[0].body)
	}
	if !strings.Contains(calls[0].body, "unprocessed comment") {
		t.Errorf("comment must name the cause, got: %s", calls[0].body)
	}
	if n := commentEjectCount(eng, "owner/repo#1"); n != 0 {
		t.Errorf("comment eject must not count toward MaxMergeTrainEjections, got %d", n)
	}
	if len(client.addLabelCalls) != 0 || len(client.removeLabelCalls) != 0 {
		t.Errorf("comment eject must not mutate labels (no pause, no cycle reset): add=%v remove=%v", client.addLabelCalls, client.removeLabelCalls)
	}

	// PAT-mode self-trigger guard: the eject comment must never be re-flagged.
	posted := gh.Comment{ID: "c2", Author: "alice", Body: calls[0].body}
	self := queuedCommentItem(1, posted)
	if got := eng.findNewComments(self); len(got) != 0 {
		t.Errorf("findNewComments must skip the eject comment, got %d", len(got))
	}
}

func TestEjectQueuedMemberForComments_RerouteFailure_PostsNothing(t *testing.T) {
	client := &mockGitHubClient{updateProjectItemStatusFn: func(_, _, _, _ string) error { return errors.New("boom") }}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	eng.ejectQueuedMemberForComments("PVT_1", queuedCommentItem(1, queuedHumanComment("c1", "x")))

	client.mu.Lock()
	n := len(client.addCommentCalls)
	client.mu.Unlock()
	if n != 0 {
		t.Errorf("failed reroute must post nothing, got %d comment(s)", n)
	}
	if c := commentEjectCount(eng, "owner/repo#1"); c != 0 {
		t.Errorf("failed reroute must not count, got %d", c)
	}
}

func TestApplyPendingReviewEjects_CommentSignal(t *testing.T) {
	client := &mockGitHubClient{}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	members := []trainMember{
		{item: gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}},
		{item: gh.ProjectItem{Number: 2, ItemID: "PVTI_2", Repo: "owner/repo", Status: "Queued"}},
		{item: gh.ProjectItem{Number: 3, ItemID: "PVTI_3", Repo: "owner/repo", Status: "Queued"}},
	}
	eng.markPendingCommentEject("owner/repo", 2)

	remaining, ejected := eng.applyPendingReviewEjects("PVT_1", "owner/repo", members)
	if ejected != 1 || len(remaining) != 2 {
		t.Fatalf("expected 1 ejected / 2 remaining, got %d / %d", ejected, len(remaining))
	}
	for _, m := range remaining {
		if m.item.Number == 2 {
			t.Error("ejected #2 must not remain")
		}
	}
	if n := commentEjectCount(eng, "owner/repo#2"); n != 0 {
		t.Errorf("comment eject must not count, got %d", n)
	}
	// One-shot.
	if _, ejected2 := eng.applyPendingReviewEjects("PVT_1", "owner/repo", members); ejected2 != 0 {
		t.Errorf("second apply must be a no-op, ejected %d", ejected2)
	}
}

func TestApplyPendingReviewEjects_BothCauses_ReviewWinsSingleEject(t *testing.T) {
	client := &mockGitHubClient{}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	members := []trainMember{{item: gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}}}
	eng.markPendingReviewEject("owner/repo", 1, 2)
	eng.markPendingCommentEject("owner/repo", 1)

	remaining, ejected := eng.applyPendingReviewEjects("PVT_1", "owner/repo", members)
	if ejected != 1 || len(remaining) != 0 {
		t.Fatalf("expected a single eject, got ejected=%d remaining=%d", ejected, len(remaining))
	}
	if len(client.updateStatusCalls) != 1 {
		t.Errorf("expected exactly one reroute, got %d", len(client.updateStatusCalls))
	}
	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()
	if len(calls) != 1 || !strings.Contains(calls[0].body, "has left the Queued column") {
		t.Errorf("expected the review-finding wording only, got %+v", calls)
	}
	if n := commentEjectCount(eng, "owner/repo#1"); n != 1 {
		t.Errorf("review cause still counts, got %d", n)
	}
	if eng.takePendingCommentEject("owner/repo", 1) {
		t.Error("comment signal must have been consumed alongside the review signal")
	}
}

func TestDeferRedMember_DropsPendingCommentEject(t *testing.T) {
	client := &mockGitHubClient{}
	eng := admissionEngine(t, client)
	eng.markPendingCommentEject("owner/repo", 1)

	eng.deferRedMember("PVT_1", "owner", "repo", admissionMembers(1)[0], nil, "")

	if eng.takePendingCommentEject("owner/repo", 1) {
		t.Error("pending comment-eject signal must be dropped on a CI deferral")
	}
}

// ── settle-scan comment cause ─────────────────────────────────────────────────

func settleCommentBoard(items ...gh.ProjectItem) *gh.ProjectBoard {
	return &gh.ProjectBoard{ProjectID: "PVT_1", Items: items}
}

func settleCommentEngine(t *testing.T, client *mockGitHubClient, comments map[int][]gh.Comment) *Engine {
	t.Helper()
	// The scan re-deep-fetches each item; supply the comments through the fetch seam,
	// as the real FetchItemDetails would populate them.
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		item.Comments = comments[item.Number]
		return nil
	}
	return trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
}

func TestSettleQueuedComment_DirectEject_NoWorker(t *testing.T) {
	client := &mockGitHubClient{}
	eng := settleCommentEngine(t, client, map[int][]gh.Comment{1: {queuedHumanComment("c1", "please change X")}})

	eng.settleQueuedReviewFindings(settleCommentBoard(queuedCommentItem(1)))

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 reroute, got %d", len(client.updateStatusCalls))
	}
	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()
	if len(calls) != 1 || !strings.Contains(calls[0].body, "unprocessed comment") {
		t.Fatalf("expected the comment-cause eject comment, got %+v", calls)
	}
	if n := commentEjectCount(eng, "owner/repo#1"); n != 0 {
		t.Errorf("must not count toward MaxMergeTrainEjections, got %d", n)
	}
}

func TestSettleQueuedComment_PendingFlag_InLiveBatch(t *testing.T) {
	client := &mockGitHubClient{}
	eng := settleCommentEngine(t, client, map[int][]gh.Comment{1: {queuedHumanComment("c1", "x")}})
	eng.store.EnterRepoWorker(mergeTrainKey("owner/repo", defaultPartitionBase))
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", defaultPartitionBase), &mergeTrainWorkerState{
		projectID:    "PVT_1",
		batchNumbers: map[int]bool{1: true},
	})

	eng.settleQueuedReviewFindings(settleCommentBoard(queuedCommentItem(1)))

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("in-batch member must not be rerouted by the scan, got %d", len(client.updateStatusCalls))
	}
	if !eng.takePendingCommentEject("owner/repo", 1) {
		t.Fatal("expected a pending comment-eject signal")
	}
}

func TestSettleQueuedComment_BeyondBatchCap_DirectEject(t *testing.T) {
	client := &mockGitHubClient{}
	eng := settleCommentEngine(t, client, map[int][]gh.Comment{2: {queuedHumanComment("c1", "x")}})
	eng.store.EnterRepoWorker(mergeTrainKey("owner/repo", defaultPartitionBase))
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", defaultPartitionBase), &mergeTrainWorkerState{
		projectID:    "PVT_1",
		batchNumbers: map[int]bool{1: true},
	})

	eng.settleQueuedReviewFindings(settleCommentBoard(queuedCommentItem(2)))

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected direct reroute for batch-cap overflow, got %d", len(client.updateStatusCalls))
	}
	if eng.takePendingCommentEject("owner/repo", 2) {
		t.Error("no pending signal expected for a directly ejected member")
	}
}

func TestSettleQueuedComment_NonHumanOrProcessedComments_NoEject(t *testing.T) {
	cases := map[string]gh.Comment{
		"bot author":       {ID: "c1", Author: "copilot[bot]", Body: "looks good"},
		"empty author":     {ID: "c2", Author: "", Body: "hi"},
		"fabrik prefix":    {ID: "c3", Author: "alice", Body: "🏭 **Fabrik — stage: Validate**\nreport"},
		"rocket reaction":  {ID: "c4", Author: "alice", Body: "done", Reactions: []gh.ReactionGroup{{Content: "ROCKET", Count: 1}}},
		"bot service note": {ID: "c5", Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client := &mockGitHubClient{}
			eng := settleCommentEngine(t, client, map[int][]gh.Comment{1: {c}})

			eng.settleQueuedReviewFindings(settleCommentBoard(queuedCommentItem(1)))

			if len(client.updateStatusCalls) != 0 {
				t.Errorf("%s must not eject, got %d reroute(s)", name, len(client.updateStatusCalls))
			}
			if eng.takePendingCommentEject("owner/repo", 1) {
				t.Errorf("%s must not flag a pending signal", name)
			}
		})
	}
}

func TestSettleQueuedComment_StoreWatermarkedComment_NoEject(t *testing.T) {
	client := &mockGitHubClient{}
	c := queuedHumanComment("c1", "x")
	eng := settleCommentEngine(t, client, map[int][]gh.Comment{1: {c}})
	eng.markCommentsProcessed(queuedCommentItem(1), []gh.Comment{c})

	eng.settleQueuedReviewFindings(settleCommentBoard(queuedCommentItem(1)))

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("a store-watermarked comment must not eject, got %d", len(client.updateStatusCalls))
	}
}

func TestSettleQueuedComment_ReviewFindingsTakePrecedence(t *testing.T) {
	client := &mockGitHubClient{}
	eng := settleCommentEngine(t, client, map[int][]gh.Comment{1: {queuedHumanComment("c1", "x")}})
	item := unresolvedThreadItem(1)

	eng.settleQueuedReviewFindings(settleCommentBoard(item))

	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()
	if len(calls) != 1 || !strings.Contains(calls[0].body, "has left the Queued column") {
		t.Fatalf("expected only the review-finding eject, got %+v", calls)
	}
	if n := commentEjectCount(eng, "owner/repo#1"); n != 1 {
		t.Errorf("review-finding cause counts, got %d", n)
	}
}

func TestSettleQueuedComment_SkippedMembers(t *testing.T) {
	human := map[int][]gh.Comment{1: {queuedHumanComment("c1", "x")}}

	t.Run("native merge queue", func(t *testing.T) {
		client := &mockGitHubClient{}
		eng := settleCommentEngine(t, client, human)
		eng.cfg.MergeQueue = "auto"
		item := queuedCommentItem(1)
		item.LinkedPRIsMergeQueueEnabled = true
		eng.settleQueuedReviewFindings(settleCommentBoard(item))
		if len(client.updateStatusCalls) != 0 {
			t.Error("native-queue member must be skipped")
		}
	})
	t.Run("auto-merge-enabled", func(t *testing.T) {
		client := &mockGitHubClient{}
		eng := settleCommentEngine(t, client, human)
		item := queuedCommentItem(1)
		item.Labels = []string{"fabrik:auto-merge-enabled"}
		eng.settleQueuedReviewFindings(settleCommentBoard(item))
		if len(client.updateStatusCalls) != 0 {
			t.Error("auto-merge-enabled member must be skipped")
		}
	})
	t.Run("paused", func(t *testing.T) {
		client := &mockGitHubClient{}
		eng := settleCommentEngine(t, client, human)
		item := queuedCommentItem(1)
		item.Labels = []string{"fabrik:paused"}
		eng.settleQueuedReviewFindings(settleCommentBoard(item))
		if len(client.updateStatusCalls) != 0 {
			t.Error("paused member must be skipped")
		}
	})
	t.Run("closed", func(t *testing.T) {
		client := &mockGitHubClient{}
		eng := settleCommentEngine(t, client, human)
		item := queuedCommentItem(1)
		item.IsClosed = true
		eng.settleQueuedReviewFindings(settleCommentBoard(item))
		if len(client.updateStatusCalls) != 0 {
			t.Error("closed member must be skipped")
		}
	})
}

func TestSettleQueuedComment_StaleSignalClearedWhenUnflagged(t *testing.T) {
	client := &mockGitHubClient{}
	eng := settleCommentEngine(t, client, map[int][]gh.Comment{1: nil})
	eng.store.EnterRepoWorker(mergeTrainKey("owner/repo", defaultPartitionBase))
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", defaultPartitionBase), &mergeTrainWorkerState{
		projectID:    "PVT_1",
		batchNumbers: map[int]bool{1: true},
	})
	eng.markPendingCommentEject("owner/repo", 1)

	eng.settleQueuedReviewFindings(settleCommentBoard(queuedCommentItem(1)))

	if eng.takePendingCommentEject("owner/repo", 1) {
		t.Error("stale signal for an unflagged in-batch member must be cleared")
	}
}

func TestSettleQueuedComment_FailedReroute_RetriesNextPoll(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	client := &mockGitHubClient{updateProjectItemStatusFn: func(_, _, _, _ string) error {
		if fail.Load() {
			return errors.New("boom")
		}
		return nil
	}}
	eng := settleCommentEngine(t, client, map[int][]gh.Comment{1: {queuedHumanComment("c1", "x")}})
	board := settleCommentBoard(queuedCommentItem(1))

	eng.settleQueuedReviewFindings(board)
	client.mu.Lock()
	n := len(client.addCommentCalls)
	fail.Store(false)
	client.mu.Unlock()
	if n != 0 {
		t.Fatalf("failed reroute must post nothing, got %d", n)
	}

	eng.settleQueuedReviewFindings(board)
	client.mu.Lock()
	n = len(client.addCommentCalls)
	client.mu.Unlock()
	if n != 1 {
		t.Errorf("expected the retry to post exactly one comment, got %d", n)
	}
}
