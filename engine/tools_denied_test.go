package engine

import (
	"context"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestToolsDenied_ExemptFromRetry_NoFailureNoPause is the core
// acceptance-criteria test (AC2/AC3/AC8): a single tools-denied detection
// must record StageAttempted (so the dispatch cooldown applies) and NOT
// increment the retry count, and must apply neither stage:<name>:failed nor
// fabrik:paused. Asserting Attempts == 0 directly against the store is what
// makes this fail if the exemption is ever removed (AC8) — a test that only
// checked "not paused" would pass for the wrong reason, since a single
// detection would never reach MaxRetries=2 anyway.
func TestToolsDenied_ExemptFromRetry_NoFailureNoPause(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{TurnsUsed: 3, CostUSD: 0.05},
				&claudeToolsDeniedError{ToolNames: []string{"Write"}}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxToolsDeniedRetries: 3, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 400, Title: "tools-denied test", Status: "Research", ItemID: "PVTI_400"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 400)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	// AC2/AC8: retry count must be 0 — a tools-denied exit must not count
	// against max_retries.
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 (tools-denied must not count against max_retries)", got)
	}
	// StageAttempted must still be recorded so the normal dispatch cooldown
	// applies (the engine must not hot-loop against the denial).
	if snap.LastAttemptAt("Research").IsZero() {
		t.Error("LastAttemptAt(\"Research\") is zero, want set (cooldown must apply)")
	}
	// Its own independent counter must have incremented.
	if got := snap.ToolsDeniedRetries("Research"); got != 1 {
		t.Errorf("ToolsDeniedRetries(\"Research\") = %d, want 1", got)
	}

	// AC3: neither stage:<name>:failed nor fabrik:paused on a single detection.
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "stage:Research:failed":
			t.Error("stage:Research:failed must NOT be applied on a single tools-denied detection")
		case "fabrik:paused":
			t.Error("fabrik:paused must NOT be applied on a single tools-denied detection")
		}
	}
}

// TestToolsDenied_LabelAndCommentAppliedOnce verifies AC4/R4: the fabrik:tools-denied
// label and explanatory comment are applied on first detection.
func TestToolsDenied_LabelAndCommentAppliedOnce(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeToolsDeniedError{ToolNames: []string{"Edit", "Write"}}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxToolsDeniedRetries: 3, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 401, Title: "tools-denied label/comment test", Status: "Research", ItemID: "PVTI_401"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	var sawLabel bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:tools-denied" {
			sawLabel = true
		}
	}
	if !sawLabel {
		t.Error("expected fabrik:tools-denied label to be applied")
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "Edit") || !strings.Contains(body, "Write") {
		t.Errorf("expected comment to name the denied tools, got: %s", body)
	}
	if !strings.Contains(body, "permission") {
		t.Errorf("expected comment to point at the permission configuration, got: %s", body)
	}
}

// TestToolsDenied_ThreeConsecutiveDetections_OneCommentOnly is the end-to-end
// AC4 scenario: three consecutive tools-denied detections must post exactly
// one comment (once-per-episode cadence) and add fabrik:tools-denied exactly
// once, mirroring TestUsageLimitExit_SecondConsecutiveHit_DoesNotRepostComment's
// shape of manually re-supplying the label between calls to simulate a fresh
// board fetch observing the first call's label add.
func TestToolsDenied_ThreeConsecutiveDetections_OneCommentOnly(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			return "", false, TokenUsage{}, &claudeToolsDeniedError{ToolNames: []string{"Write"}}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxToolsDeniedRetries: 5, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 402, Title: "Three tools-denied detections", Status: "Research", ItemID: "PVTI_402"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (first call): %v", err)
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("after first detection: expected 1 comment, got %d", len(client.addCommentCalls))
	}

	// Simulate the next two poll cycles observing the label the first call applied.
	item.Labels = []string{"fabrik:tools-denied"}
	for i := 0; i < 2; i++ {
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem (call %d): %v", i+2, err)
		}
	}

	if len(client.addCommentCalls) != 1 {
		t.Errorf("after three consecutive detections: expected comment count to stay at 1, got %d", len(client.addCommentCalls))
	}
	labelAddCount := 0
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:tools-denied" {
			labelAddCount++
		}
	}
	if labelAddCount != 1 {
		t.Errorf("expected fabrik:tools-denied to be added exactly once, got %d", labelAddCount)
	}
	if callCount != 3 {
		t.Fatalf("expected exactly 3 Claude invocations, got %d", callCount)
	}

	// Retry count must still be 0 after all three detections.
	snap, err := eng.store.Get("owner/repo", 402)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 after 3 tools-denied detections", got)
	}
	if got := snap.ToolsDeniedRetries("Research"); got != 3 {
		t.Errorf("ToolsDeniedRetries(\"Research\") = %d, want 3", got)
	}
}

// TestToolsDenied_FourthSuccessfulInvocation_ClearsLabelAndCompletes is AC5:
// after three tools-denied detections, a fourth, successful invocation must
// clear fabrik:tools-denied and complete the stage normally.
func TestToolsDenied_FourthSuccessfulInvocation_ClearsLabelAndCompletes(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			if callCount <= 3 {
				return "", false, TokenUsage{}, &claudeToolsDeniedError{ToolNames: []string{"Write"}}
			}
			return "done\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxToolsDeniedRetries: 5, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 403, Title: "Three tools-denied then success", Status: "Research", ItemID: "PVTI_403"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (first call): %v", err)
	}
	item.Labels = []string{"fabrik:tools-denied"}
	for i := 0; i < 2; i++ {
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem (call %d): %v", i+2, err)
		}
	}

	// Fourth call: a genuine successful invocation. item.Labels still carries
	// fabrik:tools-denied from the prior detections, as it would after a real
	// board re-fetch.
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (success call): %v", err)
	}

	var sawFailedLabel, sawPausedLabel, sawCompleteLabel, sawLabelRemoved bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "stage:Research:failed":
			sawFailedLabel = true
		case "fabrik:paused":
			sawPausedLabel = true
		case "stage:Research:complete":
			sawCompleteLabel = true
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:tools-denied" {
			sawLabelRemoved = true
		}
	}
	if sawFailedLabel {
		t.Error("stage:Research:failed must NOT be applied — three tools-denied detections never charged against max_retries")
	}
	if sawPausedLabel {
		t.Error("fabrik:paused must NOT be applied — three tools-denied detections never charged against max_retries")
	}
	if !sawCompleteLabel {
		t.Error("expected stage:Research:complete to be added once the successful invocation lands")
	}
	if !sawLabelRemoved {
		t.Error("expected fabrik:tools-denied to be removed on the next invocation not classified as tools-denied (R3/AC5)")
	}
	if callCount != 4 {
		t.Errorf("expected exactly 4 Claude invocations, got %d", callCount)
	}
}

// TestToolsDenied_EscalatesAtBound is AC6: after MaxToolsDeniedRetries
// consecutive detections, the issue escalates to fabrik:paused +
// fabrik:awaiting-input with an explanatory comment, and still never applies
// stage:<name>:failed — the condition is never treated as a stage failure,
// even at the bound (R5).
func TestToolsDenied_EscalatesAtBound(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, &claudeToolsDeniedError{ToolNames: []string{"Write"}}
		},
	}

	const maxToolsDeniedRetries = 2
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 10, MaxToolsDeniedRetries: maxToolsDeniedRetries, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 404, Title: "tools-denied escalation", Status: "Research", ItemID: "PVTI_404"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (call 1): %v", err)
	}
	item.Labels = []string{"fabrik:tools-denied"}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (call 2, hits the bound): %v", err)
	}

	var sawFailedLabel, sawPausedLabel, sawAwaitingInputLabel bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "stage:Research:failed":
			sawFailedLabel = true
		case "fabrik:paused":
			sawPausedLabel = true
		case "fabrik:awaiting-input":
			sawAwaitingInputLabel = true
		}
	}
	if sawFailedLabel {
		t.Error("stage:Research:failed must NOT be applied — a tools-denied escalation is never a stage failure (R5)")
	}
	if !sawPausedLabel {
		t.Error("expected fabrik:paused to be applied once MaxToolsDeniedRetries is reached")
	}
	if !sawAwaitingInputLabel {
		t.Error("expected fabrik:awaiting-input to be applied once MaxToolsDeniedRetries is reached")
	}

	snap, err := eng.store.Get("owner/repo", 404)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.ToolsDeniedRetries("Research"); got != maxToolsDeniedRetries {
		t.Errorf("ToolsDeniedRetries(\"Research\") = %d, want %d", got, maxToolsDeniedRetries)
	}

	// A comment naming the condition must have been posted (at least the
	// initial-detection comment, and the pause comment on escalation).
	if len(client.addCommentCalls) < 2 {
		t.Fatalf("expected at least 2 comments (initial detection + escalation), got %d", len(client.addCommentCalls))
	}
	lastBody := client.addCommentCalls[len(client.addCommentCalls)-1].body
	if !strings.Contains(lastBody, "permission") {
		t.Errorf("expected escalation comment to name the tools-denied condition, got: %s", lastBody)
	}
}

// TestToolsDenied_MarkerIndependence_WithBlockedOnInputMarker and
// TestToolsDenied_MarkerIndependence_WithoutBlockedOnInputMarker together
// cover AC7/R6: the outcome must be identical whether or not the worker also
// emits FABRIK_BLOCKED_ON_INPUT — the engine's own structural classification
// must be what governs the outcome, not a worker-side courtesy marker. Both
// variants must see the same tools-denied label/comment behavior and must
// NOT see fabrik:awaiting-input applied via the ordinary blockOnInput path
// (blockedOnInput := err == nil && ... is structurally unreachable once
// interpretClaudeResult returns a non-nil error for this condition).
func TestToolsDenied_MarkerIndependence_WithBlockedOnInputMarker(t *testing.T) {
	testToolsDeniedMarkerIndependence(t, "Permission to use Edit has been denied because Claude Code is running in don't ask mode.\nFABRIK_BLOCKED_ON_INPUT", 405)
}

func TestToolsDenied_MarkerIndependence_WithoutBlockedOnInputMarker(t *testing.T) {
	testToolsDeniedMarkerIndependence(t, "Permission to use Edit has been denied because Claude Code is running in don't ask mode.", 406)
}

func testToolsDeniedMarkerIndependence(t *testing.T, output string, issueNumber int) {
	t.Helper()
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return output, false, TokenUsage{}, &claudeToolsDeniedError{ToolNames: []string{"Edit"}}
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, MaxToolsDeniedRetries: 3, Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: issueNumber, Title: "tools-denied marker independence", Status: "Research", ItemID: "PVTI_marker"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", issueNumber)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts("Research"); got != 0 {
		t.Errorf("Attempts(\"Research\") = %d, want 0 regardless of FABRIK_BLOCKED_ON_INPUT presence", got)
	}

	var sawToolsDeniedLabel, sawAwaitingInputLabel bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:tools-denied":
			sawToolsDeniedLabel = true
		case "fabrik:awaiting-input":
			sawAwaitingInputLabel = true
		}
	}
	if !sawToolsDeniedLabel {
		t.Error("expected fabrik:tools-denied to be applied regardless of FABRIK_BLOCKED_ON_INPUT presence")
	}
	// The ordinary blockOnInput path must never fire for this condition — the
	// classification error makes blockedOnInput := err == nil && ...
	// structurally false, so no fabrik:awaiting-input from that path (a
	// single detection also never reaches the MaxToolsDeniedRetries pause
	// path, which is the only other source of fabrik:awaiting-input here).
	if sawAwaitingInputLabel {
		t.Error("fabrik:awaiting-input must not be applied via the blockOnInput path for a tools-denied exit (R6)")
	}
	// Two comments are expected regardless of marker presence: the stage's own
	// (non-empty) output text is always posted as a comment independent of
	// err, plus the tools-denied explanatory comment (R4). The count and
	// content must be identical whether or not FABRIK_BLOCKED_ON_INPUT is
	// present in that output text — that identity is what this pair of tests
	// (called with and without the marker) demonstrates.
	if len(client.addCommentCalls) != 2 {
		t.Fatalf("expected exactly 2 comments (stage output + tools-denied explanation), got %d", len(client.addCommentCalls))
	}
	var sawExplanatoryComment bool
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "permission") && strings.Contains(c.body, "Edit") {
			sawExplanatoryComment = true
		}
	}
	if !sawExplanatoryComment {
		t.Error("expected one comment to explain the tools-denied condition, naming the denied tool")
	}
}
