package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// #2064: the label-anchored CI/merge-gate timeouts measure from the later of the
// label's applied time and the item's latest fabrik:paused removal, through the
// one shared effectiveAnchor helper.

func removedAtFn(at time.Time, err error) func(owner, repo string, n int, label string) (time.Time, error) {
	return func(_, _ string, _ int, _ string) (time.Time, error) { return at, err }
}

func addedPaused(c *mockGitHubClient) bool {
	for _, l := range c.addLabelCalls {
		if l.labelName == "fabrik:paused" {
			return true
		}
	}
	return false
}

func TestEffectiveAnchor(t *testing.T) {
	item := gh.ProjectItem{Number: 7, Repo: "owner/repo"}
	applied := time.Now().Add(-2 * time.Hour)

	t.Run("later removal wins", func(t *testing.T) {
		resumed := time.Now().Add(-5 * time.Minute)
		client := &mockGitHubClient{fetchLabelRemovedAtFn: removedAtFn(resumed, nil)}
		eng := testEngineForMerge(t, client)
		if got := eng.effectiveAnchor(item, "owner", "repo", "t", "test", applied, 30*time.Minute); !got.Equal(resumed) {
			t.Errorf("got %v, want the resume time %v", got, resumed)
		}
	})
	t.Run("earlier or zero removal keeps the label anchor", func(t *testing.T) {
		for _, at := range []time.Time{{}, applied.Add(-time.Hour)} {
			client := &mockGitHubClient{fetchLabelRemovedAtFn: removedAtFn(at, nil)}
			eng := testEngineForMerge(t, client)
			if got := eng.effectiveAnchor(item, "owner", "repo", "t", "test", applied, 30*time.Minute); !got.Equal(applied) {
				t.Errorf("removal %v: got %v, want label anchor %v", at, got, applied)
			}
		}
	})
	t.Run("read error falls back to the label anchor", func(t *testing.T) {
		client := &mockGitHubClient{fetchLabelRemovedAtFn: removedAtFn(time.Time{}, errors.New("boom"))}
		eng := testEngineForMerge(t, client)
		if got := eng.effectiveAnchor(item, "owner", "repo", "t", "test", applied, 30*time.Minute); !got.Equal(applied) {
			t.Errorf("got %v, want label anchor %v", got, applied)
		}
	})
	t.Run("memo inside the caller's window skips the read", func(t *testing.T) {
		resumed := time.Now().Add(-5 * time.Minute)
		client := &mockGitHubClient{fetchLabelRemovedAtFn: removedAtFn(resumed, nil)}
		eng := testEngineForMerge(t, client)
		eng.effectiveAnchor(item, "owner", "repo", "t", "test", applied, 30*time.Minute)
		got := eng.effectiveAnchor(item, "owner", "repo", "t", "test", applied, 30*time.Minute)
		if !got.Equal(resumed) || len(client.fetchLabelRemovedAtCalls) != 1 {
			t.Errorf("got %v after %d reads, want memoised %v after 1 read", got, len(client.fetchLabelRemovedAtCalls), resumed)
		}
	})
	t.Run("memo outside the caller's window forces a live read", func(t *testing.T) {
		resumed := time.Now().Add(-45 * time.Minute)
		client := &mockGitHubClient{fetchLabelRemovedAtFn: removedAtFn(resumed, nil)}
		eng := testEngineForMerge(t, client)
		// Valid for a 4h window (the backstop), but long past a 30m dwell.
		eng.effectiveAnchor(item, "owner", "repo", "t", "test", applied, 4*time.Hour)
		eng.effectiveAnchor(item, "owner", "repo", "t", "test", applied, 30*time.Minute)
		if n := len(client.fetchLabelRemovedAtCalls); n != 2 {
			t.Errorf("%d reads, want 2 (a memo outside the window must not skip the read)", n)
		}
	})
}

// ── classifyCIFromMergeableState: R3 never-checked dwell and blocked dwell ────

func mergeableStateDwell(t *testing.T, state string, resumedAt time.Time, resumeErr error, appliedAgo time.Duration) (*mockGitHubClient, bool, bool, bool) {
	t.Helper()
	client := &mockGitHubClient{
		fetchLabelAppliedAtFn: func(_, _ string, _ int, _ string) (time.Time, error) {
			return time.Now().Add(-appliedAgo), nil
		},
		fetchLabelRemovedAtFn: removedAtFn(resumedAt, resumeErr),
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = 30 * time.Minute
	tr := true
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:awaiting-ci"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	settle := PRSettleResult{
		Status:         PRMergeUnsettled,
		MergeableState: state,
		PR:             &gh.PRDetails{Number: 5, HeadSHA: "sha", State: "open"},
	}
	blocked, _, timedOut, terminated := eng.checkCIGate(nil, item, stage, settle)
	return client, blocked, timedOut, terminated
}

func TestCIWaitDwells_ExcludePausedTime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		// escalated reports whether the dwell fired for this outcome.
		escalated func(blocked, timedOut, terminated bool) bool
	}{
		{"R3 never-checked dwell", "blocked", func(_, _, terminated bool) bool { return terminated }},
		{"mergeable-state-blocked dwell", "behind", func(_, timedOut, _ bool) bool { return timedOut }},
	} {
		t.Run(tc.name+"/resumed recently is not escalated", func(t *testing.T) {
			_, blocked, timedOut, terminated := mergeableStateDwell(t, tc.state, time.Now().Add(-5*time.Minute), nil, 2*time.Hour)
			if tc.escalated(blocked, timedOut, terminated) || !blocked {
				t.Errorf("blocked=%v timedOut=%v terminated=%v, want blocked and not escalated", blocked, timedOut, terminated)
			}
		})
		t.Run(tc.name+"/resumed longer ago than the timeout is escalated", func(t *testing.T) {
			_, blocked, timedOut, terminated := mergeableStateDwell(t, tc.state, time.Now().Add(-time.Hour), nil, 2*time.Hour)
			if !tc.escalated(blocked, timedOut, terminated) {
				t.Errorf("blocked=%v timedOut=%v terminated=%v, want escalated", blocked, timedOut, terminated)
			}
		})
		t.Run(tc.name+"/unreadable removal time falls back to the label anchor", func(t *testing.T) {
			_, blocked, timedOut, terminated := mergeableStateDwell(t, tc.state, time.Time{}, errors.New("boom"), 2*time.Hour)
			if !tc.escalated(blocked, timedOut, terminated) {
				t.Errorf("blocked=%v timedOut=%v terminated=%v, want escalated", blocked, timedOut, terminated)
			}
		})
		t.Run(tc.name+"/under the timeout costs no event-log read", func(t *testing.T) {
			client, blocked, timedOut, terminated := mergeableStateDwell(t, tc.state, time.Time{}, nil, time.Minute)
			if tc.escalated(blocked, timedOut, terminated) || len(client.fetchLabelRemovedAtCalls) != 0 {
				t.Errorf("escalated=%v reads=%d, want neither", tc.escalated(blocked, timedOut, terminated), len(client.fetchLabelRemovedAtCalls))
			}
		})
	}
}

// ── checkAutoMergeConvergence: merge-queue stall dwell and ConvergenceBudget ──

func queueStallRun(t *testing.T, ciWait time.Duration, appliedAgo time.Duration, resumedAt time.Time, resumeErr error) *mockGitHubClient {
	t.Helper()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(_, _ string, _ int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", IsMergeQueueEnabled: true}, nil
		},
		fetchLabelAppliedAtFn: func(_, _ string, _ int, _ string) (time.Time, error) {
			return time.Now().Add(-appliedAgo), nil
		},
		fetchLabelRemovedAtFn: removedAtFn(resumedAt, resumeErr),
		addCommentFn:          func(_, _ string, _ int, _ string) (int, error) { return 999, nil },
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.CIWaitTimeout = ciWait
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	settle := PRSettleResult{Status: PRMergeQueued, PR: &gh.PRDetails{Number: 10, HeadSHA: "abc12345", IsMergeQueueEnabled: true}}
	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Validate"}, settle, false)
	return client
}

func TestMergeQueueStall_ExcludesPausedTime(t *testing.T) {
	t.Run("resumed recently is not escalated", func(t *testing.T) {
		if c := queueStallRun(t, 30*time.Minute, 2*time.Hour, time.Now().Add(-5*time.Minute), nil); addedPaused(c) {
			t.Error("stall pause fired for an item resumed 5m ago")
		}
	})
	t.Run("resumed longer ago than the timeout is escalated", func(t *testing.T) {
		if c := queueStallRun(t, 30*time.Minute, 2*time.Hour, time.Now().Add(-time.Hour), nil); !addedPaused(c) {
			t.Error("expected the stall pause")
		}
	})
	t.Run("unreadable removal time falls back to the label anchor", func(t *testing.T) {
		if c := queueStallRun(t, 30*time.Minute, 2*time.Hour, time.Time{}, errors.New("boom")); !addedPaused(c) {
			t.Error("expected the stall pause on fallback")
		}
	})
	t.Run("under the timeout costs no event-log read", func(t *testing.T) {
		if c := queueStallRun(t, 30*time.Minute, time.Minute, time.Time{}, nil); len(c.fetchLabelRemovedAtCalls) != 0 {
			t.Errorf("%d event-log reads, want 0", len(c.fetchLabelRemovedAtCalls))
		}
	})
	t.Run("CIWaitTimeout zero still disables the check", func(t *testing.T) {
		c := queueStallRun(t, 0, 2*time.Hour, time.Time{}, nil)
		if addedPaused(c) || len(c.fetchLabelRemovedAtCalls) != 0 {
			t.Error("stall check ran with CIWaitTimeout == 0")
		}
	})
}

func budgetRun(t *testing.T, appliedAgo time.Duration, resumedAt time.Time, resumeErr error) *mockGitHubClient {
	t.Helper()
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(_, _ string, _ int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, State: "open", AutoMergeEnabled: true, MergeableState: "blocked"}, nil
		},
		fetchLabelAppliedAtFn: func(_, _ string, _ int, _ string) (time.Time, error) {
			return time.Now().Add(-appliedAgo), nil
		},
		fetchLabelRemovedAtFn: removedAtFn(resumedAt, resumeErr),
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.ConvergenceBudget = 30 * time.Minute
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	settle := PRSettleResult{Status: PRMergeBlocked, PR: &gh.PRDetails{Number: 10, MergeableState: "blocked"}}
	eng.checkAutoMergeConvergence(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"}, settle, false)
	return client
}

func TestConvergenceBudget_ExcludesPausedTime(t *testing.T) {
	t.Run("resumed recently is not escalated", func(t *testing.T) {
		if c := budgetRun(t, 2*time.Hour, time.Now().Add(-5*time.Minute), nil); addedPaused(c) {
			t.Error("budget pause fired for an item resumed 5m ago")
		}
	})
	t.Run("resumed longer ago than the budget is escalated", func(t *testing.T) {
		if c := budgetRun(t, 2*time.Hour, time.Now().Add(-time.Hour), nil); !addedPaused(c) {
			t.Error("expected the budget pause")
		}
	})
	t.Run("unreadable removal time falls back to the label anchor", func(t *testing.T) {
		if c := budgetRun(t, 2*time.Hour, time.Time{}, errors.New("boom")); !addedPaused(c) {
			t.Error("expected the budget pause on fallback")
		}
	})
	t.Run("under the budget costs no event-log read", func(t *testing.T) {
		if c := budgetRun(t, time.Minute, time.Time{}, nil); len(c.fetchLabelRemovedAtCalls) != 0 {
			t.Errorf("%d event-log reads, want 0", len(c.fetchLabelRemovedAtCalls))
		}
	})
}
