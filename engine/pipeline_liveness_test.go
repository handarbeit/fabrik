package engine

import (
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

func TestComputePollVerdict(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	iv := 30 * time.Second
	cases := []struct {
		name    string
		in      pollVerdictInput
		known   bool
		overdue bool
		cause   string
		blocked time.Duration
	}{
		{"unknown interval is never healthy or overdue", pollVerdictInput{Now: t0, InProgress: true, PollStartedAt: t0.Add(-time.Hour)}, false, false, "", 0},
		{"in progress under K x interval", pollVerdictInput{Now: t0, Interval: iv, InProgress: true, PollStartedAt: t0.Add(-59 * time.Second)}, true, false, "", 59 * time.Second},
		{"in progress at exactly K x interval is not yet overdue", pollVerdictInput{Now: t0, Interval: iv, InProgress: true, PollStartedAt: t0.Add(-60 * time.Second)}, true, false, "", 60 * time.Second},
		{"in progress past K x interval", pollVerdictInput{Now: t0, Interval: iv, InProgress: true, PollStartedAt: t0.Add(-923 * time.Second)}, true, true, pollCauseInProgress, 923 * time.Second},
		{"in progress with unknown start is unknown", pollVerdictInput{Now: t0, Interval: iv, InProgress: true}, false, false, "", 0},
		{"no poll: fresh start measured from daemon start", pollVerdictInput{Now: t0, Interval: iv, DaemonStartedAt: t0.Add(-59 * time.Second)}, true, false, "", 59 * time.Second},
		{"no poll: never started past K x interval", pollVerdictInput{Now: t0, Interval: iv, DaemonStartedAt: t0.Add(-5 * time.Minute)}, true, true, pollCauseNoPoll, 5 * time.Minute},
		{"no poll: measured from the engine's own next-due", pollVerdictInput{Now: t0, Interval: iv, DaemonStartedAt: t0.Add(-24 * time.Hour), NextDueAt: t0.Add(30 * time.Minute)}, true, false, "", 0},
		{"no poll: idle back-off to the 60 min webhook cap is not a stall", pollVerdictInput{Now: t0, Interval: iv, DaemonStartedAt: t0.Add(-3 * time.Hour), NextDueAt: t0.Add(59 * time.Minute)}, true, false, "", 0},
		{"no poll: past next-due by K x interval", pollVerdictInput{Now: t0, Interval: iv, NextDueAt: t0.Add(-61 * time.Second)}, true, true, pollCauseNoPoll, 61 * time.Second},
		{"no poll: just past next-due is healthy", pollVerdictInput{Now: t0, Interval: iv, NextDueAt: t0.Add(-10 * time.Second)}, true, false, "", 10 * time.Second},
		{"no poll: no baseline at all is unknown", pollVerdictInput{Now: t0, Interval: iv}, false, false, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := computePollVerdict(c.in)
			if v.Known != c.known || v.Overdue != c.overdue || v.Cause != c.cause || v.Blocked != c.blocked {
				t.Fatalf("got %+v, want known=%v overdue=%v cause=%q blocked=%v", v, c.known, c.overdue, c.cause, c.blocked)
			}
			if c.known && v.OverdueAfter != pipelineOverdueFactor*iv {
				t.Fatalf("OverdueAfter = %v", v.OverdueAfter)
			}
		})
	}
}

func TestComputePartitionLiveness(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	thr := time.Minute
	old := now.Add(-5 * time.Minute)
	base := partitionLivenessInput{Now: now, TrainOn: true, Threshold: thr, Members: 2, OldestQueued: old}
	cases := []struct {
		name       string
		mod        func(*partitionLivenessInput)
		reason     string
		stalled    bool
		stallCause string
	}{
		{"queued, no run, nothing known: unknown reason, stalled past threshold", func(in *partitionLivenessInput) {}, idleReasonUnknown, true, "merge-train-idle"},
		{"within the threshold is not stalled", func(in *partitionLivenessInput) { in.OldestQueued = now.Add(-30 * time.Second) }, idleReasonUnknown, false, ""},
		{"active run: no idle reason", func(in *partitionLivenessInput) { in.RunActive = true }, "", false, ""},
		{"no members: nothing to report", func(in *partitionLivenessInput) { in.Members = 0 }, "", false, ""},
		{"merge_train off: nothing to report", func(in *partitionLivenessInput) { in.TrainOn = false }, "", false, ""},
		{"poll overdue names the poll", func(in *partitionLivenessInput) {
			in.Poll = pollVerdict{Known: true, Overdue: true, Cause: pollCauseInProgress, Blocked: 900 * time.Second}
		}, idleReasonPollOverdue, true, idleReasonPollOverdue},
		{"infra cooldown is shown but never stalls", func(in *partitionLivenessInput) {
			in.InfraHeld, in.InfraRemaining = true, 90*time.Second
		}, idleReasonInfra, false, ""},
		{"infra cooldown wins nothing over an overdue poll for the reason, yet still never stalls", func(in *partitionLivenessInput) {
			in.InfraHeld = true
			in.Poll = pollVerdict{Known: true, Overdue: true, Cause: pollCauseNoPoll}
		}, idleReasonPollOverdue, false, ""},
		{"deliberate poll back-off never stalls", func(in *partitionLivenessInput) { in.PollDeliberatelyPending = true }, idleReasonUnknown, false, ""},
		{"unknown queued-since never stalls", func(in *partitionLivenessInput) { in.OldestQueued = time.Time{} }, idleReasonUnknown, false, ""},
		{"zero threshold disables the stall", func(in *partitionLivenessInput) { in.Threshold = 0 }, idleReasonUnknown, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base
			c.mod(&in)
			got := computePartitionLiveness(in)
			if got.IdleReason != c.reason || got.Stalled != c.stalled || got.Cause != c.stallCause {
				t.Fatalf("got %+v, want reason=%q stalled=%v cause=%q", got, c.reason, c.stalled, c.stallCause)
			}
		})
	}
}

// Health reports the partition's liveness from the Store, the durable run
// phases and the non-mutating infra-cooldown peek.
func TestHealthPartitionLiveness(t *testing.T) {
	e := apiEngine(t, 0)
	e.cfg.MergeTrain = "on"
	e.cfg.PollSeconds = 30
	now := e.now()
	e.health.markStarted(now.Add(-time.Hour))
	e.seedAPIItem(t, 1, "Queued")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Queued"}})

	part := func() localapi.TrainPartition {
		h, err := e.LocalAPIBackend().Health(localapi.HealthParams{})
		if err != nil {
			t.Fatal(err)
		}
		if len(h.MergeTrain) != 1 {
			t.Fatalf("partitions = %+v", h.MergeTrain)
		}
		return h.MergeTrain[0]
	}

	p := part()
	if !p.OldestQueuedSince.Valid || p.RunActive || p.Phase != "none" || p.IdleReason == "" {
		t.Fatalf("queued, no run: %+v", p)
	}
	// The poll was never started and the daemon is an hour old: overdue, and the
	// idle reason says so.
	if p.IdleReason != idleReasonPollOverdue {
		t.Fatalf("idle reason = %q, want %q", p.IdleReason, idleReasonPollOverdue)
	}

	// A healthy poll: the reason is unknown, never invented.
	e.health.beginPoll(now.Add(-time.Second), pollVerdict{})
	e.health.endPoll(now, pollVerdict{}, now.Add(30*time.Second))
	if p = part(); p.IdleReason != idleReasonUnknown {
		t.Fatalf("healthy poll: idle reason = %q, want unknown", p.IdleReason)
	}

	// The infra cooldown is named, and reading it does not mutate state.
	e.markInfraAbandon("owner/repo")
	if p = part(); p.IdleReason != idleReasonInfra || p.IdleDetail == "" {
		t.Fatalf("cooldown: %+v", p)
	}
	e.mergeTrainInfraMu.Lock()
	_, held := e.mergeTrainInfraCooldown["owner/repo"]
	e.mergeTrainInfraMu.Unlock()
	if !held {
		t.Fatal("reading health consumed the cooldown entry")
	}

	// An in-flight worker for the partition is an active run.
	e.store.EnterRepoWorker("owner/repo")
	if p = part(); !p.RunActive || p.IdleReason != "" {
		t.Fatalf("worker in flight: %+v", p)
	}
}

func TestInfraCooldownPeekDoesNotMutate(t *testing.T) {
	e := apiEngine(t, 0)
	e.markInfraAbandon("owner/repo")
	if rem, ok := e.infraCooldownPeek("owner/repo"); !ok || rem <= 0 {
		t.Fatalf("peek = %v, %v", rem, ok)
	}
	e.SetClock(fixedClock{e.now().Add(time.Hour)})
	if _, ok := e.infraCooldownPeek("owner/repo"); ok {
		t.Fatal("an elapsed cooldown must read as not held")
	}
	e.mergeTrainInfraMu.Lock()
	_, present := e.mergeTrainInfraCooldown["owner/repo"]
	e.mergeTrainInfraMu.Unlock()
	if !present {
		t.Fatal("peek must leave an elapsed entry in place")
	}
	if _, ok := e.infraCooldownRemaining("owner/repo"); ok {
		t.Fatal("remaining must also read as not held")
	}
}

// A poll that errors does not reset the Run() ticker, so the next poll comes
// after the period the engine last chose — a deliberate back-off included. The
// fallback next-due time must follow it, not the configured interval, or one
// transient error in a deeply idle daemon reads as a missed poll (#2124).
func TestErroredPollKeepsTheBackoffPeriodForNextDue(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return nil, errors.New("network error")
		},
	}
	e := testEngine(t, client, &mockClaudeInvoker{})
	e.cfg.PollSeconds = 30
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	e.SetClock(fixedClock{t0})
	// The last successful poll chose a 60-minute idle back-off period.
	e.health.setPollPeriod(time.Hour, backoffReasonIdle)

	if err := e.PollOnce(t.Context()); err == nil {
		t.Fatal("expected the poll to error")
	}
	s := e.health.snapshot()
	if want := t0.Add(time.Hour); !s.NextPollDueAt.Equal(want) {
		t.Fatalf("next due = %v, want %v (the back-off period, not the configured interval)", s.NextPollDueAt, want)
	}
	if s.BackoffReason != backoffReasonIdle {
		t.Fatalf("backoff reason = %q, want %q", s.BackoffReason, backoffReasonIdle)
	}
	if v := e.pollVerdictFor(s, t0.Add(10*time.Minute)); !v.Known || v.Overdue {
		t.Fatalf("an errored poll during idle back-off must not read as overdue: %+v", v)
	}

	// With no back-off recorded the configured interval is the fallback, and a
	// genuinely missed poll still reads as overdue.
	e2 := testEngine(t, client, &mockClaudeInvoker{})
	e2.cfg.PollSeconds = 30
	e2.SetClock(fixedClock{t0})
	if err := e2.PollOnce(t.Context()); err == nil {
		t.Fatal("expected the poll to error")
	}
	s2 := e2.health.snapshot()
	if want := t0.Add(30 * time.Second); !s2.NextPollDueAt.Equal(want) {
		t.Fatalf("next due = %v, want %v", s2.NextPollDueAt, want)
	}
	if v := e2.pollVerdictFor(s2, t0.Add(10*time.Minute)); !v.Overdue {
		t.Fatalf("a missed poll with no back-off must read as overdue: %+v", v)
	}
}
