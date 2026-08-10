package engine

import (
	"context"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// stubClock is a fixed-time Clock for tests.
type stubClock struct{ t time.Time }

func (c stubClock) Now() time.Time { return c.t }

// TestEngine_Now_DefaultsToRealTime confirms an engine with no injected
// clock behaves byte-identically to the pre-seam time.Now()-based code: e.now()
// must return a time indistinguishable from time.Now() (within a generous
// tolerance to absorb test scheduling jitter).
func TestEngine_Now_DefaultsToRealTime(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if eng.clock != nil {
		t.Fatal("clock should be unset for a fresh NewWithDeps engine")
	}
	before := time.Now()
	got := eng.now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("now() = %v, want within [%v, %v] (unset clock should fall back to time.Now())", got, before, after)
	}
}

// TestEngine_SetClock_OverridesNow confirms SetClock actually redirects
// e.now() to the injected clock — the failure mode a no-op SetClock would
// silently pass.
func TestEngine_SetClock_OverridesNow(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	fixed := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	eng.SetClock(stubClock{t: fixed})

	if got := eng.now(); !got.Equal(fixed) {
		t.Errorf("now() = %v, want %v (SetClock did not take effect)", got, fixed)
	}

	// A distinct call after re-setting the clock proves now() re-reads the
	// clock on every call rather than caching the first value.
	later := fixed.Add(time.Hour)
	eng.SetClock(stubClock{t: later})
	if got := eng.now(); !got.Equal(later) {
		t.Errorf("now() = %v, want %v (now() appears cached rather than live)", got, later)
	}
}

// TestPoll_PeriodicReEvalCooldown_UsesInjectedClock is a regression guard for
// the poll.go:1133 CooldownAt["periodic-re-eval"] write site: with a fixed
// clock injected, the recorded cooldown deadline must be exactly
// fixedNow+cooldown, not time.Now()+cooldown. Mirrors
// TestPoll_CruiseValidateComplete_NoRepeatDeepFetch's setup.
func TestPoll_PeriodicReEvalCooldown_UsesInjectedClock(t *testing.T) {
	stgs := testStagesWithValidate()
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number:    732,
						ItemID:    "PVTI_732",
						Status:    "Validate",
						Repo:      "owner/repo",
						UpdatedAt: time.Now().Add(-time.Hour),
						Labels:    []string{"stage:Validate:complete", "fabrik:cruise"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { return nil },
	}

	eng := NewWithDeps(Config{
		Owner: "owner", Repo: "", ProjectNum: 1, User: "testuser", Token: "token",
		MaxConcurrent: 5, PollSeconds: 1, Stages: stgs,
	}, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	fixedNow := time.Date(2020, 6, 15, 12, 0, 0, 0, time.UTC)
	eng.SetClock(stubClock{t: fixedNow})

	// Force entry into the refresh path: expired cooldown so the item is
	// admitted for deep-fetch and the poll's deferred refresh fires.
	eng.store.Apply(itemstate.CooldownRecorded{
		Repo: "owner/repo", Number: 732, Reason: "periodic-re-eval",
		Until: fixedNow.Add(-2 * time.Minute),
	})

	if _, err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 732)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	wantCooldown := time.Duration(eng.cfg.PollSeconds*10) * time.Second
	want := fixedNow.Add(wantCooldown)
	if got := snap.CooldownAt("periodic-re-eval"); !got.Equal(want) {
		t.Errorf("CooldownAt[periodic-re-eval] = %v, want %v (poll.go's refresh site is not reading e.now())", got, want)
	}
}

// TestRecordLabelAppliedAtNow_UsesInjectedClock is a regression guard for
// the mutate.go recordLabelAppliedAtNow write site (discovered while
// building #1449's sim-harness review-wait-timeout scenario: this
// in-memory cache — #1314 — is consulted ahead of a live
// FetchLabelAppliedAt call, so a scenario backdating the GitHubClient
// response alone cannot control ReviewWaitTimeout unless this site also
// respects the injected Clock).
func TestRecordLabelAppliedAtNow_UsesInjectedClock(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	fixedNow := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	eng.SetClock(stubClock{t: fixedNow})

	item := gh.ProjectItem{Number: 55, Repo: "owner/repo", Labels: []string{"fabrik:awaiting-review"}}
	eng.recordLabelAppliedAtNow(item, "fabrik:awaiting-review")

	snap, err := eng.store.Get("owner/repo", 55)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := snap.LabelAppliedAt("fabrik:awaiting-review"); !got.Equal(fixedNow) {
		t.Errorf("LabelAppliedAt = %v, want %v (recordLabelAppliedAtNow is not reading e.now())", got, fixedNow)
	}
}
