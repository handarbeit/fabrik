package engine

import (
	"strings"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// channelStreamer is the engine's localapi.Streamer: the subscribe / unsubscribe
// / attach half of protocol v2 over the channel hub (#1968). It touches the hub
// and in-memory engine state only — never GitHub (R9).
type channelStreamer struct {
	e   *Engine
	hub *channelevents.Hub
}

// channelStreamer returns the Streamer to hand the local API server, or nil
// when the hub is not running (the server then does not advertise streaming).
func (e *Engine) channelStreamer() localapi.Streamer {
	ce := e.channelEvents()
	if ce == nil {
		return nil
	}
	return channelStreamer{e: e, hub: ce.hub}
}

func (s channelStreamer) Subscribe(p localapi.SubscribeParams) (*localapi.SubscribeResult, error) {
	sub := channelevents.Subscription{
		Subscriber:    p.Subscriber,
		Repos:         p.Repos,
		Milestone:     p.Milestone,
		Labels:        p.Labels,
		ExcludeLabels: p.ExcludeLabels,
		DigestSeconds: p.DigestSeconds,
	}
	// Version skew is the normal case (the daemon self-upgrades, a shim lives as
	// long as its Claude session), so a request naming an event type this daemon
	// does not know must not fail whole: legacy names are mapped, unknown ones are
	// dropped, and both are reported (#2168, ADR 2168). A request that named
	// events and keeps none is refused — an empty Events list would mean "every
	// event" — and stores nothing, leaving any existing subscription untouched.
	requested := make([]channelevents.EventType, len(p.Events))
	for i, t := range p.Events {
		requested[i] = channelevents.EventType(t)
	}
	kept, mapped, rejected := channelevents.NormalizeEvents(requested)
	if len(requested) > 0 && len(kept) == 0 {
		names := make([]string, len(rejected))
		for i, t := range rejected {
			names[i] = string(t)
		}
		return nil, localapi.Errorf(localapi.CodeBadRequest, "unknown event type %s; no known event type left to subscribe to, so nothing was changed", strings.Join(names, ", "))
	}
	sub.Events = kept
	backend := localAPIBackend{e: s.e}
	for _, ref := range p.Issues {
		repo, n, err := backend.resolveIssue(ref)
		if err != nil {
			return nil, err
		}
		sub.Issues = append(sub.Issues, channelevents.IssueRef{Repo: repo, Number: n})
	}
	stored, created, err := s.hub.SubscribeNew(sub)
	if err != nil {
		return nil, localapi.Errorf(localapi.CodeBadRequest, "%v", err)
	}
	if created {
		// A subscriber that arrives after an item settled would otherwise never
		// hear about it: queue a snapshot of what is waiting on a human right now.
		s.e.catchUp(stored.Subscriber, stored.ID)
	}
	res := &localapi.SubscribeResult{Subscription: stored, Subscriptions: s.hub.Subscriptions(p.Subscriber)}
	for _, m := range mapped {
		res.Mapped = append(res.Mapped, localapi.EventMapping{From: string(m.From), To: string(m.To)})
	}
	for _, t := range rejected {
		res.Rejected = append(res.Rejected, string(t))
	}
	return res, nil
}

func (s channelStreamer) Unsubscribe(p localapi.UnsubscribeParams) (*localapi.UnsubscribeResult, error) {
	if err := channelevents.ValidateSubscriberName(p.Subscriber); err != nil {
		return nil, localapi.Errorf(localapi.CodeBadRequest, "%v", err)
	}
	n := s.hub.Unsubscribe(p.Subscriber, p.ID)
	return &localapi.UnsubscribeResult{Removed: n, Subscriptions: s.hub.Subscriptions(p.Subscriber)}, nil
}

func (s channelStreamer) Attach(p localapi.AttachParams, sink channelevents.Sink) (func(), error) {
	if p.CatchUp {
		// A fresh session (not a reconnect): queued before the attach so it is
		// delivered after anything already held, in order, ahead of live events.
		s.e.catchUp(p.Subscriber, "")
	}
	return s.hub.Attach(p.Subscriber, sink), nil
}
