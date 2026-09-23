package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// #1831: stage retry backoff must not scale with poll. These pin the two
// concepts apart so a future change cannot quietly re-couple them.

func TestStageRetryBackoff_IndependentOfPoll(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 180
	eng.cfg.RetryBackoff = 60 * time.Second

	if got := eng.stageRetryBackoff(); got != 60*time.Second {
		t.Errorf("stageRetryBackoff() = %v, want 60s — raising poll to 180 must not change retry latency", got)
	}
	if got := eng.githubRecheckInterval(); got != 30*time.Minute {
		t.Errorf("githubRecheckInterval() = %v, want 30m (10 × poll)", got)
	}
}

func TestGitHubRecheckInterval_UnaffectedByRetryBackoff(t *testing.T) {
	// The re-check cadence protects the GitHub rate limit and must keep scaling
	// with poll even when retry backoff is tiny — the opposite coupling is just
	// as wrong.
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 180
	eng.cfg.RetryBackoff = time.Second

	if got := eng.githubRecheckInterval(); got != 30*time.Minute {
		t.Errorf("githubRecheckInterval() = %v, want 30m — it must not follow retry_backoff", got)
	}
}

func TestStageRetryBackoff_ZeroFallsBackToRecheckInterval(t *testing.T) {
	// A Config built without the CLI (tests, embedders) leaves RetryBackoff at
	// zero; it keeps the pre-#1831 timing rather than retrying with no floor.
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 180
	eng.cfg.RetryBackoff = 0

	if got := eng.stageRetryBackoff(); got != 30*time.Minute {
		t.Errorf("stageRetryBackoff() with RetryBackoff unset = %v, want 30m fallback", got)
	}
}

// TestItemNeedsWork_RetryBackoffDecoupledFromPoll drives the real dispatch
// gate. Under the pre-#1831 code the 90s-ago attempt is suppressed, because
// 90s < 10 × 180s; with the fix it must be eligible. The 30s case proves the
// backoff is still enforced, not removed.
func TestItemNeedsWork_RetryBackoffDecoupledFromPoll(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sinceLast time.Duration
		want      bool
	}{
		{"attempt older than retry_backoff is retried even though poll is 180s", 90 * time.Second, true},
		{"attempt inside retry_backoff is still held off", 30 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
			eng.cfg.PollSeconds = 180
			eng.cfg.RetryBackoff = 60 * time.Second

			item := gh.ProjectItem{Number: 1831, Status: "Research"}
			eng.store.Apply(itemstate.StageAttempted{
				Repo: "owner/repo", Number: 1831, StageName: "Research",
				At: time.Now().Add(-tc.sinceLast),
			})

			if got := eng.itemNeedsWork(item); got != tc.want {
				t.Errorf("itemNeedsWork() = %v, want %v (last attempt %v ago, retry_backoff 60s, poll 180s)", got, tc.want, tc.sinceLast)
			}
		})
	}
}
