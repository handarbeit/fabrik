package engine

import (
	"sort"
	"sync/atomic"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Restart delivery and downtime deltas (#2169, ADR 2169).
//
// After a daemon restart a reacting subscriber must receive, in order,
// daemon-reachable (the shim writes it at the attach ack), one catch-up snapshot
// of what needs it, and real deltas for changes made while the daemon was down —
// and no replay of unchanged state.
//
//   - The ledger (channelevents.Ledger, items.json) persists each item's labels
//     and column as last observed. The first observation of an item after start
//     is diffed against it (downtimeCheck) instead of being a silent baseline;
//     an item with no record is still a baseline.
//   - The deltas are held (warmBuf) until the catch-up has been queued. A catch-up
//     taken right after a restart would read a label-less store and miss exactly
//     the settled items the snapshot exists for, so the daemon is "warming" until
//     the board is populated and every open item in a managed stage has its
//     labels, or until channelReadyTimeout. Then checkReady queues the catch-up to
//     every subscriber — attached or not, whatever the shim sent — and publishes
//     the held deltas, so queue order is deterministic.
//
// Observation only (R10): nothing here reads GitHub; item state comes from the
// store's Scan/Peek and the ledger.

// channelReadyTimeout bounds the warming period (real time, a var so tests can
// shorten it): an open item that is never deep-fetched must not postpone the
// catch-up for ever.
var channelReadyTimeout = 3 * time.Minute

// downtimeDisabled neutralises the downtime diff (tests only), so a restart
// behaves as before #2169: every first observation is a silent baseline.
var downtimeDisabled atomic.Bool

// priorEntry is the previous run's record for one item while parts of it still
// await a comparable observation.
type priorEntry struct {
	rec        channelevents.ItemRecord
	statusDone bool
	labelsDone bool
}

// unattributed reports whether a change of this origin was seen only by polling
// (a reconcile, deep fetch or probe), so no known write or webhook backs it.
func unattributed(o itemstate.ChangeOrigin) bool {
	return o == itemstate.OriginOther || o == itemstate.OriginProbe
}

// observeLedger records the item's state as just observed.
//
// An item whose downtime delta is still held back (warmBuf) is not recorded: the
// ledger keeps the previous run's state for it until the delta has been
// published, so a crash or kill while warming cannot swallow the delta (the next
// start diffs it again, at worst delivering it twice, marked unverified).
func (ce *channelEvents) observeLedger(key string, st *itemstate.ItemState) {
	if ce.ledger == nil {
		return
	}
	ce.dmu.Lock()
	held := ce.unpub[key]
	ce.dmu.Unlock()
	if held {
		return
	}
	ce.ledger.Observe(key, channelevents.Observation{
		Labels:      st.Labels,
		LabelsKnown: st.LabelsKnown,
		Status:      st.Status,
		Closed:      st.IsClosed,
	})
}

// downtimeCheck diffs the item's current state against the previous run's
// record, once per part: the column and closed state at the first observation
// that has a column, the labels at the first one that has labels. It returns the
// resulting events (marked source=downtime-delta and unverified, actor unknown:
// a diff names no writer) and whether one of them is a column change or a close,
// so the live path does not report the same move or close again. An item
// with no record is a baseline and yields nothing.
func (ce *channelEvents) downtimeCheck(key string, st *itemstate.ItemState) (evs []channelevents.Event, column, closed bool) {
	if ce.ledger == nil || downtimeDisabled.Load() {
		return nil, false, false
	}
	ce.dmu.Lock()
	defer ce.dmu.Unlock()
	p := ce.pend[key]
	if p == nil {
		if ce.looked[key] {
			return nil, false, false
		}
		ce.looked[key] = true
		rec, ok := ce.ledger.TakePrior(key)
		if !ok {
			return nil, false, false
		}
		p = &priorEntry{rec: rec, labelsDone: !rec.LabelsKnown}
		ce.pend[key] = p
	}
	e := ce.e
	zero := itemstate.Change{Repo: st.Repo, Number: st.Number}
	if !p.statusDone && st.Status != "" {
		p.statusDone = true
		if p.rec.Status != "" && p.rec.Status != st.Status {
			c := zero
			c.PrevStatus = p.rec.Status
			evs = append(evs, e.columnEvent(st, c))
			column = true
		}
		if st.IsClosed && !p.rec.Closed {
			evs = append(evs, ce.closedEvent(key, st, zero))
			closed = true
		}
	}
	if !p.labelsDone && st.LabelsKnown {
		p.labelsDone = true
		now := channelevents.LedgerLabels(st.Labels)
		for _, d := range diffLabelSets(p.rec.Labels, now) {
			evs = append(evs, e.labelEvent(st, zero, d))
			evs = append(evs, e.labelDerivedEvents(st, d)...)
		}
	}
	if p.statusDone && p.labelsDone {
		delete(ce.pend, key)
	}
	if len(evs) > 0 && !ce.ready.Load() {
		ce.unpub[key] = true
	}
	for i := range evs {
		if evs[i].Meta == nil {
			evs[i].Meta = map[string]string{}
		}
		evs[i].Meta[channelevents.MetaSource] = channelevents.SourceDowntimeDelta
		evs[i].Meta[channelevents.MetaUnverified] = "true"
		evs[i].Content += " (changed while the daemon was down)"
	}
	return evs, column, closed
}

// diffLabelSets lists the labels added to and removed from before to reach after
// (both sorted, ledger form).
func diffLabelSets(before, after []string) []itemstate.LabelDelta {
	has := func(set []string, l string) bool {
		i := sort.SearchStrings(set, l)
		return i < len(set) && set[i] == l
	}
	var out []itemstate.LabelDelta
	for _, l := range after {
		if !has(before, l) {
			out = append(out, itemstate.LabelDelta{Label: l, Added: true})
		}
	}
	for _, l := range before {
		if !has(after, l) {
			out = append(out, itemstate.LabelDelta{Label: l, Added: false})
		}
	}
	return out
}

// holdWarm keeps a downtime delta back while the daemon is warming and reports
// whether it did. Past readiness the caller publishes it directly.
func (ce *channelEvents) holdWarm(ev channelevents.Event) bool {
	ce.dmu.Lock()
	defer ce.dmu.Unlock()
	if ce.ready.Load() {
		return false
	}
	ce.warmBuf = append(ce.warmBuf, ev)
	return true
}

// spillWarm publishes any deltas still held back, so a daemon that exits while
// warming does not lose them. They reach the held queues ahead of the next start's catch-up, which is a
// harmless reordering in a rare case. Safe with nothing held.
func (ce *channelEvents) spillWarm() {
	ce.dmu.Lock()
	buf := ce.warmBuf
	ce.warmBuf = nil
	ce.dmu.Unlock()
	for _, ev := range buf {
		ce.hub.Publish(finishEvent(ev))
	}
	ce.releaseUnpub()
}

// releaseUnpub lets the ledger follow the items whose held deltas have now been
// published (or handed to the hub), recording each one's current state.
func (ce *channelEvents) releaseUnpub() {
	ce.dmu.Lock()
	keys := make([]string, 0, len(ce.unpub))
	for k := range ce.unpub {
		keys = append(keys, k)
	}
	ce.unpub = map[string]bool{}
	ce.dmu.Unlock()
	for _, k := range keys {
		repo, n, ok := parseIssueRef(k)
		if !ok {
			continue
		}
		if snap, ok := ce.e.store.Peek(repo, n); ok {
			st := snap.State()
			ce.observeLedger(k, &st)
		}
	}
}

// readyToCatchUp reports whether the store can answer the catch-up: the board is
// populated and every open item in a managed stage has its labels. Items in a
// parking or cleanup column are never deep-fetched, so they do not hold it.
func (ce *channelEvents) readyToCatchUp() bool {
	e := ce.e
	if !e.boardPopulated.Load() {
		return false
	}
	ready := true
	e.store.Scan(func(st *itemstate.ItemState) {
		if !ready || st.IsPR || st.IsClosed || st.LabelsKnown {
			return
		}
		if s := e.stageByName(st.Status); s == nil || s.Unmanaged || s.CleanupWorktree {
			return
		}
		ready = false
	})
	return ready
}

// checkReady runs the readiness pass once: when the store is ready (or the
// deadline passed), queue the restart catch-up to every subscriber, then publish
// the held downtime deltas. Consumer goroutine only.
func (ce *channelEvents) checkReady() {
	if ce.ready.Load() {
		return
	}
	if time.Now().Before(ce.readyBy) && !ce.readyToCatchUp() {
		return
	}
	ce.e.catchUpAll()
	ce.dmu.Lock()
	buf := ce.warmBuf
	ce.warmBuf = nil
	ce.ready.Store(true)
	ce.dmu.Unlock()
	for _, ev := range buf {
		ce.hub.Publish(finishEvent(ev))
	}
	ce.releaseUnpub()
	if len(buf) > 0 {
		ce.e.logf(0, "channel", "restart: %d change(s) made while the daemon was down delivered\n", len(buf))
	}
}

// catchUpAll queues one current-state snapshot to every subscriber that holds a
// subscription, attached or not. The snapshot is built once.
func (e *Engine) catchUpAll() {
	ce := e.channelEvents()
	if ce == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			e.logf(0, "channel", "restart catch-up panic recovered: %v\n", r)
		}
	}()
	evs := e.catchUpEvents()
	for _, name := range ce.hub.SubscriberNames() {
		if n := ce.hub.CatchUp(name, "", evs); n > 0 {
			e.logf(0, "channel", "restart catch-up for %q: %d event(s)\n", name, n)
		}
	}
}
