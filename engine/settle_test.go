package engine

import (
	gh "github.com/handarbeit/fabrik/github"
	"testing"
)

// testSettleRetryStage is a synthetic retry-stage constant for exercising the
// generic settle-trio helpers directly, independent of any real settle
// scan's constant.
const testSettleRetryStage = "__test_settle__"

const testSettleMarkerLabel = "fabrik:test-settle-marker"

// TestRecordSettleRetry_EscalatesAtMaxRetries verifies that recordSettleRetry
// invokes onEscalate only once the attempt counter reaches e.cfg.MaxRetries,
// not before — mirroring the per-scan escalation-after-MaxRetries tests
// (TestRecordNoWorkNeededRetry_EscalatesAtMaxRetries and siblings) but against
// the shared helper directly.
func TestRecordSettleRetry_EscalatesAtMaxRetries(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 3

	item := gh.ProjectItem{Number: 5, Repo: "owner/repo"}

	var escalations int
	onEscalate := func(gh.ProjectItem) { escalations++ }

	for i := 1; i < eng.cfg.MaxRetries; i++ {
		eng.recordSettleRetry(item, testSettleRetryStage, onEscalate)
		if escalations != 0 {
			t.Fatalf("expected no escalation before MaxRetries reached (attempt %d), got %d escalation(s)", i, escalations)
		}
	}

	eng.recordSettleRetry(item, testSettleRetryStage, onEscalate)
	if escalations != 1 {
		t.Fatalf("expected exactly 1 escalation once MaxRetries reached, got %d", escalations)
	}
}

// TestRecordSettleRetry_MaxRetriesDisabled verifies the MaxRetries<=0 guard:
// no counter increment and no escalation, mirroring recordNoWorkNeededRetry's
// original unlimited-retries behavior.
func TestRecordSettleRetry_MaxRetriesDisabled(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 0

	item := gh.ProjectItem{Number: 5, Repo: "owner/repo"}

	var escalations int
	onEscalate := func(gh.ProjectItem) { escalations++ }

	for i := 0; i < 10; i++ {
		eng.recordSettleRetry(item, testSettleRetryStage, onEscalate)
	}
	if escalations != 0 {
		t.Fatalf("expected no escalation when MaxRetries <= 0, got %d", escalations)
	}
}

// TestClearSettleMarker_RemovesLabelAndClearsCounter verifies clearSettleMarker
// removes the marker label from GitHub and resets the retry counter.
func TestClearSettleMarker_RemovesLabelAndClearsCounter(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 5

	item := gh.ProjectItem{Number: 5, Repo: "owner/repo"}

	// Bump the retry counter without reaching MaxRetries.
	eng.recordSettleRetry(item, testSettleRetryStage, func(gh.ProjectItem) {
		t.Fatal("onEscalate must not fire before MaxRetries")
	})

	eng.clearSettleMarker(item, "owner", "repo", testSettleMarkerLabel, testSettleRetryStage)

	found := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == testSettleMarkerLabel {
			found = true
		}
	}
	if !found {
		t.Errorf("expected RemoveLabelFromIssue(%q) to be called", testSettleMarkerLabel)
	}

	snap, err := eng.store.Get("owner/repo", item.Number)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.Attempts(testSettleRetryStage); got != 0 {
		t.Errorf("expected retry counter cleared to 0, got %d", got)
	}
}

// TestEscalateSettle_PausesAndPostsComment verifies escalateSettle pauses the
// issue, removes the marker label, and invokes the postComment hook exactly
// once — the sequence every settle scan's escalate* function delegates to.
func TestEscalateSettle_PausesAndPostsComment(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 5, Repo: "owner/repo"}

	var postCommentCalls int
	eng.escalateSettle(item, testSettleMarkerLabel, testSettleRetryStage, func(gh.ProjectItem) {
		postCommentCalls++
	})

	if postCommentCalls != 1 {
		t.Fatalf("expected postComment hook to be invoked exactly once, got %d", postCommentCalls)
	}

	pausedAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added")
	}

	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == testSettleMarkerLabel {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Errorf("expected marker label %q to be removed", testSettleMarkerLabel)
	}
}
