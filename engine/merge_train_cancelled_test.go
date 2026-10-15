package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// #2296: the merge train never reads a cancelled / stale / unrecognised latest
// run as green, and never treats it as a code failure either.

func TestClassifyLandingCI_NonPassing_IsNotGreenAndNotRed(t *testing.T) {
	for _, conclusion := range []string{"cancelled", "stale", "", "future_value"} {
		t.Run("conclusion="+conclusion, func(t *testing.T) {
			eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
			runs := []gh.CheckRun{
				{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
				{ID: 2, Name: "test", Status: "completed", Conclusion: conclusion},
			}
			got, detail := eng.classifyLandingCI("owner", "repo", "clean", "sha1", runs)
			if got != TrainCINotPassed {
				t.Fatalf("result = %v (%s), want TrainCINotPassed", got, detail)
			}
			if !strings.Contains(detail, "test (") || strings.Contains(detail, "failed") {
				t.Errorf("detail %q should name the check with its real conclusion and never say 'failed'", detail)
			}
		})
	}
}

// A genuine failure in the same set is the stronger disposition.
func TestClassifyLandingCI_CancelledAndFailed_IsRed(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	got, _ := eng.classifyLandingCI("owner", "repo", "clean", "sha1", []gh.CheckRun{
		{ID: 1, Name: "a", Status: "completed", Conclusion: "cancelled"},
		{ID: 2, Name: "b", Status: "completed", Conclusion: "failure"},
	})
	if got != TrainCIRed {
		t.Fatalf("result = %v, want TrainCIRed", got)
	}
}

// skipped and neutral stay passing.
func TestClassifyLandingCI_SkippedAndNeutral_StillGreen(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	got, detail := eng.classifyLandingCI("owner", "repo", "clean", "sha1", []gh.CheckRun{
		{ID: 1, Name: "a", Status: "completed", Conclusion: "skipped"},
		{ID: 2, Name: "b", Status: "completed", Conclusion: "neutral"},
	})
	if got != TrainCIGreen {
		t.Fatalf("result = %v (%s), want TrainCIGreen", got, detail)
	}
}

func TestSingletonFastPathEligible_CancelledHead_Declines(t *testing.T) {
	client := &mockGitHubClient{
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) { return 0, nil },
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{{ID: 1, Name: "test", Status: "completed", Conclusion: "cancelled"}}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	p, m, pr := singletonFastPathTestSetup(eng)
	eligible, reason := eng.singletonFastPathEligible(p, m, pr)
	if eligible {
		t.Fatal("fast path must not land a member whose own latest run is cancelled")
	}
	if !strings.Contains(reason, "cancelled") {
		t.Errorf("reason %q should name the cancelled check", reason)
	}
}

// The landing poll returns at once (no 4h backstop wait) and does not proceed.
func TestPollForMergeable_CancelledRun_ReturnsFalseImmediately(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "unstable", HeadSHA: "sha-cancelled"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{{ID: 1, Name: "test", Status: "completed", Conclusion: "cancelled"}}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if res := eng.settleAndMerge(ctx, "owner", "repo", 42, ""); res.outcome != outcomeNotPassed {
		t.Fatal("a cancelled run on the landing PR must not be landable")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("settleAndMerge waited %v; a cancelled run must return immediately", time.Since(start))
	}
}
