package channelevents

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for Options.
const (
	DefaultHeldMax       = 200
	DefaultMaxAge        = 7 * 24 * time.Hour
	DefaultSubscriberTTL = 30 * 24 * time.Hour
	seenMax              = 5000
	seenTTL              = 14 * 24 * time.Hour
	// stateFlushDelay coalesces dedup/counter writes: Publish runs on the
	// engine's poll and tick paths, so it must not rewrite dedup.json per event.
	stateFlushDelay = 250 * time.Millisecond
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
	// MinSituation is the lower situation-cadence bound (default MinSituation);
	// tests shorten it.
	MinSituation time.Duration
	Now          func() time.Time
	Logf         func(format string, args ...any)

	// beforeDeliver, when set (tests only), runs in the drain loop after a batch
	// has been taken and before the epoch recheck that precedes Deliver.
	beforeDeliver func()
}

type registryFile struct {
	Subscriptions []Subscription       `json:"subscriptions"`
	LastAttach    map[string]time.Time `json:"last_attach,omitempty"`
}

type stateFile struct {
	Seen     map[string]time.Time `json:"seen,omitempty"`
	Counters map[string]int       `json:"counters,omitempty"`
	// Reminders holds, per "<subscriber>|<episode key>", when a decision was
	// last announced to that subscriber as a reminder (#2120 R6a).
	Reminders map[string]Reminder `json:"reminders,omitempty"`
	// Situation holds, per subscriber, the fingerprint of the situation digest
	// last sent to it (#2171), so an unchanged view is not re-sent — across a
	// restart too, since the restart catch-up already delivered one.
	Situation map[string]string `json:"situation,omitempty"`
}

// Reminder is the persisted reminder memory for one (subscriber, episode).
type Reminder struct {
	At time.Time `json:"at"`
	N  int       `json:"n"`
}

type attachment struct {
	sink Sink
	stop chan struct{}
}

type subscriber struct {
	name   string
	path   string
	sendMu sync.Mutex // serialises one peek→deliver→remove cycle across attachments
	mu     sync.Mutex // guards q, att and epoch
	q      queueFile
	att    *attachment
	wake   chan struct{}
	// epoch is bumped by every Unsubscribe that removed something. The drain loop
	// records it with each batch and rechecks it immediately before each Deliver,
	// so a batch taken before the unsubscribe is abandoned rather than written.
	epoch uint64
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
	reminders   map[string]Reminder
	situation   map[string]string
	closed      bool

	// Dedup keys and counters are persisted lazily: stateDirty marks unwritten
	// changes, stateTimer is the pending flush, stateWriteMu orders the writes
	// (taken before h.mu, never while holding it) and stateWrites counts them.
	stateDirty   bool
	stateTimer   *time.Timer
	stateWriteMu sync.Mutex
	stateWrites  atomic.Int64

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
	if o.MinSituation <= 0 {
		o.MinSituation = MinSituation
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
		reminders:   map[string]Reminder{},
		situation:   map[string]string{},
	}
	now := o.Now()

	var reg registryFile
	if ok, q, err := loadJSON(h.registryPath(), &reg, now); err != nil {
		return nil, err
	} else if q != "" {
		o.Logf("channel: corrupt subscriptions file quarantined to %s\n", q)
	} else if ok {
		h.subs = reg.Subscriptions
		if migrateSubscriptions(h.subs) {
			o.Logf("channel: migrated legacy event type names in persisted subscriptions\n")
			if err := saveJSON(h.registryPath(), registryFile{Subscriptions: h.subs, LastAttach: reg.LastAttach}); err != nil {
				o.Logf("channel: saving migrated subscriptions: %v\n", err)
			}
		}
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
		for k, v := range st.Reminders {
			h.reminders[k] = v
		}
		for k, v := range st.Situation {
			h.situation[k] = v
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
			if migrateQueue(&q) {
				if err := saveJSON(p, q); err != nil {
					o.Logf("channel: saving migrated held queue: %v\n", err)
				}
			}
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

// saveStateLocked schedules a coalesced write of the dedup state. h.mu held; it
// does no I/O. A crash inside the delay can lose the newest dedup keys (an event
// could then be published once more after restart); Close and the SIGHUP path
// flush synchronously.
func (h *Hub) saveStateLocked() {
	h.stateDirty = true
	if h.stateTimer == nil && !h.closed {
		h.stateTimer = time.AfterFunc(stateFlushDelay, h.flushState)
	}
}

// Flush writes any pending dedup and counter state to disk now. The queues and
// the registry are written on every change; only the dedup state is coalesced.
func (h *Hub) Flush() { h.flushState() }

// flushState writes the dedup state if it changed, without holding h.mu across
// the disk write.
func (h *Hub) flushState() {
	h.stateWriteMu.Lock()
	defer h.stateWriteMu.Unlock()
	h.mu.Lock()
	h.stateTimer = nil
	if !h.stateDirty {
		h.mu.Unlock()
		return
	}
	h.stateDirty = false
	h.trimSeenLocked()
	st := stateFile{Seen: make(map[string]time.Time, len(h.seen)), Counters: make(map[string]int, len(h.counters))}
	for k, v := range h.seen {
		st.Seen[k] = v
	}
	for k, v := range h.counters {
		st.Counters[k] = v
	}
	if len(h.reminders) > 0 {
		st.Reminders = make(map[string]Reminder, len(h.reminders))
		for k, v := range h.reminders {
			st.Reminders[k] = v
		}
	}
	if len(h.situation) > 0 {
		st.Situation = make(map[string]string, len(h.situation))
		for k, v := range h.situation {
			st.Situation[k] = v
		}
	}
	h.mu.Unlock()
	h.stateWrites.Add(1)
	if err := saveJSON(h.statePath(), st); err != nil {
		h.opt.Logf("channel: saving dedup state: %v\n", err)
	}
}

func (h *Hub) trimSeenLocked() {
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
	stored, _, err := h.SubscribeNew(sub)
	return stored, err
}

// SubscribeNew is Subscribe that also reports whether the subscription was newly
// created (false: an identical one already existed).
func (h *Hub) SubscribeNew(sub Subscription) (Subscription, bool, error) {
	if err := sub.ValidateMin(h.opt.MinDigest, h.opt.MinSituation); err != nil {
		return Subscription{}, false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := sub.canonical()
	for _, ex := range h.subs {
		if ex.Subscriber == sub.Subscriber && ex.canonical() == key {
			return ex, false, nil
		}
	}
	sub.ID = newID()
	h.subs = append(h.subs, sub)
	if h.lastAttach[sub.Subscriber].IsZero() {
		h.lastAttach[sub.Subscriber] = h.opt.Now()
	}
	h.saveRegistryLocked()
	return sub, true, nil
}

// CatchUp queues a snapshot of current state for one subscriber: every event of
// evs that one of its subscriptions matches (only the subscription subID when it
// is non-empty), immediately and in order, after anything already held. A
// catch-up carries no dedup key, so it never interferes with the per-episode
// dedup of live events, and an event the queue already holds for the same
// (type, repo, issue) is skipped so a held announcement is not doubled. It
// returns how many events were queued.
//
// A DecisionPending event handed in with the open episode's DedupKey also
// restarts that subscriber's reminder clock (#2120 R6a): the catch-up is a fresh
// announcement of the same decision, so a reminder due "since the original
// announcement" must not fire right behind it. The key is read here and cleared
// before the event is queued, as for every catch-up event.
func (h *Hub) CatchUp(name, subID string, evs []Event) int {
	return h.CatchUpWhere(name, subID, evs, nil)
}

// CatchUpWhere is CatchUp restricted to the subscriptions take accepts (nil
// accepts all). The situation catch-up uses it to give the per-item snapshot only
// to subscriptions that do not take the digest (#2171).
func (h *Hub) CatchUpWhere(name, subID string, evs []Event, take func(Subscription) bool) int {
	now := h.opt.Now()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0
	}
	var subs []Subscription
	for _, sub := range h.subs {
		if sub.Subscriber == name && (subID == "" || sub.ID == subID) && (take == nil || take(sub)) {
			subs = append(subs, sub)
		}
	}
	if len(subs) == 0 {
		h.mu.Unlock()
		return 0
	}
	s := h.subscribers[name]
	if s == nil {
		s = newSubscriber(name, h.heldPath(name), queueFile{})
		h.subscribers[name] = s
	}
	h.mu.Unlock()

	queued := 0
	for _, ev := range evs {
		matched := false
		for _, sub := range subs {
			if sub.Matches(ev) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if ev.At.IsZero() {
			ev.At = now
		}
		if ev.Type == DecisionPending && ev.DedupKey != "" {
			h.restartReminderClock(name, ev, ev.DedupKey, now)
		}
		ev.DedupKey = ""
		s.mu.Lock()
		held := false
		for _, e := range s.q.Entries {
			if e.Event.Type == ev.Type && e.Event.Repo == ev.Repo && e.Event.Issue == ev.Issue {
				held = true
				break
			}
		}
		s.mu.Unlock()
		if held {
			continue
		}
		h.enqueue(s, ev, 0, now)
		queued++
	}
	return queued
}

// RestartReminders restarts name's reminder clock for every DecisionPending event
// in evs that carries its episode's DedupKey, as CatchUp does for the events it
// queues. The situation catch-up announces those decisions inside the digest
// instead of as events, and a reminder due "since the original announcement"
// must not fire right behind it (#2171).
func (h *Hub) RestartReminders(name string, evs []Event) {
	now := h.opt.Now()
	for _, ev := range evs {
		if ev.Type == DecisionPending && ev.DedupKey != "" {
			h.restartReminderClock(name, ev, ev.DedupKey, now)
		}
	}
}

// SubscriberNames lists, sorted, every subscriber that holds at least one
// subscription — attached or not. The restart readiness pass queues its catch-up
// to each of them (#2169).
func (h *Hub) SubscriberNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, sub := range h.subs {
		if !seen[sub.Subscriber] {
			seen[sub.Subscriber] = true
			out = append(out, sub.Subscriber)
		}
	}
	sort.Strings(out)
	return out
}

// restartReminderClock sets the reminder anchor of (name, episode key) to now,
// keeping its count, when one of the subscriber's subscriptions matching ev asks
// for reminders. Nothing is recorded for a subscriber that does not.
func (h *Hub) restartReminderClock(name string, ev Event, dedupKey string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	wants := false
	for _, sub := range h.subs {
		if sub.Subscriber == name && sub.RemindAfterSeconds > 0 && sub.Matches(ev) {
			wants = true
			break
		}
	}
	if !wants {
		return
	}
	key := name + "|" + dedupKey
	rem := h.reminders[key]
	rem.At = now
	h.reminders[key] = rem
	h.saveStateLocked()
}

// Unsubscribe removes one subscription by id, or every subscription of the
// subscriber when id is empty. It is a hard stop: queued entries that no
// remaining subscription matches are discarded (all of them, and the drop
// counters, when the last subscription goes), a batch the drain loop already
// took is abandoned before it is written, and the call does not return until a
// Deliver already in flight has finished, so no delivery for a removed
// subscription completes after it returns. That wait is bounded by the sink's
// own write deadline. The attachment itself stays bound (a re-subscribe under
// the same name resumes delivery on it). It returns how many subscriptions were
// removed.
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
	var sub *subscriber
	var remaining []Subscription
	if removed > 0 {
		sub = h.subscribers[subscriberName]
		remaining = h.subscriptionsLocked(subscriberName)
		h.saveRegistryLocked()
		if !h.takesSituationLocked(subscriberName) {
			// Nothing left to send it to: forget what was last sent, so a later
			// subscription starts from a clean slate.
			if _, ok := h.situation[subscriberName]; ok {
				delete(h.situation, subscriberName)
				h.saveStateLocked()
			}
		}
	}
	h.mu.Unlock()
	if sub == nil {
		return removed
	}
	sub.mu.Lock()
	sub.epoch++
	changed := sub.q.retainMatching(remaining)
	switch {
	case len(remaining) == 0 && sub.att == nil:
		_ = os.Remove(sub.path)
	case len(remaining) == 0 || changed:
		h.saveQueue(sub)
	}
	sub.mu.Unlock()
	// Fence: the drain loop holds sendMu across a whole peek→deliver→remove
	// cycle, so once it is ours any Deliver that was in flight has finished and
	// the next cycle starts from the purged queue and the current subscriptions.
	// No other lock is held while waiting on it.
	sub.sendMu.Lock()
	sub.sendMu.Unlock() //nolint:staticcheck // SA2001: the empty critical section is the fence
	return removed
}

// takesSituationLocked reports whether any of name's subscriptions takes the
// situation digest. h.mu held.
func (h *Hub) takesSituationLocked(name string) bool {
	for _, s := range h.subs {
		if s.Subscriber == name && s.TakesSituation() {
			return true
		}
	}
	return false
}

// PublishSituation delivers a situation digest to one subscriber (#2171). It is
// a separate path from Publish because its dedup is not Publish's: Publish drops
// an event whose DedupKey it has ever seen, which cannot say "A, then B, then A
// again must be sent again". Here the memory is the fingerprint of the digest
// last sent to name.
//
// It does nothing, and reports false, when none of name's subscriptions takes
// situation, or when force is false and fingerprint equals the one last sent.
// Otherwise it replaces any situation digest still held in the queue (so a
// subscriber that is away holds at most one, however many ticks pass), queues ev
// and records the fingerprint. Like every publish it never blocks on a session.
func (h *Hub) PublishSituation(name string, ev Event, fingerprint string, force bool) bool {
	now := h.opt.Now()
	if ev.At.IsZero() {
		ev.At = now
	}
	ev.Type = Situation
	ev.DedupKey = ""
	h.mu.Lock()
	if h.closed || !h.takesSituationLocked(name) {
		h.mu.Unlock()
		return false
	}
	if !force && fingerprint != "" && h.situation[name] == fingerprint {
		h.mu.Unlock()
		return false
	}
	if fingerprint != "" {
		h.situation[name] = fingerprint
		h.saveStateLocked()
	}
	s := h.subscribers[name]
	if s == nil {
		s = newSubscriber(name, h.heldPath(name), queueFile{})
		h.subscribers[name] = s
	}
	h.mu.Unlock()

	s.mu.Lock()
	dropped := s.q.dropType(Situation)
	changed := s.q.push(ev, false, time.Time{}, false, h.opt.HeldMax)
	if changed || dropped {
		h.saveQueue(s)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

// SituationFingerprint returns the fingerprint of the digest last sent to name,
// "" when none was.
func (h *Hub) SituationFingerprint(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.situation[name]
}

// Attached reports whether a session is bound to name right now.
func (h *Hub) Attached(name string) bool {
	h.mu.Lock()
	s := h.subscribers[name]
	h.mu.Unlock()
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.att != nil
}

// subscriptionsLocked lists a subscriber's subscriptions. h.mu held.
func (h *Hub) subscriptionsLocked(name string) []Subscription {
	var out []Subscription
	for _, s := range h.subs {
		if s.Subscriber == name {
			out = append(out, s)
		}
	}
	return out
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

// SetCounter sets and persists a counter.
func (h *Hub) SetCounter(key string, v int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counters[key] = v
	h.saveStateLocked()
}

// BumpCounter increments and persists a counter, returning the new value.
func (h *Hub) BumpCounter(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counters[key]++
	h.saveStateLocked()
	return h.counters[key]
}

// PublishReminders re-announces a still-pending decision as a reminder to every
// attached subscriber with a matching subscription that asks for them
// (RemindAfterSeconds > 0) and is due (#2120 R6a). ev is the current form of the
// original event; dedupKey is that event's DedupKey, the episode identity.
//
// A subscriber is due once its interval has passed since the later of the
// original announcement (the hub's persisted dedup time for dedupKey) and its
// previous reminder (persisted per subscriber). Both anchors are persisted
// times, so a restart neither fires nor postpones a reminder: it never runs off
// daemon uptime. With no announcement time on record the first look only plants
// the anchor. Only attached subscribers are reminded — a detached one is
// caught up on attach — and a reminder is never held. It returns how many
// subscribers were reminded.
func (h *Hub) PublishReminders(ev Event, dedupKey string) int {
	now := h.opt.Now()
	h.mu.Lock()
	if h.closed || dedupKey == "" {
		h.mu.Unlock()
		return 0
	}
	announced, haveAnnounced := h.seen[dedupKey]
	type due struct {
		s *subscriber
		k string
		r Reminder
	}
	var dues []due
	var order []string
	interval := map[string]time.Duration{}
	for _, sub := range h.subs {
		if sub.RemindAfterSeconds <= 0 || !sub.Matches(ev) {
			continue
		}
		d := sub.Remind()
		if cur, ok := interval[sub.Subscriber]; !ok {
			order = append(order, sub.Subscriber)
			interval[sub.Subscriber] = d
		} else if d < cur {
			interval[sub.Subscriber] = d
		}
	}
	for _, name := range order {
		s := h.subscribers[name]
		if s == nil {
			continue
		}
		s.mu.Lock()
		attached := s.att != nil
		s.mu.Unlock()
		key := name + "|" + dedupKey
		rem, have := h.reminders[key]
		anchor := rem.At
		if !have {
			if !haveAnnounced {
				// Nothing on record to measure from: plant the anchor and wait.
				h.reminders[key] = Reminder{At: now}
				h.saveStateLocked()
				continue
			}
			anchor = announced
		}
		if now.Sub(anchor) < interval[name] || !attached {
			continue
		}
		rem.At, rem.N = now, rem.N+1
		h.reminders[key] = rem
		dues = append(dues, due{s: s, k: key, r: rem})
	}
	if len(dues) > 0 {
		h.saveStateLocked()
	}
	h.mu.Unlock()

	for _, d := range dues {
		out := ev
		out.DedupKey = ""
		out.At = now
		out.Meta = make(map[string]string, len(ev.Meta)+2)
		for k, v := range ev.Meta {
			out.Meta[k] = v
		}
		out.Meta["reminder"] = "true"
		out.Meta["reminder_count"] = strconv.Itoa(d.r.N)
		out.Content = "(reminder) " + ev.Content
		h.enqueue(d.s, out, 0, now)
	}
	return len(dues)
}

// RemindersEnabled reports whether any subscription asks for reminders, so the
// caller can skip building them.
func (h *Hub) RemindersEnabled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		if s.RemindAfterSeconds > 0 {
			return true
		}
	}
	return false
}

// ClearReminders forgets the reminder memory of an episode that ended.
func (h *Hub) ClearReminders(dedupKey string) {
	if dedupKey == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	suffix := "|" + dedupKey
	changed := false
	for k := range h.reminders {
		if strings.HasSuffix(k, suffix) {
			delete(h.reminders, k)
			changed = true
		}
	}
	if changed {
		h.saveStateLocked()
	}
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
	changed := s.q.push(ev, digest > 0, due, s.att == nil, h.opt.HeldMax)
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
	// Registered while still holding h.mu, so Close (which sets closed under it)
	// either sees this drain goroutine in its WaitGroup or this Attach saw closed.
	h.wg.Add(1)
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
	go h.drain(s, a)
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		// Close ran between the registration above and s.att being set, so it did
		// not stop this attachment; do it here or Close's wait never returns.
		h.detach(s, a)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return func() { h.detach(s, a) }
}

func (h *Hub) detach(s *subscriber, a *attachment) {
	s.mu.Lock()
	was := s.att == a
	if was {
		s.att = nil
		close(a.stop)
	}
	s.mu.Unlock()
	if was {
		// lastAttach is when the subscriber was last connected, so a session that
		// stayed attached for a long time is not mistaken for an abandoned one the
		// moment it drops (taken after s.mu is released: h.mu is acquired first).
		h.mu.Lock()
		h.lastAttach[s.name] = h.opt.Now()
		h.saveRegistryLocked()
		h.mu.Unlock()
	}
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
			// Current subscriptions, read under h.mu alone (never nested in s.mu).
			// Taken while sendMu is held, so an Unsubscribe that returned before this
			// cycle began is already reflected here.
			h.mu.Lock()
			subs := h.subscriptionsLocked(s.name)
			h.mu.Unlock()
			s.mu.Lock()
			if s.att != a {
				s.mu.Unlock()
				s.sendMu.Unlock()
				return
			}
			// Entries no subscription matches any more (a Publish or CatchUp that
			// resolved its targets before an Unsubscribe and enqueued after it, or a
			// partial removal) are never delivered.
			if s.q.retainMatching(subs) {
				h.saveQueue(s)
			}
			epoch := s.epoch
			b, w, ok := s.q.next(h.opt.Now())
			s.mu.Unlock()
			if !ok {
				s.sendMu.Unlock()
				wait = w
				break
			}
			if h.opt.beforeDeliver != nil {
				h.opt.beforeDeliver()
			}
			var failed, stale bool
			for _, ev := range b.events {
				s.mu.Lock()
				stale = s.epoch != epoch || s.att != a
				s.mu.Unlock()
				if stale {
					break
				}
				if err := a.sink.Deliver(ev); err != nil {
					failed = true
					break
				}
			}
			if stale {
				// An Unsubscribe (or detach) overtook this batch: write nothing, leave
				// the queue as the unsubscribe left it, and start a fresh cycle.
				s.sendMu.Unlock()
				continue
			}
			if failed {
				s.sendMu.Unlock()
				h.detach(s, a)
				return
			}
			s.mu.Lock()
			s.q.remove(b.seqs)
			s.q.clearDropped(b)
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
					// Connected right now: fresh, however long ago it attached.
					h.lastAttach[name] = now
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
		delete(h.situation, name)
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
	for k, r := range h.reminders {
		if now.Sub(r.At) > MaxRemind+seenTTL {
			delete(h.reminders, k)
		}
	}
	h.saveRegistryLocked()
	h.saveStateLocked()
	h.mu.Unlock()
}

// Close stops every drain loop and flushes the dedup state. Queued events stay
// on disk.
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
	var wasAttached []string
	for _, s := range atts {
		s.mu.Lock()
		if s.att != nil {
			close(s.att.stop)
			s.att = nil
			wasAttached = append(wasAttached, s.name)
		}
		s.mu.Unlock()
	}
	h.wg.Wait()
	h.mu.Lock()
	// A session connected at shutdown was connected "now", not at its attach time;
	// without this a daemon restart after SubscriberTTL would prune it on Open.
	if len(wasAttached) > 0 {
		now := h.opt.Now()
		for _, name := range wasAttached {
			h.lastAttach[name] = now
		}
		h.saveRegistryLocked()
	}
	if h.stateTimer != nil {
		h.stateTimer.Stop()
	}
	h.mu.Unlock()
	h.flushState()
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
