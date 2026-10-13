package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// #2179: a reinvoke registers a Worker for heartbeat/liveness but never claims
// the store Lock, which mirrors the fabrik:locked:<id> label that only stage
// dispatch applies.

func lockLabelRemovals(client *mockGitHubClient) []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	var out []string
	for _, c := range client.removeLabelCalls {
		if strings.HasPrefix(c.labelName, "fabrik:locked") {
			out = append(out, c.labelName)
		}
	}
	return out
}

func TestReinvoke_LeavesNoStoreLock(t *testing.T) {
	cases := map[string]error{
		"success":     nil,
		"error":       errors.New("boom"),
		"did-not-run": &claudeAPIErrorExit{TerminalReason: "api_error", NumTurns: 1},
		"usage-limit": &claudeUsageLimitError{},
	}
	id := 4000
	for name, claudeErr := range cases {
		id++
		t.Run(name, func(t *testing.T) {
			var calls int
			eng, client, stgs := newDidNotRunReviewEngine(t, claudeErr, &calls)
			dispatchReviewRound(t, eng, stgs[0], id)

			snap, _ := eng.store.Get("owner/repo", 31)
			if lock := snap.Lock(); lock != nil {
				t.Errorf("Lock = %+v after reinvoke, want nil", lock)
			}
			if snap.Worker() != nil {
				t.Errorf("Worker still set after reinvoke")
			}

			eng.cleanupLockedIssues()
			if got := lockLabelRemovals(client); len(got) != 0 {
				t.Errorf("shutdown removed lock labels %v after a completed reinvoke, want none", got)
			}
		})
	}
}

func TestReinvoke_ContextCancelledLeavesNoStoreLock(t *testing.T) {
	var calls int
	eng, client, stgs := newDidNotRunReviewEngine(t, nil, &calls)
	// Fill the semaphore so the goroutine waits, then cancel.
	for i := 0; i < cap(eng.sem); i++ {
		eng.sem <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	item := gh.ProjectItem{Number: 31, Repo: "owner/repo", Labels: []string{"stage:Implement:complete"}}
	eng.dispatchReinvoke(ctx, &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stgs[0], reinvokeOpts{
		tag:   "test",
		build: func(string) []gh.Comment { return nil },
	})
	cancel()
	eng.wg.Wait()

	snap, _ := eng.store.Get("owner/repo", 31)
	if lock := snap.Lock(); lock != nil {
		t.Errorf("Lock = %+v after cancelled reinvoke, want nil", lock)
	}
	eng.cleanupLockedIssues()
	if got := lockLabelRemovals(client); len(got) != 0 {
		t.Errorf("shutdown removed lock labels %v, want none", got)
	}
}

func TestReinvoke_RunningWorkerHasNoLock(t *testing.T) {
	var calls int
	eng, _, stgs := newDidNotRunReviewEngine(t, nil, &calls)
	started := make(chan struct{})
	release := make(chan struct{})
	eng.claude.(*mockClaudeInvoker).invokeForCommentsFn = func(*stages.Stage, gh.ProjectItem, []gh.Comment, string, InvokeOptions) (string, bool, TokenUsage, error) {
		close(started)
		<-release
		return "", false, TokenUsage{}, nil
	}
	pctx := &phase1Ctx{
		ctx:   context.Background(),
		board: &gh.ProjectBoard{ProjectID: "PVT_1"},
		item: gh.ProjectItem{
			Number:         31,
			Repo:           "owner/repo",
			Labels:         []string{"stage:Implement:complete"},
			LinkedPRNumber: 100,
			LinkedPRReviews: []gh.PRReview{{
				Author: "handarbeit-pruefer", State: "COMMENTED",
				Body: "**File:** engine/foo.go\nmissing nil check.", DatabaseID: 4100,
			}},
		},
		stage:         stgs[0],
		hasComplete:   true,
		advancedItems: map[string]bool{},
	}
	if !eng.handleReviewGate(pctx) {
		t.Fatal("handleReviewGate did not claim the item")
	}
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("reinvoke never reached Claude")
	}
	snap, _ := eng.store.Get("owner/repo", 31)
	if snap.Worker() == nil {
		t.Error("Worker should be registered while a reinvoke runs")
	}
	if lock := snap.Lock(); lock != nil {
		t.Errorf("Lock = %+v while reinvoke runs, want nil", lock)
	}
	close(release)
	eng.wg.Wait()
}

func TestReinvoke_PreservesRealStageLock(t *testing.T) {
	var calls int
	eng, client, stgs := newDidNotRunReviewEngine(t, nil, &calls)
	eng.store.Apply(itemstate.LocalLockAcquired{Repo: "owner/repo", Number: 31, User: eng.lockIdentity(), AcquiredAt: time.Now()})

	dispatchReviewRound(t, eng, stgs[0], 4200)

	snap, _ := eng.store.Get("owner/repo", 31)
	if lock := snap.Lock(); lock == nil || !lock.HeldByThis {
		t.Fatalf("real stage lock was cleared by a reinvoke: %+v", lock)
	}
	eng.cleanupLockedIssues()
	if got := lockLabelRemovals(client); len(got) != 1 || got[0] != eng.lockLabel() {
		t.Errorf("lock label removals = %v, want exactly [%s]", got, eng.lockLabel())
	}
}
