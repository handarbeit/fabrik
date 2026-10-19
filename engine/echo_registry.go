package engine

import (
	"context"
	"sync"
	"time"
)

// echoRegistry is the engine-level mutation-echo registry (#2351, ADR 2351;
// amends ADR 042). When Fabrik writes to GitHub it remembers the write so the
// webhook that echoes it back can be recognised as Fabrik's own rather than an
// external change. The registry used to live inside *webhookManager, so it
// worked for one transport only and Hookdeck had no-op stubs. It now belongs to
// the Engine, is wired into the board cache once, and serves every transport.
//
// Two properties differ from the old per-manager registry:
//
//   - Reserve-before-write. A call site reserves the echo BEFORE it issues the
//     write and cancels the reservation if the write fails. A sub-millisecond
//     echo (a fast direct receiver) can therefore never arrive ahead of its
//     registration, and a failed write leaves no orphaned pending entry that a
//     later identical human event could consume.
//   - Registration is unconditional while the registry exists. The engine
//     creates it only when at least one transport is attached, so a poll-only
//     engine still reserves nothing (the old per-manager nil guards).
//     The old "suppress while the gh-forward subprocess is starting up" rule
//     moved to the one consumer of echo misses that cares about it
//     (webhookManager.noteEchoMisses).
//
// All methods are safe on a nil *echoRegistry (a no-op / "no echo"), so call
// sites need no guard.
type echoRegistry struct {
	mu sync.Mutex
	// pending maps the composite key ("eventType:action:key") to its entry.
	pending map[string]echoEntry
	gen     uint64

	// subscribed reports whether any attached transport receives eventType.
	// Nil means "all event types are subscribed".
	subscribed func(eventType string) bool

	// missFn is told about each batch of expired (never-matched) entries, with
	// their registration times. Set once before the sweep starts.
	missFn func(registeredAt []time.Time)

	// nowFn is the clock; time.Now unless a test overrides it.
	nowFn func() time.Time
}

type echoEntry struct {
	at  time.Time
	gen uint64
	via string // overseer requester behind the write (#2125), "" when none
	// prev is the entry this reservation displaced for the same key (nil when
	// none), so a Cancel of the NEWER reservation can put it back: a failed write
	// must not take an earlier, successful write's pending echo with it. A match
	// still consumes the whole key (see MatchEchoVia).
	prev *echoEntry
}

// echoChainMax bounds the displaced-entry chain behind one key.
const echoChainMax = 8

func newEchoRegistry() *echoRegistry {
	return &echoRegistry{pending: make(map[string]echoEntry), nowFn: time.Now}
}

// echoReservation is the handle a call site holds across its write. Cancel is
// safe on a nil reservation and idempotent.
type echoReservation struct {
	r   *echoRegistry
	key string
	gen uint64
}

// Cancel removes the reserved entry when the write it described failed. Only
// this reservation's own entry goes: if a newer reservation of the same key has
// since displaced it, it is unlinked from behind that one; if it is still the
// current entry, the entry it displaced (an earlier write whose echo is still
// expected) is put back. An entry already consumed by a match is already gone.
func (res *echoReservation) Cancel() {
	if res == nil || res.r == nil {
		return
	}
	res.r.mu.Lock()
	defer res.r.mu.Unlock()
	cur, ok := res.r.pending[res.key]
	if !ok {
		return
	}
	if cur.gen == res.gen {
		if cur.prev != nil {
			res.r.pending[res.key] = *cur.prev
		} else {
			delete(res.r.pending, res.key)
		}
		return
	}
	for l := &cur; l.prev != nil; l = l.prev {
		if l.prev.gen == res.gen {
			l.prev = l.prev.prev
			break
		}
	}
	res.r.pending[res.key] = cur
}

// echoKey builds the composite key used in the registry:
// "eventType:action:discriminatingKey".
func echoKey(eventType, action, key string) string {
	return eventType + ":" + action + ":" + key
}

func (r *echoRegistry) now() time.Time {
	if r.nowFn != nil {
		return r.nowFn()
	}
	return time.Now()
}

func (r *echoRegistry) reserve(eventType, action, key, requester string) *echoReservation {
	k := echoKey(eventType, action, key)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	// An engine-autonomous reservation (empty requester) supersedes any overseer
	// requester still pending for the same key, so its echo is never attributed
	// to the overseer.
	e := echoEntry{at: r.now(), gen: r.gen, via: requester}
	if old, ok := r.pending[k]; ok {
		o := old
		e.prev = &o
		// Bound the chain: drop everything beyond echoChainMax links.
		n := 1
		for l := e.prev; l != nil; l = l.prev {
			if n++; n >= echoChainMax {
				l.prev = nil
				break
			}
		}
	}
	r.pending[k] = e
	return &echoReservation{r: r, key: k, gen: r.gen}
}

// Reserve records a pending echo for a write about to be made. Call it before
// the write; Cancel the returned reservation if the write fails.
func (r *echoRegistry) Reserve(eventType, action, key string) *echoReservation {
	if r == nil {
		return nil
	}
	return r.reserve(eventType, action, key, "")
}

// ReserveVia is Reserve that also records the overseer requester behind the
// write, so the matching webhook can be attributed to it (#2125). An empty
// requester is exactly Reserve.
func (r *echoRegistry) ReserveVia(eventType, action, key, requester string) *echoReservation {
	if r == nil {
		return nil
	}
	return r.reserve(eventType, action, key, requester)
}

// ReserveIfSubscribed reserves only when some attached transport receives
// eventType. Used for projects_v2_item (R7), where the event may have been
// dynamically dropped from a gh-forward subscription.
func (r *echoRegistry) ReserveIfSubscribed(eventType, action, key string) *echoReservation {
	return r.ReserveIfSubscribedVia(eventType, action, key, "")
}

// ReserveIfSubscribedVia is ReserveIfSubscribed recording the overseer
// requester behind the write (#2123).
func (r *echoRegistry) ReserveIfSubscribedVia(eventType, action, key, requester string) *echoReservation {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	sub := r.subscribed
	r.mu.Unlock()
	if sub != nil && !sub(eventType) {
		return nil
	}
	return r.reserve(eventType, action, key, requester)
}

// MatchEcho removes the pending echo for an inbound webhook that matched a
// write. Called from CacheImpl.ApplyDelta via the injected matchEchoFn.
func (r *echoRegistry) MatchEcho(eventType, action, key string) {
	r.MatchEchoReported(eventType, action, key)
}

// MatchEchoReported is MatchEcho that also reports whether a pending echo
// existed — i.e. whether the inbound webhook is the echo of a write Fabrik
// made. Channel-event actor attribution (#1968 R3) is its consumer.
func (r *echoRegistry) MatchEchoReported(eventType, action, key string) bool {
	ok, _ := r.MatchEchoVia(eventType, action, key)
	return ok
}

// MatchEchoVia is MatchEchoReported that also returns the overseer requester
// recorded with the reservation ("" when none), consuming the entry (#2125).
func (r *echoRegistry) MatchEchoVia(eventType, action, key string) (bool, string) {
	if r == nil {
		return false, ""
	}
	k := echoKey(eventType, action, key)
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.pending[k]
	delete(r.pending, k)
	return ok, e.via
}

// sweep expires entries older than timeout, reporting each batch to missFn.
// Safe to call from tests directly (no ticker dependency). It returns the
// number of expired entries.
func (r *echoRegistry) sweep(timeout time.Duration) int {
	if r == nil {
		return 0
	}
	now := r.now()
	var expired []time.Time
	r.mu.Lock()
	for k, e := range r.pending {
		if now.Sub(e.at) >= timeout {
			delete(r.pending, k)
			expired = append(expired, e.at)
		}
	}
	fn := r.missFn
	r.mu.Unlock()
	if len(expired) > 0 && fn != nil {
		fn(expired)
	}
	return len(expired)
}

// run is the goroutine that periodically sweeps expired entries.
func (r *echoRegistry) run(ctx context.Context) {
	if r == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep(webhookEchoTimeout)
		}
	}
}

// has reports whether an entry is pending for the key (tests).
func (r *echoRegistry) has(key string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.pending[key]
	return ok
}

// len returns the number of pending entries (tests).
func (r *echoRegistry) len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// setSubscribed installs the "does any transport receive this event?" predicate.
func (r *echoRegistry) setSubscribed(fn func(eventType string) bool) {
	r.mu.Lock()
	r.subscribed = fn
	r.mu.Unlock()
}

// setMissFn installs the expired-entry observer.
func (r *echoRegistry) setMissFn(fn func(registeredAt []time.Time)) {
	r.mu.Lock()
	r.missFn = fn
	r.mu.Unlock()
}
