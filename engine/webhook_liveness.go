package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/warnings"
)

// Webhook route liveness (#2452, ADR 2452; supersedes the DNS probe of ADR 2425).
//
// The question an operator cares about is whether GitHub can deliver to the
// receiver: GitHub's resolvers find the name, the route (Tailscale Funnel, a
// proxy) carries it, the receiver acknowledges. GitHub records every attempt in
// the App's delivery log, so the log answers directly, with no DNS of our own.
//
// Passive signal: a recent 2xx attempt to the hook's current URL is reachable; a
// run of failed attempts to it with no later 2xx is unreachable, with GitHub's
// status text as the reason. The list endpoint carries no per-attempt URL, so
// the decisive attempts are checked with a bounded number of detail reads.
//
// Active signal: when the log decides nothing, the controller asks GitHub to
// redeliver a delivery the intake ledger already holds. The copy is deduplicated
// by the intake and never applied; this file must not reference the transport
// sink or Submit (TestWebhookLivenessNeverSubmits). Its arrival at the receiver
// or the new attempt in the log decides liveness.
//
// A failure to read the log, the config or to request a redelivery never means
// unreachable: only attempts GitHub itself recorded as failed do.

// Route liveness constants. Fixed, not configuration; variables so tests can
// shrink them (SetLivenessConstantsForTest).
var (
	// livenessFreshWindow is how recent a 2xx must be to establish reachable.
	livenessFreshWindow = 15 * time.Minute
	// livenessFailRun is the number of consecutive failed attempts that make the
	// route unreachable. Two, not three: probes are rate-limited, and each failure
	// is GitHub's own record.
	livenessFailRun = 2
	// livenessFailWindow is how recent the newest failure of the run must be.
	livenessFailWindow = 30 * time.Minute
	// livenessQuiet is the quiet period after which an active check is made.
	livenessQuiet = 15 * time.Minute
	// livenessMinProbeInterval is the minimum gap between active checks (30 min
	// while unreachable).
	livenessMinProbeInterval = 15 * time.Minute
	// livenessUnreachableProbeInterval is the gap while the route is unreachable.
	livenessUnreachableProbeInterval = 30 * time.Minute
	// livenessRetryDelay is the one early retry after a check that ended without a
	// verdict or with a failed attempt, so a broken route can reach a run of two.
	livenessRetryDelay = 2 * time.Minute
	// livenessProbesPerHour caps redelivery requests in any rolling hour.
	livenessProbesPerHour = 4
	// livenessMaxProbesPerGUID bounds how often one delivery is probed per 24 h.
	livenessMaxProbesPerGUID = 2
	// livenessMaxEligibleAge keeps a probe's delivery well inside the ledger's
	// window: the ledger drops entries at 24 h, and a redelivered copy of a dropped
	// one would be applied to the board.
	livenessMaxEligibleAge = 20 * time.Hour
	// livenessPassiveInterval is how often the log is re-read without a trigger.
	livenessPassiveInterval = 5 * time.Minute
	// livenessSustained is how long an unreachable verdict must last before the
	// warnings panel is told.
	livenessSustained = 10 * time.Minute
	// livenessResultWait / livenessResultPoll bound the wait for a probe's result.
	livenessResultWait = 3 * time.Minute
	livenessResultPoll = 20 * time.Second
	// livenessMaxPages and livenessDetailBudget bound the reads of one evaluation.
	livenessMaxPages     = 2
	livenessDetailBudget = 3
	// livenessLogWindow is how far back the log is read.
	livenessLogWindow = 24 * time.Hour
	// livenessTick is the controller's idle wake-up.
	livenessTick = time.Minute
)

const livenessPanelType = "webhook_route_unreachable"

// livenessAPI is the App-JWT surface the controller needs. *githubauth.Reconciler
// implements it; the detail read is optional (a fake without it cannot verify an
// attempt's URL, so nothing is verified and nothing decides).
type livenessAPI interface {
	ListAppHookDeliveries(cursor string, perPage int) ([]gh.AppHookDelivery, string, error)
	RedeliverAppHookDelivery(deliveryID int64) error
}

type livenessDetailAPI interface {
	FetchAppHookDelivery(deliveryID int64) (gh.AppHookDeliveryDetail, error)
}

// livenessLedger is the read-only slice of the delivery ledger the probe uses.
type livenessLedger interface {
	ledgerReceivedAt(id string, now time.Time) (time.Time, bool)
}

// routeSink receives the decided liveness; the App webhook transport implements it.
type routeSink interface {
	SetRouteLiveness(localapi.RouteLiveness)
}

type routeLivenessConfig struct {
	Tag    string
	API    livenessAPI
	Ledger livenessLedger
	Sink   routeSink
	// HookURL reads the hook's current URL (GET /app/hook/config).
	HookURL func() (string, error)
	// OwnURL is the URL this receiver is served at, "" when not known (the direct
	// transport's public name belongs to an external proxy).
	OwnURL func() string
	Logf   func(format string, args ...any)
	Now    func() time.Time
	// Wait sleeps d, returning false if ctx ended first.
	Wait func(ctx context.Context, d time.Duration) bool
	// Warmup is the span after Start in which failed attempts do not count (the
	// tailscale Funnel needs about this long to route to a new node).
	Warmup time.Duration
	// Report and Resolve are the warnings-panel hooks.
	Report  func(c warnings.Condition)
	Resolve func(key string)
}

type outstandingProbe struct {
	guid    string
	at      time.Time
	arrived chan struct{}
}

// routeLiveness is the controller; one per App webhook transport.
type routeLiveness struct {
	cfg     routeLivenessConfig
	started time.Time
	trigger chan struct{}

	mu          sync.Mutex
	state       string // localapi.RouteReachable / RouteUnreachable / RouteUnknown
	reason      string
	evaluatedAt time.Time
	evidenceAt  time.Time // when the last evidence for the current state was recorded
	unreachAt   time.Time // when the current unreachable verdict began
	panelOn     bool
	lastURL     string
	lastProbeAt time.Time
	retryAt     time.Time
	retryUsed   bool
	probeLog    []time.Time
	perGUID     map[string]*guidProbe
	out         *outstandingProbe
	lastPassive time.Time
	cfgLogged   string
}

type guidProbe struct {
	n  int
	at time.Time
}

func newRouteLiveness(cfg routeLivenessConfig) *routeLiveness {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = realReplayWait
	}
	if cfg.OwnURL == nil {
		cfg.OwnURL = func() string { return "" }
	}
	r := &routeLiveness{
		cfg:     cfg,
		started: cfg.Now(),
		trigger: make(chan struct{}, 1),
		state:   localapi.RouteUnknown,
		perGUID: make(map[string]*guidProbe),
	}
	if cfg.Sink != nil {
		cfg.Sink.SetRouteLiveness(localapi.RouteLiveness{Liveness: localapi.RouteUnknown})
	}
	return r
}

func (r *routeLiveness) log(format string, args ...any) {
	if r.cfg.Logf != nil {
		r.cfg.Logf(format, args...)
	}
}

// Trigger requests an evaluation (a transport reconnect). Never blocks; nil-safe.
func (r *routeLiveness) Trigger() {
	if r == nil {
		return
	}
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Run evaluates at start, on every trigger and whenever a passive re-read or a
// scheduled check is due, until ctx ends.
func (r *routeLiveness) Run(ctx context.Context) {
	r.evaluate(ctx, true)
	for {
		woke := make(chan bool, 1)
		tctx, cancel := context.WithCancel(ctx)
		go func() { woke <- r.cfg.Wait(tctx, livenessTick) }()
		triggered := false
		select {
		case <-ctx.Done():
			cancel()
			return
		case <-r.trigger:
			triggered = true
		case ok := <-woke:
			if !ok {
				cancel()
				return
			}
		}
		cancel()
		r.evaluate(ctx, triggered)
	}
}

// Arrival is called by the transport for every verified, non-ping request before
// it is handed to the intake. It reports whether the request is the copy of an
// outstanding probe redelivery, in which case the transport leaves its
// last-delivery time alone. A request that is not a probe copy is a real
// delivery from GitHub: it proves the route and decides reachable at once.
func (r *routeLiveness) Arrival(guid string) (probe bool) {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if r.out != nil && guid != "" && r.out.guid == guid {
		select {
		case <-r.out.arrived:
		default:
			close(r.out.arrived)
		}
		r.mu.Unlock()
		return true
	}
	r.mu.Unlock()
	r.decide(localapi.RouteReachable, "")
	return false
}

// groupInfo summarises the attempts of one GUID.
type groupInfo struct {
	guid     string
	ping     bool
	original int64 // delivery id of the original (non-redelivery) attempt, else any
	any      int64
}

// logRead is one read of the delivery log.
type logRead struct {
	attempts []gh.AppHookDelivery // newest first, within the window
	groups   []groupInfo          // in first-appearance (newest-first) order
}

func (r *routeLiveness) readLog(now time.Time, pages int) (logRead, error) {
	var out logRead
	bound := now.Add(-livenessLogWindow)
	idx := map[string]int{}
	cursor := ""
	for page := 1; ; page++ {
		items, next, err := r.cfg.API.ListAppHookDeliveries(cursor, 100)
		if err != nil {
			return logRead{}, err
		}
		reached := false
		for _, d := range items {
			if d.DeliveredAt.Before(bound) {
				reached = true
				continue
			}
			out.attempts = append(out.attempts, d)
			if d.GUID == "" {
				continue
			}
			i, ok := idx[d.GUID]
			if !ok {
				idx[d.GUID] = len(out.groups)
				out.groups = append(out.groups, groupInfo{guid: d.GUID})
				i = len(out.groups) - 1
			}
			g := &out.groups[i]
			if d.Event == "ping" {
				g.ping = true
			}
			if g.any == 0 {
				g.any = d.ID
			}
			if !d.Redelivery && g.original == 0 {
				g.original = d.ID
			}
		}
		if reached || next == "" || len(items) == 0 || page >= pages {
			break
		}
		cursor = next
	}
	return out, nil
}

// passiveResult is what the log decides on its own.
type passiveResult struct {
	state  string // RouteReachable, RouteUnreachable or "" (undecided)
	reason string
	at     time.Time // delivered_at of the deciding attempt
}

// classify decides liveness from attempts (newest first) for hookURL. verify
// reports whether an attempt went to the current URL (ok=false: unreadable, the
// attempt counts for nothing). now and floor are the evaluation time and the
// earliest time a failure counts (process start plus warm-up).
func classifyAttempts(attempts []gh.AppHookDelivery, now, floor time.Time, verify func(id int64) (match, ok bool)) passiveResult {
	budget := livenessDetailBudget
	run := 0
	var newestFail *gh.AppHookDelivery
	for i := range attempts {
		d := attempts[i]
		if d.Event == "ping" || d.ThrottledAt != nil || isThrottledText(d.Status) {
			continue
		}
		// A failure that predates the floor (process start plus warm-up) never
		// counts, and neither does anything older once a run has begun.
		if !d.Succeeded() && d.DeliveredAt.Before(floor) {
			continue
		}
		if budget <= 0 {
			break
		}
		budget--
		match, ok := verify(d.ID)
		if !ok || !match {
			continue
		}
		if d.Succeeded() {
			if run > 0 {
				break // a success older than the run: the run is what is newest
			}
			if now.Sub(d.DeliveredAt) <= livenessFreshWindow {
				return passiveResult{state: localapi.RouteReachable, at: d.DeliveredAt}
			}
			return passiveResult{}
		}
		run++
		if newestFail == nil {
			cp := d
			newestFail = &cp
		}
		if run >= livenessFailRun {
			if now.Sub(newestFail.DeliveredAt) <= livenessFailWindow {
				return passiveResult{state: localapi.RouteUnreachable, reason: sanitizeStatusText(newestFail.Status, newestFail.StatusCode), at: newestFail.DeliveredAt}
			}
			return passiveResult{}
		}
	}
	return passiveResult{}
}

func isThrottledText(s string) bool {
	return strings.Contains(strings.ToLower(s), "throttl")
}

// sanitizeStatusText makes GitHub's status text safe for a panel and a log: one
// line, bounded.
func sanitizeStatusText(s string, code int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "'")), " ")
	if s == "" {
		s = fmt.Sprintf("HTTP status %d", code)
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// decide records a liveness verdict from evidence at now.
func (r *routeLiveness) decide(state, reason string) {
	now := r.cfg.Now()
	r.mu.Lock()
	prev, prevReason := r.state, r.reason
	r.state, r.reason = state, reason
	r.evaluatedAt = now
	if state != localapi.RouteUnknown {
		r.evidenceAt = now
	}
	if state == localapi.RouteUnreachable && prev != localapi.RouteUnreachable {
		r.unreachAt = now
	}
	if state != localapi.RouteUnreachable {
		r.unreachAt = time.Time{}
	}
	if state == localapi.RouteReachable {
		r.retryUsed = false
		r.retryAt = time.Time{}
	}
	rl := localapi.RouteLiveness{Liveness: state, Reason: reason, EvaluatedAt: localapi.KnownTime(now)}
	r.mu.Unlock()
	if r.cfg.Sink != nil {
		r.cfg.Sink.SetRouteLiveness(rl)
	}
	if prev != state || prevReason != reason {
		if state == localapi.RouteUnreachable {
			r.log("webhook route %s → %s: %s\n", prev, state, reason)
		} else {
			r.log("webhook route %s → %s\n", prev, state)
		}
	}
	r.syncPanel()
}

// syncPanel raises the warnings-panel entry once an unreachable verdict has been
// held for livenessSustained and clears it whenever the verdict is not unreachable.
func (r *routeLiveness) syncPanel() {
	now := r.cfg.Now()
	r.mu.Lock()
	state, reason, since, on := r.state, r.reason, r.unreachAt, r.panelOn
	want := state == localapi.RouteUnreachable && !since.IsZero() && now.Sub(since) >= livenessSustained
	r.panelOn = want
	r.mu.Unlock()
	key := warnings.Condition{Type: livenessPanelType, Subject: r.cfg.Tag}.Key()
	switch {
	case want && !on:
		if r.cfg.Report != nil {
			r.cfg.Report(livenessCondition(r.cfg.Tag, reason))
		}
	case !want && on:
		if r.cfg.Resolve != nil {
			r.cfg.Resolve(key)
		}
	}
}

func livenessCondition(tag, reason string) warnings.Condition {
	return warnings.Condition{
		Type:    livenessPanelType,
		Subject: tag,
		Title:   fmt.Sprintf("GitHub cannot deliver webhooks to the %s receiver", tag),
		Detail: fmt.Sprintf("GitHub's delivery log shows consecutive failed attempts to the App's webhook URL, and no success since. GitHub's recorded reason: %s.\n\n"+
			"Polling and the periodic reconcile keep the board current, at poll latency. Check that the App's webhook URL is this daemon's public URL, "+
			"that the route to it (for tailscale: Funnel granted to the node's tag, HTTPS certificates enabled; for direct: your proxy) is up, "+
			"and that its public name resolves. The entry clears once GitHub delivers successfully.", reason),
		Fingerprint: livenessPanelType + ":" + tag,
		LogLine:     fmt.Sprintf("GitHub cannot deliver webhooks to the %s receiver: %s", tag, reason),
	}
}

// evaluate runs one passive read and, when it decides nothing and an active
// check is due, one probe.
func (r *routeLiveness) evaluate(ctx context.Context, forced bool) {
	now := r.cfg.Now()
	r.mu.Lock()
	due := forced || r.lastPassive.IsZero() || now.Sub(r.lastPassive) >= livenessPassiveInterval ||
		(!r.retryAt.IsZero() && !now.Before(r.retryAt))
	r.mu.Unlock()
	if !due {
		r.age(now)
		r.syncPanel()
		return
	}

	hookURL, err := r.hookURL()
	if err != nil {
		r.logOnce("could not read the App's webhook configuration (%v) — route liveness unchanged", err)
		r.age(now)
		return
	}
	r.mu.Lock()
	urlChanged := r.lastURL != "" && normalizeWebhookURL(r.lastURL) != normalizeWebhookURL(hookURL)
	r.lastURL = hookURL
	if urlChanged {
		// Evidence for another URL says nothing about this one.
		r.state, r.reason, r.evidenceAt, r.unreachAt = localapi.RouteUnknown, "", time.Time{}, time.Time{}
		r.retryUsed, r.retryAt, r.lastProbeAt = false, time.Time{}, time.Time{}
	}
	r.lastPassive = now
	r.mu.Unlock()
	if urlChanged {
		r.log("the App's webhook URL changed — re-checking the route\n")
		r.decide(localapi.RouteUnknown, "")
		forced = true
	}

	lr, err := r.readLog(now, livenessMaxPages)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		r.logOnce("could not read the App's webhook deliveries (%v) — route liveness unchanged", err)
		r.age(now)
		return
	}
	res := r.classifyLog(lr, hookURL, now)
	if res.state != "" {
		r.decide(res.state, res.reason)
		return
	}
	r.age(now)
	r.maybeProbe(ctx, lr, hookURL, now, forced)
}

// classifyLog verifies attempt URLs through the detail endpoint when available.
func (r *routeLiveness) classifyLog(lr logRead, hookURL string, now time.Time) passiveResult {
	detail, _ := r.cfg.API.(livenessDetailAPI)
	want := normalizeWebhookURL(hookURL)
	verify := func(id int64) (bool, bool) {
		if detail == nil {
			return false, false
		}
		d, err := detail.FetchAppHookDelivery(id)
		if err != nil || d.URL == "" {
			return false, false
		}
		return normalizeWebhookURL(d.URL) == want, true
	}
	r.mu.Lock()
	floor := r.started.Add(r.cfg.Warmup)
	r.mu.Unlock()
	return classifyAttempts(lr.attempts, now, floor, verify)
}

func (r *routeLiveness) hookURL() (string, error) {
	if r.cfg.HookURL == nil {
		return "", errors.New("no hook URL source")
	}
	return r.cfg.HookURL()
}

// logOnce logs a read failure once per distinct message.
func (r *routeLiveness) logOnce(format string, err error) {
	msg := fmt.Sprintf(format, err)
	r.mu.Lock()
	same := r.cfgLogged == msg
	r.cfgLogged = msg
	r.mu.Unlock()
	if !same {
		r.log("%s\n", msg)
	}
}

// age moves a verdict whose evidence has grown too old to unknown: a reachable
// verdict after twice the quiet period, an unreachable one after the log window.
func (r *routeLiveness) age(now time.Time) {
	r.mu.Lock()
	state, ev := r.state, r.evidenceAt
	r.mu.Unlock()
	switch {
	case state == localapi.RouteReachable && !ev.IsZero() && now.Sub(ev) > 2*livenessQuiet:
		r.decide(localapi.RouteUnknown, "")
	case state == localapi.RouteUnreachable && !ev.IsZero() && now.Sub(ev) > livenessLogWindow:
		r.decide(localapi.RouteUnknown, "")
	}
}

// maybeProbe makes an active check when one is due and allowed.
func (r *routeLiveness) maybeProbe(ctx context.Context, lr logRead, hookURL string, now time.Time, forced bool) {
	r.mu.Lock()
	if r.out != nil {
		r.mu.Unlock()
		return
	}
	interval := livenessMinProbeInterval
	if r.state == localapi.RouteUnreachable {
		interval = livenessUnreachableProbeInterval
	}
	quietSince := r.evidenceAt
	if quietSince.IsZero() {
		quietSince = r.started
	}
	retryDue := !r.retryAt.IsZero() && !now.Before(r.retryAt)
	switch {
	case retryDue:
	case r.lastProbeAt.IsZero() && (forced || now.Sub(quietSince) >= livenessQuiet):
	case forced && now.Sub(r.lastProbeAt) >= interval:
	case now.Sub(quietSince) >= livenessQuiet && now.Sub(r.lastProbeAt) >= interval:
	default:
		r.mu.Unlock()
		return
	}
	// Rolling-hour cap.
	cut := 0
	for cut < len(r.probeLog) && now.Sub(r.probeLog[cut]) > time.Hour {
		cut++
	}
	r.probeLog = r.probeLog[cut:]
	if len(r.probeLog) >= livenessProbesPerHour {
		r.mu.Unlock()
		return
	}
	for g, p := range r.perGUID {
		if now.Sub(p.at) > livenessLogWindow {
			delete(r.perGUID, g)
		}
	}
	r.mu.Unlock()

	// Newest eligible delivery: in the ledger, well inside its window, not a ping.
	var pick *groupInfo
	r.mu.Lock()
	for i := range lr.groups {
		g := &lr.groups[i]
		if g.ping {
			continue
		}
		if p := r.perGUID[g.guid]; p != nil && p.n >= livenessMaxProbesPerGUID {
			continue
		}
		at, ok := r.cfg.Ledger.ledgerReceivedAt(g.guid, now)
		if !ok || now.Sub(at) > livenessMaxEligibleAge {
			continue
		}
		pick = g
		break
	}
	r.mu.Unlock()
	if pick == nil {
		r.log("no delivery eligible for a route check — liveness stays unknown until one arrives\n")
		return
	}
	id := pick.original
	if id == 0 {
		id = pick.any
	}

	out := &outstandingProbe{guid: pick.guid, at: now, arrived: make(chan struct{})}
	r.mu.Lock()
	r.out = out
	r.lastProbeAt = now
	r.probeLog = append(r.probeLog, now)
	p := r.perGUID[pick.guid]
	if p == nil {
		p = &guidProbe{}
		r.perGUID[pick.guid] = p
	}
	p.n++
	p.at = now
	r.retryAt = time.Time{}
	r.mu.Unlock()

	if err := r.cfg.API.RedeliverAppHookDelivery(id); err != nil {
		r.mu.Lock()
		r.out = nil
		r.mu.Unlock()
		if ctx.Err() == nil {
			r.logOnce("could not request a route-check redelivery (%v) — route liveness unchanged", err)
		}
		return
	}
	r.log("requested a redelivery of %s to check the webhook route\n", pick.guid)
	r.awaitProbe(ctx, out, hookURL)
}

// awaitProbe waits for the probe's copy to arrive or its attempt to show in the
// log. A timeout or a failed attempt that does not yet make a run schedules one
// early retry; it is never an unreachable verdict by itself.
func (r *routeLiveness) awaitProbe(ctx context.Context, out *outstandingProbe, hookURL string) {
	defer func() {
		r.mu.Lock()
		r.out = nil
		r.mu.Unlock()
	}()
	deadline := out.at.Add(livenessResultWait)
	for {
		select {
		case <-out.arrived:
			if r.arrivalProves(hookURL) {
				r.decide(localapi.RouteReachable, "")
				return
			}
			r.log("the route-check redelivery arrived but the App's webhook URL is not this receiver's — not counting it\n")
			r.scheduleRetry()
			return
		default:
		}
		now := r.cfg.Now()
		if !now.Before(deadline) {
			r.scheduleRetry()
			return
		}
		if !r.cfg.Wait(ctx, livenessResultPoll) {
			return
		}
		select {
		case <-out.arrived:
			continue
		default:
		}
		lr, err := r.readLog(r.cfg.Now(), 1)
		if err != nil {
			continue
		}
		for _, d := range lr.attempts {
			if d.GUID != out.guid || !d.Redelivery || d.DeliveredAt.Before(out.at.Add(-replaySkewMargin)) || d.ThrottledAt != nil {
				continue
			}
			res := r.classifyLog(logRead{attempts: []gh.AppHookDelivery{d}}, hookURL, r.cfg.Now())
			if res.state == localapi.RouteReachable {
				r.decide(localapi.RouteReachable, "")
				return
			}
			// A failed (or foreign-URL) attempt: let the passive rule see the log
			// again, now with this failure in it, after one early retry.
			full, ferr := r.readLog(r.cfg.Now(), livenessMaxPages)
			if ferr == nil {
				if res2 := r.classifyLog(full, hookURL, r.cfg.Now()); res2.state != "" {
					r.decide(res2.state, res2.reason)
					return
				}
			}
			r.scheduleRetry()
			return
		}
	}
}

// arrivalProves reports whether a probe copy arriving at this receiver proves
// the route for the hook's current URL: where the receiver knows its own URL it
// must be the one the App is configured with.
func (r *routeLiveness) arrivalProves(hookURL string) bool {
	own := r.cfg.OwnURL()
	return own == "" || normalizeWebhookURL(own) == normalizeWebhookURL(hookURL)
}

func (r *routeLiveness) scheduleRetry() {
	r.mu.Lock()
	if !r.retryUsed {
		r.retryUsed = true
		r.retryAt = r.cfg.Now().Add(livenessRetryDelay)
	}
	r.mu.Unlock()
}
