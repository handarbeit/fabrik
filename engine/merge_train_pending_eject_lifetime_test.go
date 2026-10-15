package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// stalePendingEjectEnv builds a single-member (#1) fast-path-eligible engine: a run of
// it lands #1 through the #1644 singleton fast path and returns through finishTrain.
func stalePendingEjectEnv(t *testing.T) (*Engine, *mockGitHubClient) {
	t.Helper()
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)
	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "file1.txt", "content1\n")

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			if issueNumber == 1 {
				return &gh.PRDetails{Number: 10, HeadSHA: sha1, State: "open"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, HeadSHA: sha1, MergeableState: "clean"}, nil
		},
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) { return 0, nil },
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{{Name: "build", Status: "completed", Conclusion: "success"}}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()
	return eng, client
}

// runStalePendingEjectWorker registers a fresh run state for the (owner/repo, main)
// partition and runs a worker for #1 over it to completion.
func runStalePendingEjectWorker(t *testing.T, eng *Engine) *mergeTrainWorkerState {
	t.Helper()
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_1", batchNumbers: map[int]bool{1: true},
		trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().UnixNano())}
	key := mergeTrainKey("owner/repo", "main")
	eng.mergeTrainInFlight.Store(key, state)
	eng.store.EnterRepoWorker(key)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", "main", []gh.ProjectItem{makeTrainItem(1, "Issue 1")})
	return state
}

func reroutedOffQueued(c *mockGitHubClient) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, u := range c.updateStatusCalls {
		if u.optionID == "opt-implement" {
			return true
		}
	}
	return false
}

// TestStalePendingReviewEject_DoesNotActInLaterRun is #1557 AC1/AC3: a review-eject
// signal that reaches the map after run 1's last checkpoint (here: after the run
// finished) must not eject the same issue number from a later run whose member has no
// finding.
func TestStalePendingReviewEject_DoesNotActInLaterRun(t *testing.T) {
	eng, client := stalePendingEjectEnv(t)

	run1 := runStalePendingEjectWorker(t, eng)
	if len(client.mergePRAtHeadSHACalls) != 1 || reroutedOffQueued(client) {
		t.Fatalf("setup: run 1 should land #1 via the fast path, got merges=%+v", client.mergePRAtHeadSHACalls)
	}

	// The late mark: the poll goroutine's settle scan flagged #1 for run 1 after that
	// run's claim was already released (it read the run's state before its network fetch).
	eng.markPendingReviewEject("owner/repo", 1, 2, run1)

	runStalePendingEjectWorker(t, eng)

	if reroutedOffQueued(client) {
		t.Fatal("run 2 ejected #1 on a signal raised for run 1 — a stale signal acted in a later run")
	}
	if len(client.mergePRAtHeadSHACalls) != 2 {
		t.Errorf("run 2 should have landed #1 via the fast path, got merges=%+v", client.mergePRAtHeadSHACalls)
	}
}

// TestStalePendingCommentEject_DoesNotActInLaterRun is the comment-signal twin.
func TestStalePendingCommentEject_DoesNotActInLaterRun(t *testing.T) {
	eng, client := stalePendingEjectEnv(t)

	run1 := runStalePendingEjectWorker(t, eng)
	eng.markPendingCommentEject("owner/repo", 1, run1)
	runStalePendingEjectWorker(t, eng)

	if reroutedOffQueued(client) {
		t.Fatal("run 2 ejected #1 on a comment signal raised for run 1")
	}
}

// TestStaleSignalDropped_UnresolvedFindingStillEjectedByNextRun is #1557 AC2, the R2
// guard, paired with AC1 above: with the stale signal from run 1 in the map, a
// still-unresolved finding on the still-Queued member is re-raised by the next poll's
// settle scan against the live run 2, and run 2's first checkpoint ejects it. AC1's
// run (no finding, no scan) keeps #1 in the batch, so the two together show the
// outcome follows the finding and not an unconditional eject.
func TestStaleSignalDropped_UnresolvedFindingStillEjectedByNextRun(t *testing.T) {
	eng, client := stalePendingEjectEnv(t)

	run1 := runStalePendingEjectWorker(t, eng)
	eng.markPendingReviewEject("owner/repo", 1, 2, run1) // stale, as in AC1

	// Run 2 is claimed; the next poll's scan sees the unresolved finding on #1.
	key := mergeTrainKey("owner/repo", "main")
	run2 := &mergeTrainWorkerState{assembling: true, projectID: "PVT_1", batchNumbers: map[int]bool{1: true},
		trialName: fmt.Sprintf("merge-train-repo-%d", time.Now().UnixNano())}
	eng.mergeTrainInFlight.Store(key, run2)
	eng.store.EnterRepoWorker(key)
	item := unresolvedThreadItem(1)
	item.Labels = []string{"base:main"}
	eng.settleQueuedReviewFindings(&gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{item}})
	if reroutedOffQueued(client) {
		t.Fatal("the scan must leave an in-batch member to the run's checkpoint, not eject it directly")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, run2, "owner", "repo", "main", []gh.ProjectItem{makeTrainItem(1, "Issue 1")})

	if !reroutedOffQueued(client) {
		t.Fatal("the unresolved finding was lost: run 2 did not eject #1 (R2 / ADR-1208 guarantee)")
	}
	if len(client.mergePRAtHeadSHACalls) != 1 {
		t.Errorf("#1 must not be landed by run 2, got merges=%+v", client.mergePRAtHeadSHACalls)
	}
}

// TestStaleSignalDropped_NoLiveRun_ScanEjectsDirectly is the other half of R2: once the
// run is gone the stale signal is moot and the scan ejects a still-flagged member itself.
func TestStaleSignalDropped_NoLiveRun_ScanEjectsDirectly(t *testing.T) {
	eng, client := stalePendingEjectEnv(t)

	run1 := runStalePendingEjectWorker(t, eng)
	eng.markPendingReviewEject("owner/repo", 1, 2, run1)

	eng.settleQueuedReviewFindings(&gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{unresolvedThreadItem(1)}})

	if !reroutedOffQueued(client) {
		t.Fatal("with no live run the scan must eject the flagged member directly")
	}
}

func TestTakePendingEjectFor_MatchesOwnerAndDiscardsStale(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	a, b := &mergeTrainWorkerState{}, &mergeTrainWorkerState{}

	eng.markPendingReviewEject("owner/repo", 1, 4, a)
	if count, ok := eng.takePendingReviewEjectFor("owner/repo", 1, a); !ok || count != 4 {
		t.Fatalf("matching owner must receive the signal, got (%d, %v)", count, ok)
	}
	if _, ok := eng.takePendingReviewEjectFor("owner/repo", 1, a); ok {
		t.Error("signal is one-shot")
	}

	eng.markPendingReviewEject("owner/repo", 2, 1, a)
	if _, ok := eng.takePendingReviewEjectFor("owner/repo", 2, b); ok {
		t.Error("a signal stamped with another run must not be acted on")
	}
	if _, ok := eng.takePendingReviewEject("owner/repo", 2); ok {
		t.Error("a stale signal must be discarded on sight, not left in the map")
	}

	eng.markPendingCommentEject("owner/repo", 3, a)
	if eng.takePendingCommentEjectFor("owner/repo", 3, b) {
		t.Error("a comment signal stamped with another run must not be acted on")
	}
	if eng.takePendingCommentEject("owner/repo", 3) {
		t.Error("a stale comment signal must be discarded on sight")
	}
	eng.markPendingCommentEject("owner/repo", 3, a)
	if !eng.takePendingCommentEjectFor("owner/repo", 3, a) {
		t.Error("matching owner must receive the comment signal")
	}
}

// TestFinishTrain_DrainsOnlyItsOwnRunsSignals covers the run-end drain and ADR-1648:
// two partitions of one repo share the repo-keyed maps, and finishing one run must not
// touch the other's signals.
func TestFinishTrain_DrainsOnlyItsOwnRunsSignals(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	mainKey := mergeTrainKey("owner/repo", "main")
	relKey := mergeTrainKey("owner/repo", "release")
	mainRun, relRun := &mergeTrainWorkerState{}, &mergeTrainWorkerState{}
	eng.mergeTrainInFlight.Store(mainKey, mainRun)
	eng.mergeTrainInFlight.Store(relKey, relRun)
	eng.store.EnterRepoWorker(mainKey)
	eng.store.EnterRepoWorker(relKey)

	eng.markPendingReviewEject("owner/repo", 1, 2, mainRun)
	eng.markPendingCommentEject("owner/repo", 2, mainRun)
	eng.markPendingReviewEject("owner/repo", 3, 1, relRun)
	eng.markPendingCommentEject("owner/repo", 4, relRun)

	eng.finishTrain(mainKey)

	if _, ok := eng.takePendingReviewEject("owner/repo", 1); ok {
		t.Error("finished run's review signal must be drained")
	}
	if eng.takePendingCommentEject("owner/repo", 2) {
		t.Error("finished run's comment signal must be drained")
	}
	if _, ok := eng.takePendingReviewEjectFor("owner/repo", 3, relRun); !ok {
		t.Error("the other partition's review signal must survive")
	}
	if !eng.takePendingCommentEjectFor("owner/repo", 4, relRun) {
		t.Error("the other partition's comment signal must survive")
	}
	if _, ok := eng.mergeTrainInFlight.Load(mainKey); ok {
		t.Error("finishTrain must still release the claim")
	}
}

// TestPendingEject_LateMarkAfterDrainIsInert is the scan-vs-finish race: a mark stamped
// with a finished run lands after the drain, concurrently with the next run's takes.
// Run under -race; the stale entry must never be acted on.
func TestPendingEject_LateMarkAfterDrainIsInert(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	key := mergeTrainKey("owner/repo", "main")
	old, next := &mergeTrainWorkerState{}, &mergeTrainWorkerState{}
	eng.mergeTrainInFlight.Store(key, old)
	eng.store.EnterRepoWorker(key)
	eng.finishTrain(key)
	eng.mergeTrainInFlight.Store(key, next)

	var wg sync.WaitGroup
	acted := make(chan struct{}, 64)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			eng.markPendingReviewEject("owner/repo", 1, 2, old)
			eng.markPendingCommentEject("owner/repo", 1, old)
		}()
		go func() {
			defer wg.Done()
			if _, ok := eng.takePendingReviewEjectFor("owner/repo", 1, next); ok {
				acted <- struct{}{}
			}
			if eng.takePendingCommentEjectFor("owner/repo", 1, next) {
				acted <- struct{}{}
			}
		}()
	}
	wg.Wait()
	if len(acted) != 0 {
		t.Fatalf("a signal stamped with the finished run was acted on %d time(s)", len(acted))
	}
}
