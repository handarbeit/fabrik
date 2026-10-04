package engine

import (
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
	for _, t := range p.Events {
		sub.Events = append(sub.Events, channelevents.EventType(t))
	}
	backend := localAPIBackend{e: s.e}
	for _, ref := range p.Issues {
		repo, n, err := backend.resolveIssue(ref)
		if err != nil {
			return nil, err
		}
		sub.Issues = append(sub.Issues, channelevents.IssueRef{Repo: repo, Number: n})
	}
	stored, err := s.hub.Subscribe(sub)
	if err != nil {
		return nil, localapi.Errorf(localapi.CodeBadRequest, "%v", err)
	}
	return &localapi.SubscribeResult{Subscription: stored, Subscriptions: s.hub.Subscriptions(p.Subscriber)}, nil
}

func (s channelStreamer) Unsubscribe(p localapi.UnsubscribeParams) (*localapi.UnsubscribeResult, error) {
	if err := channelevents.ValidateSubscriberName(p.Subscriber); err != nil {
		return nil, localapi.Errorf(localapi.CodeBadRequest, "%v", err)
	}
	n := s.hub.Unsubscribe(p.Subscriber, p.ID)
	return &localapi.UnsubscribeResult{Removed: n, Subscriptions: s.hub.Subscriptions(p.Subscriber)}, nil
}

func (s channelStreamer) Attach(p localapi.AttachParams, sink channelevents.Sink) (func(), error) {
	return s.hub.Attach(p.Subscriber, sink), nil
}
