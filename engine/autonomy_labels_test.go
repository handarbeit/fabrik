package engine

import (
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tui"
)

// TestRefreshAutonomyLabels_FetchError_LeavesSnapshotAndLogsWarning is
// Acceptance Criterion 5: a re-fetch error leaves the snapshot in place and
// logs a warning naming the item and the error (R2).
func TestRefreshAutonomyLabels_FetchError_LeavesSnapshotAndLogsWarning(t *testing.T) {
	fetchErr := testSentinelError("boom")
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return nil, fetchErr
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	events := make(chan tui.Event, 8)
	eng.events = events

	item := gh.ProjectItem{Number: 42, Labels: []string{"fabrik:cruise"}}
	eng.refreshAutonomyLabels(&item)

	close(events)
	eng.events = nil

	if len(item.Labels) != 1 || item.Labels[0] != "fabrik:cruise" {
		t.Fatalf("expected snapshot to survive a fetch error unchanged, got %v", item.Labels)
	}

	var logged []tui.LogEvent
	for ev := range events {
		if le, ok := ev.(tui.LogEvent); ok {
			logged = append(logged, le)
		}
	}
	if len(logged) != 1 {
		t.Fatalf("expected exactly 1 warning log event, got %d: %v", len(logged), logged)
	}
	if logged[0].IssueNumber != 42 {
		t.Errorf("log event IssueNumber = %d, want 42", logged[0].IssueNumber)
	}
	if !strings.Contains(logged[0].Message, "boom") {
		t.Errorf("log message %q does not name the underlying error", logged[0].Message)
	}
}

// TestRefreshAutonomyLabels_ZeroLabelsSuccess_ReplacesSnapshot is Acceptance
// Criterion 6 / R3: a successful fetch returning zero labels is real data
// and must replace the snapshot, not be discarded — even though the old,
// buggy `len(freshLabels) > 0` guard would have kept the stale
// fabrik:cruise label in this exact scenario.
func TestRefreshAutonomyLabels_ZeroLabelsSuccess_ReplacesSnapshot(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:cruise"}}
	eng.refreshAutonomyLabels(&item)

	if hasCruiseLabel(item) {
		t.Error("expected fabrik:cruise to be cleared by a successful zero-labels fetch, but hasCruiseLabel still reports true")
	}
	if len(item.Labels) != 0 {
		t.Errorf("expected item.Labels to be replaced with the empty fetch result, got %v", item.Labels)
	}
}

// TestRefreshAutonomyLabels_Success_ReplacesEvenNonEmptySnapshot is a
// non-vacuousness check (Acceptance 7) for the success path: a successful
// fetch returning a *different* non-empty label set must also replace the
// snapshot outright — proving the assignment isn't merely additive or a
// no-op that happens to coincide with the old value.
func TestRefreshAutonomyLabels_Success_ReplacesEvenNonEmptySnapshot(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"fabrik:yolo"}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:cruise"}}
	eng.refreshAutonomyLabels(&item)

	if hasCruiseLabel(item) {
		t.Error("stale fabrik:cruise must not survive a successful fetch that no longer reports it")
	}
	if !hasYoloLabel(item) {
		t.Error("expected fabrik:yolo from the fresh fetch to be observed")
	}
}

// testSentinelError is a distinct error type from errFetchLabelsNotConfigured,
// used where a test wants to assert its own error message is the one
// surfaced in the log, rather than the mock's built-in default.
type testSentinelError string

func (e testSentinelError) Error() string { return string(e) }
