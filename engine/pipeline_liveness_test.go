package engine

import (
	"testing"
	"time"
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
