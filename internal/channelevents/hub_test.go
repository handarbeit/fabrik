package channelevents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	mu         sync.Mutex
	got        []Event
	superseded bool
	fail       bool
	ch         chan Event
}

func newSink() *fakeSink { return &fakeSink{ch: make(chan Event, 1000)} }

func (f *fakeSink) Deliver(ev Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("peer gone")
	}
	f.got = append(f.got, ev)
	f.ch <- ev
	return nil
}
func (f *fakeSink) Superseded()         { f.mu.Lock(); f.superseded = true; f.mu.Unlock() }
func (f *fakeSink) wasSuperseded() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.superseded }
func (f *fakeSink) wait(t *testing.T) Event {
	t.Helper()
	select {
	case ev := <-f.ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for delivery")
		return Event{}
	}
}
func (f *fakeSink) none(t *testing.T) {
	t.Helper()
	select {
	case ev := <-f.ch:
		t.Fatalf("unexpected delivery: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}

func openHub(t *testing.T, dir string, mod func(*Options)) *Hub {
	t.Helper()
	o := Options{Dir: dir, MinDigest: time.Millisecond}
	if mod != nil {
		mod(&o)
	}
	h, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func ev(n int, typ EventType) Event {
	return Event{Type: typ, Repo: "o/r", Issue: n, Content: fmt.Sprintf("event %d", n)}
}

func TestHubHeldThenLiveInOrder(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	if _, err := h.Subscribe(Subscription{Subscriber: "X"}); err != nil {
		t.Fatal(err)
	}
	// Subscribed, never attached: three events are held.
	for i := 1; i <= 3; i++ {
		h.Publish(ev(i, Merged))
	}
	if got := h.Queued("X"); got != 3 {
		t.Fatalf("Queued=%d want 3", got)
	}
	s := newSink()
	h.Attach("X", s)
	for i := 1; i <= 3; i++ {
		if got := s.wait(t); got.Issue != i {
			t.Fatalf("held event %d arrived as issue %d", i, got.Issue)
		}
	}
	h.Publish(ev(4, Merged))
	if got := s.wait(t); got.Issue != 4 {
		t.Fatalf("live event: issue %d", got.Issue)
	}
	waitFor(t, func() bool { return h.Queued("X") == 0 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestHubSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	h := openHub(t, dir, nil)
	sub, _ := h.Subscribe(Subscription{Subscriber: "X"})
	h.Publish(ev(1, Merged))
	h.Publish(Event{Type: ValidateSettled, Repo: "o/r", Issue: 2, DedupKey: "vs:2:1", Content: "settled"})
	h.BumpCounter("ep:2")
	h.Close()

	h2 := openHub(t, dir, nil)
	if got := h2.Subscriptions("X"); len(got) != 1 || got[0].ID != sub.ID {
		t.Fatalf("subscription lost: %+v", got)
	}
	if h2.Counter("ep:2") != 1 {
		t.Fatal("counter lost")
	}
	// A replay of the same transition after restart must not duplicate.
	h2.Publish(Event{Type: ValidateSettled, Repo: "o/r", Issue: 2, DedupKey: "vs:2:1", Content: "settled"})
	if got := h2.Queued("X"); got != 2 {
		t.Fatalf("Queued=%d want 2 (replay deduped)", got)
	}
	s := newSink()
	h2.Attach("X", s)
	if s.wait(t).Issue != 1 || s.wait(t).Issue != 2 {
		t.Fatal("held events not delivered in order after restart")
	}
}

func TestHubOverflowDropsOldestAndNotices(t *testing.T) {
	h := openHub(t, t.TempDir(), func(o *Options) { o.HeldMax = 3 })
	h.Subscribe(Subscription{Subscriber: "X"})
	for i := 1; i <= 5; i++ {
		h.Publish(ev(i, Merged))
	}
	s := newSink()
	h.Attach("X", s)
	notice := s.wait(t)
	if notice.Type != EventsDropped || notice.Meta["count"] != "2" {
		t.Fatalf("want drop notice for 2, got %+v", notice)
	}
	for _, want := range []int{3, 4, 5} {
		if got := s.wait(t); got.Issue != want {
			t.Fatalf("got issue %d want %d", got.Issue, want)
		}
	}
}

func TestHubDedupWithinQueue(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	h.Subscribe(Subscription{Subscriber: "X"})
	e := ev(1, Merged)
	e.DedupKey = "k"
	h.Publish(e)
	h.Publish(e)
	if h.Queued("X") != 1 {
		t.Fatalf("Queued=%d", h.Queued("X"))
	}
}

func TestHubDigestBatchesOrdinaryButNotImmediate(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	h.Subscribe(Subscription{Subscriber: "X", DigestSeconds: 1})
	s := newSink()
	h.Attach("X", s)

	h.Publish(ev(1, Merged))
	h.Publish(ev(2, Stalled))
	// Immediate types bypass the digest.
	h.Publish(ev(3, ValidateSettled))
	if got := s.wait(t); got.Type != ValidateSettled {
		t.Fatalf("immediate event first, got %+v", got)
	}
	h.Publish(ev(4, Escalated))
	if got := s.wait(t); got.Type != Escalated {
		t.Fatalf("got %+v", got)
	}
	d := s.wait(t) // digest falls due after ~1s
	if d.Type != Digest || d.Meta["count"] != "2" {
		t.Fatalf("want digest of 2, got %+v", d)
	}
}

func TestHubReplayedHeldDigestEventsAreIndividual(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	h.Subscribe(Subscription{Subscriber: "X", DigestSeconds: 3600})
	h.Publish(ev(1, Merged))
	h.Publish(ev(2, Merged))
	s := newSink()
	h.Attach("X", s)
	if s.wait(t).Issue != 1 || s.wait(t).Issue != 2 {
		t.Fatal("held digest-mode events must replay individually in order")
	}
}

func TestHubSameNameSupersedes(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	h.Subscribe(Subscription{Subscriber: "X"})
	a, b := newSink(), newSink()
	h.Attach("X", a)
	h.Attach("X", b)
	if !a.wasSuperseded() {
		t.Fatal("old attachment should be superseded")
	}
	h.Publish(ev(1, Merged))
	b.wait(t)
	a.none(t)
}

func TestHubFailedDeliveryKeepsEventAndDetaches(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	h.Subscribe(Subscription{Subscriber: "X"})
	a := newSink()
	a.fail = true
	h.Attach("X", a)
	h.Publish(ev(1, Merged))
	time.Sleep(100 * time.Millisecond)
	if h.Queued("X") != 1 {
		t.Fatalf("event must stay queued after failed delivery, Queued=%d", h.Queued("X"))
	}
	b := newSink()
	h.Attach("X", b)
	if b.wait(t).Issue != 1 {
		t.Fatal("event not redelivered")
	}
}

func TestHubScopeRouting(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	h.Subscribe(Subscription{Subscriber: "A", Issues: []IssueRef{{Repo: "o/r", Number: 1}}})
	h.Subscribe(Subscription{Subscriber: "B", Issues: []IssueRef{{Repo: "o/r", Number: 2}}})
	h.Subscribe(Subscription{Subscriber: "C", Issues: []IssueRef{{Number: 99}}})
	a, b := newSink(), newSink()
	h.Attach("A", a)
	h.Attach("B", b)
	h.Publish(ev(1, Merged))
	a.wait(t)
	b.none(t)
	// Account-wide event reaches everyone whose filter admits it.
	h.Publish(Event{Type: ClaudeLimitSuspended, Content: "limit"})
	a.wait(t)
	b.wait(t)
	if h.Queued("C") != 1 {
		t.Fatalf("C should hold the account-wide event, Queued=%d", h.Queued("C"))
	}
}

func TestHubSubscribeIsIdempotent(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	a, _ := h.Subscribe(Subscription{Subscriber: "X", Repos: []string{"o/r"}})
	b, _ := h.Subscribe(Subscription{Subscriber: "X", Repos: []string{"o/r"}})
	if a.ID != b.ID || len(h.Subscriptions("X")) != 1 {
		t.Fatalf("duplicate subscription created: %+v %+v", a, b)
	}
	if n := h.Unsubscribe("X", ""); n != 1 {
		t.Fatalf("Unsubscribe removed %d", n)
	}
}

func TestHubCorruptFilesQuarantined(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "subscriptions.json"), []byte("{not json"), 0o600)
	os.WriteFile(filepath.Join(dir, "held-aaaa.json"), []byte("garbage"), 0o600)
	h := openHub(t, dir, nil)
	if len(h.Subscriptions("")) != 0 {
		t.Fatal("corrupt registry should start empty")
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*.corrupt-*"))
	if len(m) != 2 {
		t.Fatalf("want 2 quarantined files, got %v", m)
	}
}

func TestHubPruneAbandonedSubscriberAndOldEvents(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	var cmu sync.Mutex
	clock := now
	setClock := func(t time.Time) { cmu.Lock(); clock = t; cmu.Unlock() }
	h := openHub(t, t.TempDir(), func(o *Options) {
		o.Now = func() time.Time { cmu.Lock(); defer cmu.Unlock(); return clock }
		o.SubscriberTTL = 24 * time.Hour
		o.MaxAge = time.Hour
	})
	h.Subscribe(Subscription{Subscriber: "old"})
	h.Publish(ev(1, Merged))
	setClock(now.Add(2 * time.Hour))
	h.Prune(now.Add(2 * time.Hour))
	s := newSink()
	h.Attach("old", s)
	if got := s.wait(t); got.Type != EventsDropped {
		t.Fatalf("aged-out event should be replaced by a drop notice, got %+v", got)
	}
	setClock(now.Add(72 * time.Hour))
	h.Subscribe(Subscription{Subscriber: "gone", Repos: []string{"o/r"}})
	setClock(now.Add(200 * time.Hour))
	// "old" is attached, so it survives; "gone" is abandoned.
	h.Prune(now.Add(200 * time.Hour))
	if len(h.Subscriptions("gone")) != 0 {
		t.Fatal("abandoned subscriber should be pruned")
	}
}

func TestHubConcurrentPublishAttachRace(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	h.Subscribe(Subscription{Subscriber: "X"})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				h.Publish(ev(g*100+i, Merged))
			}
		}(g)
	}
	s := newSink()
	for i := 0; i < 3; i++ {
		h.Attach("X", s)
		time.Sleep(5 * time.Millisecond)
	}
	wg.Wait()
	seen := map[int]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < 100 {
		select {
		case e := <-s.ch:
			seen[e.Issue] = true
		case <-deadline:
			t.Fatalf("only %d/100 delivered", len(seen))
		}
	}
}
