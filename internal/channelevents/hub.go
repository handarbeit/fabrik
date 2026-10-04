package channelevents

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Defaults for Options.
const (
	DefaultHeldMax       = 200
	DefaultMaxAge        = 7 * 24 * time.Hour
	DefaultSubscriberTTL = 30 * 24 * time.Hour
	seenMax              = 5000
	seenTTL              = 14 * 24 * time.Hour
)

// Sink is one attached subscriber session. Deliver blocks until the event is
// written (with its own write deadline) and returns an error when the peer is
// gone; Superseded tells a displaced session that a newer attach under the same
// name replaced it, so it must stop reconnecting.
type Sink interface {
	Deliver(Event) error
	Superseded()
}

// Options configures a Hub.
type Options struct {
	// Dir holds the persisted state (created if absent).
	Dir string
	// HeldMax bounds each subscriber's queue (default DefaultHeldMax).
	HeldMax int
	// MaxAge prunes queued events older than this (default DefaultMaxAge).
	MaxAge time.Duration
	// SubscriberTTL prunes a subscriber that has not attached for this long
	// (default DefaultSubscriberTTL).
	SubscriberTTL time.Duration
	// MinDigest is the lower digest bound (default MinDigest); tests shorten it.
	MinDigest time.Duration
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

type registryFile struct {
	Subscriptions []Subscription       `json:"subscriptions"`
	LastAttach    map[string]time.Time `json:"last_attach,omitempty"`
}

type stateFile struct {
	Seen     map[string]time.Time `json:"seen,omitempty"`
	Counters map[string]int       `json:"counters,omitempty"`
}

type attachment struct {
	sink Sink
	stop chan struct{}
}

type subscriber struct {
	name   string
	path   string
	sendMu sync.Mutex // serialises one peek→deliver→remove cycle across attachments
	mu     sync.Mutex // guards q and att
	q      queueFile
	att    *attachment
	wake   chan struct{}
}

// Hub routes published events to subscribers, holding them while a subscriber
// has no attached session.
type Hub struct {
	opt Options

	mu          sync.Mutex
	subs        []Subscription
	lastAttach  map[string]time.Time
	subscribers map[string]*subscriber
	seen        map[string]time.Time
	counters    map[string]int
	closed      bool

	wg sync.WaitGroup
}

// Open loads (or initialises) the hub state under o.Dir. A corrupt state file is
// quarantined and logged, never fatal.
func Open(o Options) (*Hub, error) {
	if o.Dir == "" {
		return nil, fmt.Errorf("channelevents: Dir is required")
	}
	if o.HeldMax <= 0 {
		o.HeldMax = DefaultHeldMax
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultMaxAge
	}
	if o.SubscriberTTL <= 0 {
		o.SubscriberTTL = DefaultSubscriberTTL
	}
	if o.MinDigest <= 0 {
		o.MinDigest = MinDigest
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating channel state dir: %w", err)
	}
	h := &Hub{
		opt:         o,
		lastAttach:  map[string]time.Time{},
		subscribers: map[string]*subscriber{},
		seen:        map[string]time.Time{},
		counters:    map[string]int{},
	}
	now := o.Now()

	var reg registryFile
	if ok, q, err := loadJSON(h.registryPath(), &reg, now); err != nil {
		return nil, err
	} else if q != "" {
		o.Logf("channel: corrupt subscriptions file quarantined to %s\n", q)
	} else if ok {
		h.subs = reg.Subscriptions
		for k, v := range reg.LastAttach {
			h.lastAttach[k] = v
		}
	}
	var st stateFile
	if ok, q, err := loadJSON(h.statePath(), &st, now); err != nil {
		return nil, err
	} else if q != "" {
		o.Logf("channel: corrupt dedup state quarantined to %s\n", q)
	} else if ok {
		for k, v := range st.Seen {
			h.seen[k] = v
		}
		for k, v := range st.Counters {
			h.counters[k] = v
		}
	}

	matches, _ := filepath.Glob(filepath.Join(o.Dir, "held-*.json"))
	for _, p := range matches {
		var q queueFile
		ok, quarantined, err := loadJSON(p, &q, now)
		if err != nil {
			return nil, err
		}
		if quarantined != "" {
			o.Logf("channel: corrupt held queue quarantined to %s\n", quarantined)
			continue
		}
		if ok && q.Name != "" {
			h.subscribers[q.Name] = newSubscriber(q.Name, p, q)
		}
	}
	h.Prune(now)
	return h, nil
}

func newSubscriber(name, path string, q queueFile) *subscriber {
	q.Name = name
	return &subscriber{name: name, path: path, q: q, wake: make(chan struct{}, 1)}
}

func (h *Hub) registryPath() string { return filepath.Join(h.opt.Dir, "subscriptions.json") }
func (h *Hub) statePath() string    { return filepath.Join(h.opt.Dir, "dedup.json") }
func (h *Hub) heldPath(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(h.opt.Dir, "held-"+hex.EncodeToString(sum[:6])+".json")
}

// saveRegistryLocked persists subscriptions and last-attach times. h.mu held.
func (h *Hub) saveRegistryLocked() {
	reg := registryFile{Subscriptions: h.subs, LastAttach: h.lastAttach}
	if err := saveJSON(h.registryPath(), reg); err != nil {
		h.opt.Logf("channel: saving subscriptions: %v\n", err)
	}
}

func (h *Hub) saveStateLocked() {
	if len(h.seen) > seenMax {
		type kv struct {
			k string
			t time.Time
		}
		all := make([]kv, 0, len(h.seen))
		for k, t := range h.seen {
			all = append(all, kv{k, t})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for _, e := range all[:len(all)-seenMax] {
			delete(h.seen, e.k)
		}
	}
	if err := saveJSON(h.statePath(), stateFile{Seen: h.seen, Counters: h.counters}); err != nil {
		h.opt.Logf("channel: saving dedup state: %v\n", err)
	}
}

func (h *Hub) saveQueue(s *subscriber) {
	if err := saveJSON(s.path, s.q); err != nil {
		h.opt.Logf("channel: saving held queue for %q: %v\n", s.name, err)
	}
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "sub-" + hex.EncodeToString(b[:])
}

// Subscribe registers sub. An identical subscription (same subscriber and
// fields) already present is returned unchanged, so a session re-subscribing on
// every start does not accumulate duplicates.
func (h *Hub) Subscribe(sub Subscription) (Subscription, error) {
	if err := sub.Validate(h.opt.MinDigest); err != nil {
		return Subscription{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := sub.canonical()
	for _, ex := range h.subs {
		if ex.Subscriber == sub.Subscriber && ex.canonical() == key {
			return ex, nil
		}
	}
	sub.ID = newID()
	h.subs = append(h.subs, sub)
	if h.lastAttach[sub.Subscriber].IsZero() {
		h.lastAttach[sub.Subscriber] = h.opt.Now()
	}
	h.saveRegistryLocked()
	return sub, nil
}

// Unsubscribe removes one subscription by id, or every subscription of the
// subscriber when id is empty; dropping the last one also discards its held
// queue. It returns how many subscriptions were removed.
func (h *Hub) Unsubscribe(subscriberName, id string) int {
	h.mu.Lock()
	removed := 0
	kept := h.subs[:0:0]
	for _, s := range h.subs {
		if s.Subscriber == subscriberName && (id == "" || s.ID == id) {
			removed++
			continue
		}
		kept = append(kept, s)
	}
	h.subs = kept
	var drop *subscriber
	if removed > 0 {
		if !h.hasSubsLocked(subscriberName) {
			drop = h.subscribers[subscriberName]
		}
		h.saveRegistryLocked()
	}
	h.mu.Unlock()
	if drop != nil {
		drop.mu.Lock()
		drop.q.Entries, drop.q.Dropped = nil, 0
		if drop.att == nil {
			_ = os.Remove(drop.path)
		} else {
			h.saveQueue(drop)
		}
		drop.mu.Unlock()
	}
	return removed
}

func (h *Hub) hasSubsLocked(name string) bool {
	for _, s := range h.subs {
		if s.Subscriber == name {
			return true
		}
	}
	return false
}

// Subscriptions lists a subscriber's subscriptions; an empty name lists all.
func (h *Hub) Subscriptions(subscriberName string) []Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Subscription
	for _, s := range h.subs {
		if subscriberName == "" || s.Subscriber == subscriberName {
			out = append(out, s)
		}
	}
	return out
}

// Counter returns a persisted counter (0 when unset).
func (h *Hub) Counter(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counters[key]
}

// BumpCounter increments and persists a counter, returning the new value.
func (h *Hub) BumpCounter(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counters[key]++
	h.saveStateLocked()
	return h.counters[key]
}

type target struct {
	name   string
	digest time.Duration // 0 = immediate
}

// Publish routes ev to every subscriber with a matching subscription and
// appends it to that subscriber's queue; an attached subscriber's drain loop
// delivers it. It never blocks on a session. A non-empty DedupKey already seen
// (this run or a previous one) drops the event.
func (h *Hub) Publish(ev Event) {
	now := h.opt.Now()
	if ev.At.IsZero() {
		ev.At = now
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if ev.DedupKey != "" {
		if _, dup := h.seen[ev.DedupKey]; dup {
			h.mu.Unlock()
			return
		}
		h.seen[ev.DedupKey] = now
		h.saveStateLocked()
	}
	targets := map[string]*target{}
	var order []string
	for _, sub := range h.subs {
		if !sub.Matches(ev) {
			continue
		}
		t, ok := targets[sub.Subscriber]
		if !ok {
			t = &target{name: sub.Subscriber, digest: -1}
			targets[sub.Subscriber] = t
			order = append(order, sub.Subscriber)
		}
		d := sub.Digest()
		if IsImmediate(ev.Type) {
			d = 0
		}
		// Any matching non-digest subscription makes the delivery immediate;
		// otherwise the shortest digest interval wins.
		switch {
		case d == 0:
			t.digest = 0
		case t.digest == -1 || (t.digest > 0 && d < t.digest):
			t.digest = d
		}
	}
	subs := make([]*subscriber, 0, len(order))
	for _, name := range order {
		s := h.subscribers[name]
		if s == nil {
			s = newSubscriber(name, h.heldPath(name), queueFile{})
			h.subscribers[name] = s
		}
		subs = append(subs, s)
	}
	h.mu.Unlock()

	for i, s := range subs {
		t := targets[order[i]]
		h.enqueue(s, ev, t.digest, now)
	}
}

func (h *Hub) enqueue(s *subscriber, ev Event, digest time.Duration, now time.Time) {
	s.mu.Lock()
	var due time.Time
	if digest > 0 {
		for _, e := range s.q.Entries {
			if e.Digest {
				due = e.Due
				break
			}
		}
		if due.IsZero() {
			due = now.Add(digest)
		}
	}
	changed := s.q.push(ev, digest > 0, due, h.opt.HeldMax)
	if changed {
		h.saveQueue(s)
	}
	s.mu.Unlock()
	if changed {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// Attach binds sink as the live session for name and starts delivering: held
// events first, in order and individually, then live ones. A previous
// attachment under the same name is superseded. The returned func detaches sink
// (a no-op when it has already been replaced).
func (h *Hub) Attach(name string, sink Sink) func() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		sink.Superseded()
		return func() {}
	}
	s := h.subscribers[name]
	if s == nil {
		s = newSubscriber(name, h.heldPath(name), queueFile{})
		h.subscribers[name] = s
	}
	h.lastAttach[name] = h.opt.Now()
	h.saveRegistryLocked()
	h.mu.Unlock()

	a := &attachment{sink: sink, stop: make(chan struct{})}
	s.mu.Lock()
	old := s.att
	s.att = a
	if s.q.releaseDigests() {
		h.saveQueue(s)
	}
	s.mu.Unlock()
	if old != nil {
		close(old.stop)
		old.sink.Superseded()
	}
	h.wg.Add(1)
	go h.drain(s, a)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return func() { h.detach(s, a) }
}

func (h *Hub) detach(s *subscriber, a *attachment) {
	s.mu.Lock()
	if s.att == a {
		s.att = nil
		close(a.stop)
	}
	s.mu.Unlock()
}

func (h *Hub) drain(s *subscriber, a *attachment) {
	defer h.wg.Done()
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var wait time.Duration
		for {
			s.sendMu.Lock()
			s.mu.Lock()
			if s.att != a {
				s.mu.Unlock()
				s.sendMu.Unlock()
				return
			}
			b, w, ok := s.q.next(h.opt.Now())
			s.mu.Unlock()
			if !ok {
				s.sendMu.Unlock()
				wait = w
				break
			}
			var failed bool
			for _, ev := range b.events {
				if err := a.sink.Deliver(ev); err != nil {
					failed = true
					break
				}
			}
			if failed {
				s.sendMu.Unlock()
				h.detach(s, a)
				return
			}
			s.mu.Lock()
			s.q.remove(b.seqs)
			s.q.Dropped -= b.dropped
			if s.q.Dropped < 0 {
				s.q.Dropped = 0
			}
			h.saveQueue(s)
			s.mu.Unlock()
			s.sendMu.Unlock()
		}
		var tc <-chan time.Time
		if wait > 0 {
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(wait)
			}
			tc = timer.C
		}
		select {
		case <-a.stop:
			return
		case <-s.wake:
		case <-tc:
		}
	}
}

// Prune drops expired queue entries, abandoned subscribers (no attach within
// the TTL — their subscriptions and queue go too) and stale dedup keys, so the
// state files cannot grow forever.
func (h *Hub) Prune(now time.Time) {
	h.mu.Lock()
	var stale []string
	for name := range h.lastAttach {
		if now.Sub(h.lastAttach[name]) > h.opt.SubscriberTTL {
			if s := h.subscribers[name]; s != nil {
				s.mu.Lock()
				live := s.att != nil
				s.mu.Unlock()
				if live {
					continue
				}
			}
			stale = append(stale, name)
		}
	}
	for _, name := range stale {
		kept := h.subs[:0:0]
		for _, s := range h.subs {
			if s.Subscriber != name {
				kept = append(kept, s)
			}
		}
		h.subs = kept
		delete(h.lastAttach, name)
		if s := h.subscribers[name]; s != nil {
			_ = os.Remove(s.path)
			delete(h.subscribers, name)
		}
		h.opt.Logf("channel: pruned abandoned subscriber %q\n", name)
	}
	cutoff := now.Add(-h.opt.MaxAge)
	for _, s := range h.subscribers {
		s.mu.Lock()
		if s.q.pruneOlderThan(cutoff) {
			h.saveQueue(s)
		}
		s.mu.Unlock()
	}
	for k, t := range h.seen {
		if now.Sub(t) > seenTTL {
			delete(h.seen, k)
		}
	}
	h.saveRegistryLocked()
	h.saveStateLocked()
	h.mu.Unlock()
}

// Close stops every drain loop. Queued events stay on disk.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	var atts []*subscriber
	for _, s := range h.subscribers {
		atts = append(atts, s)
	}
	h.mu.Unlock()
	for _, s := range atts {
		s.mu.Lock()
		if s.att != nil {
			close(s.att.stop)
			s.att = nil
		}
		s.mu.Unlock()
	}
	h.wg.Wait()
}

// Subscribers lists known subscriber names (with a subscription or a queue).
func (h *Hub) Subscribers() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := map[string]bool{}
	for _, s := range h.subs {
		set[s.Subscriber] = true
	}
	for n := range h.subscribers {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Queued reports how many events are held for name (tests, status).
func (h *Hub) Queued(name string) int {
	h.mu.Lock()
	s := h.subscribers[name]
	h.mu.Unlock()
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.q.Entries)
}
