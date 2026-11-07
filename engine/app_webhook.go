package engine

import (
	"net"

	"github.com/handarbeit/fabrik/internal/appwebhook"
	"github.com/handarbeit/fabrik/tui"
)

// appWebhookTransport is the engine's view of the shared App webhook receiver
// (internal/appwebhook; #2475, ADR 2475; originally #2142, ADR 2142): the direct
// and tailscale event sources both run one. The receiver verifies
// X-Hub-Signature-256 against the App's static webhook secret and hands a
// delivery to the engine's intake sink; dedupe, the fast ack, echo attribution,
// reconnect catch-up and ordering all belong to the intake and are reused, not
// repeated. This type adds only what needs engine types: the engine's delivery
// struct on SetSink and the shared installation-coverage surface.
type appWebhookTransport struct {
	*appwebhook.Receiver
}

// newAppWebhookTransport builds a transport named name (the delivery's
// Transport label and the health row's name) that serves path on the listener
// listenFn returns, verifying with secret.
func newAppWebhookTransport(
	name, path string,
	listenFn func() (net.Listener, error),
	secret string,
	logFn func(issueNumber int, tag, format string, args ...any),
	emitFn func(tui.Event),
) *appWebhookTransport {
	var health appwebhook.HealthSink
	if emitFn != nil {
		health = func(state string, counts map[string]int, coverageNote string) {
			emitFn(tui.WebhookStatusEvent{State: state, EventCounts: counts, CoverageNote: &coverageNote})
		}
	}
	return &appWebhookTransport{Receiver: appwebhook.New(appwebhook.Config{
		Name:   name,
		Path:   path,
		Listen: listenFn,
		Secret: secret,
		Logf:   logFn,
		Health: health,
	})}
}

// SetSink implements ingestTransport: it adapts the engine's delivery struct to
// the receiver's.
func (t *appWebhookTransport) SetSink(sink func(delivery)) {
	t.Receiver.SetSink(func(d appwebhook.Delivery) {
		sink(delivery{ID: d.ID, EventType: d.EventType, Payload: d.Payload, Transport: d.Transport})
	})
}

// setInstallationCoverageNote implements appCoverageTransport.
func (t *appWebhookTransport) setInstallationCoverageNote(missing []string, truncated bool) {
	t.Receiver.SetInstallationCoverageNote(missing, truncated)
}

var _ ingestTransport = (*appWebhookTransport)(nil)
var _ appCoverageTransport = (*appWebhookTransport)(nil)
