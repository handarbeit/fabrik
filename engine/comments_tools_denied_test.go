package engine

import (
	"context"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestProcessComments_ToolsDenied_EscalatesAtBound is the direct-call
// equivalent of TestToolsDenied_EscalatesAtBound (engine/tools_denied_test.go)
// but exercised via eng.processComments — the shared funnel for ordinary
// comment review and all three reinvoke dispatchers (#1704 R1/R2). Asserts
// that MaxToolsDeniedRetries consecutive tool-denial detections on this path
// escalate identically to the stage-dispatch path: fabrik:paused +
// fabrik:awaiting-input, never stage:<name>:failed, and the counter matches.
func TestProcessComments_ToolsDenied_EscalatesAtBound(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeToolsDeniedError{ToolNames: []string{"Bash"}}
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	const maxToolsDeniedRetries = 2
	eng.cfg.MaxToolsDeniedRetries = maxToolsDeniedRetries

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 500, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-user", Body: "please fix"}}

	for i := 0; i < maxToolsDeniedRetries; i++ {
		_ = eng.processComments(context.Background(), board, item, stage, comments)
		// Simulate the next poll cycle observing the label the prior call applied
		// (mirrors TestToolsDenied_ThreeConsecutiveDetections_OneCommentOnly's
		// re-supply-the-label idiom).
		item.Labels = []string{"fabrik:tools-denied"}
	}

	var sawFailedLabel, sawPausedLabel, sawAwaitingInputLabel bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "stage:Review:failed":
			sawFailedLabel = true
		case "fabrik:paused":
			sawPausedLabel = true
		case "fabrik:awaiting-input":
			sawAwaitingInputLabel = true
		}
	}
	if sawFailedLabel {
		t.Error("stage:Review:failed must NOT be applied — a tools-denied escalation on the comment path is never a stage failure (R5)")
	}
	if !sawPausedLabel {
		t.Error("expected fabrik:paused once MaxToolsDeniedRetries is reached via processComments")
	}
	if !sawAwaitingInputLabel {
		t.Error("expected fabrik:awaiting-input once MaxToolsDeniedRetries is reached via processComments")
	}

	snap, err := eng.store.Get("owner/repo", 500)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.ToolsDeniedRetries("Review"); got != maxToolsDeniedRetries {
		t.Errorf("ToolsDeniedRetries(\"Review\") = %d, want %d", got, maxToolsDeniedRetries)
	}
}

// TestDispatchCIFixReinvoke_ToolsDenied_EscalatesAtBound is the acceptance
// criterion's neutralization test (#1704): it drives MaxToolsDeniedRetries
// separate ci-fix-reinvoke dispatches — the designed CI-repair path — each
// returning a tool-permission-denial exit, and asserts escalation through the
// actual reinvoke goroutine scaffold (dispatchReinvoke -> processComments),
// not merely a direct processComments call. Removing the comments.go
// classification branch this issue adds must leave this test red.
func TestDispatchCIFixReinvoke_ToolsDenied_EscalatesAtBound(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeToolsDeniedError{ToolNames: []string{"Bash"}}
		},
	}
	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
	}
	eng, _ := testEngineWithRepoAndStages(t, client, claude, stgs)
	const maxToolsDeniedRetries = 2
	eng.cfg.MaxToolsDeniedRetries = maxToolsDeniedRetries

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 501, Repo: "owner/repo", Labels: []string{"fabrik:awaiting-ci"}}
	stage := stgs[0]
	settle := PRSettleResult{
		Status:    PRMergeBlocked,
		CheckRuns: []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}},
	}

	for i := 0; i < maxToolsDeniedRetries; i++ {
		eng.dispatchCIFixReinvoke(context.Background(), board, item, stage, settle)
		// dispatchReinvoke's dispatch guard (snap.Worker() != nil) blocks
		// double-dispatch — wait for this dispatch's goroutine to finish
		// (and release its Worker state) before dispatching the next one.
		eng.wg.Wait()
		item.Labels = []string{"fabrik:awaiting-ci", "fabrik:tools-denied"}
	}

	var sawFailedLabel, sawPausedLabel, sawAwaitingInputLabel bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "stage:Implement:failed":
			sawFailedLabel = true
		case "fabrik:paused":
			sawPausedLabel = true
		case "fabrik:awaiting-input":
			sawAwaitingInputLabel = true
		}
	}
	if sawFailedLabel {
		t.Error("stage:Implement:failed must NOT be applied — a tools-denied escalation via ci-fix-reinvoke is never a stage failure")
	}
	if !sawPausedLabel {
		t.Error("expected fabrik:paused once MaxToolsDeniedRetries is reached via ci-fix-reinvoke — the designed CI-repair path must not loop indefinitely on a mode denial")
	}
	if !sawAwaitingInputLabel {
		t.Error("expected fabrik:awaiting-input once MaxToolsDeniedRetries is reached via ci-fix-reinvoke")
	}

	snap, err := eng.store.Get("owner/repo", 501)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.ToolsDeniedRetries("Implement"); got != maxToolsDeniedRetries {
		t.Errorf("ToolsDeniedRetries(\"Implement\") = %d, want %d", got, maxToolsDeniedRetries)
	}
	// R5: a denial on the reinvoke path must still not count against max_retries.
	if got := snap.Attempts("Implement"); got != 0 {
		t.Errorf("Attempts(\"Implement\") = %d, want 0 (tools-denied must not count against max_retries on the reinvoke path either)", got)
	}
}

// TestProcessComments_ToolsDenied_PauseCommentNamesUnrestrictedRemedy verifies
// R4: the escalation/pause comment produced via the comment/reinvoke path
// names fabrik:unrestricted as the actionable remedy, with its trade-off —
// the same text pauseForToolsDeniedLimit already produces for the
// stage-dispatch path, since it is reused unforked (scope constraint).
func TestProcessComments_ToolsDenied_PauseCommentNamesUnrestrictedRemedy(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeToolsDeniedError{ToolNames: []string{"Bash"}}
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxToolsDeniedRetries = 1

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 502, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-user", Body: "please fix"}}

	_ = eng.processComments(context.Background(), board, item, stage, comments)

	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected at least one comment to be posted")
	}
	lastBody := client.addCommentCalls[len(client.addCommentCalls)-1].body
	if !strings.Contains(lastBody, "fabrik:unrestricted") {
		t.Errorf("expected the pause comment to name fabrik:unrestricted as the remedy, got: %s", lastBody)
	}
	if !strings.Contains(lastBody, "removes all tool restrictions") && !strings.Contains(lastBody, "bypasses the default tool allowlist") {
		t.Errorf("expected the pause comment to note fabrik:unrestricted's trade-off, got: %s", lastBody)
	}
}

// TestProcessComments_ToolsDenied_DoesNotTripCommentBreaker verifies R2 and
// the Acceptance criterion "the comment breaker's own thresholds are
// unchanged": driving more tools-denied detections than
// MaxCommentCyclesPerWindow through processComments must never trip the
// generic comment circuit breaker — only the tools-denied escalation path
// (bounded by MaxToolsDeniedRetries) may pause the issue.
func TestProcessComments_ToolsDenied_DoesNotTripCommentBreaker(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeToolsDeniedError{ToolNames: []string{"Bash"}}
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	// A generous ToolsDeniedRetries bound (never reached in this test) and a
	// comment-breaker threshold that would trip well before the loop below
	// ends, if the tools-denied branch ever fell through into it.
	eng.cfg.MaxToolsDeniedRetries = 100
	eng.cfg.MaxCommentCyclesPerWindow = 3

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 503, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-user", Body: "please fix"}}

	for i := 0; i < 10; i++ {
		_ = eng.processComments(context.Background(), board, item, stage, comments)
	}

	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "circuit breaker") {
			t.Errorf("comment-processing circuit breaker must not trip for tools-denied cycles, got comment: %s", c.body)
		}
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must not be applied — MaxToolsDeniedRetries (100) was never reached and the comment breaker must not have tripped either")
		}
	}
}
