package engine

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
//     suspension).
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
}

// dirtyItem is a pending debounced re-evaluation.
type dirtyItem struct {
	due   time.Time
	flags itemstate.ChangeFlags
	// staleCandidate is set when an invocation was recorded while
	// fabrik:awaiting-input was present at that moment.
	staleCandidate bool
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
	// usage-limit suspension) without waiting for the next tick.
	nudge bool

	// consumer-owned (no lock): only run() touches these.
	memo          map[string]*itemMemo
	claudeLimited bool

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
	e.startChannelEventsIn(channelStateDir(e.fabrikDir))
}

func (e *Engine) startChannelEventsIn(dir string) {
	hub, err := channelevents.Open(channelevents.Options{
		Dir:     dir,
		HeldMax: e.cfg.ChannelHeldMax,
		Now:     e.now,
		Logf: func(format string, args ...any) {
			e.logf(0, "channel", format, args...)
		},
	})
	if err != nil {
		e.logf(0, "channel", "channel events unavailable (continuing without push): %v\n", err)
		return
	}
	ce := &channelEvents{
		e:     e,
		hub:   hub,
		dirty: map[string]*dirtyItem{},
		memo:  map[string]*itemMemo{},
		wake:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
	}
	ce.seed()
	ce.unsub = e.store.Subscribe(itemstate.ObserverFunc(ce.onChange))
	e.channelMu.Lock()
	e.channel = ce
	e.channelMu.Unlock()
	ce.wg.Add(1)
	go ce.run()
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
// Claude suspension transition points; a void, non-blocking observation call.
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
	suspended := e.channelSuspendedUntil(now)
	ce.claudeLimited = !suspended.IsZero()
	e.store.Scan(func(st *itemstate.ItemState) {
		if st.IsPR {
			return
		}
		key := issueRef(st.Repo, st.Number)
		m := ce.memoFor(key)
		res := attention.Classify(e.attentionInput(st, now, threshold, suspended))
		m.pauseKey = pauseFamilyKey(res)
		ce.armCounters(m, st, true)
		m.staleDone = hasLabelStr(st.Labels, "fabrik:awaiting-input")
	})
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
	var evs []channelevents.Event
	for _, d := range c.LabelDeltas {
		evs = append(evs, ce.e.labelEvent(&st, c, d))
	}
	staleCand := c.Fields&itemstate.InvocationChanged != 0 && hasLabelStr(st.Labels, "fabrik:awaiting-input")
	const interesting = itemstate.LabelsChanged | itemstate.StageStateChanged | itemstate.StatusChanged |
		itemstate.StateChanged | itemstate.InvocationChanged | itemstate.BlockedByChanged |
		itemstate.LinkedPRChanged | itemstate.WorkerLifecycleChanged
	key := issueRef(c.Repo, c.Number)
	ce.mu.Lock()
	ce.queue = append(ce.queue, evs...)
	if c.Fields&interesting != 0 {
		d := ce.dirty[key]
		if d == nil {
			d = &dirtyItem{due: time.Now().Add(channelDebounce)}
			ce.dirty[key] = d
		}
		d.flags |= c.Fields
		d.staleCandidate = d.staleCandidate || staleCand
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
		ce.hub.Publish(finishEvent(ev))
	}
	for _, k := range due {
		ce.evaluateItem(k, flags[k])
	}
	for k, ev := range settle {
		ce.settleCandidate(k, ev)
	}
	if nudged {
		ce.checkClaudeLimit()
	}
}

// actorOf attributes a label change: fabrik (the engine's own write, or the
// webhook echo of one), human, bot (another bot), or unknown (a reconcile or
// deep fetch saw it first and no webhook told us who did it).
func (e *Engine) labelActor(c itemstate.Change) string {
	switch c.Origin {
	case itemstate.OriginEngine:
		return "fabrik"
	case itemstate.OriginWebhook:
		switch {
		case c.EchoOfEngine:
			return "fabrik"
		case c.Sender == "":
			return "unknown"
		case e.ghAppAuth != nil && strings.EqualFold(c.Sender, e.selfLogin()):
			return "fabrik"
		case strings.HasSuffix(strings.ToLower(c.Sender), "[bot]"):
			return "bot"
		default:
			return "human"
		}
	}
	return "unknown"
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
	actor := e.labelActor(c)
	ev := e.baseEvent(st, typ)
	ev.Label = d.Label
	ev.Meta["label"] = d.Label
	ev.Meta["actor"] = actor
	ev.Content = fmt.Sprintf("%s %s on %s (by %s)", d.Label, verb, issueRef(st.Repo, st.Number), actor)
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
