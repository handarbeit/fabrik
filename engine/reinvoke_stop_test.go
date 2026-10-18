package engine

import (
	"context"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// A stop request reaches every kind of reinvoke worker, not only the stage run (#2349, R3):
// the worker registers a cancel handle on issueCtxs, the stop cancels it, the cycle charge
// is refunded, and no handle is left behind.
func TestStopRequest_CancelsRunningReinvokes(t *testing.T) {
	skipIfNoGit(t)
	kinds := map[string]func(eng *Engine, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage){
		"ci-fix": func(eng *Engine, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
			eng.store.Apply(itemstate.CIFixCycleIncremented{Repo: "owner/repo", Number: 31, StageName: "Implement"})
			eng.dispatchCIFixReinvoke(context.Background(), board, item, stage, PRSettleResult{})
		},
		"rebase": func(eng *Engine, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
			eng.store.Apply(itemstate.RebaseCycleIncremented{Repo: "owner/repo", Number: 31, StageName: "Implement"})
			eng.dispatchRebaseReinvoke(context.Background(), board, item, stage)
		},
		"review": func(eng *Engine, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) {
			eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 31, StageName: "Implement"})
			eng.dispatchReviewReinvoke(context.Background(), board, item, stage,
				[]gh.Comment{{ID: "x", Body: "finding", Author: "handarbeit-pruefer"}}, false)
		},
	}
	for name, dispatch := range kinds {
		t.Run(name, func(t *testing.T) {
			running := make(chan struct{}, 1)
			client := &mockGitHubClient{}
			claude := &mockClaudeInvoker{
				invokeForCommentsCtxFn: func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
					running <- struct{}{}
					<-ctx.Done()
					return "", false, TokenUsage{}, ctx.Err()
				},
			}
			stgs := []*stages.Stage{{Name: "Implement", Order: 1, Prompt: "implement"}}
			eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)
			if _, err := eng.worktreesFor("owner/repo").EnsureWorktree(31, "main", false); err != nil {
				t.Fatalf("EnsureWorktree: %v", err)
			}
			item := gh.ProjectItem{Number: 31, Repo: "owner/repo", LinkedPRNumber: 100,
				LinkedPRReviews: []gh.PRReview{{Author: "handarbeit-pruefer", State: "COMMENTED", Body: "**File:** engine/foo.go\nmissing nil check.", DatabaseID: 3400}}}
			board := &gh.ProjectBoard{ProjectID: "PVT_1"}

			dispatch(eng, board, item, stgs[0])
			select {
			case <-running:
			case <-time.After(30 * time.Second):
				t.Fatal("the reinvoke never reached Claude")
			}
			if _, ok := eng.issueCtxs.Load("owner/repo#31"); !ok {
				t.Fatal("a running reinvoke registered no cancel handle on issueCtxs")
			}

			eng.handleStopRequest(context.Background(), tui.StopRequest{IssueNumber: 31, Repo: "owner/repo", StageName: "Implement"})

			done := make(chan struct{})
			go func() { eng.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the stop request did not cancel the running reinvoke")
			}
			if _, ok := eng.issueCtxs.Load("owner/repo#31"); ok {
				t.Error("a stale cancel handle remains registered after the worker ended")
			}
			snap, _ := eng.store.Get("owner/repo", 31)
			if got := snap.CIFixCycles("Implement") + snap.RebaseCycles("Implement") + snap.ReviewCycles("Implement"); got != 0 {
				t.Errorf("cycle charge not refunded for a cancelled pass: %d", got)
			}
		})
	}
}

// A stop request when nothing is running cancels nothing and leaves no handle behind.
func TestStopRequest_NoWorkerLeavesNoHandle(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.handleStopRequest(context.Background(), tui.StopRequest{IssueNumber: 31, Repo: "owner/repo"})
	n := 0
	eng.issueCtxs.Range(func(_, _ any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("%d handle(s) registered with no worker", n)
	}
}
