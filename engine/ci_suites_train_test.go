package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// The merge-train surfaces of #1822: classifyLandingCI, singletonFastPathEligible
// (which calls it) and pollTrainCI must all refuse to read an all-green run set
// as a complete pass while a check suite has work outstanding — the same rule
// settlePRMergeState applies, through the same ciSuiteHold primitive.

func trainRunningSuite() gh.CheckSuite {
	return gh.CheckSuite{AppSlug: "github-actions", Status: "in_progress", LatestCheckRunsCount: 5, CreatedAt: time.Now().Add(-time.Hour)}
}

func trainInertSuite() gh.CheckSuite {
	return gh.CheckSuite{AppSlug: "cursor", Status: "queued", CreatedAt: time.Now().Add(-4 * time.Hour)}
}

func TestClassifyLandingCI_SuiteAware(t *testing.T) {
	green := []gh.CheckRun{{Name: "build", Status: "completed", Conclusion: "success"}}
	failed := []gh.CheckRun{{Name: "E2E", Status: "completed", Conclusion: "failure"}}
	tests := []struct {
		name   string
		runs   []gh.CheckRun
		suites []gh.CheckSuite
		err    error
		want   TrainCIResult
		sub    string
	}{
		{"green, no suites", green, nil, nil, TrainCIGreen, ""},
		{"green, inert suite", green, []gh.CheckSuite{trainInertSuite()}, nil, TrainCIGreen, ""},
		{"green, suite outstanding", green, []gh.CheckSuite{trainRunningSuite()}, nil, TrainCIPending, "github-actions (in_progress, 5 runs)"},
		{"green, suite read error", green, nil, errors.New("boom"), TrainCIPending, "check-suite read failed"},
		{"red wins over outstanding suite", failed, []gh.CheckSuite{trainRunningSuite()}, nil, TrainCIRed, ""},
		{"zero runs clean, no suites", nil, nil, nil, TrainCIGreen, ""},
		{"zero runs clean, suite outstanding", nil, []gh.CheckSuite{trainRunningSuite()}, nil, TrainCIPending, "zero check runs"},
		{"zero runs clean, suite read error", nil, nil, errors.New("boom"), TrainCIPending, "check-suite read failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &mockGitHubClient{
				fetchCheckSuitesFn: func(owner, repo, sha string) ([]gh.CheckSuite, error) { return tc.suites, tc.err },
			}
			eng := trainTestEngine(t, c, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
			got, detail := eng.classifyLandingCI("owner", "repo", "clean", "sha1", tc.runs)
			if got != tc.want {
				t.Fatalf("result = %v (%s), want %v", got, detail, tc.want)
			}
			if !strings.Contains(detail, tc.sub) {
				t.Errorf("detail %q should contain %q", detail, tc.sub)
			}
		})
	}
}

func TestSingletonFastPathEligible_OutstandingSuite_FallsThrough(t *testing.T) {
	client := &mockGitHubClient{
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) { return 0, nil },
		fetchCheckRunsFn:     greenCompletedCheckRunFn,
		fetchCheckSuitesFn: func(owner, repo, sha string) ([]gh.CheckSuite, error) {
			return []gh.CheckSuite{trainRunningSuite()}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	p, m, pr := singletonFastPathTestSetup(eng)
	eligible, reason := eng.singletonFastPathEligible(p, m, pr)
	if eligible {
		t.Fatal("fast path must not land a member whose check suite is still running — its all-green runs are only a prefix")
	}
	if !strings.Contains(reason, "CI not confirmed green and complete") {
		t.Errorf("reason %q", reason)
	}
}

func TestSingletonFastPathEligible_SuiteReadError_FallsThrough(t *testing.T) {
	client := &mockGitHubClient{
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) { return 0, nil },
		fetchCheckRunsFn:     greenCompletedCheckRunFn,
		fetchCheckSuitesFn: func(owner, repo, sha string) ([]gh.CheckSuite, error) {
			return nil, errors.New("api down")
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	p, m, pr := singletonFastPathTestSetup(eng)
	if eligible, _ := eng.singletonFastPathEligible(p, m, pr); eligible {
		t.Fatal("a failed suite read must fail closed")
	}
}

func TestSingletonFastPathEligible_InertSuiteStillEligible(t *testing.T) {
	client := &mockGitHubClient{
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) { return 0, nil },
		fetchCheckRunsFn:     greenCompletedCheckRunFn,
		fetchCheckSuitesFn: func(owner, repo, sha string) ([]gh.CheckSuite, error) {
			return []gh.CheckSuite{trainInertSuite()}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	p, m, pr := singletonFastPathTestSetup(eng)
	if eligible, reason := eng.singletonFastPathEligible(p, m, pr); !eligible {
		t.Fatalf("an inert App suite must not deadlock the fast path: %s", reason)
	}
}

// TestPollTrainCI_HoldsWhileSuiteOutstandingThenGoesGreen: the suite is running
// for the first reads, completes, and only then may the trial be called green.
func TestPollTrainCI_HoldsWhileSuiteOutstandingThenGoesGreen(t *testing.T) {
	var reads atomic.Int32
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) { return nil, "blocked", nil },
		fetchCheckRunsFn:         greenCompletedCheckRunFn,
		fetchCheckSuitesFn: func(owner, repo, sha string) ([]gh.CheckSuite, error) {
			if reads.Add(1) <= 2 {
				return []gh.CheckSuite{trainRunningSuite()}, nil
			}
			return []gh.CheckSuite{{AppSlug: "github-actions", Status: "completed", LatestCheckRunsCount: 5}}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.trainCIPollInterval = time.Millisecond
	eng.cfg.CIBackstopTimeout = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
	if result != TrainCIGreen {
		t.Fatalf("result = %v, want Green once the suite completes", result)
	}
	if got := reads.Load(); got < 3 {
		t.Errorf("suite read %d times, want >= 3 — green must not be declared while the suite is outstanding", got)
	}
}

// TestPollTrainCI_StuckSuite_TimesOutPending: a suite stuck in_progress forever
// is bounded by CIBackstopTimeout and returns Pending, never Green (Req 6).
func TestPollTrainCI_StuckSuite_TimesOutPending(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) { return nil, "blocked", nil },
		fetchCheckRunsFn:         greenCompletedCheckRunFn,
		fetchCheckSuitesFn: func(owner, repo, sha string) ([]gh.CheckSuite, error) {
			return []gh.CheckSuite{trainRunningSuite()}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.trainCIPollInterval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123"); result != TrainCIPending {
		t.Errorf("result = %v, want Pending at the CI backstop", result)
	}
}

func TestPollTrainCI_ZeroRunsClean_SuiteOutstanding_NotGreen(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) { return &tr, "clean", nil },
		fetchCheckSuitesFn: func(owner, repo, sha string) ([]gh.CheckSuite, error) {
			return []gh.CheckSuite{trainRunningSuite()}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.trainCIPollInterval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123"); result == TrainCIGreen {
		t.Error("zero check runs + clean must not be green while a suite reports runs")
	}
}

func TestPollTrainCI_FailedRunWithOutstandingSuite_ReturnsRed(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) { return nil, "blocked", nil },
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{{Name: "E2E", Status: "completed", Conclusion: "failure"}}, nil
		},
		fetchCheckSuitesFn: func(owner, repo, sha string) ([]gh.CheckSuite, error) {
			return []gh.CheckSuite{trainRunningSuite()}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result, _ := eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123"); result != TrainCIRed {
		t.Errorf("result = %v, want Red — a confirmed failure must not wait on the suite", result)
	}
}
