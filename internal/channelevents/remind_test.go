package channelevents

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func decision(n int) Event {
	e := ev(n, DecisionPending)
	e.Meta = map[string]string{"column": "Specify", "decision": "advance"}
	e.CommentURL = "https://example.test/c/1"
	e.DedupKey = "dp:o/r#" + string(rune('0'+n)) + ":1"
	return e
}

func remindHub(t *testing.T, dir string, clk *testClock) *Hub {
	return openHub(t, dir, func(o *Options) { o.Now = clk.Now })
}

func TestRemindAfterValidation(t *testing.T) {
	for _, tc := range []struct {
		secs int
		ok   bool
	}{{0, true}, {600, true}, {int((30 * 24 * time.Hour) / time.Second), true}, {59, false}, {599, false}, {-1, false}, {int((31 * 24 * time.Hour) / time.Second), false}} {
		err := Subscription{Subscriber: "X", RemindAfterSeconds: tc.secs}.Validate(MinDigest)
		if (err == nil) != tc.ok {
			t.Errorf("remind_after=%d: err=%v, want ok=%v", tc.secs, err, tc.ok)
		}
	}
}

func TestNoReminderUnlessConfigured(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	h := remindHub(t, t.TempDir(), clk)
	if _, err := h.Subscribe(Subscription{Subscriber: "X"}); err != nil {
		t.Fatal(err)
	}
	s := newSink()
	h.Attach("X", s)
	e := decision(1)
	h.Publish(e)
	s.wait(t)
	for i := 0; i < 5; i++ {
		clk.advance(48 * time.Hour)
		if n := h.PublishReminders(e, e.DedupKey); n != 0 {
			t.Fatalf("reminded %d subscriber(s) with no remind_after configured", n)
		}
	}
	s.none(t)
}

func TestReminderTimingAndAttributes(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	h := remindHub(t, t.TempDir(), clk)
	if _, err := h.Subscribe(Subscription{Subscriber: "X", RemindAfterSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	s := newSink()
	h.Attach("X", s)
	e := decision(1)
	h.Publish(e)
	orig := s.wait(t)

	clk.advance(59 * time.Minute)
	if n := h.PublishReminders(e, e.DedupKey); n != 0 {
		t.Fatalf("reminded after 59m of a 60m interval")
	}
	clk.advance(2 * time.Minute)
	if n := h.PublishReminders(e, e.DedupKey); n != 1 {
		t.Fatalf("reminded %d, want 1 once the interval passed", n)
	}
	rem := s.wait(t)
	if rem.Meta["reminder"] != "true" || rem.Meta["reminder_count"] != "1" || !strings.HasPrefix(rem.Content, "(reminder)") {
		t.Errorf("not marked as a reminder: %+v", rem)
	}
	if rem.CommentURL != orig.CommentURL || rem.Meta["column"] != orig.Meta["column"] || rem.Meta["decision"] != orig.Meta["decision"] {
		t.Errorf("a reminder carries the original attributes (incl. the comment link): %+v vs %+v", rem, orig)
	}
	if rem.DedupKey != "" {
		t.Errorf("a reminder must not carry the episode's dedup key")
	}
	// The next one runs from the previous reminder, not the announcement.
	clk.advance(30 * time.Minute)
	if n := h.PublishReminders(e, e.DedupKey); n != 0 {
		t.Fatalf("reminded again 30m after the last reminder")
	}
	clk.advance(31 * time.Minute)
	if n := h.PublishReminders(e, e.DedupKey); n != 1 {
		t.Fatalf("second reminder missing")
	}
	if got := s.wait(t); got.Meta["reminder_count"] != "2" {
		t.Errorf("count = %s", got.Meta["reminder_count"])
	}
}

func TestReminderIsPerSubscriptionAndAttachedOnly(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	h := remindHub(t, t.TempDir(), clk)
	for _, sub := range []Subscription{
		{Subscriber: "on", RemindAfterSeconds: 600},
		{Subscriber: "off"},
		{Subscriber: "detached", RemindAfterSeconds: 600},
	} {
		if _, err := h.Subscribe(sub); err != nil {
			t.Fatal(err)
		}
	}
	on, off := newSink(), newSink()
	h.Attach("on", on)
	h.Attach("off", off)
	e := decision(1)
	h.Publish(e)
	on.wait(t)
	off.wait(t)
	clk.advance(11 * time.Minute)
	if n := h.PublishReminders(e, e.DedupKey); n != 1 {
		t.Fatalf("reminded %d, want only the attached subscriber that asked", n)
	}
	on.wait(t)
	off.none(t)
	if got := h.Queued("detached"); got != 1 {
		t.Errorf("detached subscriber holds %d (the original only; reminders are never held)", got)
	}
}

func TestReminderAnchorsSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	clk := &testClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	h, err := Open(Options{Dir: dir, MinDigest: time.Millisecond, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe(Subscription{Subscriber: "X", RemindAfterSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	s := newSink()
	h.Attach("X", s)
	e := decision(1)
	h.Publish(e)
	s.wait(t)
	clk.advance(61 * time.Minute)
	if h.PublishReminders(e, e.DedupKey) != 1 {
		t.Fatal("first reminder missing")
	}
	s.wait(t)
	h.Close() // flushes the persisted anchors

	// A "restart" 30 minutes later: the interval runs from the persisted
	// reminder, not from process start.
	clk.advance(30 * time.Minute)
	h2, err := Open(Options{Dir: dir, MinDigest: time.Millisecond, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	s2 := newSink()
	h2.Attach("X", s2)
	// Drain the held original-less queue: nothing is held after a clean delivery.
	if n := h2.PublishReminders(e, e.DedupKey); n != 0 {
		t.Fatalf("a restart 30m after a reminder fired another (%d)", n)
	}
	clk.advance(31 * time.Minute)
	if n := h2.PublishReminders(e, e.DedupKey); n != 1 {
		t.Fatalf("reminder due 61m after the persisted one did not fire (%d)", n)
	}
}

func TestReminderNeverFiresFromUptimeAlone(t *testing.T) {
	// A hub with no record of the announcement plants an anchor instead of
	// reminding: process start is not an announcement.
	clk := &testClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	h := remindHub(t, t.TempDir(), clk)
	if _, err := h.Subscribe(Subscription{Subscriber: "X", RemindAfterSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	s := newSink()
	h.Attach("X", s)
	e := decision(1) // never published through this hub
	clk.advance(24 * time.Hour)
	if n := h.PublishReminders(e, e.DedupKey); n != 0 {
		t.Fatalf("reminded with no announcement on record")
	}
	clk.advance(11 * time.Minute)
	if n := h.PublishReminders(e, e.DedupKey); n != 1 {
		t.Fatalf("the planted anchor should make the next interval due")
	}
}

func TestClearRemindersEndsTheEpisode(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	h := remindHub(t, t.TempDir(), clk)
	if _, err := h.Subscribe(Subscription{Subscriber: "X", RemindAfterSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	s := newSink()
	h.Attach("X", s)
	e := decision(1)
	h.Publish(e)
	s.wait(t)
	clk.advance(11 * time.Minute)
	h.PublishReminders(e, e.DedupKey)
	s.wait(t)
	h.ClearReminders(e.DedupKey)
	h.mu.Lock()
	left := len(h.reminders)
	h.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d reminder entries survive the episode", left)
	}
}

func TestNewEventsAreCatalogued(t *testing.T) {
	if !IsImmediate(SettlementWithdrawn) || IsImmediate(DecisionPending) {
		t.Errorf("settlement-withdrawn is immediate, decision-pending is digest-able")
	}
	found := map[EventType]bool{}
	for _, e := range Subscribable() {
		found[e] = true
	}
	if !found[DecisionPending] || !found[SettlementWithdrawn] {
		t.Errorf("new events must be subscribable")
	}
}

// TestCatchUpRestartsTheReminderClock pins the review finding on R6a: a
// subscriber that attaches after the interval has already run out gets the
// decision in its catch-up, and the next tick must not send a reminder right
// behind it. The catch-up counts as an announcement for that subscriber.
func TestCatchUpRestartsTheReminderClock(t *testing.T) {
	run := func(t *testing.T, withKey bool) (afterCatchUp, afterInterval int) {
		clk := &testClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
		h := remindHub(t, t.TempDir(), clk)
		if _, err := h.Subscribe(Subscription{Subscriber: "X", RemindAfterSeconds: 3600}); err != nil {
			t.Fatal(err)
		}
		e := decision(1)
		h.Publish(e) // announced while X was detached; the reminder is long overdue by attach time
		clk.advance(5 * time.Hour)
		s := newSink()
		h.Attach("X", s)
		c := e
		if !withKey {
			c.DedupKey = ""
		}
		h.CatchUp("X", "", []Event{c})
		s.wait(t)
		afterCatchUp = h.PublishReminders(e, e.DedupKey)
		clk.advance(61 * time.Minute)
		afterInterval = h.PublishReminders(e, e.DedupKey)
		return
	}
	if a, b := run(t, true); a != 0 || b != 1 {
		t.Fatalf("with the episode key: reminders right after catch-up = %d (want 0), one interval later = %d (want 1)", a, b)
	}
	// Control: without the key the catch-up leaves the clock alone and the
	// overdue reminder fires immediately, which is the defect.
	if a, _ := run(t, false); a != 1 {
		t.Fatalf("control: reminder after a keyless catch-up = %d, want 1 (the unfixed behaviour)", a)
	}
}
