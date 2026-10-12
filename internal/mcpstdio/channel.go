package mcpstdio

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// Claude Code Channels push (#1968, ADR-1966-b).
//
// A Channels notification is an MCP notification, method
// "notifications/claude/channel", with params {content, meta}. The contract is
// a research preview, so everything that depends on it lives in this file:
// channelNotification builds the params, and ChannelMethod / the capability in
// initialize name it. Claude Code never acknowledges a notification; a session
// not launched with the channel flag (or blocked by org policy) drops them
// silently, so "delivered" here only ever means "written to stdout".

// ChannelMethod is the MCP notification method Claude Code listens on.
const ChannelMethod = "notifications/claude/channel"

// DefaultUnreachableGrace is how long the held stream must stay down before the
// shim reports daemon-unreachable: a SIGHUP re-exec normally re-binds the
// socket well inside it, and a crash loop must not spam the session.
const DefaultUnreachableGrace = 10 * time.Second

// MetaOrigin is the wire name of the provenance key (channelevents.MetaSource).
// Claude Code sets the <channel> tag's own `source` attribute from the server's
// configured name, so a meta key called `source` would clash with it; the hub and
// the daemon keep `source` (the name the issue and the event stream use) and the
// shim renames it here, in the one place that depends on the Channels contract
// (#2169, ADR 2169).
const MetaOrigin = "origin"

// wireMetaKey maps an event meta key to the attribute name sent to the session.
func wireMetaKey(k string) string {
	if k == channelevents.MetaSource {
		return MetaOrigin
	}
	return k
}

// StreamFunc runs the held connection. localapi.RunStream is the production value.
type StreamFunc func(ctx context.Context, opts localapi.StreamOptions) error

// metaKey reports whether k survives Claude Code's channel-meta filter: letters,
// digits and underscore only. Keys with hyphens or other characters are
// silently dropped by the client, so they are dropped here, visibly (never sent).
func metaKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// channelNotification builds the notification params for one event. Every meta
// value is a string; `event` is always present, and `repo`, `issue`, `stage`,
// `pr` and `comment` are carried when known (R5). Event type names appear only
// as values, never keys.
func channelNotification(ev channelevents.Event) map[string]any {
	meta := map[string]string{}
	for k, v := range ev.Meta {
		if metaKey(k) {
			meta[wireMetaKey(k)] = v
		}
	}
	// Every pushed event says where it came from. The daemon stamps its own; the
	// events the shim builds (daemon-reachable, daemon-unreachable,
	// stream-superseded) are live by definition.
	if meta[MetaOrigin] == "" {
		meta[MetaOrigin] = channelevents.SourceLive
	}
	if meta["event"] == "" {
		meta["event"] = string(ev.Type)
	}
	if ev.Repo != "" && meta["repo"] == "" {
		meta["repo"] = ev.Repo
	}
	if ev.Issue != 0 && meta["issue"] == "" {
		meta["issue"] = strconv.Itoa(ev.Issue)
	}
	if ev.Stage != "" && meta["stage"] == "" {
		meta["stage"] = ev.Stage
	}
	if ev.PR != 0 && meta["pr"] == "" {
		meta["pr"] = strconv.Itoa(ev.PR)
	}
	if ev.CommentURL != "" && meta["comment"] == "" {
		meta["comment"] = ev.CommentURL
	}
	return map[string]any{"content": ev.Content, "meta": meta}
}

func (s *Server) notifyChannel(ev channelevents.Event) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": ChannelMethod, "params": channelNotification(ev)})
}

// startStream launches the held connection once, after the client has said it is
// initialized (a notification before that could be dropped). A shim launched
// without a subscriber name has no push: the read tools work unchanged.
func (s *Server) startStream(ctx context.Context) {
	if s.Subscriber == "" {
		return
	}
	s.streamOnce.Do(func() {
		stream := s.Stream
		if stream == nil {
			stream = localapi.RunStream
		}
		grace := s.UnreachableGrace
		if grace <= 0 {
			grace = DefaultUnreachableGrace
		}
		watch := &reachability{srv: s, grace: grace}
		s.streamWG.Add(1)
		go func() {
			defer s.streamWG.Done()
			defer watch.stop()
			err := stream(ctx, localapi.StreamOptions{
				Path:       s.SocketPath,
				Subscriber: s.Subscriber,
				OnEvent:    s.notifyChannel,
				OnState:    watch.onState,
				Logf:       func(f string, a ...any) { s.logf(f, a...) },
			})
			if err != nil && ctx.Err() == nil {
				s.logf("channel stream stopped: %v", err)
			}
		}()
	})
}

// reachability turns the stream's connected/disconnected transitions into
// daemon-unreachable / daemon-reachable events. They are live-only: a daemon
// that is down cannot hold them, and the session that experienced the outage is
// the one that needs to hear about it. unreachable fires only if the stream is
// still down after the grace period; reachable fires on the first re-attach
// that follows a reported outage.
type reachability struct {
	srv   *Server
	grace time.Duration

	mu       sync.Mutex
	timer    *time.Timer
	downAt   time.Time
	reported bool
	warned   bool
}

func (r *reachability) onState(st localapi.StreamState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st.Superseded {
		// The stream is over for good: tell the session why its push stopped,
		// instead of going silent. Any pending unreachable report is moot.
		if r.timer != nil {
			r.timer.Stop()
			r.timer = nil
		}
		r.srv.notifyChannel(channelevents.Event{
			Type:    channelevents.StreamSuperseded,
			Content: fmt.Sprintf("Another session attached as subscriber %q and took over this stream; this session will receive no more Fabrik events. Use a distinct --subscriber name per session to keep both.", r.srv.Subscriber),
			Meta:    map[string]string{"event": string(channelevents.StreamSuperseded), "subscriber": r.srv.Subscriber},
		})
		return
	}
	if st.Connected {
		if r.timer != nil {
			r.timer.Stop()
			r.timer = nil
		}
		if r.reported {
			down := st.At.Sub(r.downAt)
			r.reported = false
			r.srv.notifyChannel(channelevents.Event{
				Type:    channelevents.DaemonReachable,
				Content: fmt.Sprintf("Fabrik daemon is reachable again after %s; held events follow", down.Round(time.Second)),
				Meta:    map[string]string{"event": string(channelevents.DaemonReachable), "down_for_seconds": strconv.FormatInt(int64(down/time.Second), 10)},
			})
		}
		return
	}
	if st.Unsupported {
		if !r.warned {
			r.warned = true
			r.srv.logf("daemon does not support the push stream (older protocol); channel events are off, read tools still work")
		}
		return
	}
	if r.reported || r.timer != nil {
		return
	}
	r.downAt = st.At
	reason := ""
	if st.Err != nil {
		reason = st.Err.Error()
	}
	r.timer = time.AfterFunc(r.grace, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.timer == nil || r.reported {
			return
		}
		r.timer = nil
		r.reported = true
		meta := map[string]string{"event": string(channelevents.DaemonUnreachable)}
		if reason != "" {
			meta["error"] = reason
		}
		r.srv.notifyChannel(channelevents.Event{
			Type:    channelevents.DaemonUnreachable,
			Content: "Fabrik daemon is unreachable; no events will arrive until it is back (read tools will report the same). Held events are delivered on reconnect.",
			Meta:    meta,
		})
	})
}

func (r *reachability) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}

// channelInstructions is appended to the server's instructions: how events look
// in the session and what is required for them to appear at all.
const channelInstructions = " Fabrik also pushes events into this session as <channel source=\"fabrik\" event=... repo=... issue=...> messages when something needs attention " +
	"(validate-settled, decision-pending, settlement-withdrawn, paused, escalated, label changes, and more); the tag attributes say which issue and event, and the body is a one-line summary. " +
	"Label-change events are opt-in (name label-applied / label-removed in fabrik_subscribe's events) and are not a readiness signal; only validate-settled and decision-pending say an item awaits you, and settlement-withdrawn says that stopped being true. " +
	"Every event carries an `origin` attribute: live (as it happened), catch_up (a snapshot of what needs you now, not a transition) or downtime-delta (a change made while the daemon was down; its actor is unknown and it is marked unverified). " +
	"After a daemon restart you get daemon-reachable, then one catch_up snapshot, then the downtime-delta events - never a replay of unchanged state. " +
	"A validate-settled with a `cause` attribute (autonomy-changed, revalidated, new-head, other) is a re-settle: the item settled again for that reason, and for autonomy-changed the `actor` attribute says who changed it. " +
	"A `situation` event is the rolled-up digest of everything that needs you - the same view fabrik_situation returns - with the readable view as its body and the section counts as attributes (`trigger` says why it was sent: catch_up, cadence or request); one is sent when nothing changed since the last. " +
	"A decision-bearing event carries an `options` attribute (a JSON list of {action, tool}); an event with reminder=\"true\" repeats a decision that is still pending. " +
	"Use fabrik_subscribe to choose what you hear about and fabrik_unsubscribe to stop. Pushes need this session to have been started with Channels enabled for this server " +
	"(during the research preview: claude --dangerously-load-development-channels server:fabrik, or plugin:fabrik@fabrik when installed through the fabrik plugin); otherwise Claude Code drops them silently and only the read tools work."
