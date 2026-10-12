package engine

import (
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// anomalyEngine is a channel engine (hub running, subscriber "T" attached, an
// adjustable clock, an hour-long ticker so the test drives checkAnomalies itself)
// with short anomaly thresholds. The thresholds are set before the hub starts
// because the consumer reads them without a lock.
func anomalyEngine(t *testing.T) (*Engine, *chanSink, *adjClock, *channelEvents) {
	t.Helper()
	return anomalyEngineWithPoll(t, 0)
}

func anomalyEngineWithPoll(t *testing.T, pollSeconds int) (*Engine, *chanSink, *adjClock, *channelEvents) {
	t.Helper()
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = 20*time.Millisecond, time.Hour
	t.Cleanup(func() { channelDebounce, channelTick = oldD, oldT })
	e := apiEngine(t, 0)
	e.cfg.PollSeconds = pollSeconds
	e.SetAnomalyThresholdsForTest(5*time.Minute, 10*time.Minute, 5*time.Minute)
	e, sink := channelEngineOn(t, e, 0, nil)
	return e, sink, e.clock.(*adjClock), e.channelEvents()
}

func implementStage() *stages.Stage { return &stages.Stage{Name: "Implement"} }

func anomalyItem(n int) gh.ProjectItem {
	return gh.ProjectItem{Number: n, Repo: "owner/repo", Status: "Implement"}
}

func checkAndDrain(ce *channelEvents, sink *chanSink) []channelevents.Event {
	ce.checkAnomalies()
	ce.checkAnomalies()
	return drain(sink, 120*time.Millisecond)
}

func TestRetryStarved_NoWorkerSlotThenResumedOnDispatch(t *testing.T) {
	e, sink, clk, ce := anomalyEngine(t)
	e.seedAPIItem(t, 1, "Implement")
	t0 := clk.Now()
	e.emitRetryScheduled(anomalyItem(1), implementStage(), "wall-time-kill", t0.Add(time.Minute))
	release := e.HoldGeneralWorkerSlotsForTest()
	defer release()

	// Before the threshold: nothing.
	clk.set(t0.Add(4 * time.Minute))
	if evs := checkAndDrain(ce, sink); len(ofType(evs, channelevents.RetryStarved)) != 0 {
		t.Fatalf("starved before the threshold: %+v", evs)
	}
	// Past the threshold, however often it is evaluated: exactly one.
	clk.set(t0.Add(7 * time.Minute))
	evs := checkAndDrain(ce, sink)
	st := ofType(evs, channelevents.RetryStarved)
	if len(st) != 1 {
		t.Fatalf("retry-starved events = %d, want 1: %+v", len(st), evs)
	}
	m := st[0].Meta
	if m["cause"] != "wall-time-kill" || m["reason"] != "no worker slot" || m["slots_in_use"] != "3" || m["slots_max"] != "3" ||
		m["due_at"] == "" || m["stage"] != "Implement" || st[0].Issue != 1 {
		t.Fatalf("meta = %v", m)
	}
	clk.set(t0.Add(9 * time.Minute))
	if evs := checkAndDrain(ce, sink); len(ofType(evs, channelevents.RetryStarved)) != 0 {
		t.Fatalf("a persisting episode must not repeat: %+v", evs)
	}

	// Dispatch ends it with exactly one retry-resumed.
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 1, StageName: "Implement", StartedAt: clk.Now()})
	evs = checkAndDrain(ce, sink)
	if rs := ofType(evs, channelevents.RetryResumed); len(rs) != 1 || rs[0].Meta["cause"] != "wall-time-kill" {
		t.Fatalf("recovery events: %+v", evs)
	}
	if evs := checkAndDrain(ce, sink); len(ofType(evs, channelevents.RetryResumed)) != 0 {
		t.Fatalf("retry-resumed repeated: %+v", evs)
	}
}

func TestRetryStarved_NotRaisedWhenDispatchedInTime(t *testing.T) {
	e, sink, clk, ce := anomalyEngine(t)
	e.seedAPIItem(t, 1, "Implement")
	t0 := clk.Now()
	e.emitRetryScheduled(anomalyItem(1), implementStage(), "max-turns", t0.Add(time.Minute))
	clk.set(t0.Add(2 * time.Minute))
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 1, StageName: "Implement", StartedAt: clk.Now()})
	clk.set(t0.Add(30 * time.Minute))
	if evs := checkAndDrain(ce, sink); len(ofType(evs, channelevents.RetryStarved))+len(ofType(evs, channelevents.RetryResumed)) != 0 {
		t.Fatalf("a retry dispatched in time must raise nothing: %+v", evs)
	}
}

func TestRetryStarved_Reasons(t *testing.T) {
	t.Run("poll overdue", func(t *testing.T) {
		e, sink, clk, ce := anomalyEngineWithPoll(t, 30)
		e.seedAPIItem(t, 1, "Implement")
		t0 := clk.Now()
		e.health.beginPoll(t0, pollVerdict{}) // a poll that never finishes
		e.emitRetryScheduled(anomalyItem(1), implementStage(), "error", t0.Add(time.Minute))
		clk.set(t0.Add(8 * time.Minute))
		evs := ofType(checkAndDrain(ce, sink), channelevents.RetryStarved)
		if len(evs) != 1 || evs[0].Meta["reason"] != "poll overdue" {
			t.Fatalf("got %+v", evs)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		e, sink, clk, ce := anomalyEngine(t)
		e.seedAPIItem(t, 1, "Implement")
		t0 := clk.Now()
		e.emitRetryScheduled(anomalyItem(1), implementStage(), "incomplete", t0)
		clk.set(t0.Add(8 * time.Minute))
		evs := ofType(checkAndDrain(ce, sink), channelevents.RetryStarved)
		if len(evs) != 1 || evs[0].Meta["reason"] != "unknown" {
			t.Fatalf("got %+v", evs)
		}
	})
	t.Run("deferred dispatch counts as no worker slot", func(t *testing.T) {
		e, sink, clk, ce := anomalyEngine(t)
		e.seedAPIItem(t, 1, "Implement")
		t0 := clk.Now()
		e.emitRetryScheduled(anomalyItem(1), implementStage(), "max-turns", t0)
		clk.set(t0.Add(8 * time.Minute))
		e.emitDispatchDeferred([]string{"owner/repo#1"}) // the poll that still cannot get a slot
		evs := ofType(checkAndDrain(ce, sink), channelevents.RetryStarved)
		if len(evs) != 1 || evs[0].Meta["reason"] != "no worker slot" {
			t.Fatalf("got %+v", evs)
		}
	})
	// A deferral that stopped recurring (the slots freed up) no longer explains why
	// the retry is not running; the third reason must not be masked.
	t.Run("a stale deferral does not mask unknown", func(t *testing.T) {
		e, sink, clk, ce := anomalyEngine(t)
		e.seedAPIItem(t, 1, "Implement")
		t0 := clk.Now()
		e.emitRetryScheduled(anomalyItem(1), implementStage(), "max-turns", t0)
		e.emitDispatchDeferred([]string{"owner/repo#1"})
		clk.set(t0.Add(8 * time.Minute))
		evs := ofType(checkAndDrain(ce, sink), channelevents.RetryStarved)
		if len(evs) != 1 || evs[0].Meta["reason"] != "unknown" {
			t.Fatalf("got %+v", evs)
		}
	})
}

// An announced retry-starved episode always gets its closing event, even when the
// item stops being a retry candidate without ever being dispatched.
func TestRetryStarved_AnnouncedEpisodeClosesWhenItemStopsWaiting(t *testing.T) {
	cases := []struct {
		name, how string
		end       func(e *Engine)
	}{
		{"paused", "held", func(e *Engine) {
			e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:paused"})
		}},
		{"closed", "closed", func(e *Engine) {
			e.store.Apply(itemstate.LocalIssueClosed{Repo: "owner/repo", Number: 1})
		}},
		{"moved on", "advanced", func(e *Engine) {
			e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "stage:Implement:complete"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, sink, clk, ce := anomalyEngine(t)
			e.seedAPIItem(t, 1, "Implement")
			t0 := clk.Now()
			e.emitRetryScheduled(anomalyItem(1), implementStage(), "wall-time-kill", t0)
			clk.set(t0.Add(8 * time.Minute))
			if evs := ofType(checkAndDrain(ce, sink), channelevents.RetryStarved); len(evs) != 1 {
				t.Fatalf("setup: %+v", evs)
			}
			tc.end(e)
			clk.set(t0.Add(9 * time.Minute))
			evs := ofType(checkAndDrain(ce, sink), channelevents.RetryResumed)
			if len(evs) != 1 || evs[0].Meta["how"] != tc.how {
				t.Fatalf("want one retry-resumed how=%s, got %+v", tc.how, evs)
			}
			clk.set(t0.Add(20 * time.Minute))
			if more := ofType(checkAndDrain(ce, sink), channelevents.RetryResumed); len(more) != 0 {
				t.Fatalf("closing event repeated: %+v", more)
			}
		})
	}
}

func TestRetryStarved_PausedOrBlockedItemsAreNotReported(t *testing.T) {
	for _, label := range []string{"fabrik:paused", "fabrik:awaiting-input", "fabrik:blocked"} {
		t.Run(label, func(t *testing.T) {
			e, sink, clk, ce := anomalyEngine(t)
			e.seedAPIItem(t, 1, "Implement", label)
			t0 := clk.Now()
			e.emitRetryScheduled(anomalyItem(1), implementStage(), "error", t0)
			clk.set(t0.Add(time.Hour))
			if evs := checkAndDrain(ce, sink); len(ofType(evs, channelevents.RetryStarved)) != 0 {
				t.Fatalf("%s item reported as starved: %+v", label, evs)
			}
		})
	}
}

// A retry held back by the account-wide usage-limit hold is waiting for the hold,
// not starved: starvation is measured from the later of the due time and the lift.
func TestRetryStarved_MeasuredFromUsageLimitLift(t *testing.T) {
	e, sink, clk, ce := anomalyEngine(t)
	e.seedAPIItem(t, 1, "Implement")
	t0 := clk.Now()
	e.emitRetryScheduled(anomalyItem(1), implementStage(), "claude-usage-limit", t0.Add(time.Minute))
	e.activateClaudeUsageLimit(1, &claudeerr.UsageLimitError{Message: "blocking_limit", ResetAt: t0.Add(time.Hour)}, t0)

	clk.set(t0.Add(30 * time.Minute))
	if evs := checkAndDrain(ce, sink); len(ofType(evs, channelevents.RetryStarved)) != 0 {
		t.Fatalf("starved while the usage-limit hold is active: %+v", evs)
	}
	clk.set(t0.Add(time.Hour + 2*time.Minute)) // lifted 2 minutes ago, due long ago
	if evs := checkAndDrain(ce, sink); len(ofType(evs, channelevents.RetryStarved)) != 0 {
		t.Fatalf("starved before the threshold has passed since the lift: %+v", evs)
	}
	clk.set(t0.Add(time.Hour + 7*time.Minute))
	evs := ofType(checkAndDrain(ce, sink), channelevents.RetryStarved)
	if len(evs) != 1 || evs[0].Meta["cause"] != "claude-usage-limit" {
		t.Fatalf("got %+v", evs)
	}
}

// An operator clearing the hold early lifts it now, not at its scheduled end: a
// future lift time would suppress a genuinely starved retry for the rest of the hour.
func TestRetryStarved_EarlyClearedUsageLimitAnchorsAtNow(t *testing.T) {
	e, sink, clk, ce := anomalyEngine(t)
	e.seedAPIItem(t, 1, "Implement")
	t0 := clk.Now()
	e.emitRetryScheduled(anomalyItem(1), implementStage(), "claude-usage-limit", t0.Add(time.Minute))
	e.activateClaudeUsageLimit(1, &claudeerr.UsageLimitError{Message: "blocking_limit", ResetAt: t0.Add(time.Hour)}, t0)

	clk.set(t0.Add(10 * time.Minute))
	if evs := checkAndDrain(ce, sink); len(ofType(evs, channelevents.RetryStarved)) != 0 {
		t.Fatalf("starved while the usage-limit hold is active: %+v", evs)
	}
	e.clearClaudeUsageLimit("operator")
	checkAndDrain(ce, sink)           // the lift is observed now (the clear nudges the channel)
	clk.set(t0.Add(17 * time.Minute)) // cleared 7 minutes ago, scheduled end 43 minutes away
	evs := ofType(checkAndDrain(ce, sink), channelevents.RetryStarved)
	if len(evs) != 1 {
		t.Fatalf("a retry starved for longer than the threshold after an early clear must be reported: %+v", evs)
	}
}

// The catch-up snapshot of an open episode reports the same starved_seconds
// anchor as the announcement (the later of due and the usage-limit lift), not the
// original due time, so a late subscriber is not told the hold period counted.
func TestRetryStarved_CatchUpUsesAnnouncementAnchor(t *testing.T) {
	e, sink, clk, ce := anomalyEngine(t)
	e.seedAPIItem(t, 1, "Implement")
	t0 := clk.Now()
	e.emitRetryScheduled(anomalyItem(1), implementStage(), "claude-usage-limit", t0.Add(time.Minute))
	e.activateClaudeUsageLimit(1, &claudeerr.UsageLimitError{Message: "blocking_limit", ResetAt: t0.Add(time.Hour)}, t0)
	clk.set(t0.Add(10 * time.Minute))
	checkAndDrain(ce, sink)
	e.clearClaudeUsageLimit("operator")
	checkAndDrain(ce, sink)
	clk.set(t0.Add(17 * time.Minute))
	evs := ofType(checkAndDrain(ce, sink), channelevents.RetryStarved)
	if len(evs) != 1 {
		t.Fatalf("setup: want one retry-starved, got %+v", evs)
	}
	clk.set(t0.Add(20 * time.Minute))
	cu := ofType(e.anomalyCatchUpEvents(clk.Now()), channelevents.RetryStarved)
	if len(cu) != 1 {
		t.Fatalf("catch-up = %+v", cu)
	}
	got, err := strconv.Atoi(cu[0].Meta["starved_seconds"])
	if err != nil {
		t.Fatal(err)
	}
	// Lift at t0+10m, check at t0+20m: 600 s. Measured from due (t0+1m) it would be 1140 s.
	if got != 600 {
		t.Fatalf("catch-up starved_seconds = %d, want 600 (measured from the lift, not the due time)", got)
	}
}

func TestRetryStarved_ThresholdHasAPollIntervalFloor(t *testing.T) {
	e := apiEngine(t, 0)
	e.SetAnomalyThresholdsForTest(time.Minute, time.Minute, 0)
	e.cfg.PollSeconds = 120
	if got := e.retryStarvedThreshold(); got != 4*time.Minute {
		t.Errorf("retry threshold = %v, want the 2 x poll floor of 4m", got)
	}
	if got := e.itemWedgedThreshold(gateFeedback); got != 4*time.Minute {
		t.Errorf("wedge threshold = %v, want 4m", got)
	}
	if got := e.itemWedgedThreshold(gateCI); got != e.ciBackstopTimeout()+4*time.Minute {
		t.Errorf("ci wedge threshold = %v, want backstop + 2 polls", got)
	}
	e.SetAnomalyThresholdsForTest(0, 0, 0)
	e.cfg.PollSeconds = 0
	if e.retryStarvedThreshold() != 5*time.Minute || e.itemWedgedThreshold(gateFeedback) != 10*time.Minute || e.cacheDivergenceWindow() != 5*time.Minute {
		t.Error("zero config must fall back to the 5m / 10m / 5m defaults")
	}
}

// ---- item-wedged ----

// wedgeEngine is a merge-test engine (real feedbackGateBlocks against a mock
// client) with the channel hub started.
func wedgeEngine(t *testing.T) (*Engine, *chanSink, *adjClock, *channelEvents) {
	t.Helper()
	return wedgeEngineWithPoll(t, 0)
}

func wedgeEngineWithPoll(t *testing.T, pollSeconds int) (*Engine, *chanSink, *adjClock, *channelEvents) {
	t.Helper()
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = 20*time.Millisecond, time.Hour
	t.Cleanup(func() { channelDebounce, channelTick = oldD, oldT })
	e := testEngineForMerge(t, landingClient())
	e.cfg.PollSeconds = pollSeconds
	e.SetAnomalyThresholdsForTest(5*time.Minute, 10*time.Minute, 5*time.Minute)
	e, sink := channelEngineOn(t, e, 0, nil)
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Implement", ItemID: "PVTI_1"}})
	return e, sink, e.clock.(*adjClock), e.channelEvents()
}

// The #2125 shape: a body-only review the gate holds on, re-evaluated every poll,
// and no worker ever dispatched to address it.
func wedgedItem() gh.ProjectItem {
	return feedbackItem(func(i *gh.ProjectItem) {
		i.Status = "Implement"
		i.LinkedPRReviews = []gh.PRReview{bodyOnlyReview}
	})
}

func holdFor(t *testing.T, e *Engine, clk *adjClock, ce *channelEvents, sink *chanSink, item gh.ProjectItem, polls int, every time.Duration) []channelevents.Event {
	t.Helper()
	var all []channelevents.Event
	for i := 0; i < polls; i++ {
		if !e.feedbackGateBlocks(item, true, "advance") {
			t.Fatal("the gate must hold")
		}
		all = append(all, checkAndDrain(ce, sink)...)
		clk.set(clk.Now().Add(every))
	}
	return all
}

func TestItemWedged_FeedbackHoldWithNoDispatch(t *testing.T) {
	e, sink, clk, ce := wedgeEngine(t)
	evs := holdFor(t, e, clk, ce, sink, wedgedItem(), 14, time.Minute)
	w := ofType(evs, channelevents.ItemWedged)
	if len(w) != 1 {
		t.Fatalf("item-wedged events = %d, want exactly 1 over a long persisting hold: %+v", len(w), evs)
	}
	m := w[0].Meta
	if m["gate"] != "feedback" || m["feedback_ids"] != "5356334497" || m["held_seconds"] == "" || m["reason"] == "" || w[0].Issue != 1 {
		t.Fatalf("meta = %v", m)
	}
	if !strings.Contains(w[0].Content, "5356334497") {
		t.Errorf("content must name the feedback ID: %q", w[0].Content)
	}
}

func TestItemWedged_NotRaisedWhenAWorkerIsDispatchedInTime(t *testing.T) {
	e, sink, clk, ce := wedgeEngine(t)
	item := wedgedItem()
	evs := holdFor(t, e, clk, ce, sink, item, 4, time.Minute)
	// The reinvoke the gate was waiting for is dispatched inside the threshold.
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 1, StageName: "Implement", StartedAt: clk.Now()})
	e.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 1})
	// A dispatch restarts the clock: a hold that persists less than the threshold
	// after it is still not wedged.
	evs = append(evs, holdFor(t, e, clk, ce, sink, item, 8, time.Minute)...)
	if len(ofType(evs, channelevents.ItemWedged)) != 0 {
		t.Fatalf("a dispatched item is not wedged: %+v", evs)
	}
}

// The review and CI gates are re-evaluated only every githubRecheckInterval (10
// polls) once nothing bumps the item. A lapse shorter than that cadence would end
// the hold between confirmations and restart firstHeld, so the hold could never
// age to its threshold.
func TestItemWedged_ReviewHoldSurvivesItsRecheckCadence(t *testing.T) {
	e, sink, clk, ce := wedgeEngineWithPoll(t, 30)
	e.cfg.ReviewWaitTimeout = time.Minute
	item := wedgedItem()
	recheck := e.githubRecheckInterval()
	threshold := e.itemWedgedThreshold(gateReview)
	if recheck <= e.wedgeLapse() {
		t.Fatalf("setup: recheck %v must exceed the base lapse %v", recheck, e.wedgeLapse())
	}
	var evs []channelevents.Event
	start := clk.Now()
	next := start
	for clk.Now().Sub(start) <= threshold+2*recheck {
		if !clk.Now().Before(next) {
			e.noteGateHold(item, gateReview, "waiting for reviewers to respond", nil)
			next = clk.Now().Add(recheck)
		}
		// A healthy poll loop: one poll every interval, so the overdue verdict
		// (which would suppress the detector) stays false.
		e.health.beginPoll(clk.Now(), pollVerdict{})
		e.health.endPoll(clk.Now(), pollVerdict{}, clk.Now().Add(30*time.Second))
		evs = append(evs, checkAndDrain(ce, sink)...)
		clk.set(clk.Now().Add(30 * time.Second))
	}
	w := ofType(evs, channelevents.ItemWedged)
	if len(w) != 1 || w[0].Meta["gate"] != "review" {
		t.Fatalf("a review hold re-confirmed every %v must still be reported once past %v: %+v", recheck, threshold, evs)
	}
	if len(ofType(evs, channelevents.ItemUnwedged)) != 0 {
		t.Fatalf("the hold lapsed between confirmations: %+v", evs)
	}
}

func TestItemWedged_EpisodeEndsOnDispatchClearAdvanceAndLapse(t *testing.T) {
	for _, how := range []string{"dispatched", "cleared", "advanced", "lapsed"} {
		t.Run(how, func(t *testing.T) {
			e, sink, clk, ce := wedgeEngine(t)
			item := wedgedItem()
			evs := holdFor(t, e, clk, ce, sink, item, 12, time.Minute)
			if len(ofType(evs, channelevents.ItemWedged)) != 1 {
				t.Fatalf("setup: want one item-wedged: %+v", evs)
			}
			switch how {
			case "dispatched":
				e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 1, StageName: "Implement", StartedAt: clk.Now()})
			case "cleared":
				e.noteGateClear(item, gateFeedback)
			case "advanced":
				e.store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 1, NewStatus: "Review"})
			case "lapsed":
				clk.set(clk.Now().Add(10 * time.Minute)) // never re-confirmed
			}
			evs = checkAndDrain(ce, sink)
			un := ofType(evs, channelevents.ItemUnwedged)
			if len(un) != 1 || un[0].Meta["how"] != how || un[0].Meta["gate"] != "feedback" {
				t.Fatalf("recovery: %+v", evs)
			}
			if evs := checkAndDrain(ce, sink); len(ofType(evs, channelevents.ItemUnwedged)) != 0 {
				t.Fatalf("item-unwedged repeated: %+v", evs)
			}
		})
	}
}

func TestItemWedged_NotRaisedWhileThePollIsOverdueOrTheItemPausedOrQueued(t *testing.T) {
	t.Run("poll overdue", func(t *testing.T) {
		e, sink, clk, ce := wedgeEngineWithPoll(t, 30)
		e.health.beginPoll(clk.Now(), pollVerdict{}) // never ends
		evs := holdFor(t, e, clk, ce, sink, wedgedItem(), 14, time.Minute)
		if len(ofType(evs, channelevents.ItemWedged)) != 0 {
			t.Fatalf("pipeline-stalled explains this; got %+v", evs)
		}
	})
	t.Run("paused", func(t *testing.T) {
		e, sink, clk, ce := wedgeEngine(t)
		e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:paused"})
		if evs := holdFor(t, e, clk, ce, sink, wedgedItem(), 14, time.Minute); len(ofType(evs, channelevents.ItemWedged)) != 0 {
			t.Fatalf("paused item reported: %+v", evs)
		}
	})
	t.Run("queued for a slot", func(t *testing.T) {
		e, sink, clk, ce := wedgeEngine(t)
		item := wedgedItem()
		// Every poll that still cannot get a slot defers the item again.
		var all []channelevents.Event
		for i := 0; i < 14; i++ {
			e.feedbackGateBlocks(item, true, "advance")
			e.emitDispatchDeferred([]string{"owner/repo#1"})
			all = append(all, checkAndDrain(ce, sink)...)
			clk.set(clk.Now().Add(time.Minute))
		}
		if len(ofType(all, channelevents.ItemWedged)) != 0 {
			t.Fatalf("an item waiting for a slot is queued, not wedged: %+v", all)
		}
	})
	t.Run("a stale deferral does not mask a wedge", func(t *testing.T) {
		e, sink, clk, ce := wedgeEngine(t)
		item := wedgedItem()
		e.feedbackGateBlocks(item, true, "advance")
		e.emitDispatchDeferred([]string{"owner/repo#1"})
		if evs := holdFor(t, e, clk, ce, sink, item, 14, time.Minute); len(ofType(evs, channelevents.ItemWedged)) != 1 {
			t.Fatalf("a deferral that stopped recurring must not hide the wedge: %+v", evs)
		}
	})
}

// Review, CI and merge holds wait on people and machines; they count only once
// they have outlived their own backstop.
func TestItemWedged_ReviewCIAndMergeUseTheirBackstop(t *testing.T) {
	e, sink, clk, ce := wedgeEngine(t)
	item := anomalyItem(1)
	for i := 0; i < 30; i++ { // 30 minutes of an ordinary CI wait
		e.noteGateHold(item, gateCI, "waiting for CI checks", nil)
		clk.set(clk.Now().Add(time.Minute))
		ce.checkAnomalies()
	}
	if evs := drain(sink, 120*time.Millisecond); len(ofType(evs, channelevents.ItemWedged)) != 0 {
		t.Fatalf("a normal CI wait must not be wedged: %+v", evs)
	}
	if got, want := e.itemWedgedThreshold(gateCI), e.ciBackstopTimeout(); got < want {
		t.Fatalf("ci threshold %v below its backstop %v", got, want)
	}
}

// ---- cache-divergence ----

// The A9 shape: the engine writes a label, then a light reconcile (a snapshot
// taken before the write reached GitHub) lacks it.
func TestCacheDivergence_ReconcileRevertsRecentEngineLabelWrite(t *testing.T) {
	e, sink, clk, _ := anomalyEngine(t)
	e.seedAPIItem(t, 1, "Implement")
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "stage:Implement:in_progress"})
	clk.set(clk.Now().Add(90 * time.Second))
	e.store.Apply(itemstate.ShallowBoardItemUpdated{Repo: "owner/repo", Number: 1, Item: gh.ProjectItem{
		Number: 1, Repo: "owner/repo", Status: "Implement", Labels: nil,
	}})

	evs := drain(sink, 300*time.Millisecond)
	div := ofType(evs, channelevents.CacheDivergence)
	if len(div) != 1 {
		t.Fatalf("cache-divergence events = %d, want 1: %+v", len(div), evs)
	}
	m := div[0].Meta
	if m["what"] != "label:stage:Implement:in_progress" || m["written"] != "added" || m["observed"] != "removed" ||
		m["path"] != "reconcile" || m["age_seconds"] != "90" {
		t.Fatalf("meta = %v", m)
	}
	var removed, applied *channelevents.Event
	for i := range evs {
		switch evs[i].Type {
		case channelevents.LabelRemoved:
			removed = &evs[i]
		case channelevents.LabelApplied:
			applied = &evs[i]
		}
	}
	if removed == nil || removed.Meta[channelevents.MetaUnverified] != "true" {
		t.Fatalf("the label-removed derived from the revert must be unverified: %+v", evs)
	}
	if applied == nil || applied.Meta[channelevents.MetaUnverified] != "" {
		t.Fatalf("the engine's own write is not unverified: %+v", applied)
	}
	if evs[0].Type != channelevents.LabelApplied {
		// order: the engine write first, then the divergence ahead of the revert's label event
		t.Logf("event order: %v", evs)
	}
}

func TestCacheDivergence_ProbeAndDeepFetchAreNamedAndColumnReverts(t *testing.T) {
	e, sink, clk, _ := anomalyEngine(t)
	e.seedAPIItem(t, 1, "Implement")
	e.store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 1, NewStatus: "Review"})
	clk.set(clk.Now().Add(time.Minute))
	e.store.Apply(itemstate.FromProbe{Inner: itemstate.ProbeBoardItemUpdated{Repo: "owner/repo", Number: 1, Item: gh.BoardProbeItem{Repo: "owner/repo", Number: 1, Status: "Implement"}}})
	div := ofType(drain(sink, 300*time.Millisecond), channelevents.CacheDivergence)
	if len(div) != 1 || div[0].Meta["path"] != "probe" || div[0].Meta["what"] != "column" ||
		div[0].Meta["written"] != "Review" || div[0].Meta["observed"] != "Implement" {
		t.Fatalf("column revert by probe: %+v", div)
	}

	e.seedAPIItem(t, 2, "Implement")
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 2, Label: "fabrik:awaiting-ci"})
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 2, FreshState: gh.ProjectItem{Number: 2, Repo: "owner/repo", Status: "Implement"}})
	div = ofType(drain(sink, 300*time.Millisecond), channelevents.CacheDivergence)
	if len(div) != 1 || div[0].Meta["path"] != "deep-fetch" || div[0].Issue != 2 {
		t.Fatalf("label revert by deep fetch: %+v", div)
	}
}

func TestCacheDivergence_NotRaised(t *testing.T) {
	apply := func(e *Engine, labels ...string) {
		e.store.Apply(itemstate.ShallowBoardItemUpdated{Repo: "owner/repo", Number: 1, Item: gh.ProjectItem{
			Number: 1, Repo: "owner/repo", Status: "Implement", Labels: labels,
		}})
	}
	t.Run("write older than the window", func(t *testing.T) {
		e, sink, clk, _ := anomalyEngine(t)
		e.seedAPIItem(t, 1, "Implement")
		e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "x"})
		clk.set(clk.Now().Add(6 * time.Minute))
		apply(e)
		if evs := drain(sink, 300*time.Millisecond); len(ofType(evs, channelevents.CacheDivergence)) != 0 {
			t.Fatalf("legitimate late difference reported: %+v", evs)
		}
	})
	t.Run("reconcile agrees", func(t *testing.T) {
		e, sink, _, _ := anomalyEngine(t)
		e.seedAPIItem(t, 1, "Implement")
		e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "x"})
		apply(e, "x")
		if evs := drain(sink, 300*time.Millisecond); len(ofType(evs, channelevents.CacheDivergence)) != 0 {
			t.Fatalf("agreeing reconcile reported: %+v", evs)
		}
	})
	t.Run("a label the engine never wrote", func(t *testing.T) {
		e, sink, _, _ := anomalyEngine(t)
		e.seedAPIItem(t, 1, "Implement", "y")
		apply(e)
		evs := drain(sink, 300*time.Millisecond)
		if len(ofType(evs, channelevents.CacheDivergence)) != 0 {
			t.Fatalf("reported: %+v", evs)
		}
		for _, ev := range evs {
			if ev.Meta[channelevents.MetaUnverified] != "" {
				t.Fatalf("an unrelated reconcile change must not be unverified: %+v", ev)
			}
		}
	})
	t.Run("a webhook confirms the write", func(t *testing.T) {
		e, sink, _, _ := anomalyEngine(t)
		e.seedAPIItem(t, 1, "Implement")
		e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "x"})
		e.store.Apply(itemstate.IssueUnlabeled{Repo: "owner/repo", Number: 1, Label: "x", Sender: "alice"})
		apply(e)
		if evs := drain(sink, 300*time.Millisecond); len(ofType(evs, channelevents.CacheDivergence)) != 0 {
			t.Fatalf("a human removal seen by webhook is not a cache revert: %+v", evs)
		}
	})
	t.Run("one divergence per reverting change", func(t *testing.T) {
		e, sink, _, _ := anomalyEngine(t)
		e.seedAPIItem(t, 1, "Implement")
		e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "x"})
		apply(e)
		apply(e, "x") // GitHub did have it: the reconcile re-adds
		apply(e)      // and a stale one reverts again, but the entry is consumed
		if got := len(ofType(drain(sink, 300*time.Millisecond), channelevents.CacheDivergence)); got != 1 {
			t.Fatalf("cache-divergence events = %d, want 1", got)
		}
	})
}

// ---- non-vacuity (FR-014): each detector's events vanish when it is disabled ----

func TestAnomalyDetectors_NonVacuous(t *testing.T) {
	t.Run("retry-starved", func(t *testing.T) {
		defer SetAnomalyDetectorsDisabledForTest(true, false, false)()
		e, sink, clk, ce := anomalyEngine(t)
		e.seedAPIItem(t, 1, "Implement")
		t0 := clk.Now()
		e.emitRetryScheduled(anomalyItem(1), implementStage(), "wall-time-kill", t0)
		clk.set(t0.Add(time.Hour))
		if evs := checkAndDrain(ce, sink); len(ofType(evs, channelevents.RetryStarved)) != 0 {
			t.Fatalf("disabled detector still published: %+v", evs)
		}
	})
	t.Run("item-wedged", func(t *testing.T) {
		defer SetAnomalyDetectorsDisabledForTest(false, true, false)()
		e, sink, clk, ce := wedgeEngine(t)
		if evs := holdFor(t, e, clk, ce, sink, wedgedItem(), 14, time.Minute); len(ofType(evs, channelevents.ItemWedged)) != 0 {
			t.Fatalf("disabled detector still published: %+v", evs)
		}
	})
	t.Run("cache-divergence", func(t *testing.T) {
		defer SetAnomalyDetectorsDisabledForTest(false, false, true)()
		e, sink, _, _ := anomalyEngine(t)
		e.seedAPIItem(t, 1, "Implement")
		e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "x"})
		e.store.Apply(itemstate.ShallowBoardItemUpdated{Repo: "owner/repo", Number: 1, Item: gh.ProjectItem{Number: 1, Repo: "owner/repo", Status: "Implement"}})
		evs := drain(sink, 300*time.Millisecond)
		if len(ofType(evs, channelevents.CacheDivergence)) != 0 {
			t.Fatalf("disabled detector still published: %+v", evs)
		}
		for _, ev := range evs {
			if ev.Meta[channelevents.MetaUnverified] != "" {
				t.Fatalf("disabled detector still marked unverified: %+v", ev)
			}
		}
	})
}

// A hook is a no-op without a hub, so engine behaviour is identical with and
// without push (R10).
func TestAnomalyHooksAreNilSafe(t *testing.T) {
	e := apiEngine(t, 0)
	e.emitRetryScheduled(anomalyItem(1), implementStage(), "error", time.Now())
	e.emitDispatchDeferred([]string{"owner/repo#1"})
	e.noteGateHold(anomalyItem(1), gateFeedback, "x", nil)
	e.noteGateClear(anomalyItem(1), gateFeedback)
	if got := e.anomalyCatchUpEvents(time.Now()); len(got) != 0 {
		t.Fatalf("catch-up without a hub: %+v", got)
	}
}

func TestAnomalyCatchUpListsOpenEpisodes(t *testing.T) {
	e, sink, clk, ce := wedgeEngine(t)
	evs := holdFor(t, e, clk, ce, sink, wedgedItem(), 12, time.Minute)
	if len(ofType(evs, channelevents.ItemWedged)) != 1 {
		t.Fatalf("setup: %+v", evs)
	}
	cu := e.anomalyCatchUpEvents(clk.Now())
	if len(cu) != 1 || cu[0].Type != channelevents.ItemWedged || cu[0].Meta["catch_up"] != "true" || cu[0].DedupKey != "" {
		t.Fatalf("catch-up = %+v", cu)
	}
}
