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
	started := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	human := func(id string, at time.Time) gh.Comment {
		return gh.Comment{ID: id, DatabaseID: 1, Author: "alice", Body: "please change X", CreatedAt: at}
	}
	cases := []struct {
		name     string
		comments []gh.Comment
		status   string
		labels   []string
		ctxDone  bool
		disabled bool
		want     bool
	}{
		{name: "comment that arrived during the run admits", comments: []gh.Comment{human("c1", started.Add(time.Minute))}, status: "Implement", want: true},
		{name: "comment created at the run's start admits", comments: []gh.Comment{human("c1", started)}, status: "Implement", want: true},
		{name: "comment pending before the run started does not admit", comments: []gh.Comment{human("c1", started.Add(-time.Second))}, status: "Implement", want: false},
		{name: "comment with an unknown timestamp does not admit", comments: []gh.Comment{human("c1", time.Time{})}, status: "Implement", want: false},
		{name: "an old comment does not mask a new one", comments: []gh.Comment{human("c1", started.Add(-time.Hour)), human("c2", started.Add(time.Minute))}, status: "Implement", want: true},
		{name: "no feedback does not admit (ADR 039)", status: "Implement", want: false},
		{name: "fabrik output does not admit", comments: []gh.Comment{{ID: "c2", DatabaseID: 2, Author: "bot", Body: "🏭 **Fabrik — stage: Plan**", CreatedAt: started.Add(time.Minute)}}, status: "Implement", want: false},
		{name: "paused item is not admitted", comments: []gh.Comment{human("c1", started.Add(time.Minute))}, status: "Implement", labels: []string{"fabrik:paused", "fabrik:awaiting-input"}, want: false},
		{name: "shutdown skips", comments: []gh.Comment{human("c1", started.Add(time.Minute))}, status: "Implement", ctxDone: true, want: false},
		{name: "disabled seam skips", comments: []gh.Comment{human("c1", started.Add(time.Minute))}, status: "Implement", disabled: true, want: false},
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
			eng.postRunFeedbackAdmission(ctx, item, started)
			if got := mayNeedWorkHas(eng, issueKey(item, eng.defaultRepo())); got != tc.want {
				t.Errorf("admitted = %v, want %v", got, tc.want)
			}
		})
	}
}

// The restriction to feedback that arrived during the run is what keeps a pass
// that failed to clear its feedback from re-admitting itself (#2232 FR-001b):
// lifting it admits the same pre-existing comment.
func TestPostRunFeedbackAdmission_ArrivalRestrictionIsWhatRefusesPreExisting(t *testing.T) {
	started := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	before := gh.Comment{ID: "c1", DatabaseID: 1, Author: "alice", Body: "please change X", CreatedAt: started.Add(-time.Minute)}
	run := func(t *testing.T, lift bool) bool {
		eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
		item := gh.ProjectItem{Number: 5, Repo: cooldownTestRepo, Title: "x", Status: "Implement", Comments: []gh.Comment{before}}
		eng.store.Apply(itemstate.IssueOpened{Item: item})
		if lift {
			defer SetPostRunArrivalRestrictionDisabledForTest(true)()
		}
		eng.postRunFeedbackAdmission(context.Background(), item, started)
		return mayNeedWorkHas(eng, issueKey(item, eng.defaultRepo()))
	}
	if run(t, false) {
		t.Error("feedback pending before the run started admitted the item")
	}
	if !run(t, true) {
		t.Error("with the restriction lifted the same comment should admit (non-vacuity)")
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
	rec(1, "periodic-re-eval", 10*time.Second) // long reason: not a short cooldown
	rec(1, "dep-blocked", 5*time.Second)       // long reason: not a short cooldown
	if got := eng.earliestShortCooldown(start); !got.IsZero() {
		t.Fatalf("long reasons must not count as short, got %v", got)
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
	rec(1, feedbackReadFailedReason, 7*time.Second) // #2498: short reason counts
	if got := eng.earliestShortCooldown(start); !got.Equal(start.Add(7 * time.Second)) {
		t.Fatalf("earliest with feedback-read-failed = %v, want start+7s", got)
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
	// #2528 (ADR 2528): a long cooldown caps too, but at due + one configured
	// interval (90s + 30s), so staggered dues coalesce.
	if got := run(t, "periodic-re-eval", false); got != 120*time.Second {
		t.Errorf("a long cooldown caps at due + configured interval, got %v, want 120s", got)
	}
}

// #2528 FR-001/FR-005: a column move made during the run admits on the next poll;
// an exit that changed nothing does not (ADR 039).
func TestPostRunAdvanceAdmission(t *testing.T) {
	cases := []struct {
		name     string
		start    string
		now      string
		labels   []string
		closed   bool
		ctxDone  bool
		disabled bool
		want     bool
	}{
		{name: "moved during the run admits", start: "Research", now: "Plan", want: true},
		{name: "unchanged status does not admit (ADR 039 early return)", start: "Research", now: "Research", want: false},
		{name: "unknown start status does not admit", start: "", now: "Plan", want: false},
		{name: "paused item is not admitted", start: "Research", now: "Plan", labels: []string{"fabrik:paused", "fabrik:awaiting-input"}, want: false},
		{name: "closed item is not admitted", start: "Research", now: "Plan", closed: true, want: false},
		{name: "shutdown skips", start: "Research", now: "Plan", ctxDone: true, want: false},
		{name: "disabled seam skips", start: "Research", now: "Plan", disabled: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
			item := gh.ProjectItem{Number: 5, Repo: cooldownTestRepo, Title: "x", Status: tc.now, Labels: tc.labels}
			eng.store.Apply(itemstate.IssueOpened{Item: item})
			if tc.closed {
				eng.store.Apply(itemstate.LocalIssueClosed{Repo: cooldownTestRepo, Number: 5})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.ctxDone {
				cancel()
			}
			eng.SetPostRunAdvanceAdmissionDisabledForTest(tc.disabled)
			eng.postRunAdvanceAdmission(ctx, item, tc.start)
			if got := mayNeedWorkHas(eng, issueKey(item, eng.defaultRepo())); got != tc.want {
				t.Errorf("admitted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStatusAtWorkerStartPrefersTheStore(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 5, Repo: cooldownTestRepo, Title: "x", Status: "Plan"}})
	stale := gh.ProjectItem{Number: 5, Repo: cooldownTestRepo, Status: "Research"}
	if got := eng.statusAtWorkerStart(stale); got != "Plan" {
		t.Errorf("start status = %q, want the Store's Plan", got)
	}
	unknown := gh.ProjectItem{Number: 99, Repo: cooldownTestRepo, Status: "Research"}
	if got := eng.statusAtWorkerStart(unknown); got != "Research" {
		t.Errorf("start status for an unknown item = %q, want the copy's Research", got)
	}
}

func TestEarliestLongCooldown(t *testing.T) {
	eng, start := cooldownAdmissionEngine(t, &mockGitHubClient{})
	open := func(n int, status string) {
		eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: n, Title: "x", Status: status}})
	}
	rec := func(n int, reason string, d time.Duration) {
		eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: n, Reason: reason, Until: start.Add(d)})
	}
	open(1, "Research")
	open(2, "Research")
	rec(1, "retry-backoff", 5*time.Second) // short: not counted here
	if got := eng.earliestLongCooldown(start); !got.IsZero() {
		t.Fatalf("short reasons must not count as long, got %v", got)
	}
	rec(1, itemstate.ArchiveEligibleCooldown, -24*time.Hour)
	rec(2, "periodic-re-eval", -time.Second) // expired
	if got := eng.earliestLongCooldown(start); !got.IsZero() {
		t.Fatalf("archive-eligible-at and expired entries must not count, got %v", got)
	}
	rec(2, "periodic-re-eval", 40*time.Second)
	rec(1, "dep-blocked", 25*time.Second)
	if got := eng.earliestLongCooldown(start); !got.Equal(start.Add(25 * time.Second)) {
		t.Fatalf("earliest = %v, want start+25s", got)
	}
}

// #2528 FR-003/FR-008: with the idle backoff at its maximum and a periodic
// re-evaluation due sooner, the next poll is scheduled at the cooldown (plus one
// configured interval), not at the backoff; the cap is neutralisable, is skipped
// under GraphQL rate-limit backoff's own handling, and ignores archive-eligible-at.
func TestPollWithBackoff_IdleIntervalCappedToPeriodicReEval(t *testing.T) {
	run := func(t *testing.T, reason string, off bool) time.Duration {
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
		eng.idleStart = time.Now().Add(-time.Hour)
		eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 7, Repo: cooldownTestRepo, Title: "x", Status: "Implement"}})
		eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 7, Reason: reason, Until: now.Add(90 * time.Second)})
		eng.SetLongCooldownCapDisabledForTest(off)
		res, err := eng.PollWithBackoff(context.Background(), 30*time.Second)
		if err != nil {
			t.Fatalf("PollWithBackoff: %v", err)
		}
		return res.NextInterval
	}
	if got := run(t, "periodic-re-eval", false); got != 120*time.Second {
		t.Errorf("capped interval = %v, want 120s (due 90s + one 30s interval)", got)
	}
	if got := run(t, "periodic-re-eval", true); got <= 120*time.Second {
		t.Errorf("with the cap disabled the interval = %v, want the idle interval (> 120s)", got)
	}
	if got := run(t, itemstate.ArchiveEligibleCooldown, false); got <= 120*time.Second {
		t.Errorf("archive-eligible-at must not cap, got %v", got)
	}
}

// Staggered dues stamped by different polls coalesce: once the earliest has
// expired it no longer counts, and a re-armed entry is a full recheck interval
// away, so the cap cannot loop.
func TestLongCooldownCapDoesNotLoopOnRearmedCooldown(t *testing.T) {
	eng, start := cooldownAdmissionEngine(t, &mockGitHubClient{})
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 1, Title: "x", Status: "Research"}})
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 1, Reason: "periodic-re-eval", Until: start.Add(10 * time.Second)})
	later := start.Add(11 * time.Second)
	if got := eng.earliestLongCooldown(later); !got.IsZero() {
		t.Fatalf("an expired entry must not cap, got %v", got)
	}
	rearmed := later.Add(eng.githubRecheckInterval())
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 1, Reason: "periodic-re-eval", Until: rearmed})
	due := eng.earliestLongCooldown(later)
	if d := due.Sub(later); d < eng.githubRecheckInterval() {
		t.Fatalf("re-armed cap is %v away, want at least %v", d, eng.githubRecheckInterval())
	}
}
