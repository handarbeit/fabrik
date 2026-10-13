package engine

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Channel events (#1968, ADR-1966-b): the engine side of proactive push.
//
// The deriver turns three kinds of input into channelevents.Events and hands
// them to a channelevents.Hub, which routes, holds and delivers them:
//
//   - itemstate observer changes (label deltas, and "something about this item
//     changed" nudges that schedule a debounced re-evaluation);
//   - explicit, nil-safe emitEvent calls at engine transition points that have
//     no store signal of their own;
//   - a ticker for the time-driven events (stalled, a lazily lifted Claude
//     usage-limit hold).
//
// Zero-GitHub-cost rule (R9) and observation-only rule (R10): nothing here
// reaches GitHub — no e.client/e.readClient, no Store.Get (it has a GitHub
// fallback), no labelAppliedAt live fallback; item state is read through
// Store.Peek/Scan only. TestChannelEventsSourceDoesNotReachGitHub pins that.
// Every hook is a void call that cannot change a return value or a decision,
// and a panic in this code is recovered and logged, never propagated.
//
// Concurrency: observers run on whichever goroutine called Store.Apply, so
// OnChange only appends to a mutex-guarded queue and returns. One consumer
// goroutine drains the queue, owns every piece of per-item memory and is the
// only caller of Hub.Publish, which is what serialises ordering and dedup.

// These are vars so tests can shorten them. The debounce and the ticker are
// real-time mechanisms (wall clock, not the e.now() seam): a debounce that
// followed a frozen test clock would never elapse.
var (
	channelDebounce = 750 * time.Millisecond
	channelTick     = 30 * time.Second
	channelPrune    = time.Hour
)

// itemMemo is the consumer-owned memory for one item.
type itemMemo struct {
	// pauseKey is the last pause-family classification key ("" = none).
	pauseKey string
	// staleDone is true once awaiting-input-stale fired for this awaiting-input episode.
	staleDone bool
	// near[kind] is true while a cycle-limit-near event has fired and the
	// counter has not dropped back below limit-1.
	near map[string]bool
	// lastComment is the comment link the previous event for this item carried.
	lastComment string
	// dp is the open decision-pending episode, if any (channel_decision.go);
	// dpSeen is when the item was first seen suspended without an exact GitHub
	// time (real time), for decisionAnchorGrace.
	dp     *decisionEpisode
	dpSeen time.Time
	// rebaseSeen mirrors dirtyItem.rebaseSeen for the evaluation in progress.
	rebaseSeen bool
}

// dirtyItem is a pending debounced re-evaluation.
type dirtyItem struct {
	due   time.Time
	flags itemstate.ChangeFlags
	// staleCandidate is set when an invocation was recorded while
	// fabrik:awaiting-input was present at that moment.
	staleCandidate bool
	// rebaseSeen is set when fabrik:rebase-needed was applied to the item since
	// the last evaluation, even if it was removed again before this one ran (the
	// engine applies it and dispatches the rebase within one pass), so a
	// withdrawal is attributed to the rebase rather than to the worker it caused.
	rebaseSeen bool
}

type channelEvents struct {
	e   *Engine
	hub *channelevents.Hub

	mu    sync.Mutex
	queue []channelevents.Event
	dirty map[string]*dirtyItem
	// settleCand holds items whose Validate stage reached the engine's own
	// "gates clear" point (runCatchUpPhase2) since the last drain.
	settleCand map[string]settleCandidate
	// nudge asks the consumer to re-check account-wide state (the Claude
	// usage-limit hold) without waiting for the next tick.
	nudge bool
	// arrivals are items created after the board was populated that have not been
	// announced yet (key → when noted); each is held until the item's labels are
	// known and it carries a board Status, so item-added never reports an empty
	// label set (FR-005). landings are items whose landing was announced by
	// emitLanded (#2123). Both guarded by mu; see channel_lifecycle.go.
	arrivals map[string]time.Time
	landings map[string]time.Time

	// consumer-owned (no lock): only run() touches these.
	memo          map[string]*itemMemo
	claudeLimited bool
	// pollEp / partEp are the open pipeline stall episodes (#2124); in memory
	// only, so a still-stalled daemon re-raises after a restart.
	pollEp stallEpisode
	partEp map[string]*stallEpisode
	// episodeSeq numbers pipeline stall episodes for their dedup keys.
	episodeSeq uint64

	// sitEval is when each subscriber's situation cadence last evaluated
	// (#2171); consumer-owned.
	sitEval map[string]time.Time

	// anom is the retry/wedge episode memory and the divergence ledger
	// (#2170); it has its own mutex because hooks and the observer write it.
	anom anomalyState

	// ledger is the persisted last-observed per-item state a restart diffs its
	// first observation against (#2169); nil when it could not be opened.
	ledger *channelevents.Ledger
	// ready is false from start until the readiness pass has queued the restart
	// catch-up (see channel_downtime.go); readyBy is the real-time deadline.
	ready   atomic.Bool
	readyBy time.Time
	// dmu guards the downtime-diff bookkeeping: looked/pend (first-observation
	// state per item) and warmBuf (deltas held back until the catch-up is queued).
	dmu     sync.Mutex
	looked  map[string]bool
	pend    map[string]*priorEntry
	warmBuf []channelevents.Event
	// unpub holds the items with a downtime delta still in warmBuf: the ledger
	// does not follow them until the delta is published, so a hard kill while
	// warming leaves the old record to be diffed again by the next start.
	unpub map[string]bool
	// autonomy is who last wrote each item's fabrik:cruise / fabrik:yolo label,
	// for the cause of a re-settle (also under dmu).
	autonomy map[string]autonomyActor

	wake  chan struct{}
	stop  chan struct{}
	wg    sync.WaitGroup
	unsub func()
}

// channelStateDir is where the hub persists subscriptions, held queues and
// dedup state.
func channelStateDir(fabrikDir string) string {
	return filepath.Join(fabrikDir, ".fabrik", "state", "channel")
}

// startChannelEvents opens the hub, seeds per-item memory from the current
// store (so a (re)start announces nothing about the existing board), subscribes
// the observer and starts the consumer. Failure is non-fatal: without a hub the
// read API keeps working and push is simply off.
func (e *Engine) startChannelEvents() {
	e.startChannelEventsIn(channelStateDir(e.fabrikDir), true)
}

// startChannelEventsIn starts the hub under dir. warming is true in production:
// every daemon start is a restart, so the first catch-up waits for the readiness
// pass (channel_downtime.go) and downtime deltas are held until it has run. The
// test seam StartChannelEventsForTest passes false (ready at once, no restart
// catch-up); StartChannelEventsWarmingForTest passes true.
func (e *Engine) startChannelEventsIn(dir string, warming bool) {
	hub, err := channelevents.Open(channelevents.Options{
		Dir:     dir,
		HeldMax: e.cfg.ChannelHeldMax,
		// Zero keeps the production floor; the test seam shortens it.
		MinSituation: time.Duration(situationMinOverride.Load()),
		Now:          e.now,
		Logf: func(format string, args ...any) {
			e.logf(0, "channel", format, args...)
		},
	})
	if err != nil {
		e.logf(0, "channel", "channel events unavailable (continuing without push): %v\n", err)
		return
	}
	ce := &channelEvents{
		e:        e,
		hub:      hub,
		arrivals: map[string]time.Time{},
		landings: map[string]time.Time{},
		dirty:    map[string]*dirtyItem{},
		memo:     map[string]*itemMemo{},
		looked:   map[string]bool{},
		pend:     map[string]*priorEntry{},
		unpub:    map[string]bool{},
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		readyBy:  time.Now().Add(channelReadyTimeout),
	}
	ce.ledger = channelevents.OpenLedger(dir, e.now, func(format string, args ...any) {
		e.logf(0, "channel", format, args...)
	})
	ce.ready.Store(!warming)
	ce.seed()
	ce.unsub = e.store.Subscribe(itemstate.ObserverFunc(ce.onChange))
	e.channelMu.Lock()
	e.channel = ce
	e.channelMu.Unlock()
	ce.wg.Add(1)
	go ce.run()
}

// flushChannelEvents persists the hub's coalesced dedup and counter state without
// tearing anything down. The self-upgrade exec calls it just before replacing the
// process: that exec runs no deferred cleanup, and the dedup state is written
// lazily, so a settle announced within the flush window would otherwise be
// announced again by the new process. Nothing is closed because a failed exec
// leaves this process running. Safe with no hub, and idempotent.
func (e *Engine) flushChannelEvents() {
	if ce := e.channelEvents(); ce != nil {
		ce.spillWarm()
		ce.ledger.Flush()
		ce.hub.Flush()
	}
}

// closeChannelEvents stops the consumer and closes the hub. Idempotent. Hub
// state is persisted on every write, so nothing needs flushing before an exec.
func (e *Engine) closeChannelEvents() {
	e.channelMu.Lock()
	ce := e.channel
	e.channel = nil
	e.channelMu.Unlock()
	if ce == nil {
		return
	}
	if ce.unsub != nil {
		ce.unsub()
	}
	close(ce.stop)
	ce.wg.Wait()
	ce.spillWarm()
	ce.ledger.Close()
	ce.hub.Close()
}

func (e *Engine) channelEvents() *channelEvents {
	e.channelMu.Lock()
	defer e.channelMu.Unlock()
	return e.channel
}

// emitEvent hands an event to the hub's queue. Nil-safe and non-blocking: with
// no hub (not started, or failed to open) it does nothing, so engine behaviour
// is identical with and without push (R10).
func (e *Engine) emitEvent(ev channelevents.Event) {
	ce := e.channelEvents()
	if ce == nil {
		return
	}
	ce.enqueue(ev)
}

func (ce *channelEvents) enqueue(evs ...channelevents.Event) {
	if len(evs) == 0 {
		return
	}
	ce.mu.Lock()
	ce.queue = append(ce.queue, evs...)
	ce.mu.Unlock()
	ce.signal()
}

// channelNudge asks the deriver to re-check account-wide state. Called from the
// Claude usage-limit hold transition points; a void, non-blocking observation call.
func (e *Engine) channelNudge() {
	ce := e.channelEvents()
	if ce == nil {
		return
	}
	ce.mu.Lock()
	ce.nudge = true
	ce.mu.Unlock()
	ce.signal()
}

func (ce *channelEvents) signal() {
	select {
	case ce.wake <- struct{}{}:
	default:
	}
}

// seed records the current classification of every cached item as the
// baseline, so only transitions after startup produce events.
func (ce *channelEvents) seed() {
	e := ce.e
	now := e.now()
	threshold := e.channelStallThreshold()
	usageLimited := e.channelUsageLimitedUntil(now)
	ce.claudeLimited = !usageLimited.IsZero()
	look := e.scanBlockerIndex()
	var pending []string
	e.store.Scan(func(st *itemstate.ItemState) {
		if st.IsPR {
			return
		}
		// The previous run's persisted state is diffed against this first
		// observation (column and closed state now, labels once they are known)
		// and the ledger starts following the item (#2169).
		ref := issueRef(st.Repo, st.Number)
		if evs, _, _ := ce.downtimeCheck(ref, st); len(evs) > 0 {
			ce.enqueue(evs...)
		}
		ce.observeLedger(ref, st)
		// An item whose labels are not known yet (probe-seeded) has no meaningful
		// classification; its first label-bearing evaluation is the baseline (#2123).
		if !st.LabelsKnown {
			return
		}
		key := ref
		m := ce.memoFor(key)
		res := attention.Classify(e.attentionInput(st, now, threshold, usageLimited, look))
		m.pauseKey = pauseFamilyKey(res)
		ce.armCounters(m, st, true)
		m.staleDone = hasLabelStr(st.Labels, "fabrik:awaiting-input")
		if res.State == attention.Suspended {
			pending = append(pending, st.Repo+"#"+strconv.Itoa(st.Number))
		}
	})
	// A suspended item with no open persisted episode is announced once; the
	// scan's read lock is released first, because the step reads the store again.
	for _, key := range pending {
		repo, n, ok := parseIssueRef(key)
		if !ok {
			continue
		}
		snap, ok := e.store.Peek(repo, n)
		if !ok {
			continue
		}
		st := snap.State()
		res := attention.Classify(e.attentionInput(&st, now, threshold, usageLimited, e.peekBlockerLookup()))
		ce.enqueueDerived(ce.decisionStep(key, ce.memoFor(key), &st, res))
	}
}

func (ce *channelEvents) memoFor(key string) *itemMemo {
	m := ce.memo[key]
	if m == nil {
		m = &itemMemo{near: map[string]bool{}}
		ce.memo[key] = m
	}
	return m
}

// onChange is the itemstate observer. It runs on the Apply caller's goroutine,
// so it only builds label events from the change's own delta and schedules a
// debounced re-evaluation; all heavier work happens on the consumer.
func (ce *channelEvents) onChange(c itemstate.Change, snap itemstate.Snapshot) {
	defer func() {
		if r := recover(); r != nil {
			ce.e.logf(0, "channel", "observer panic recovered: %v\n", r)
		}
	}()
	if c.Repo == "" || c.Number == 0 || c.Fields&itemstate.ItemRemoved != 0 {
		return
	}
	st := snap.State()
	if st.IsPR {
		return
	}
	key := issueRef(c.Repo, c.Number)
	if c.Fields&itemstate.WorkerLifecycleChanged != 0 && st.Worker != nil {
		ce.noteWorkerEntered(key)
	}
	// A reopened issue or a failed landing verification ends the announced
	// landing, so a later close is attributed from its own change, not from a
	// stale mark.
	if c.Fields&itemstate.StateChanged != 0 && !st.IsClosed {
		ce.clearLanding(key)
	}
	// A reconcile, deep fetch or probe that reverts a recent engine write is
	// reported once, ahead of the label events it caused, and those are marked
	// unverified (#2170): the cache and GitHub disagree, so the change may be a
	// phantom.
	// The first observation after a restart is diffed against the previous run's
	// persisted state; what differs is a change made while the daemon was down
	// (#2169). Its column and closed deltas replace the live column-changed and
	// closed for this change, which would otherwise report the same move again.
	dtEvs, dtColumn, dtClosed := ce.downtimeCheck(key, &st)
	ce.observeLedger(key, &st)
	evs := append(dtEvs, ce.noteChangeForDivergence(c, &st, ce.e.now())...)
	diverged := len(evs) // events from here on are built from the suspect change
	rebaseApplied := false
	for _, d := range c.LabelDeltas {
		if d.Added && d.Label == "fabrik:landing-verification-failed" {
			ce.clearLanding(key)
		}
		if d.Added && d.Label == "fabrik:rebase-needed" {
			rebaseApplied = true
		}
		if d.Added && d.Label == "fabrik:revalidate" && ce.ledger != nil {
			// Sent back through Validate on purpose: the next settle says so.
			ce.ledger.MarkRevalidated(key)
		}
		ce.noteAutonomyWrite(key, d, ce.e.labelAttribute(c))
		le := ce.e.labelEvent(&st, c, d)
		if closeTrackingLabel(d.Label) && ce.isLanding(key) {
			le.Meta["landing"] = "true"
		}
		evs = append(evs, le)
		evs = append(evs, ce.e.labelDerivedEvents(&st, d)...)
	}
	if c.Fields&itemstate.StatusChanged != 0 && !c.Created && c.PrevStatus != "" && st.Status != "" && st.Status != c.PrevStatus && !dtColumn {
		evs = append(evs, ce.e.columnEvent(&st, c))
	}
	if c.Closed && !dtClosed {
		evs = append(evs, ce.closedEvent(key, &st, c))
	}
	if diverged > len(dtEvs) || unattributed(c.Origin) {
		// Every event built from a change a cache-divergence was raised for (label
		// events, the label-derived merged / blocker-cleared / landing-verification-
		// failed, column-changed, closed) may be a phantom (#2170 R3). So is one
		// built from a change only a reconcile, deep fetch or probe saw: no known
		// write or webhook backs it (#2169 R3a).
		start := diverged
		if start < len(dtEvs) {
			start = len(dtEvs)
		}
		for i := start; i < len(evs); i++ {
			if evs[i].Meta == nil {
				evs[i].Meta = map[string]string{}
			}
			evs[i].Meta[channelevents.MetaUnverified] = "true"
		}
	}
	staleCand := c.Fields&itemstate.InvocationChanged != 0 && hasLabelStr(st.Labels, "fabrik:awaiting-input")
	const interesting = itemstate.LabelsChanged | itemstate.StageStateChanged | itemstate.StatusChanged |
		itemstate.StateChanged | itemstate.InvocationChanged | itemstate.BlockedByChanged |
		itemstate.LinkedPRChanged | itemstate.WorkerLifecycleChanged
	ce.mu.Lock()
	ce.queue = append(ce.queue, evs...)
	// An item the running store learns about after the board was populated is an
	// arrival; it is announced once its labels and column are known (see
	// announceArrival), so every later change to it re-evaluates it.
	if c.Created && ce.e.boardPopulated.Load() {
		ce.noteArrival(key)
	}
	_, arrivalPending := ce.arrivals[key]
	if c.Fields&interesting != 0 || arrivalPending {
		d := ce.dirty[key]
		if d == nil {
			d = &dirtyItem{due: time.Now().Add(channelDebounce)}
			ce.dirty[key] = d
		}
		d.flags |= c.Fields
		d.staleCandidate = d.staleCandidate || staleCand
		d.rebaseSeen = d.rebaseSeen || rebaseApplied
	}
	ce.mu.Unlock()
	ce.signal()
}

// run is the single consumer goroutine.
func (ce *channelEvents) run() {
	defer ce.wg.Done()
	tick := time.NewTicker(channelTick)
	defer tick.Stop()
	prune := time.NewTicker(channelPrune)
	defer prune.Stop()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		ce.safely("drain", ce.drain)
		wait := ce.nextDue()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-ce.stop:
			return
		case <-ce.wake:
		case <-timer.C:
		case <-tick.C:
			ce.safely("tick", ce.onTick)
		case <-prune.C:
			ce.hub.Prune(ce.e.now())
			ce.pruneLifecycle()
		}
	}
}

func (ce *channelEvents) safely(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			ce.e.logf(0, "channel", "%s panic recovered: %v\n", what, r)
		}
	}()
	fn()
}

func (ce *channelEvents) nextDue() time.Duration {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	if len(ce.queue) > 0 || ce.nudge || len(ce.settleCand) > 0 {
		return 0
	}
	wait := time.Hour
	now := time.Now()
	for _, d := range ce.dirty {
		if w := d.due.Sub(now); w < wait {
			wait = w
		}
	}
	if wait < 0 {
		wait = 0
	}
	return wait
}

// drain publishes queued events in order, then evaluates every item whose
// debounce has elapsed.
func (ce *channelEvents) drain() {
	ce.mu.Lock()
	q := ce.queue
	ce.queue = nil
	nudged := ce.nudge
	ce.nudge = false
	settle := ce.settleCand
	ce.settleCand = nil
	now := time.Now()
	var due []string
	flags := map[string]*dirtyItem{}
	for k, d := range ce.dirty {
		if !d.due.After(now) {
			due = append(due, k)
			flags[k] = d
			delete(ce.dirty, k)
		}
	}
	ce.mu.Unlock()
	for _, ev := range q {
		if ce.holdWarm(ev) {
			continue
		}
		ce.hub.Publish(finishEvent(ev))
	}
	ce.checkReady()
	for _, k := range due {
		ce.evaluateItem(k, flags[k])
	}
	for k, ev := range settle {
		ce.settleCandidate(k, ev)
	}
	if nudged {
		ce.checkClaudeLimit()
	}
	ce.checkAnomalies()
}

// labelAttribution is who caused a label change, as pushed on a label event.
type labelAttribution struct {
	// Actor is fabrik, overseer, human, bot or unknown.
	Actor string
	// Login is the GitHub login of a human or bot actor; empty otherwise.
	Login string
	// Via is the sanitised overseer subscriber behind an overseer actor.
	Via string
}

// labelAttribute attributes a label change (#1968 R3, #2125). In order:
//  1. a requester carried by an engine write or a matched webhook echo: an
//     overseer action, whatever the App-sender check would say;
//  2. the engine's own write, or the webhook echo of one: fabrik;
//  3. a webhook without a sender, and everything first seen by a reconcile,
//     deep fetch or probe (no read the engine already makes names the actor, so
//     none is added; ADR 2125): unknown;
//  4. a sender equal to the App's own login under App auth: fabrik;
//  5. a sender ending in [bot]: bot, with its login;
//  6. any other sender: human, with its login.
func (e *Engine) labelAttribute(c itemstate.Change) labelAttribution {
	switch c.Origin {
	case itemstate.OriginEngine:
		if c.Requester != "" {
			return labelAttribution{Actor: "overseer", Via: c.Requester}
		}
		return labelAttribution{Actor: "fabrik"}
	case itemstate.OriginWebhook:
		switch {
		case c.EchoOfEngine && c.Requester != "":
			return labelAttribution{Actor: "overseer", Via: c.Requester}
		case c.EchoOfEngine:
			return labelAttribution{Actor: "fabrik"}
		case c.Sender == "":
			return labelAttribution{Actor: "unknown"}
		case e.ghAppAuth != nil && strings.EqualFold(c.Sender, e.selfLogin()):
			return labelAttribution{Actor: "fabrik"}
		case strings.HasSuffix(strings.ToLower(c.Sender), "[bot]"):
			return labelAttribution{Actor: "bot", Login: c.Sender}
		default:
			return labelAttribution{Actor: "human", Login: c.Sender}
		}
	}
	return labelAttribution{Actor: "unknown"}
}

// maxPushTitleRunes caps the title a push carries.
const maxPushTitleRunes = 200

// pushTitle renders an issue title for a push: line breaks and backticks become
// spaces (the value is shown inline), runs of whitespace collapse, and a long
// title is cut at maxPushTitleRunes with an ellipsis.
func pushTitle(title string) string {
	t := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r == '`' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, title)), " ")
	if r := []rune(t); len(r) > maxPushTitleRunes {
		t = string(r[:maxPushTitleRunes-1]) + "…"
	}
	return t
}

// baseEvent fills the routing and identity fields shared by every event.
func (e *Engine) baseEvent(st *itemstate.ItemState, typ channelevents.EventType) channelevents.Event {
	ev := channelevents.Event{
		Type:  typ,
		Repo:  st.Repo,
		Issue: st.Number,
		Stage: st.Status,
		At:    e.now(),
		Meta:  map[string]string{},
	}
	if lpr := st.LinkedPR; lpr != nil && lpr.Number != 0 {
		ev.PR = lpr.Number
	}
	// Every item-scoped push says what the item is, so a reacting session does
	// not need a fabrik_status round trip (#2127 R8).
	if t := pushTitle(st.Title); t != "" && !readPolishDisabled(8) {
		ev.Meta["title"] = t
	}
	if st.MilestoneKnown {
		ev.MilestoneKnown = true
		if st.Milestone != nil {
			ev.MilestoneTitle, ev.MilestoneNumber = st.Milestone.Title, st.Milestone.Number
		}
	}
	return ev
}

func (e *Engine) labelEvent(st *itemstate.ItemState, c itemstate.Change, d itemstate.LabelDelta) channelevents.Event {
	typ, verb := channelevents.LabelRemoved, "removed"
	if d.Added {
		typ, verb = channelevents.LabelApplied, "applied"
	}
	attr := e.labelAttribute(c)
	ev := e.baseEvent(st, typ)
	ev.Label = d.Label
	ev.Meta["label"] = d.Label
	ev.Meta["actor"] = attr.Actor
	by := attr.Actor
	if attr.Via != "" {
		ev.Meta["via"] = attr.Via
		by += " " + attr.Via
	}
	if attr.Login != "" {
		ev.Meta["actor_login"] = attr.Login
		by += " " + attr.Login
	}
	ev.Content = fmt.Sprintf("%s %s on %s (by %s)", d.Label, verb, issueRef(st.Repo, st.Number), by)
	if st.Status != "" {
		ev.Content += " at " + st.Status
	}
	return ev
}

// finish stamps the standard meta keys onto ev and returns it. Meta keys are
// identifiers only (Claude Code drops any other key) and values are strings.
func finishEvent(ev channelevents.Event) channelevents.Event {
	if ev.Meta == nil {
		ev.Meta = map[string]string{}
	}
	if ev.Issue != 0 {
		ev.Meta["repo"] = ev.Repo
		ev.Meta["issue"] = strconv.Itoa(ev.Issue)
	}
	ev.Meta["event"] = string(ev.Type)
	if ev.Meta[channelevents.MetaSource] == "" {
		if ev.Meta["catch_up"] == "true" {
			ev.Meta[channelevents.MetaSource] = channelevents.SourceCatchUp
		} else {
			ev.Meta[channelevents.MetaSource] = channelevents.SourceLive
		}
	}
	if ev.Stage != "" {
		ev.Meta["stage"] = ev.Stage
	}
	if ev.PR != 0 {
		ev.Meta["pr"] = strconv.Itoa(ev.PR)
	}
	if ev.CommentURL != "" {
		ev.Meta["comment"] = ev.CommentURL
	}
	return ev
}
