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
)

// Delivery replay (#2144, ADR 2144).
//
// A direct App webhook receiver has no broker in front of it, so a delivery
// sent while the daemon is down is lost: GitHub does not retry it. GitHub does
// keep the App's delivery log and can redeliver on request, so a replay pass
// lists the log, picks the deliveries that failed and that the engine never
// received, and asks GitHub to redeliver them.
//
// A pass NEVER applies anything itself. Redelivered copies come back through
// the transport's normal HTTP handler and intake.Submit, which dedupes them on
// X-GitHub-Delivery (FR-008); this file must not reference the transport sink
// or Submit (TestDeliveryReplayNeverSubmits). The ledger is only asked
// "have you seen this?" through the read-only Seen: Accept would record the
// ID and make the redelivery itself look like a duplicate.
//
// Caps are fixed, not configurable. They are variables only so tests can
// shrink them.
var (
	replayPerPage          = 100
	replayMaxPages         = 10
	replayMaxRedeliveries  = 50
	replayPace             = 250 * time.Millisecond
	replayAbortAfterFailed = 3
	// replaySkewMargin widens the lookback below the newest ledger entry, which
	// is stamped with the local clock while delivered_at is GitHub's.
	replaySkewMargin = 5 * time.Minute
	// replayEmptyLedgerLookback is the lookback when the ledger holds nothing.
	replayEmptyLedgerLookback = time.Hour
	// replayFollowUpDelay is how long after a pass that requested redeliveries the
	// one follow-up pass runs. GitHub accepts a redelivery request (202) before it
	// attempts the delivery, so a pass cannot see the outcome of what it asked for;
	// the follow-up reads the log again and re-requests a redelivery that itself
	// failed (once).
	replayFollowUpDelay = 3 * time.Minute
)

// replayMaxRequestsPerGUID bounds how often one GUID is requested in this
// process: the first request, plus one more when that redelivery itself failed.
// A dead endpoint therefore cannot loop.
const replayMaxRequestsPerGUID = 2

// replayRequest records the requests made for one GUID.
type replayRequest struct {
	at time.Time // when the newest request was made
	n  int       // requests made so far
}

// hookDeliveryAPI is the App-JWT surface a pass needs. *githubauth.Reconciler
// implements it.
type hookDeliveryAPI interface {
	ListAppHookDeliveries(cursor string, perPage int) ([]gh.AppHookDelivery, string, error)
	RedeliverAppHookDelivery(deliveryID int64) error
}

// replayLedger is the read-only slice of the delivery ledger a pass uses.
type replayLedger interface {
	ledgerSeen(id string) bool
	ledgerNewest() time.Time
}

// replayStatusSink receives the summary of the last pass; the App webhook
// transport (direct and tailscale) implements it so its health row reports it. Calling it with a zero value
// before the first pass marks replay as enabled for the row.
type replayStatusSink interface {
	SetReplayStatus(localapi.ReplayHealth)
}

type deliveryReplayer struct {
	api    hookDeliveryAPI
	ledger replayLedger
	status replayStatusSink
	logf   func(format string, args ...any)
	now    func() time.Time

	trigger       chan struct{}
	followTrigger chan struct{}

	// wait sleeps for d and reports false if ctx ended first. A field so tests
	// (and the engine's seam) control the clock of the follow-up and start grace.
	wait func(ctx context.Context, d time.Duration) bool

	// startNewest is the ledger's newest receipt time, snapshotted when the
	// replayer is built — before the listener accepts anything. The startup
	// pass uses it instead of the live value: GitHub resumes live deliveries as
	// soon as the daemon is back, and one accepted before the pass reads the
	// ledger would move "newest" to about now and collapse the lookback past the
	// whole outage. A zero snapshot means the ledger was empty at start.
	startNewest time.Time

	mu            sync.Mutex
	startDone     bool                      // the startup snapshot has been consumed by a pass
	requested     map[string]*replayRequest // guid -> requests made in this process
	lastBound     time.Time                 // lookback bound of the last full pass; follow-up passes reuse it
	followPending bool                      // a follow-up pass is already scheduled
}

// realReplayWait sleeps for d, returning false if ctx ends first.
func realReplayWait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func newDeliveryReplayer(api hookDeliveryAPI, ledger replayLedger, status replayStatusSink, logf func(format string, args ...any), now func() time.Time) *deliveryReplayer {
	if now == nil {
		now = time.Now
	}
	r := &deliveryReplayer{
		api:           api,
		ledger:        ledger,
		status:        status,
		logf:          logf,
		now:           now,
		trigger:       make(chan struct{}, 1),
		followTrigger: make(chan struct{}, 1),
		wait:          realReplayWait,
		requested:     make(map[string]*replayRequest),
	}
	if ledger != nil {
		r.startNewest = ledger.ledgerNewest()
	}
	if status != nil {
		status.SetReplayStatus(localapi.ReplayHealth{})
	}
	return r
}

// Trigger requests a pass. It never blocks; triggers arriving while one is
// pending or running coalesce into a single further pass.
func (r *deliveryReplayer) Trigger() {
	if r == nil {
		return
	}
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Run executes passes as they are triggered until ctx is cancelled.
func (r *deliveryReplayer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.trigger:
			if r.passMode(ctx, false) > 0 {
				r.armFollowUp(ctx)
			}
		case <-r.followTrigger:
			// A follow-up never schedules another: what it re-requests is bounded by
			// replayMaxRequestsPerGUID.
			r.passMode(ctx, true)
		}
	}
}

// armFollowUp schedules one follow-up pass replayFollowUpDelay from now, unless
// one is already scheduled.
func (r *deliveryReplayer) armFollowUp(ctx context.Context) {
	r.mu.Lock()
	if r.followPending {
		r.mu.Unlock()
		return
	}
	r.followPending = true
	r.mu.Unlock()
	go func() {
		ok := r.wait(ctx, replayFollowUpDelay)
		r.mu.Lock()
		r.followPending = false
		r.mu.Unlock()
		if !ok {
			return
		}
		select {
		case r.followTrigger <- struct{}{}:
		default:
		}
	}()
}

func (r *deliveryReplayer) log(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}

// lowerBound is the oldest delivered_at a pass considers.
// The first pass reads the startup snapshot; later passes (reconnects) read the
// live ledger.
func (r *deliveryReplayer) lowerBound(now time.Time) time.Time {
	r.mu.Lock()
	first := !r.startDone
	r.startDone = true
	r.mu.Unlock()
	var newest time.Time
	if first {
		newest = r.startNewest
	} else {
		newest = r.ledger.ledgerNewest()
	}
	if newest.IsZero() {
		return now.Add(-replayEmptyLedgerLookback)
	}
	bound := newest.Add(-replaySkewMargin)
	if floor := now.Add(-deliveryLedgerWindow); bound.Before(floor) {
		bound = floor
	}
	return bound
}

// replayCandidate is one GUID worth asking GitHub to redeliver.
type replayCandidate struct {
	guid string
	id   int64 // delivery id of the newest failed attempt
}

// pass runs one full pass; it returns nothing to the caller that does not care
// how many redeliveries were requested.
func (r *deliveryReplayer) pass(ctx context.Context) { r.passMode(ctx, false) }

// passMode runs one pass and returns how many redeliveries it requested. A
// follow-up pass reuses the lookback bound of the pass it follows: the live
// ledger has moved on since (live deliveries are accepted again), and a bound
// derived from it would drop the outage the pass is meant to cover.
func (r *deliveryReplayer) passMode(ctx context.Context, followUp bool) int {
	now := r.now()
	var bound time.Time
	r.mu.Lock()
	reuse := followUp && !r.lastBound.IsZero()
	if reuse {
		bound = r.lastBound
	}
	r.mu.Unlock()
	if !reuse {
		bound = r.lowerBound(now)
		r.mu.Lock()
		r.lastBound = bound
		r.mu.Unlock()
	}

	r.mu.Lock()
	for g, rq := range r.requested {
		if now.Sub(rq.at) > deliveryLedgerWindow {
			delete(r.requested, g)
		}
	}
	r.mu.Unlock()

	sum := localapi.ReplayHealth{}
	finish := func(found, requested, capped, failed int, outcome string) {
		sum.LastPassAt = localapi.Some(r.now())
		sum.FoundFailed = localapi.Some(int64(found))
		sum.Requested = localapi.Some(int64(requested))
		sum.Capped = localapi.Some(int64(capped))
		sum.Failed = localapi.Some(int64(failed))
		sum.Outcome = outcome
		if r.status != nil {
			r.status.SetReplayStatus(sum)
		}
		r.log("replay: found %d failed, requested %d, capped %d, failed %d (%s)\n", found, requested, capped, failed, outcome)
	}

	// List newest-first, grouping entries by GUID in first-appearance order.
	type group struct {
		failedID int64 // newest failed attempt's delivery id (0 = none)
		ok       bool  // some attempt returned 2xx
		ping     bool
		// newestRedelivery/newestAt describe the newest attempt in the log: a
		// failed redelivery newer than our request means our request was attempted
		// and failed.
		seen            bool
		newestRedeliver bool
		newestAt        time.Time
	}
	groups := map[string]*group{}
	var order []string
	cursor := ""
	pageCapped := false
	for page := 1; ; page++ {
		items, next, err := r.api.ListAppHookDeliveries(cursor, replayPerPage)
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			r.logDegraded("listing deliveries", err)
			finish(0, 0, 0, 0, "degraded: "+shortErr(err))
			return 0
		}
		reachedBound := false
		for _, d := range items {
			if d.DeliveredAt.Before(bound) {
				reachedBound = true
				continue
			}
			if d.GUID == "" {
				continue
			}
			g := groups[d.GUID]
			if g == nil {
				g = &group{}
				groups[d.GUID] = g
				order = append(order, d.GUID)
			}
			if d.Event == "ping" {
				g.ping = true
			}
			if !g.seen {
				g.seen = true
				g.newestRedeliver = d.Redelivery
				g.newestAt = d.DeliveredAt
			}
			if d.Succeeded() {
				g.ok = true
			} else if g.failedID == 0 {
				g.failedID = d.ID
			}
		}
		if reachedBound || next == "" || len(items) == 0 {
			break
		}
		if page >= replayMaxPages {
			pageCapped = true
			break
		}
		cursor = next
	}

	var cands []replayCandidate
	rerequest := 0
	for _, guid := range order {
		g := groups[guid]
		if g.ok || g.ping || g.failedID == 0 || r.ledger.ledgerSeen(guid) {
			continue
		}
		r.mu.Lock()
		rq := r.requested[guid]
		r.mu.Unlock()
		if rq != nil {
			// Requested before: ask again only when that redelivery was attempted
			// and failed (the newest attempt is a redelivery newer than our
			// request), and only once more per GUID.
			attempted := g.newestRedeliver && g.newestAt.After(rq.at.Add(-replaySkewMargin))
			if rq.n >= replayMaxRequestsPerGUID || !attempted {
				continue
			}
			rerequest++
		}
		cands = append(cands, replayCandidate{guid: guid, id: g.failedID})
	}

	found := len(cands)
	capped := 0
	if found > replayMaxRedeliveries {
		capped = found - replayMaxRedeliveries
		cands = cands[:replayMaxRedeliveries]
	}

	if rerequest > 0 {
		r.log("replay: %d redelivery request(s) were attempted and failed — requesting them once more\n", rerequest)
	}
	requested, failed, consecutive := 0, 0, 0
	outcome := "ok"
	for i, c := range cands {
		if ctx.Err() != nil {
			return requested
		}
		if i > 0 && replayPace > 0 {
			select {
			case <-ctx.Done():
				return requested
			case <-time.After(replayPace):
			}
		}
		r.mu.Lock()
		prev := r.requested[c.guid]
		n := 1
		if prev != nil {
			n = prev.n + 1
		}
		r.requested[c.guid] = &replayRequest{at: r.now(), n: n}
		r.mu.Unlock()
		err := r.api.RedeliverAppHookDelivery(c.id)
		if err == nil {
			requested++
			consecutive = 0
			continue
		}
		// Not requested after all: let a later pass try again.
		r.mu.Lock()
		if prev != nil {
			r.requested[c.guid] = prev
		} else {
			delete(r.requested, c.guid)
		}
		r.mu.Unlock()
		failed++
		consecutive++
		if endsPass(err) {
			r.logDegraded("requesting redelivery", err)
			capped += len(cands) - i - 1
			outcome = "degraded: " + shortErr(err)
			finish(found, requested, capped, failed, outcome)
			return requested
		}
		r.log("replay: redelivery of %s (delivery %d) failed: %v\n", c.guid, c.id, err)
		if consecutive >= replayAbortAfterFailed {
			r.log("replay: %d redelivery requests failed in a row — ending the pass\n", consecutive)
			capped += len(cands) - i - 1
			outcome = "degraded: repeated redelivery failures"
			finish(found, requested, capped, failed, outcome)
			return requested
		}
	}

	if pageCapped {
		r.log("replay: capped — listing stopped at the %d-page cap before reaching the lookback bound; older failed deliveries are left to reconcile\n", replayMaxPages)
		outcome = "capped"
	}
	if capped > 0 {
		r.log("replay: capped — %d failed deliveries exceed the %d-request cap and were not requested; they are left to reconcile\n", found, replayMaxRedeliveries)
		outcome = "capped"
	}
	finish(found, requested, capped, failed, outcome)
	return requested
}

func (r *deliveryReplayer) logDegraded(what string, err error) {
	switch {
	case errors.Is(err, gh.ErrAppUnauthorized), errors.Is(err, gh.ErrNotFound):
		r.log("replay: the App cannot read its webhook deliveries (%s: %v) — relying on reconcile only\n", what, err)
	default:
		r.log("replay: %s failed: %v — relying on reconcile only\n", what, err)
	}
}

// endsPass reports whether a redelivery error means further requests in this
// pass would be pointless or harmful: the App is not allowed to, GitHub is
// rate limiting, or GitHub's spam guard (a 422) has tripped.
func endsPass(err error) bool {
	var rl *gh.RateLimitError
	if errors.Is(err, gh.ErrAppUnauthorized) || errors.Is(err, gh.ErrNotFound) || errors.As(err, &rl) {
		return true
	}
	return strings.Contains(err.Error(), "returned 422")
}

func shortErr(err error) string {
	switch {
	case errors.Is(err, gh.ErrAppUnauthorized):
		return "not authorized to read deliveries"
	case errors.Is(err, gh.ErrNotFound):
		return "deliveries endpoint not found"
	}
	var rl *gh.RateLimitError
	if errors.As(err, &rl) {
		return "rate limited"
	}
	if strings.Contains(err.Error(), "returned 422") {
		return "GitHub refused the request (422)"
	}
	msg := err.Error()
	if len(msg) > 80 {
		msg = msg[:80] + "…"
	}
	return fmt.Sprint(msg)
}

// newDeliveryReplayer builds the replayer for transport tr (log tag tag: the
// transport's name, "direct" or "tailscale"), or nil when replay is off or
// cannot work (no App auth, no intake): in those cases nothing is reported on
// the health row either.
func (e *Engine) newDeliveryReplayer(tr replayStatusSink, tag string) *deliveryReplayer {
	var api hookDeliveryAPI = e.replayAPIOverride
	if api == nil && e.ghAppAuth != nil {
		api = e.ghAppAuth
	}
	if !e.cfg.ReplayFailedDeliveries || api == nil || e.intake == nil {
		return nil
	}
	r := newDeliveryReplayer(api, e.intake, tr,
		func(format string, args ...any) { e.logf(0, tag, format, args...) }, e.now)
	if e.replayWaitOverride != nil {
		r.wait = e.replayWaitOverride
	}
	return r
}

// startDeliveryReplay runs r in the background until ctx is cancelled and
// requests the startup pass. nil r is a no-op.
func (e *Engine) startDeliveryReplay(ctx context.Context, r *deliveryReplayer) {
	e.startDeliveryReplayAfter(ctx, r, 0)
}

// startDeliveryReplayAfter is startDeliveryReplay with the startup pass held
// back by grace (cancelled by ctx). The tailscale transport uses it: its node
// is up when Up returns, but Funnel ingress needs a while longer to route to it,
// and redeliveries requested before then fail (#2143). A trigger from a
// reconnect during the grace still runs a pass at once.
func (e *Engine) startDeliveryReplayAfter(ctx context.Context, r *deliveryReplayer, grace time.Duration) {
	if r == nil {
		return
	}
	go r.Run(ctx)
	if grace <= 0 {
		r.Trigger()
		return
	}
	go func() {
		if r.wait(ctx, grace) {
			r.Trigger()
		}
	}()
}
