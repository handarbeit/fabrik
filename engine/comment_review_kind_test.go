package engine

import (
	"context"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// runCommentReviewObservingWorker runs processComments for item 10 with the given
// pre-existing Worker handle and returns the Worker kind/trigger the invoker saw
// mid-run and the handle left behind afterwards.
func runCommentReviewObservingWorker(t *testing.T, entered itemstate.WorkerEntered) (during, after itemstate.WorkerHandle) {
	t.Helper()
	skipIfNoGit(t)
	var mu sync.Mutex
	var eng *Engine
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			mu.Lock()
			defer mu.Unlock()
			snap, _ := eng.store.Peek("owner/repo", 10)
			if w := snap.Worker(); w != nil {
				during = *w
			}
			return "done", false, TokenUsage{}, nil
		},
	}
	eng = testEngineWithRepo(t, &mockGitHubClient{}, claude)
	bootstrapItem(t, eng, 10, nil)
	entered.Repo, entered.Number = "owner/repo", 10
	eng.store.Apply(entered)

	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{Number: 10, Body: "spec", Comments: []gh.Comment{}}
	comments := []gh.Comment{{ID: "C_77", DatabaseID: 77, Author: "testuser", Body: "please look again"}}
	if err := eng.processComments(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, comments); err != nil {
		t.Fatalf("processComments: %v", err)
	}
	snap, _ := eng.store.Peek("owner/repo", 10)
	if w := snap.Worker(); w != nil {
		after = *w
	}
	return during, after
}

// F4 (#2121): a comment review running in a stage worker goroutine must be
// reported as a comment review naming the comment, not as a plain stage run.
func TestProcessComments_MarksTheStageWorkerAsACommentReview(t *testing.T) {
	during, after := runCommentReviewObservingWorker(t, itemstate.WorkerEntered{
		StageName: "Research", Kind: itemstate.WorkerKindStageRun,
	})
	if during.Kind != itemstate.WorkerKindCommentReview || during.Trigger != "C_77" {
		t.Errorf("worker during the review = kind %q trigger %q, want comment-review / C_77", during.Kind, during.Trigger)
	}
	if after.Kind != itemstate.WorkerKindStageRun || after.Trigger != "" {
		t.Errorf("worker after the review = kind %q trigger %q, want the stage-run kind restored", after.Kind, after.Trigger)
	}
}

// The comment path also serves the three reinvokes: their kind must survive.
func TestProcessComments_NeverOverridesAReinvokeKind(t *testing.T) {
	during, after := runCommentReviewObservingWorker(t, itemstate.WorkerEntered{
		StageName: "Research", Kind: itemstate.WorkerKindCIFixReinvoke,
	})
	if during.Kind != itemstate.WorkerKindCIFixReinvoke || after.Kind != itemstate.WorkerKindCIFixReinvoke {
		t.Errorf("kind during/after = %q/%q, want ci-fix-reinvoke throughout", during.Kind, after.Kind)
	}
}
