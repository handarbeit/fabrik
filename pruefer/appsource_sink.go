package pruefer

import (
	"context"
	"time"

	"github.com/handarbeit/fabrik/internal/appwebhook"
	"github.com/handarbeit/fabrik/internal/events"
)

// The delivery path of the direct and tailscale event sources (#2476,
// ADR 2476). The shared receiver (internal/appwebhook) has already verified the
// method, path, size, signature and JSON; what arrives here is a Delivery. It is
// converted to the same normalized events.GitHubEvent a Hookdeck delivery
// becomes and handed to the same daemonEventSink, so the dispatch (a review for
// a pull_request action in reviewTriggerActions, a re-derivation for an
// installation event) is exactly the one Hookdeck deliveries already take.
//
// Between conversion and the sink sit two checks, in this order:
//
//  1. Installation ownership (FR-007), the backstop for a shared App that still
//     points at this daemon. It runs first so a foreign delivery is never
//     recorded as seen by the dedupe, and it runs after the receiver's HMAC
//     check, so it is never counted as a signature failure.
//  2. The delivery-ID dedupe (FR-005), the same events.Dedupe type Hookdeck's
//     source uses.
//
// Deliver runs on the receiver's HTTP goroutine, before the 200 is written, so
// it must not block: normalization is a JSON parse, the checks are map lookups,
// and daemonEventSink.Handle returns immediately by construction.

// appSink turns verified receiver deliveries into sink calls.
type appSink struct {
	daemon *Daemon
	sink   events.EventSink
	dedupe *events.Dedupe
	// ctx is the source's run context: daemonEventSink.Handle starts a review
	// goroutine that must outlive the HTTP request but not the daemon.
	ctx context.Context
	now func() time.Time
}

func newAppSink(ctx context.Context, d *Daemon, sink events.EventSink, dedupe *events.Dedupe) *appSink {
	return &appSink{daemon: d, sink: sink, dedupe: dedupe, ctx: ctx, now: time.Now}
}

// Deliver implements appwebhook.Sink.
func (s *appSink) Deliver(del appwebhook.Delivery) {
	ev, err := events.NormalizeWebhook(del.Payload, del.EventType, del.ID, s.now())
	if err != nil {
		// The receiver already checked json.Valid, so this is a body that is
		// valid JSON but not an object; there is nothing to route.
		logf(0, "warn", "%s delivery %s (%s): %v — dropping\n", del.Transport, del.ID, del.EventType, err)
		s.daemon.recordDrop(events.DropMalformedPayload)
		return
	}
	if !s.daemon.deliveryIsOurs(ev) {
		if ev.Installation == 0 {
			// An App-level event (e.g. github_app_authorization) carries no
			// installation.id, so it cannot be matched to this daemon. Say so
			// rather than blaming a foreign App.
			logf(0, "warn", "%s delivery %s (%s): carries no installation.id, so it cannot be matched to this daemon — ignoring\n",
				del.Transport, del.ID, del.EventType)
		} else {
			logf(0, "warn", "%s delivery %s (%s): installation %d is not served by this daemon — ignoring (is another GitHub App still pointed at this webhook URL?)\n",
				del.Transport, del.ID, del.EventType, ev.Installation)
		}
		s.daemon.recordDrop(events.DropForeignInstallation)
		return
	}
	if s.dedupe.SeenBefore(ev.DeliveryID) {
		s.daemon.recordDrop(events.DropDedupe)
		return
	}
	s.sink.Handle(s.ctx, ev)
}

// ownInstallations returns the installation IDs this daemon serves: the pinned
// github_app_installation_id when set, plus every installation the last
// derivation found (including ones it could not mint a token for or that serve
// none of the watched repos — they are still installations of this App). An
// empty result means ownership is not known yet.
func (d *Daemon) ownInstallations() map[int64]bool {
	cfg := d.config()
	set := d.derivedSet()
	ids := make(map[int64]bool, len(set.Installations)+1)
	if cfg.AppInstallationID != 0 {
		ids[cfg.AppInstallationID] = true
	}
	for _, in := range set.Installations {
		if in.InstallationID != 0 {
			ids[in.InstallationID] = true
		}
	}
	for _, r := range set.Repos {
		if r.InstallationID != 0 {
			ids[r.InstallationID] = true
		}
	}
	return ids
}

// deliveryIsOurs reports whether ev may be processed by this daemon.
//
// With no installation known (a first start before derivation completes) the
// daemon cannot judge, so everything passes: this check is a backstop, not a
// gate, and a wrong "foreign" verdict would silently drop real work.
//
// Once installations are known, an event must carry one of them. An event with
// no installation.id cannot be matched and is treated as foreign.
//
// One exception: an installation "created" event is let through even for an ID
// not yet in the set, because it is the event that tells a daemon holding
// several installations of its App about a new one (it triggers the
// re-derivation that adds it). The receiver's HMAC check has already shown the
// event came from this daemon's App, and re-derivation reads only that App's own
// installations, so the exception cannot be used to pull in another App's data.
func (d *Daemon) deliveryIsOurs(ev events.GitHubEvent) bool {
	own := d.ownInstallations()
	if len(own) == 0 {
		return true
	}
	if ev.Installation != 0 && own[ev.Installation] {
		return true
	}
	return ev.EventType == "installation" && ev.Action == "created"
}
