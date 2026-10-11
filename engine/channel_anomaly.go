package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Per-item and cache anomaly events (#2170, ADR 2170): retry-starved /
// retry-resumed, item-wedged / item-unwedged and cache-divergence.
//
// The detectors are pure observers (R5, R10). They read the Store only through
// Peek, the poll-liveness verdict through the health mirror and the worker-slot
// counts through len/cap of the semaphore; the facts the Store does not carry (a
// scheduled retry's cause and due time, a gate's holding reason, a deferred
// dispatch) arrive through void hooks in channel_hooks.go, called from the engine
// files that already know them. Nothing here reaches GitHub or changes a return
// value.
//
// Episode state is in memory only, matching pipeline-stalled (#2124): a restarted
// daemon that is still in the condition raises a fresh episode. Every
// announcement carries a dedup key unique to its episode, because the hub
// persists dedup keys and a static key would swallow every later episode.
//
// Locking: anomalyState.mu guards every map. It is taken by hooks (any
// goroutine), by the itemstate observer (the writer's goroutine) and by the
// consumer. Store observers run outside the Store's lock, so the consumer may
// call Peek while holding mu without inverting the order.

// anomalyDisabled neutralises one detector so a test can show it is not vacuous
// (FR-014). Production never sets it.
var anomalyDisabled struct{ retry, wedge, divergence atomic.Bool }

const (
	defaultRetryStarvedThreshold = 5 * time.Minute
	defaultItemWedgedThreshold   = 10 * time.Minute
	defaultCacheDivergenceWindow = 5 * time.Minute
	// wedgeLapseFloor is the least time a hold may go unconfirmed before it is
	// considered over.
	wedgeLapseFloor = 2 * time.Minute
)

// Gate names carried by item-wedged.
const (
	gateFeedback = "feedback"
	gateReview   = "review"
	gateCI       = "ci"
	gateMerge    = "merge"
)

// Reasons a due retry is not running.
const (
	retryReasonNoSlot      = "no worker slot"
	retryReasonPollOverdue = "poll overdue"
	retryReasonUnknown     = "unknown"
)

// retryFact is one scheduled stage retry the engine is waiting to dispatch.
type retryFact struct {
	ref    string
	repo   string
	number int
	stage  string
	cause  string
	due    time.Time
	// deferred: a dispatch of this item was refused for lack of a worker slot
	// after the fact was recorded.
	deferred bool
	// dispatched: a worker entered the item after the fact was recorded.
	dispatched bool
	announced  bool
	openedAt   time.Time
	seq        uint64
	reason     string
	slotsInUse int
	slotsMax   int
}

// wedgeHold is one gate holding one item's advance or landing.
type wedgeHold struct {
	ref           string
	repo          string
	number        int
	gate          string
	reason        string
	ids           []string
	status        string
	firstHeld     time.Time
	lastConfirmed time.Time
	deferred      bool
	dispatched    bool
	cleared       bool
	announced     bool
	seq           uint64
}

// ledgerEntry is one recent engine write the cache could revert.
type ledgerEntry struct {
	// what is "label:<name>" or "column".
	what string
	// added is the written direction for a label write.
	added bool
	// written is the written column for a column write.
	written string
	at      time.Time
}

type anomalyState struct {
	mu      sync.Mutex
	retries map[string]*retryFact
	holds   map[string]*wedgeHold
	ledger  map[string]ledgerEntry
	seq     uint64
	// limitedUntil is the usage-limit hold end last seen; lastLift is when the
	// most recent hold ended. A retry cannot start before the hold lifts, so it is
	// measured from the later of its due time and the lift.
	limitedUntil time.Time
	lastLift     time.Time
}

// ---- thresholds ----

func positiveOr(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// anomalyFloor keeps a configured threshold from falling inside the poll cadence:
// whatever an operator sets, a retry or hold younger than two poll intervals has
// not had a chance to be dispatched (ADR 1831).
func (e *Engine) anomalyFloor() time.Duration { return 2 * e.pollInterval() }

func (e *Engine) retryStarvedThreshold() time.Duration {
	return maxDur(positiveOr(e.cfg.RetryStarvedThreshold, defaultRetryStarvedThreshold), e.anomalyFloor())
}

// itemWedgedThreshold is how long gate may hold an item with no worker before it
// is wedged. The feedback gate's next step is always the engine's own (dispatch a
// reinvoke), so it uses the configured threshold directly. Review, CI and merge
// holds wait on people or machines and already end in their own timeout pauses,
// so they count only once they have outlived that backstop plus two polls.
func (e *Engine) itemWedgedThreshold(gate string) time.Duration {
	base := maxDur(positiveOr(e.cfg.ItemWedgedThreshold, defaultItemWedgedThreshold), e.anomalyFloor())
	var backstop time.Duration
	switch gate {
	case gateReview:
		backstop = 3 * e.cfg.ReviewWaitTimeout
	case gateCI, gateMerge:
		backstop = e.ciBackstopTimeout()
	default:
		return base
	}
	return maxDur(base, backstop+e.anomalyFloor())
}

func (e *Engine) wedgeLapse() time.Duration {
	return maxDur(3*e.pollInterval(), wedgeLapseFloor)
}

func (e *Engine) cacheDivergenceWindow() time.Duration {
	return positiveOr(e.cfg.CacheDivergenceWindow, defaultCacheDivergenceWindow)
}

// ---- hook-side recording (any goroutine) ----

func (a *anomalyState) nextSeq() uint64 {
	a.seq++
	return a.seq
}

func (ce *channelEvents) recordRetry(repo string, number int, stage, cause string, due time.Time) {
	if anomalyDisabled.retry.Load() {
		return
	}
	a := &ce.anom
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.retries == nil {
		a.retries = map[string]*retryFact{}
	}
	ref := issueRef(repo, number)
	if old := a.retries[ref]; old != nil && old.announced && old.dispatched {
		// The previous episode's dispatch has not been consumed yet; keep it so
		// its retry-resumed is not lost, under a key the new fact cannot reuse.
		a.retries[ref+"~"+strconv.FormatUint(old.seq, 10)] = old
	}
	a.retries[ref] = &retryFact{ref: ref, repo: repo, number: number, stage: stage, cause: cause, due: due}
}

func (ce *channelEvents) recordDeferred(refs []string) {
	a := &ce.anom
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ref := range refs {
		if f := a.retries[ref]; f != nil {
			f.deferred = true
		}
		if h := a.holds[ref]; h != nil {
			h.deferred = true
		}
	}
}

func (ce *channelEvents) recordGateHold(repo string, number int, status, gate, reason string, ids []string, now time.Time) {
	if anomalyDisabled.wedge.Load() {
		return
	}
	a := &ce.anom
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.holds == nil {
		a.holds = map[string]*wedgeHold{}
	}
	ref := issueRef(repo, number)
	h := a.holds[ref]
	if h != nil && (h.gate != gate || h.cleared || h.dispatched || h.status != status) {
		// A different gate (or a hold that already ended) is a new episode. The old
		// one, if announced, is closed by the next check: keep it under a side key.
		if h.announced {
			h.cleared = true
			a.holds[ref+"~"+strconv.FormatUint(h.seq, 10)] = h
		}
		h = nil
	}
	if h == nil {
		h = &wedgeHold{ref: ref, repo: repo, number: number, gate: gate, status: status, firstHeld: now}
		a.holds[ref] = h
	}
	h.reason, h.ids, h.lastConfirmed = reason, ids, now
}

func (ce *channelEvents) recordGateClear(repo string, number int, gate string) {
	a := &ce.anom
	a.mu.Lock()
	defer a.mu.Unlock()
	if h := a.holds[issueRef(repo, number)]; h != nil && h.gate == gate {
		h.cleared = true
	}
}

// noteWorkerEntered marks the retry fact and gate hold of an item that has just
// been given a worker. Called from the itemstate observer.
func (ce *channelEvents) noteWorkerEntered(ref string) {
	a := &ce.anom
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range a.retries {
		if f.ref == ref {
			f.dispatched = true
		}
	}
	for _, h := range a.holds {
		if h.ref == ref {
			h.dispatched = true
		}
	}
}

// ---- divergence ledger (observer goroutine) ----

// noteChangeForDivergence records engine writes and reports reverts of them. It
// returns the cache-divergence events for this change, in order, and whether the
// label events derived from the change must be marked unverified.
func (ce *channelEvents) noteChangeForDivergence(c itemstate.Change, st *itemstate.ItemState, now time.Time) []channelevents.Event {
	if anomalyDisabled.divergence.Load() {
		return nil
	}
	e := ce.e
	window := e.cacheDivergenceWindow()
	ref := issueRef(c.Repo, c.Number)
	a := &ce.anom
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ledger == nil {
		a.ledger = map[string]ledgerEntry{}
	}
	for k, en := range a.ledger {
		if now.Sub(en.at) > window {
			delete(a.ledger, k)
		}
	}
	lkey := func(what string) string { return ref + "|" + what }

	switch {
	case c.Origin == itemstate.OriginEngine:
		for _, d := range c.LabelDeltas {
			what := "label:" + d.Label
			a.ledger[lkey(what)] = ledgerEntry{what: what, added: d.Added, at: now}
		}
	case c.Origin == itemstate.OriginWebhook:
		// A webhook is GitHub speaking, not a cache refresh: the write was
		// confirmed or legitimately overridden.
		for _, d := range c.LabelDeltas {
			delete(a.ledger, lkey("label:"+d.Label))
		}
	}
	if c.EngineStatusWrite && c.StatusTo != "" {
		a.ledger[lkey("column")] = ledgerEntry{what: "column", written: c.StatusTo, at: now}
	} else if c.Origin == itemstate.OriginWebhook && c.StatusTo != "" {
		delete(a.ledger, lkey("column"))
	}
	if c.Path == itemstate.PathNone {
		return nil
	}

	var out []channelevents.Event
	for _, d := range c.LabelDeltas {
		what := "label:" + d.Label
		en, ok := a.ledger[lkey(what)]
		if !ok || en.added == d.Added {
			continue
		}
		delete(a.ledger, lkey(what))
		written, observed := "removed", "added"
		if en.added {
			written, observed = "added", "removed"
		}
		out = append(out, ce.divergenceEvent(c, st, what, written, observed, en.at, now, a.nextSeq()))
	}
	if c.StatusTo != "" {
		if en, ok := a.ledger[lkey("column")]; ok && en.written != c.StatusTo {
			delete(a.ledger, lkey("column"))
			out = append(out, ce.divergenceEvent(c, st, "column", en.written, c.StatusTo, en.at, now, a.nextSeq()))
		}
	}
	return out
}

func (ce *channelEvents) divergenceEvent(c itemstate.Change, st *itemstate.ItemState, what, written, observed string, wroteAt, now time.Time, seq uint64) channelevents.Event {
	ref := issueRef(c.Repo, c.Number)
	age := now.Sub(wroteAt)
	path := c.Path.String()
	subject := what
	if strings.HasPrefix(what, "label:") {
		subject = "label " + strings.TrimPrefix(what, "label:")
	}
	ev := ce.e.baseEvent(st, channelevents.CacheDivergence)
	ev.At = now
	ev.DedupKey = fmt.Sprintf("cache-divergence:%s:%s:%s:%d", ref, what, now.UTC().Format(time.RFC3339Nano), seq)
	ev.Content = fmt.Sprintf("%s: the engine wrote %s (%s) %d s ago, but a %s now reports %s; the cache and GitHub disagree, so label events from this change are marked unverified (a column revert may instead be a legitimate external move)",
		ref, subject, written, int64(age.Seconds()), path, observed)
	ev.Meta["what"] = what
	ev.Meta["written"] = written
	ev.Meta["observed"] = observed
	ev.Meta["path"] = path
	ev.Meta["age_seconds"] = strconv.FormatInt(int64(age.Seconds()), 10)
	return ev
}

func mergeMeta(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// ---- consumer-side detection ----

// checkAnomalies evaluates the retry and wedge episodes. Consumer goroutine only;
// it is the only publisher of retry-starved/-resumed and item-wedged/-unwedged.
func (ce *channelEvents) checkAnomalies() {
	e := ce.e
	now := e.now()
	until := e.channelUsageLimitedUntil(now)

	a := &ce.anom
	a.mu.Lock()
	if !until.IsZero() {
		a.limitedUntil = until
	} else if !a.limitedUntil.IsZero() {
		a.lastLift, a.limitedUntil = a.limitedUntil, time.Time{}
	}
	if len(a.retries) == 0 && len(a.holds) == 0 {
		// Nothing to judge: skip the config and health reads entirely, so an idle
		// engine pays nothing for the detectors on every drain.
		a.mu.Unlock()
		return
	}
	pollOverdue := e.pollVerdictFor(e.health.snapshot(), now).Overdue
	var out []channelevents.Event
	if !anomalyDisabled.retry.Load() {
		out = append(out, ce.checkRetriesLocked(now, pollOverdue, !until.IsZero())...)
	}
	if !anomalyDisabled.wedge.Load() {
		out = append(out, ce.checkWedgesLocked(now, pollOverdue)...)
	}
	a.mu.Unlock()
	ce.enqueueDerived(out)
}

// itemBlockedFromRunning reports a state in which an item is deliberately not
// being dispatched; it already has its own push (paused, awaiting-input, ...) and
// must not also be reported as starved or wedged.
func itemBlockedFromRunning(st *itemstate.ItemState) bool {
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input", "fabrik:blocked", "fabrik:awaiting-done"} {
		if hasLabelStr(st.Labels, l) {
			return true
		}
	}
	return false
}

func (ce *channelEvents) checkRetriesLocked(now time.Time, pollOverdue, usageLimited bool) []channelevents.Event {
	e := ce.e
	a := &ce.anom
	if len(a.retries) == 0 {
		return nil
	}
	threshold := e.retryStarvedThreshold()
	var out []channelevents.Event
	keys := make([]string, 0, len(a.retries))
	for k := range a.retries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f := a.retries[k]
		if f.dispatched {
			if f.announced {
				out = append(out, ce.retryResumedEvent(f, now))
			}
			delete(a.retries, k)
			continue
		}
		snap, ok := e.store.Peek(f.repo, f.number)
		if !ok {
			delete(a.retries, k)
			continue
		}
		st := snap.State()
		if st.IsClosed || st.Status != f.stage || hasLabelStr(st.Labels, "stage:"+f.stage+":complete") || itemBlockedFromRunning(&st) {
			delete(a.retries, k)
			continue
		}
		if f.announced || usageLimited {
			continue
		}
		anchor := f.due
		if a.lastLift.After(anchor) {
			anchor = a.lastLift
		}
		if now.Sub(anchor) < threshold {
			continue
		}
		inUse, capacity := len(e.sem), cap(e.sem)
		switch {
		case f.deferred || (capacity > 0 && inUse >= capacity):
			f.reason = retryReasonNoSlot
		case pollOverdue:
			f.reason = retryReasonPollOverdue
		default:
			f.reason = retryReasonUnknown
		}
		f.slotsInUse, f.slotsMax = inUse, capacity
		f.announced, f.openedAt, f.seq = true, now, a.nextSeq()
		out = append(out, ce.retryStarvedEvent(f, &st, now, anchor))
	}
	return out
}

func retryKey(prefix string, f *retryFact) string {
	return fmt.Sprintf("%s:%s:%s:%d", prefix, f.ref, f.due.UTC().Format(time.RFC3339Nano), f.seq)
}

func (ce *channelEvents) retryStarvedEvent(f *retryFact, st *itemstate.ItemState, now, anchor time.Time) channelevents.Event {
	ev := ce.e.baseEvent(st, channelevents.RetryStarved)
	ev.At = now
	ev.DedupKey = retryKey("retry-starved", f)
	ev.Meta["cause"] = f.cause
	ev.Meta["due_at"] = f.due.UTC().Format(time.RFC3339)
	ev.Meta["starved_seconds"] = strconv.FormatInt(int64(now.Sub(anchor).Seconds()), 10)
	ev.Meta["reason"] = f.reason
	why := f.reason
	if f.reason == retryReasonNoSlot {
		ev.Meta["slots_in_use"] = strconv.Itoa(f.slotsInUse)
		ev.Meta["slots_max"] = strconv.Itoa(f.slotsMax)
		why = fmt.Sprintf("%s (%d/%d in use)", f.reason, f.slotsInUse, f.slotsMax)
	}
	ev.Content = fmt.Sprintf("%s: the %s retry after %s was due at %s and has not been dispatched for %d s: %s",
		f.ref, f.stage, f.cause, f.due.UTC().Format(time.RFC3339), int64(now.Sub(anchor).Seconds()), why)
	return ev
}

func (ce *channelEvents) retryResumedEvent(f *retryFact, now time.Time) channelevents.Event {
	ev := channelevents.Event{
		Type:     channelevents.RetryResumed,
		Repo:     f.repo,
		Issue:    f.number,
		Stage:    f.stage,
		At:       now,
		DedupKey: retryKey("retry-resumed", f),
		Content:  fmt.Sprintf("%s: the starved %s retry (after %s) was dispatched, %d s after the episode began", f.ref, f.stage, f.cause, int64(now.Sub(f.openedAt).Seconds())),
		Meta: map[string]string{
			"cause":           f.cause,
			"starved_seconds": strconv.FormatInt(int64(now.Sub(f.openedAt).Seconds()), 10),
		},
	}
	if snap, ok := ce.e.store.Peek(f.repo, f.number); ok {
		st := snap.State()
		base := ce.e.baseEvent(&st, channelevents.RetryResumed)
		base.Stage = f.stage
		base.Content, base.Meta, base.DedupKey = ev.Content, mergeMeta(base.Meta, ev.Meta), ev.DedupKey
		return base
	}
	return ev
}

func (ce *channelEvents) checkWedgesLocked(now time.Time, pollOverdue bool) []channelevents.Event {
	e := ce.e
	a := &ce.anom
	if len(a.holds) == 0 {
		return nil
	}
	lapse := e.wedgeLapse()
	var out []channelevents.Event
	keys := make([]string, 0, len(a.holds))
	for k := range a.holds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h := a.holds[k]
		end := func(how string) {
			if h.announced {
				out = append(out, ce.itemUnwedgedEvent(h, how, now))
			}
			delete(a.holds, k)
		}
		if h.dispatched {
			end("dispatched")
			continue
		}
		snap, ok := e.store.Peek(h.repo, h.number)
		if !ok {
			delete(a.holds, k)
			continue
		}
		st := snap.State()
		switch {
		case st.IsClosed || (h.status != "" && st.Status != h.status):
			end("advanced")
			continue
		case h.cleared:
			end("cleared")
			continue
		}
		if pollOverdue {
			// The poll is the reason nothing re-confirmed or dispatched; pipeline-stalled
			// already says so. Keep the hold alive rather than lapsing it.
			h.lastConfirmed = now
			continue
		}
		if now.Sub(h.lastConfirmed) > lapse {
			end("lapsed")
			continue
		}
		if h.announced || h.deferred || st.Worker != nil || itemBlockedFromRunning(&st) {
			continue
		}
		if now.Sub(h.firstHeld) < e.itemWedgedThreshold(h.gate) {
			continue
		}
		h.announced, h.seq = true, a.nextSeq()
		out = append(out, ce.itemWedgedEvent(h, &st, now))
	}
	return out
}

func wedgeKey(prefix string, h *wedgeHold) string {
	return fmt.Sprintf("%s:%s:%s:%s:%d", prefix, h.ref, h.gate, h.firstHeld.UTC().Format(time.RFC3339Nano), h.seq)
}

func (ce *channelEvents) itemWedgedEvent(h *wedgeHold, st *itemstate.ItemState, now time.Time) channelevents.Event {
	ev := ce.e.baseEvent(st, channelevents.ItemWedged)
	ev.At = now
	ev.DedupKey = wedgeKey("item-wedged", h)
	held := int64(now.Sub(h.firstHeld).Seconds())
	ev.Meta["gate"] = h.gate
	ev.Meta["reason"] = h.reason
	ev.Meta["held_seconds"] = strconv.FormatInt(held, 10)
	if len(h.ids) > 0 {
		ev.Meta["feedback_ids"] = strings.Join(h.ids, ",")
	}
	ev.Content = fmt.Sprintf("%s: the %s gate has held its advance or landing for %d s with no worker dispatched or queued: %s",
		h.ref, h.gate, held, h.reason)
	return ev
}

func (ce *channelEvents) itemUnwedgedEvent(h *wedgeHold, how string, now time.Time) channelevents.Event {
	held := int64(now.Sub(h.firstHeld).Seconds())
	ev := channelevents.Event{
		Type:     channelevents.ItemUnwedged,
		Repo:     h.repo,
		Issue:    h.number,
		At:       now,
		DedupKey: wedgeKey("item-unwedged", h),
		Content:  fmt.Sprintf("%s: the %s gate hold is over (%s) after %d s", h.ref, h.gate, how, held),
		Meta: map[string]string{
			"gate":         h.gate,
			"how":          how,
			"held_seconds": strconv.FormatInt(held, 10),
		},
	}
	if snap, ok := ce.e.store.Peek(h.repo, h.number); ok {
		st := snap.State()
		base := ce.e.baseEvent(&st, channelevents.ItemUnwedged)
		base.Content, base.Meta, base.DedupKey = ev.Content, mergeMeta(base.Meta, ev.Meta), ev.DedupKey
		return base
	}
	return ev
}

// anomalyCatchUpEvents lists the retry-starved and item-wedged episodes open now
// as catch-up snapshots for a new subscriber: current state, no dedup key and no
// episode memory touched. cache-divergence is a point event and has none. Safe
// from any goroutine.
func (e *Engine) anomalyCatchUpEvents(now time.Time) []channelevents.Event {
	ce := e.channelEvents()
	if ce == nil {
		return nil
	}
	a := &ce.anom
	a.mu.Lock()
	var retries []retryFact
	for _, f := range a.retries {
		if f.announced && !f.dispatched {
			retries = append(retries, *f)
		}
	}
	var holds []wedgeHold
	for _, h := range a.holds {
		if h.announced && !h.dispatched && !h.cleared {
			holds = append(holds, *h)
		}
	}
	a.mu.Unlock()
	sort.Slice(retries, func(i, j int) bool { return retries[i].ref < retries[j].ref })
	sort.Slice(holds, func(i, j int) bool { return holds[i].ref < holds[j].ref })

	var out []channelevents.Event
	for i := range retries {
		f := &retries[i]
		snap, ok := e.store.Peek(f.repo, f.number)
		if !ok {
			continue
		}
		st := snap.State()
		anchor := f.due
		ev := ce.retryStarvedEvent(f, &st, now, anchor)
		out = append(out, catchUpMark(ev))
	}
	for i := range holds {
		h := &holds[i]
		snap, ok := e.store.Peek(h.repo, h.number)
		if !ok {
			continue
		}
		st := snap.State()
		out = append(out, catchUpMark(ce.itemWedgedEvent(h, &st, now)))
	}
	return out
}

func catchUpMark(ev channelevents.Event) channelevents.Event {
	ev.DedupKey = ""
	ev.Content = "(catch-up) " + ev.Content
	ev.Meta["catch_up"] = "true"
	return finishEvent(ev)
}

// maxDur is the larger of two durations (the package's max is int-only).
func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
