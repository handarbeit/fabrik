package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// turnCapExitErr mimics the real-world turn-cap kill from issue #1081: the Claude
// CLI's own --max-turns self-termination exits non-zero, so completed=false AND
// err != nil together — the exact combination that must NOT be masked by an
// err == nil requirement (that's why the existing intra-dispatch hitLimit check
// never fires for this case; see interpretClaudeResult).
var turnCapExitErr = errors.New("claude exited with error: exit status 1")

// TestCappedRun_AnnotatesSuccessorAfterCappedPredecessor verifies the core fix:
// a stage invocation that is turn-capped does not carry the annotation on its own
// comment (it has no predecessor), but the very next invocation of the same stage
// does — and the annotation is recorded in ItemState.LastInvocationAfterCappedRun
// (the field the InvocationObserver forwards into history.json).
func TestCappedRun_AnnotatesSuccessorAfterCappedPredecessor(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			switch callCount {
			case 1:
				return "predecessor output", false, TokenUsage{TurnsUsed: 50}, turnCapExitErr
			case 2:
				return "successor output\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{TurnsUsed: 20}, nil
			default:
				t.Errorf("unexpected invocation #%d", callCount)
				return "", false, TokenUsage{}, nil
			}
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			Stages:     implementStages(50),
		},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 101, Title: "Capped Run Test", Status: "Implement", ItemID: "PVTI_101"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (predecessor): %v", err)
	}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (successor): %v", err)
	}

	if callCount != 2 {
		t.Fatalf("expected 2 claude invocations, got %d", callCount)
	}
	if len(client.addCommentCalls) != 2 {
		t.Fatalf("expected 2 posted comments, got %d", len(client.addCommentCalls))
	}
	predecessorComment := client.addCommentCalls[0].body
	successorComment := client.addCommentCalls[1].body

	if strings.Contains(predecessorComment, "Provenance notice") {
		t.Errorf("predecessor's own comment should not carry the annotation (no prior capped run): %q", predecessorComment)
	}
	if !strings.Contains(successorComment, "Provenance notice") {
		t.Errorf("successor comment missing provenance annotation: %q", successorComment)
	}
	if !strings.Contains(successorComment, "successor output") {
		t.Errorf("successor comment missing its own content: %q", successorComment)
	}
	if idx1, idx2 := strings.Index(successorComment, "Provenance notice"), strings.Index(successorComment, "successor output"); idx1 > idx2 {
		t.Errorf("annotation should precede the stage's own content, got annotation at %d, content at %d", idx1, idx2)
	}

	snap, err := eng.store.Get("owner/repo", 101)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !snap.State().LastInvocationAfterCappedRun {
		t.Error("ItemState.LastInvocationAfterCappedRun not set on the successor invocation")
	}
}

// TestCappedRun_NoAnnotationOnFirstRun verifies a stage's very first invocation —
// which by definition has no predecessor — never carries the annotation, even when
// that first run is itself turn-capped.
func TestCappedRun_NoAnnotationOnFirstRun(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "first run output", false, TokenUsage{TurnsUsed: 50}, turnCapExitErr
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token", Stages: implementStages(50)},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 102, Title: "First Run Test", Status: "Implement", ItemID: "PVTI_102"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 posted comment, got %d", len(client.addCommentCalls))
	}
	if strings.Contains(client.addCommentCalls[0].body, "Provenance notice") {
		t.Errorf("first-ever run should never carry the annotation: %q", client.addCommentCalls[0].body)
	}
}

// TestCappedRun_AnnotationPersistsAcrossInterveningCommentInvocation verifies the
// mis-scoping risk Research flagged: a comment-processing invocation for the same
// issue (which overwrites the item-scoped LastInvocationCompleted/LastTokenUsage
// fields) must NOT clear the stage-scoped capped-run signal. This directly
// exercises the read site in processItem (snap.StageCapped(stage.Name)) against a
// store that has had an intervening, unrelated InvocationRecorded mutation applied
// — the realistic shape of "a user commented between the capped run and the retry."
func TestCappedRun_AnnotationPersistsAcrossInterveningCommentInvocation(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			switch callCount {
			case 1:
				return "predecessor output", false, TokenUsage{TurnsUsed: 50}, turnCapExitErr
			case 2:
				return "successor output\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{TurnsUsed: 20}, nil
			default:
				t.Errorf("unexpected invocation #%d", callCount)
				return "", false, TokenUsage{}, nil
			}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token", Stages: implementStages(50)},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 103, Title: "Intervening Comment Test", Status: "Implement", ItemID: "PVTI_103"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (predecessor): %v", err)
	}

	// Simulate a comment-processing invocation completing for this issue in between
	// the capped run and the retry — this is exactly the item-scoped mutation that
	// would clobber a naive "last invocation" read.
	if _, _, err := eng.store.Apply(itemstate.InvocationRecorded{
		Repo:      "owner/repo",
		Number:    103,
		Completed: true,
		IsComment: true,
	}); err != nil {
		t.Fatalf("simulate intervening comment invocation: %v", err)
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (successor): %v", err)
	}

	if len(client.addCommentCalls) != 2 {
		t.Fatalf("expected 2 posted comments, got %d", len(client.addCommentCalls))
	}
	successorComment := client.addCommentCalls[1].body
	if !strings.Contains(successorComment, "Provenance notice") {
		t.Errorf("annotation was lost across an intervening comment-processing invocation: %q", successorComment)
	}
}

// TestCappedRun_AnnotationReappearsAcrossCappedChain verifies that a chain of
// consecutive capped runs (the RallyRaffle 51/51/23/18/18 case from the issue)
// annotates every successor, not just the second run in the chain: the flag is
// read-then-unconditionally-overwritten on every finalize, so it correctly
// tracks "immediately preceding," not "ever capped."
func TestCappedRun_AnnotationReappearsAcrossCappedChain(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			switch callCount {
			case 1:
				return "run1 output", false, TokenUsage{TurnsUsed: 50}, turnCapExitErr
			case 2:
				return "run2 output", false, TokenUsage{TurnsUsed: 50}, turnCapExitErr
			case 3:
				return "run3 output\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{TurnsUsed: 18}, nil
			default:
				t.Errorf("unexpected invocation #%d", callCount)
				return "", false, TokenUsage{}, nil
			}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token", Stages: implementStages(50)},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 104, Title: "Capped Chain Test", Status: "Implement", ItemID: "PVTI_104"}

	for i := 0; i < 3; i++ {
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem run %d: %v", i+1, err)
		}
	}

	if len(client.addCommentCalls) != 3 {
		t.Fatalf("expected 3 posted comments, got %d", len(client.addCommentCalls))
	}
	run1, run2, run3 := client.addCommentCalls[0].body, client.addCommentCalls[1].body, client.addCommentCalls[2].body

	if strings.Contains(run1, "Provenance notice") {
		t.Errorf("run 1 (no predecessor) should not carry the annotation: %q", run1)
	}
	if !strings.Contains(run2, "Provenance notice") {
		t.Errorf("run 2 (predecessor run 1 capped) missing annotation: %q", run2)
	}
	if !strings.Contains(run3, "Provenance notice") {
		t.Errorf("run 3 (predecessor run 2 capped) missing annotation: %q", run3)
	}
}

// TestCappedRun_FewTurnCompletionLogsWarning verifies the first "suspicious shape"
// heuristic: a run that completes after a capped predecessor using well under a
// quarter of the stage's turn budget logs a warning (log-only signal, no gate).
func TestCappedRun_FewTurnCompletionLogsWarning(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			switch callCount {
			case 1:
				return "predecessor output", false, TokenUsage{TurnsUsed: 50}, turnCapExitErr
			case 2:
				// 2 turns against a 50-turn budget — far under the MaxTurns/4 threshold.
				return "successor output\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{TurnsUsed: 2}, nil
			default:
				t.Errorf("unexpected invocation #%d", callCount)
				return "", false, TokenUsage{}, nil
			}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token", Stages: implementStages(50)},
		client, claude, wm,
	)
	ch := make(chan tui.Event, 200)
	eng.SetEvents(ch)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 105, Title: "Few Turn Test", Status: "Implement", ItemID: "PVTI_105"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (predecessor): %v", err)
	}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (successor): %v", err)
	}
	close(ch)

	var found bool
	for ev := range ch {
		if le, ok := ev.(tui.LogEvent); ok && le.IssueNumber == 105 && le.Tag == "warn" &&
			strings.Contains(le.Message, "immediately after a turn-capped predecessor") {
			found = true
		}
	}
	if !found {
		t.Error("expected a 'warn' log event for a few-turn completion after a capped predecessor")
	}
}

// TestCappedRun_NoFewTurnWarningWithoutCappedPredecessor verifies the few-turn
// heuristic does not fire on a normal low-turn completion that has no capped
// predecessor — it must not become a generic "stage finished quickly" warning.
func TestCappedRun_NoFewTurnWarningWithoutCappedPredecessor(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "output\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{TurnsUsed: 2}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token", Stages: implementStages(50)},
		client, claude, wm,
	)
	ch := make(chan tui.Event, 200)
	eng.SetEvents(ch)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 106, Title: "No Predecessor Few Turn Test", Status: "Implement", ItemID: "PVTI_106"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	close(ch)

	for ev := range ch {
		if le, ok := ev.(tui.LogEvent); ok && le.Tag == "warn" &&
			strings.Contains(le.Message, "immediately after a turn-capped predecessor") {
			t.Errorf("unexpected few-turn warning without a capped predecessor: %q", le.Message)
		}
	}
}

// TestCappedRun_SelfAdmissionLanguageLogsWarning verifies the second "suspicious
// shape" heuristic: output naming the exact failure mode of issue #1081 (the
// model is aware it resumed a prior interrupted session, but still reports
// verification as first-hand) logs a warning when it follows a capped predecessor.
func TestCappedRun_SelfAdmissionLanguageLogsWarning(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			switch callCount {
			case 1:
				return "predecessor output", false, TokenUsage{TurnsUsed: 50}, turnCapExitErr
			case 2:
				return "Verified the prior session's implementation is complete and correct.\nFABRIK_STAGE_COMPLETE\n",
					true, TokenUsage{TurnsUsed: 30}, nil
			default:
				t.Errorf("unexpected invocation #%d", callCount)
				return "", false, TokenUsage{}, nil
			}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token", Stages: implementStages(50)},
		client, claude, wm,
	)
	ch := make(chan tui.Event, 200)
	eng.SetEvents(ch)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 107, Title: "Self Admission Test", Status: "Implement", ItemID: "PVTI_107"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (predecessor): %v", err)
	}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (successor): %v", err)
	}
	close(ch)

	var found bool
	for ev := range ch {
		if le, ok := ev.(tui.LogEvent); ok && le.IssueNumber == 107 && le.Tag == "warn" &&
			strings.Contains(le.Message, "inheriting a prior interrupted session") {
			found = true
		}
	}
	if !found {
		t.Error("expected a 'warn' log event for self-admission language after a capped predecessor")
	}
}

// TestCappedRun_NoSelfAdmissionWarningWithoutCappedPredecessor verifies the
// self-admission heuristic is gated on afterCapped — coincidentally similar
// phrasing in ordinary output (no capped predecessor) must not trigger it.
func TestCappedRun_NoSelfAdmissionWarningWithoutCappedPredecessor(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "Verified the prior session's implementation is complete and correct.\nFABRIK_STAGE_COMPLETE\n",
				true, TokenUsage{TurnsUsed: 30}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token", Stages: implementStages(50)},
		client, claude, wm,
	)
	ch := make(chan tui.Event, 200)
	eng.SetEvents(ch)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 108, Title: "No Predecessor Self Admission Test", Status: "Implement", ItemID: "PVTI_108"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	close(ch)

	for ev := range ch {
		if le, ok := ev.(tui.LogEvent); ok && le.Tag == "warn" &&
			strings.Contains(le.Message, "inheriting a prior interrupted session") {
			t.Errorf("unexpected self-admission warning without a capped predecessor: %q", le.Message)
		}
	}
}

// TestCappedRun_RecordedDespiteGenericInvocationError verifies StageCappedRunRecorded
// still fires when the invocation returns a generic (non-start-failure) error —
// matching the real turn-cap kill, which is the CLI exiting non-zero, not a start
// failure like exec.Error or os.PathError. This is the exact condition the existing
// intra-dispatch hitLimit check gets wrong (it requires err == nil); the fix here
// must not repeat that mistake.
func TestCappedRun_RecordedDespiteGenericInvocationError(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "output", false, TokenUsage{TurnsUsed: 50}, turnCapExitErr
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token", Stages: implementStages(50)},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 109, Title: "Unrelated Error Test", Status: "Implement", ItemID: "PVTI_109"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 109)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !snap.StageCapped("Implement") {
		t.Error("StageCapped(Implement) should be true after a turn-cap-shaped error (non-start-failure)")
	}
}
