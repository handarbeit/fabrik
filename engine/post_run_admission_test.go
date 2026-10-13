package engine

import (
	"context"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Tests for #2232 (ADR 2232): engine-scheduled retries own an admission cooldown,
// feedback cached when a worker ends is admitted on the next poll, and the idle
// poll interval never sleeps past a due retry.

func mayNeedWorkHas(e *Engine, key string) bool {
	e.mayNeedWorkMu.Lock()
	defer e.mayNeedWorkMu.Unlock()
	return e.mayNeedWork[key]
}

func TestPostRunFeedbackAdmission(t *testing.T) {
	human := gh.Comment{ID: "c1", DatabaseID: 1, Author: "alice", Body: "please change X"}
	cases := []struct {
		name     string
		comments []gh.Comment
		status   string
		labels   []string
		ctxDone  bool
		disabled bool
		want     bool
	}{
		{name: "pending comment admits", comments: []gh.Comment{human}, status: "Implement", want: true},
		{name: "no feedback does not admit (ADR 039)", status: "Implement", want: false},
		{name: "fabrik output does not admit", comments: []gh.Comment{{ID: "c2", DatabaseID: 2, Author: "bot", Body: "🏭 **Fabrik — stage: Plan**"}}, status: "Implement", want: false},
		{name: "paused item is not admitted", comments: []gh.Comment{human}, status: "Implement", labels: []string{"fabrik:paused", "fabrik:awaiting-input"}, want: false},
		{name: "shutdown skips", comments: []gh.Comment{human}, status: "Implement", ctxDone: true, want: false},
		{name: "disabled seam skips", comments: []gh.Comment{human}, status: "Implement", disabled: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
			item := gh.ProjectItem{Number: 5, Repo: cooldownTestRepo, Title: "x", Status: tc.status, Comments: tc.comments, Labels: tc.labels}
			eng.store.Apply(itemstate.IssueOpened{Item: item})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.ctxDone {
				cancel()
			}
			if tc.disabled {
				defer SetPostRunFeedbackAdmissionDisabledForTest(true)()
			}
			eng.postRunFeedbackAdmission(ctx, item)
			if got := mayNeedWorkHas(eng, issueKey(item, eng.defaultRepo())); got != tc.want {
				t.Errorf("admitted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecordStageRetryCooldown_UntilIsNowPlusD(t *testing.T) {
	eng, start := cooldownAdmissionEngine(t, &mockGitHubClient{})
	item := gh.ProjectItem{Number: 90, Title: "x", Status: "Research"}
	eng.recordStageRetryCooldown(item, 0) // nothing recorded
	if snap, err := eng.store.Get(cooldownTestRepo, 90); err == nil && !snap.CooldownAt("retry-backoff").IsZero() {
		t.Fatal("d=0 must record nothing")
	}
	eng.store.Apply(itemstate.IssueOpened{Item: item})
	eng.recordStageRetryCooldown(item, 45*time.Second)
	snap, _ := eng.store.Peek(cooldownTestRepo, 90)
	if got := snap.CooldownAt("retry-backoff"); !got.Equal(start.Add(45 * time.Second)) {
		t.Fatalf("Until = %v, want %v", got, start.Add(45*time.Second))
	}
}

func TestCapIntervalToDue(t *testing.T) {
	now := time.Unix(1000, 0)
	cases := []struct {
		name      string
		effective time.Duration
		due       time.Time
		want      time.Duration
	}{
		{"no due", 5 * time.Minute, time.Time{}, 5 * time.Minute},
		{"due sooner caps", 5 * time.Minute, now.Add(90 * time.Second), 90 * time.Second},
		{"due later leaves", time.Minute, now.Add(5 * time.Minute), time.Minute},
		{"floor", 5 * time.Minute, now.Add(10 * time.Millisecond), time.Second},
		{"already due floors", 5 * time.Minute, now.Add(-time.Hour), time.Second},
		{"floor never lengthens", 500 * time.Millisecond, now.Add(10 * time.Millisecond), 500 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := capIntervalToDue(tc.effective, tc.due, now, time.Second); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEarliestShortCooldown(t *testing.T) {
	eng, start := cooldownAdmissionEngine(t, &mockGitHubClient{})
	open := func(n int, status string) {
		eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: n, Title: "x", Status: status}})
	}
	rec := func(n int, reason string, d time.Duration) {
		eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: n, Reason: reason, Until: start.Add(d)})
	}
	open(1, "Research")
	open(2, "Research")
	open(3, "Research")
	rec(1, "periodic-re-eval", 10*time.Second) // long reason: ignored
	rec(1, "dep-blocked", 5*time.Second)       // long reason: ignored
	if got := eng.earliestShortCooldown(start); !got.IsZero() {
		t.Fatalf("long reasons must not count, got %v", got)
	}
	rec(2, "retry-backoff", -time.Second) // expired: ignored
	if got := eng.earliestShortCooldown(start); !got.IsZero() {
		t.Fatalf("expired entries must not count, got %v", got)
	}
	rec(2, "merge-unsettled", 40*time.Second)
	rec(3, "retry-backoff", 20*time.Second)
	if got := eng.earliestShortCooldown(start); !got.Equal(start.Add(20 * time.Second)) {
		t.Fatalf("earliest = %v, want start+20s", got)
	}
}

// SC-002 (#2232): with idle backoff active, the next poll lands at a due
// engine-scheduled retry rather than the idle interval; a long cooldown does not
// cap; the cap is the only thing that does it.
func TestPollWithBackoff_IdleIntervalCappedToDueRetry(t *testing.T) {
	run := func(t *testing.T, reason string, disableCap bool) time.Duration {
		client := &mockGitHubClient{
			fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
				return &gh.ProjectBoard{}, nil
			},
			rateLimitStatsFn: func() (gh.RateLimitStats, gh.RateLimitStats) {
				healthy := gh.RateLimitStats{Remaining: 5000, Limit: 5000, Reset: time.Now().Add(time.Hour)}
				return healthy, healthy
			},
		}
		eng := testEngine(t, client, &mockClaudeInvoker{})
		now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		eng.SetClock(stubClock{t: now})
		eng.idleStart = time.Now().Add(-time.Hour) // idle backoff at its maximum
		eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 7, Repo: cooldownTestRepo, Title: "x", Status: "Implement"}})
		eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 7, Reason: reason, Until: now.Add(90 * time.Second)})
		if disableCap {
			defer SetIntervalCapDisabledForTest(true)()
		}
		res, err := eng.PollWithBackoff(context.Background(), 30*time.Second)
		if err != nil {
			t.Fatalf("PollWithBackoff: %v", err)
		}
		return res.NextInterval
	}
	if got := run(t, "retry-backoff", false); got != 90*time.Second {
		t.Errorf("capped interval = %v, want 90s", got)
	}
	if got := run(t, "retry-backoff", true); got <= 90*time.Second {
		t.Errorf("with the cap disabled the interval = %v, want the idle interval (> 90s)", got)
	}
	if got := run(t, "periodic-re-eval", false); got <= 90*time.Second {
		t.Errorf("a long cooldown must not cap the idle interval, got %v", got)
	}
}
