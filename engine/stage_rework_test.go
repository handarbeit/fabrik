package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// orderedLabelLog records every AddLabelToIssue/RemoveLabelFromIssue call
// against a mockGitHubClient in call order, as "add:<label>"/"remove:<label>"
// entries — the mock's own addLabelCalls/removeLabelCalls slices are
// separate, so they can't by themselves prove ordering between an add and a
// remove (the mark-first-then-clear / restore-first-then-unmark invariant
// this issue depends on for crash safety, #1802).
func orderedLabelLog(client *mockGitHubClient) (*[]string, func()) {
	var mu sync.Mutex
	log := &[]string{}
	client.addLabelToIssueFn = func(owner, repo string, issueNumber int, labelName string) error {
		mu.Lock()
		defer mu.Unlock()
		*log = append(*log, "add:"+labelName)
		return nil
	}
	client.removeLabelFromIssueFn = func(owner, repo string, issueNumber int, labelName string) error {
		mu.Lock()
		defer mu.Unlock()
		*log = append(*log, "remove:"+labelName)
		return nil
	}
	return log, func() {}
}

func indexOf(log []string, entry string) int {
	for i, e := range log {
		if e == entry {
			return i
		}
	}
	return -1
}

// TestBeginStageRework_ClearsStaleComplete verifies that when the re-entered
// stage's own stage:<Stage>:complete is present, beginStageRework marks
// fabrik:reworking BEFORE removing stage:<Stage>:complete (mark-first-then-
// clear, #1802) and reports that it acted.
func TestBeginStageRework_ClearsStaleComplete(t *testing.T) {
	client := &mockGitHubClient{}
	log, _ := orderedLabelLog(client)
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:editing", "stage:Research:complete"},
	}
	stage := &stages.Stage{Name: "Research"}

	got := eng.beginStageRework(item, stage)
	if !got {
		t.Fatal("beginStageRework returned false, want true (stage:Research:complete was present)")
	}

	addIdx := indexOf(*log, "add:fabrik:reworking")
	removeIdx := indexOf(*log, "remove:stage:Research:complete")
	if addIdx == -1 {
		t.Fatalf("fabrik:reworking was never added; log=%v", *log)
	}
	if removeIdx == -1 {
		t.Fatalf("stage:Research:complete was never removed; log=%v", *log)
	}
	if addIdx > removeIdx {
		t.Errorf("mark-first-then-clear violated: fabrik:reworking added at index %d, stage:Research:complete removed at index %d; log=%v", addIdx, removeIdx, *log)
	}
}

// TestBeginStageRework_NoOpWhenCompleteAbsent verifies the common mid-flight
// rework case (stage never completed yet, or a prior rework already cleared
// it) does nothing: no fabrik:reworking is ever applied, since there is
// nothing to lie about.
func TestBeginStageRework_NoOpWhenCompleteAbsent(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 2,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:editing"},
	}
	stage := &stages.Stage{Name: "Research"}

	got := eng.beginStageRework(item, stage)
	if got {
		t.Fatal("beginStageRework returned true, want false (stage:Research:complete was absent)")
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label mutations, got addLabelCalls=%v", client.addLabelCalls)
	}
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no label mutations, got removeLabelCalls=%v", client.removeLabelCalls)
	}
}

// TestEndStageRework_NoOpWhenNotReworking verifies endStageRework is a full
// no-op when wasReworking is false, regardless of completedThisCycle.
func TestEndStageRework_NoOpWhenNotReworking(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 3, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Research"}

	eng.endStageRework(item, stage, false, true)
	eng.endStageRework(item, stage, false, false)

	if len(client.addLabelCalls) != 0 || len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no label mutations when wasReworking=false, got add=%v remove=%v", client.addLabelCalls, client.removeLabelCalls)
	}
}

// TestEndStageRework_CompletingExit_DoesNotDirectlyRestoreComplete verifies
// that when the rework itself re-signals completion this cycle,
// endStageRework removes only fabrik:reworking — restoring
// stage:<Stage>:complete is deliberately left to handleStageComplete's own
// re-derivation (including its wait_for_ci deferral), not duplicated here.
func TestEndStageRework_CompletingExit_DoesNotDirectlyRestoreComplete(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 4, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Research"}

	eng.endStageRework(item, stage, true, true)

	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Research:complete" {
			t.Errorf("stage:Research:complete was directly re-added on a completing exit; that must be left to handleStageComplete")
		}
	}
	var sawRemove bool
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:reworking" {
			sawRemove = true
		}
	}
	if !sawRemove {
		t.Error("fabrik:reworking was not removed on a completing exit")
	}
}

// TestEndStageRework_NonCompletingExit_RestoresThenUnmarks verifies that on a
// non-completing exit, stage:<Stage>:complete is restored directly, and
// fabrik:reworking is removed only afterward — the mirror-image ordering of
// beginStageRework's mark-first-then-clear invariant.
func TestEndStageRework_NonCompletingExit_RestoresThenUnmarks(t *testing.T) {
	client := &mockGitHubClient{}
	log, _ := orderedLabelLog(client)
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 5, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Research"}

	eng.endStageRework(item, stage, true, false)

	addCompleteIdx := indexOf(*log, "add:stage:Research:complete")
	removeMarkerIdx := indexOf(*log, "remove:fabrik:reworking")
	if addCompleteIdx == -1 {
		t.Fatalf("stage:Research:complete was not restored; log=%v", *log)
	}
	if removeMarkerIdx == -1 {
		t.Fatalf("fabrik:reworking was not removed; log=%v", *log)
	}
	if addCompleteIdx > removeMarkerIdx {
		t.Errorf("restore-then-unmark violated: stage:Research:complete restored at index %d, fabrik:reworking removed at index %d; log=%v", addCompleteIdx, removeMarkerIdx, *log)
	}
}

// TestProcessComments_Rework_NonCompletingExit_RestoresCompleteLabel drives
// beginStageRework/endStageRework through the real processComments flow: a
// stage that already carries stage:<Stage>:complete is re-entered by a
// comment, the invocation errors without completing, and by the time
// processComments returns, stage:<Stage>:complete must be back and
// fabrik:reworking must be gone — the net label-set effect of R1/R2 for the
// most common non-completing exit.
func TestProcessComments_Rework_NonCompletingExit_RestoresCompleteLabel(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "still broken", false, TokenUsage{}, errors.New("boom")
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{
		Number: 20,
		Body:   "spec",
		Labels: []string{"stage:Research:complete"},
	}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 101, Author: "testuser", Body: "please redo this"}}

	if err := eng.processComments(context.Background(), board, item, stage, comments); err == nil {
		t.Fatal("expected processComments to return the invocation error")
	}

	var addedReworking, removedComplete, restoredComplete, removedReworking bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:reworking" {
			addedReworking = true
		}
		if c.labelName == "stage:Research:complete" {
			restoredComplete = true
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "stage:Research:complete" {
			removedComplete = true
		}
		if c.labelName == "fabrik:reworking" {
			removedReworking = true
		}
	}
	if !addedReworking {
		t.Error("expected fabrik:reworking to be added at the start of the rework")
	}
	if !removedComplete {
		t.Error("expected stage:Research:complete to be cleared for the rework")
	}
	if !restoredComplete {
		t.Error("expected stage:Research:complete to be restored on the non-completing exit")
	}
	if !removedReworking {
		t.Error("expected fabrik:reworking to be removed on exit")
	}
}

// TestProcessComments_Rework_CompletingExit_DeferredToHandleStageComplete
// verifies that when the re-entered stage's own invocation re-signals
// FABRIK_STAGE_COMPLETE this cycle, stage:<Stage>:complete ends up present
// again via the normal handleStageComplete flow (not a direct restore) and
// fabrik:reworking is removed.
func TestProcessComments_Rework_CompletingExit_DeferredToHandleStageComplete(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "fixed it\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{
		Number: 21,
		Body:   "spec",
		Labels: []string{"stage:Research:complete"},
	}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 101, Author: "testuser", Body: "please redo this"}}

	if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	var completeAddCount int
	var removedReworking bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Research:complete" {
			completeAddCount++
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "stage:Research:complete" {
			// cleared at rework start — expected once
		}
		if c.labelName == "fabrik:reworking" {
			removedReworking = true
		}
	}
	// handleStageComplete re-adds stage:Research:complete exactly once — not
	// duplicated by a direct restore from endStageRework.
	if completeAddCount != 1 {
		t.Errorf("expected stage:Research:complete to be (re-)added exactly once via handleStageComplete, got %d adds: %v", completeAddCount, client.addLabelCalls)
	}
	if !removedReworking {
		t.Error("expected fabrik:reworking to be removed on the completing exit")
	}
}

// TestProcessComments_Rework_CompletingExit_MarkerRemovedAfterHandleStageComplete
// verifies the ordering invariant added to close the second crash window
// (#1802): on a completing exit, fabrik:reworking must be removed AFTER
// handleStageComplete has already (re-)written stage:<Stage>:complete, never
// before. Removing it earlier would leave a crash window where the marker
// that says "a restore is owed" is gone but no completion signal has landed
// yet.
func TestProcessComments_Rework_CompletingExit_MarkerRemovedAfterHandleStageComplete(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	log, _ := orderedLabelLog(client)
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "fixed it\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{
		Number: 26,
		Body:   "spec",
		Labels: []string{"stage:Research:complete"},
	}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 101, Author: "testuser", Body: "please redo this"}}

	if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// item.Labels already carries stage:Research:complete going in (cleared by
	// beginStageRework's remove, never an add) — so the only "add" of this
	// label in the log is handleStageComplete's own re-derivation.
	restoreIdx := indexOf(*log, "add:stage:Research:complete")
	removeMarkerIdx := indexOf(*log, "remove:fabrik:reworking")
	if restoreIdx == -1 {
		t.Fatalf("stage:Research:complete was never (re-)added by handleStageComplete; log=%v", *log)
	}
	if removeMarkerIdx == -1 {
		t.Fatalf("fabrik:reworking was never removed; log=%v", *log)
	}
	if restoreIdx > removeMarkerIdx {
		t.Errorf("fabrik:reworking removed before handleStageComplete's completion write: complete added at index %d, marker removed at index %d; log=%v", restoreIdx, removeMarkerIdx, *log)
	}
}

// TestProcessComments_Rework_WaitForCI_NoDirectComplete verifies the
// wait_for_ci deferral is preserved through a rework: when the re-entered
// stage completes this cycle but is configured with wait_for_ci, the item
// must end up carrying fabrik:awaiting-ci — NOT stage:<Stage>:complete —
// exactly as handleStageComplete already does for an ordinary (non-rework)
// completion. A direct restore from endStageRework would have broken this by
// re-adding the completion label regardless of the CI gate.
func TestProcessComments_Rework_WaitForCI_NoDirectComplete(t *testing.T) {
	skipIfNoGit(t)

	waitForCI := true
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "fixed it\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Implement", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}, WaitForCI: &waitForCI}
	item := gh.ProjectItem{
		Number: 22,
		Body:   "spec",
		Labels: []string{"stage:Implement:complete"},
	}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 101, Author: "testuser", Body: "please redo this"}}

	if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Implement:complete" {
			t.Errorf("stage:Implement:complete was added despite wait_for_ci — the CI gate deferral was bypassed")
		}
	}
	var sawAwaitingCI, removedReworking bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			sawAwaitingCI = true
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:reworking" {
			removedReworking = true
		}
	}
	if !sawAwaitingCI {
		t.Error("expected fabrik:awaiting-ci to be applied (CI gate deferral)")
	}
	if !removedReworking {
		t.Error("expected fabrik:reworking to be removed regardless of the CI deferral")
	}
}

// TestProcessComments_Rework_MidFlightNoStaleComplete_NoOp verifies that when
// the re-entered stage's own stage:<Stage>:complete was never present (the
// common mid-flight-rework case — the stage hasn't completed yet, or a prior
// rework already cleared it), fabrik:reworking is never applied at all.
func TestProcessComments_Rework_MidFlightNoStaleComplete_NoOp(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "still working", false, TokenUsage{}, nil
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{
		Number: 23,
		Body:   "spec",
		// No stage:Research:complete label — mid-flight rework.
	}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 101, Author: "testuser", Body: "keep going"}}

	if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:reworking" {
			t.Errorf("fabrik:reworking applied despite stage:Research:complete never having been present")
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:reworking" {
			t.Errorf("fabrik:reworking removed despite never having been applied")
		}
	}
}

// TestProcessComments_Rework_BaseBranchFailure_RestoresCompleteLabel verifies
// that a rework begun just before a setup failure (base-branch resolution)
// is still correctly unwound: stage:<Stage>:complete is restored and
// fabrik:reworking is removed even though Claude was never invoked.
func TestProcessComments_Rework_BaseBranchFailure_RestoresCompleteLabel(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	// No git repo backing wm.baseDir: baseBranchForItem fails deterministically
	// (mirrors TestCommentBreaker_TripsOnBaseBranchFailure).
	wm := NewWorktreeManagerWithRoot(t.TempDir(), t.TempDir())
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStages(),
		},
		client,
		claude,
		wm,
	)

	item := gh.ProjectItem{
		Number: 24,
		Body:   "spec",
		Labels: []string{"stage:Review:complete"},
	}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 101, Author: "testuser", Body: "please redo this"}}
	stage := &stages.Stage{Name: "Review", Order: 1}

	if err := eng.processComments(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, comments); err == nil {
		t.Fatal("expected an error from failing base-branch resolution")
	}

	var restoredComplete, removedReworking bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Review:complete" {
			restoredComplete = true
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:reworking" {
			removedReworking = true
		}
	}
	if !restoredComplete {
		t.Errorf("expected stage:Review:complete to be restored after the setup failure; addLabelCalls=%v", client.addLabelCalls)
	}
	if !removedReworking {
		t.Errorf("expected fabrik:reworking to be removed after the setup failure; removeLabelCalls=%v", client.removeLabelCalls)
	}
	if len(claude.calls) != 0 || len(claude.forCommentsCalls) != 0 {
		t.Error("claude should never be invoked when base-branch resolution fails at setup")
	}
}

// TestItemNeedsWork_EditingGateBlocksRegardlessOfStageCompleteState is the R4
// regression guard: itemNeedsWork must refuse to dispatch while
// fabrik:editing is present, whether or not the current stage's
// stage:<Stage>:complete happens to be present — proving the rework's
// transient clear/restore of that label is fully shielded by the
// pre-existing fabrik:editing gate and can never change this decision.
func TestItemNeedsWork_EditingGateBlocksRegardlessOfStageCompleteState(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	withComplete := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:editing", "stage:Research:complete"},
	}
	withoutComplete := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:editing"},
	}

	if eng.itemNeedsWork(withComplete) {
		t.Error("itemNeedsWork returned true with fabrik:editing present and stage:Research:complete present — R4 violated")
	}
	if eng.itemNeedsWork(withoutComplete) {
		t.Error("itemNeedsWork returned true with fabrik:editing present and stage:Research:complete transiently absent — R4 violated")
	}
}

// TestProcessComments_Rework_EnsureWorktreeFailure_RestoresCompleteLabel
// mirrors TestCommentBreaker_TripsOnEnsureWorktreeFailure: base-branch
// resolution succeeds (valid bare repo) but EnsureWorktree fails
// deterministically (ENAMETOOLONG root), so the rework must still be
// unwound correctly from that second setup-failure site.
func TestProcessComments_Rework_EnsureWorktreeFailure_RestoresCompleteLabel(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManagerWithRoot(repoDir, strings.Repeat("a", 10000))
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStages(),
		},
		client,
		claude,
		wm,
	)

	item := gh.ProjectItem{
		Number: 25,
		Body:   "spec",
		Labels: []string{"stage:Review:complete"},
	}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 101, Author: "testuser", Body: "please redo this"}}
	stage := &stages.Stage{Name: "Review", Order: 1}

	if err := eng.processComments(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, comments); err == nil {
		t.Fatal("expected an error from failing worktree setup")
	}

	var restoredComplete, removedReworking bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Review:complete" {
			restoredComplete = true
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:reworking" {
			removedReworking = true
		}
	}
	if !restoredComplete {
		t.Errorf("expected stage:Review:complete to be restored after the worktree setup failure; addLabelCalls=%v", client.addLabelCalls)
	}
	if !removedReworking {
		t.Errorf("expected fabrik:reworking to be removed after the worktree setup failure; removeLabelCalls=%v", client.removeLabelCalls)
	}
	if len(claude.calls) != 0 || len(claude.forCommentsCalls) != 0 {
		t.Error("claude should never be invoked when worktree setup fails")
	}
}
