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
)

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

// replayStatusSink receives the summary of the last pass; the direct transport
// implements it so its health row reports it. Calling it with a zero value
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

	trigger chan struct{}

	mu        sync.Mutex
	requested map[string]time.Time // guid -> when redelivery was requested
}

func newDeliveryReplayer(api hookDeliveryAPI, ledger replayLedger, status replayStatusSink, logf func(format string, args ...any), now func() time.Time) *deliveryReplayer {
	if now == nil {
		now = time.Now
	}
	r := &deliveryReplayer{
		api:       api,
		ledger:    ledger,
		status:    status,
		logf:      logf,
		now:       now,
		trigger:   make(chan struct{}, 1),
		requested: make(map[string]time.Time),
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
			r.pass(ctx)
		}
	}
}

func (r *deliveryReplayer) log(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}

// lowerBound is the oldest delivered_at a pass considers.
func (r *deliveryReplayer) lowerBound(now time.Time) time.Time {
	newest := r.ledger.ledgerNewest()
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

func (r *deliveryReplayer) pass(ctx context.Context) {
	now := r.now()
	bound := r.lowerBound(now)

	r.mu.Lock()
	for g, at := range r.requested {
		if now.Sub(at) > deliveryLedgerWindow {
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
	}
	groups := map[string]*group{}
	var order []string
	cursor := ""
	pageCapped := false
	for page := 1; ; page++ {
		items, next, err := r.api.ListAppHookDeliveries(cursor, replayPerPage)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.logDegraded("listing deliveries", err)
			finish(0, 0, 0, 0, "degraded: "+shortErr(err))
			return
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
	for _, guid := range order {
		g := groups[guid]
		if g.ok || g.ping || g.failedID == 0 || r.ledger.ledgerSeen(guid) {
			continue
		}
		r.mu.Lock()
		_, already := r.requested[guid]
		r.mu.Unlock()
		if already {
			continue
		}
		cands = append(cands, replayCandidate{guid: guid, id: g.failedID})
	}

	found := len(cands)
	capped := 0
	if found > replayMaxRedeliveries {
		capped = found - replayMaxRedeliveries
		cands = cands[:replayMaxRedeliveries]
	}

	requested, failed, consecutive := 0, 0, 0
	outcome := "ok"
	for i, c := range cands {
		if ctx.Err() != nil {
			return
		}
		if i > 0 && replayPace > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(replayPace):
			}
		}
		r.mu.Lock()
		r.requested[c.guid] = r.now()
		r.mu.Unlock()
		err := r.api.RedeliverAppHookDelivery(c.id)
		if err == nil {
			requested++
			consecutive = 0
			continue
		}
		// Not requested after all: let a later pass try again.
		r.mu.Lock()
		delete(r.requested, c.guid)
		r.mu.Unlock()
		failed++
		consecutive++
		if endsPass(err) {
			r.logDegraded("requesting redelivery", err)
			capped += len(cands) - i - 1
			outcome = "degraded: " + shortErr(err)
			finish(found, requested, capped, failed, outcome)
			return
		}
		r.log("replay: redelivery of %s (delivery %d) failed: %v\n", c.guid, c.id, err)
		if consecutive >= replayAbortAfterFailed {
			r.log("replay: %d redelivery requests failed in a row — ending the pass\n", consecutive)
			capped += len(cands) - i - 1
			outcome = "degraded: repeated redelivery failures"
			finish(found, requested, capped, failed, outcome)
			return
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

// newDeliveryReplayer builds the replayer for transport tr, or nil when replay
// is off or cannot work (no App auth, no intake): in those cases nothing is
// reported on the health row either.
func (e *Engine) newDeliveryReplayer(tr replayStatusSink) *deliveryReplayer {
	var api hookDeliveryAPI = e.replayAPIOverride
	if api == nil && e.ghAppAuth != nil {
		api = e.ghAppAuth
	}
	if !e.cfg.ReplayFailedDeliveries || api == nil || e.intake == nil {
		return nil
	}
	return newDeliveryReplayer(api, e.intake, tr,
		func(format string, args ...any) { e.logf(0, "direct", format, args...) }, e.now)
}

// startDeliveryReplay runs r in the background until ctx is cancelled and
// requests the startup pass. nil r is a no-op.
func (e *Engine) startDeliveryReplay(ctx context.Context, r *deliveryReplayer) {
	if r == nil {
		return
	}
	go r.Run(ctx)
	r.Trigger()
}
