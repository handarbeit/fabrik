package engine

import (
	"context"
	"os/exec"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func conflictRecord(e *Engine, number int) *itemstate.ConflictResolution {
	snap, ok := e.store.Peek("owner/repo", number)
	if !ok {
		return nil
	}
	st := snap.State()
	return st.ConflictResolution
}

// #2121: a merge-train conflict resolution is reported on the member while it
// waits for a worker slot (queued) and while it holds one (running), on a
// ConflictResolution field — never a Worker handle — and is cleared on exit.
func TestResolveConflictWithClaude_RecordsQueuedThenRunningThenClears(t *testing.T) {
	skipIfNoGit(t)
	var eng *Engine
	var during *itemstate.ConflictResolution
	var workerDuring *itemstate.WorkerHandle
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			during = conflictRecord(eng, 5)
			snap, _ := eng.store.Peek("owner/repo", 5)
			workerDuring = snap.Worker()
			return "", false, TokenUsage{}, nil
		},
	}
	eng = trainTestEngine(t, &mockGitHubClient{}, claude, nil)
	bootstrapItem(t, eng, 5, nil)
	member := makeTrainItem(5, "member")
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init").Run(); err != nil {
		t.Fatal(err)
	}
	holding := &stages.Stage{Name: "Queued", HoldingStage: true}

	// All slots busy: the resolution is queued, not running.
	saturateSem(eng)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = eng.resolveConflictWithClaude(ctx, "owner/repo", member, dir, holding, "sha", nil, "sha", nil, InvokeOptions{})
	}()
	deadline := time.Now().Add(5 * time.Second)
	var queued *itemstate.ConflictResolution
	for time.Now().Before(deadline) {
		if queued = conflictRecord(eng, 5); queued != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if queued == nil || !queued.Queued || queued.Since.IsZero() {
		t.Fatalf("while waiting for a slot ConflictResolution = %+v, want queued", queued)
	}
	// Free a slot: it runs.
	<-eng.sem
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resolveConflictWithClaude did not finish once a slot was free")
	}
	cancel()
	drainSem(eng)
	if during == nil || during.Queued {
		t.Errorf("while the invoker ran ConflictResolution = %+v, want present and not queued", during)
	}
	if !during.GeneralSlot {
		t.Errorf("with no reserved train slot the resolution holds a general-pool slot, got %+v", during)
	}
	if workerDuring != nil {
		t.Errorf("a conflict resolution must not create a Worker handle, got %+v", workerDuring)
	}
	if after := conflictRecord(eng, 5); after != nil {
		t.Errorf("ConflictResolution = %+v after return, want cleared", after)
	}
}

func TestResolveConflictWithClaude_CancelledSlotWaitClearsTheRecord(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	bootstrapItem(t, eng, 6, nil)
	saturateSem(eng)
	defer drainSem(eng)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := eng.resolveConflictWithClaude(ctx, "owner/repo", makeTrainItem(6, "m"), t.TempDir(), &stages.Stage{Name: "Queued", HoldingStage: true}, "sha", nil, "sha", nil, InvokeOptions{})
	if err == nil {
		t.Fatal("want a not-attempted error")
	}
	if rec := conflictRecord(eng, 6); rec != nil {
		t.Errorf("ConflictResolution = %+v after a cancelled wait, want cleared", rec)
	}
}

// The pool the resolution holds is recorded: the reserved merge-train slot is not
// a general-pool slot, so health must not count it in in_use (#2121).
func TestResolveConflictWithClaude_RecordsTheReservedSlotAsNotGeneral(t *testing.T) {
	skipIfNoGit(t)
	var eng *Engine
	var during *itemstate.ConflictResolution
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			during = conflictRecord(eng, 5)
			return "", false, TokenUsage{}, nil
		},
	}
	eng = trainTestEngine(t, &mockGitHubClient{}, claude, nil)
	eng.EnableTrainReservedSlotForTest()
	bootstrapItem(t, eng, 5, nil)
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init").Run(); err != nil {
		t.Fatal(err)
	}
	holding := &stages.Stage{Name: "Queued", HoldingStage: true}
	if _, _, err := eng.resolveConflictWithClaude(context.Background(), "owner/repo", makeTrainItem(5, "member"), dir, holding, "sha", nil, "sha", nil, InvokeOptions{}); err != nil {
		t.Fatalf("resolveConflictWithClaude: %v", err)
	}
	if during == nil || during.Queued || during.GeneralSlot {
		t.Errorf("on the reserved slot ConflictResolution = %+v, want running and not general", during)
	}
}
