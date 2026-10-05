package channelevents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	got := s.wait(t)
	if got.Type != EventsDropped {
		t.Fatalf("aged-out event should be replaced by a drop notice, got %+v", got)
	}
	// An expiry must name the lost event type and blame expiry, not overflow.
	if got.Meta["dropped_types"] != "merged x1" || got.Meta["expired"] != "1" {
		t.Errorf("expiry notice meta = %v, want dropped_types=merged x1 expired=1", got.Meta)
	}
	if strings.Contains(got.Content, "overflow") || !strings.Contains(got.Content, "expired") {
		t.Errorf("expiry notice content = %q, want an expiry cause, not overflow", got.Content)
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

// Publish runs on the engine's poll and tick paths, so a burst of deduplicated
// events must not rewrite dedup.json once per event; the keys still survive a
// restart because Close flushes.
func TestHubCoalescesDedupStateWrites(t *testing.T) {
	dir := t.TempDir()
	h := openHub(t, dir, nil)
	h.Subscribe(Subscription{Subscriber: "X"})
	before := h.stateWrites.Load()
	const n = 50
	for i := 1; i <= n; i++ {
		e := ev(i, Merged)
		e.DedupKey = fmt.Sprintf("merged:%d", i)
		h.Publish(e)
	}
	if w := h.stateWrites.Load() - before; w > 5 {
		t.Fatalf("%d dedup.json writes for %d events; writes must be coalesced", w, n)
	}
	h.Close()

	h2 := openHub(t, dir, nil)
	again := ev(1, Merged)
	again.DedupKey = "merged:1"
	h2.Publish(again)
	if got := h2.Queued("X"); got != n {
		t.Fatalf("Queued=%d want %d: dedup keys lost across restart (replayed event queued)", got, n)
	}
}

// A digest collected while a session was attached keeps batching across a brief
// reconnect; only digests queued while the subscriber was away replay one by one.
func TestHubReconnectKeepsDigestCollectedWhileAttached(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	h.Subscribe(Subscription{Subscriber: "X", DigestSeconds: 3600})
	a := newSink()
	detach := h.Attach("X", a)
	h.Publish(ev(1, Merged)) // collected while attached
	detach()
	h.Publish(ev(2, Merged)) // queued while away

	b := newSink()
	h.Attach("X", b)
	if got := b.wait(t); got.Issue != 2 || got.Type == Digest {
		t.Fatalf("event queued while away must replay individually first, got %+v", got)
	}
	b.none(t) // event 1 stays in the pending hour-long digest
	if got := h.Queued("X"); got != 1 {
		t.Fatalf("Queued=%d want 1 pending digest entry", got)
	}
}

// A flood of label events while the subscriber is away must not evict a held
// headline event; the drop notice names what was lost.
func TestHubOverflowKeepsImmediateEventsAndNamesDroppedTypes(t *testing.T) {
	h := openHub(t, t.TempDir(), func(o *Options) { o.HeldMax = 3 })
	h.Subscribe(Subscription{Subscriber: "X"})
	h.Publish(ev(1, ValidateSettled))
	h.Publish(ev(2, Escalated))
	for i := 10; i < 15; i++ {
		h.Publish(ev(i, LabelApplied))
	}
	s := newSink()
	h.Attach("X", s)
	notice := s.wait(t)
	if notice.Type != EventsDropped || notice.Meta["count"] != "4" || notice.Meta["dropped_types"] != "label-applied x4" {
		t.Fatalf("want drop notice for 4 label-applied, got %+v", notice)
	}
	for _, want := range []EventType{ValidateSettled, Escalated, LabelApplied} {
		if got := s.wait(t); got.Type != want {
			t.Fatalf("got %s want %s", got.Type, want)
		}
	}
}

// When every held entry is immediate, the oldest one is the one evicted, and the
// notice still names it.
func TestHubOverflowEvictsImmediateOnlyWhenNothingElseIs(t *testing.T) {
	h := openHub(t, t.TempDir(), func(o *Options) { o.HeldMax = 2 })
	h.Subscribe(Subscription{Subscriber: "X"})
	h.Publish(ev(1, Paused))
	h.Publish(ev(2, Escalated))
	h.Publish(ev(3, ValidateSettled))
	s := newSink()
	h.Attach("X", s)
	if n := s.wait(t); n.Type != EventsDropped || n.Meta["dropped_types"] != "paused x1" {
		t.Fatalf("want notice naming paused, got %+v", n)
	}
	for _, want := range []int{2, 3} {
		if got := s.wait(t); got.Issue != want {
			t.Fatalf("got issue %d want %d", got.Issue, want)
		}
	}
}

// Flush persists the coalesced dedup state at once: an exec runs no deferred
// Close, so the engine flushes right before one. A second hub opened over the
// same directory without closing the first stands in for the re-exec'd process.
func TestHubFlushPersistsDedupWithoutClose(t *testing.T) {
	dir := t.TempDir()
	h := openHub(t, dir, nil)
	h.Subscribe(Subscription{Subscriber: "X"})
	s := newSink()
	h.Attach("X", s)
	e := ev(1, ValidateSettled)
	e.DedupKey = "vs:o/r#1:1"
	h.Publish(e)
	s.wait(t) // delivered live: only the dedup state remembers it
	h.Flush()

	h2 := openHub(t, dir, nil)
	h2.Publish(e)
	if got := h2.Queued("X"); got != 0 {
		t.Fatalf("Queued=%d want 0: the announced settle was replayed after the exec (dedup state not flushed)", got)
	}
}

// CatchUp queues a snapshot for one subscriber, only for events its subscriptions
// match, without a dedup key, and never doubles an announcement the queue already
// holds for the same (type, repo, issue).
func TestHubCatchUp(t *testing.T) {
	h := openHub(t, t.TempDir(), nil)
	sub, _, err := h.SubscribeNew(Subscription{Subscriber: "X", Events: []EventType{ValidateSettled}})
	if err != nil {
		t.Fatal(err)
	}
	held := ev(1, ValidateSettled)
	held.DedupKey = "vs:o/r#1:1"
	h.Publish(held) // already held for X

	snapshot := []Event{ev(1, ValidateSettled), ev(2, ValidateSettled), ev(3, Merged)}
	snapshot[0].DedupKey = "ignored"
	if n := h.CatchUp("X", sub.ID, snapshot); n != 1 {
		t.Fatalf("CatchUp queued %d, want 1 (#1 already held, #3 not subscribed)", n)
	}
	if n := h.CatchUp("nobody", "", snapshot); n != 0 {
		t.Fatalf("CatchUp for an unknown subscriber queued %d", n)
	}
	s := newSink()
	h.Attach("X", s)
	if got := s.wait(t); got.Issue != 1 {
		t.Fatalf("held event first, got %+v", got)
	}
	if got := s.wait(t); got.Issue != 2 || got.DedupKey != "" {
		t.Fatalf("catch-up event second and without a dedup key, got %+v", got)
	}
	s.none(t)
}

// A subscriber that stayed attached longer than SubscriberTTL is not abandoned:
// lastAttach means "last connected", so neither a prune while it is live, nor the
// prune right after it drops, nor a daemon restart may delete its subscriptions.
func TestHubPruneKeepsLongAttachedSubscriber(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	var cmu sync.Mutex
	clock := now
	setClock := func(t time.Time) { cmu.Lock(); clock = t; cmu.Unlock() }
	mod := func(o *Options) {
		o.Now = func() time.Time { cmu.Lock(); defer cmu.Unlock(); return clock }
		o.SubscriberTTL = 24 * time.Hour
	}
	dir := t.TempDir()
	h := openHub(t, dir, mod)
	if _, err := h.Subscribe(Subscription{Subscriber: "X"}); err != nil {
		t.Fatal(err)
	}
	s := newSink()
	detach := h.Attach("X", s)

	// Attached for far longer than the TTL, then pruned while still live.
	setClock(now.Add(48 * time.Hour))
	h.Prune(clock)
	if n := len(h.Subscriptions("X")); n != 1 {
		t.Fatalf("live subscriber pruned: %d subscriptions", n)
	}

	// It drops briefly; the next prune must still see a recent connection.
	setClock(now.Add(49 * time.Hour))
	detach()
	h.Prune(clock)
	if n := len(h.Subscriptions("X")); n != 1 {
		t.Fatalf("subscriber pruned right after detaching: %d subscriptions", n)
	}

	// A subscriber attached at shutdown counts as connected at shutdown, so a
	// restart after the TTL (Open prunes before anyone re-attaches) keeps it.
	h.Attach("X", newSink())
	setClock(now.Add(200 * time.Hour))
	h.Close()
	setClock(now.Add(210 * time.Hour))
	h2 := openHub(t, dir, mod)
	if n := len(h2.Subscriptions("X")); n != 1 {
		t.Fatalf("subscriber attached at shutdown pruned on restart: %d subscriptions", n)
	}

	// An abandoned one is still pruned once it has been away longer than the TTL.
	setClock(now.Add(300 * time.Hour))
	h2.Prune(clock)
	if n := len(h2.Subscriptions("X")); n != 0 {
		t.Fatalf("abandoned subscriber kept: %d subscriptions", n)
	}
}
