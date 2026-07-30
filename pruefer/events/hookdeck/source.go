package hookdeck

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/handarbeit/fabrik/pruefer/events"
)

const (
	defaultMinBackoff = 1 * time.Second
	defaultMaxBackoff = 60 * time.Second
)

// Config configures a Source.
type Config struct {
	// APIKey authenticates to Hookdeck (HTTP Basic auth on session
	// creation, and the WebSocket dial's Authorization header).
	APIKey string
	// WebhookSecret is the GitHub App's webhook secret, used to verify
	// every forwarded delivery's X-Hub-Signature-256 header. Required —
	// Hookdeck's own transport auth (APIKey) is not a substitute for
	// verifying the payload actually came from GitHub.
	WebhookSecret string

	// APIBaseURL and WSBaseURL override Hookdeck's production endpoints;
	// both default when empty. Injectable for tests to point at an
	// httptest server instead of hookdeck.com.
	APIBaseURL string
	WSBaseURL  string

	// OnHealth, when non-nil, receives transport health transitions. The
	// daemon uses a transition into HealthConnected following a prior
	// disconnect to trigger a reconciliation poll, since events may have
	// been missed while disconnected.
	OnHealth func(events.HealthEvent)

	// HTTPClient overrides the HTTP client used for session creation; nil
	// uses http.DefaultClient.
	HTTPClient *http.Client
	// Dialer overrides the WebSocket dialer; nil uses
	// websocket.DefaultDialer. Injectable for tests.
	Dialer *websocket.Dialer

	// minBackoff/maxBackoff bound the reconnect backoff; both default
	// (1s / 60s) when zero. Unexported — only hookdeck-package-internal
	// tests need to tune these for speed.
	minBackoff time.Duration
	maxBackoff time.Duration
}

// Source implements events.EventSource against Hookdeck's CLI-session
// protocol (see protocol.go for the verified wire shapes). Construct via
// NewSource; the zero value is not usable.
type Source struct {
	cfg    Config
	dedupe *events.Dedupe
}

// NewSource returns a Source ready to Run, applying defaults for any unset
// Config field.
func NewSource(cfg Config) *Source {
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = DefaultAPIBaseURL
	}
	if cfg.WSBaseURL == "" {
		cfg.WSBaseURL = DefaultWSBaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Dialer == nil {
		cfg.Dialer = websocket.DefaultDialer
	}
	if cfg.minBackoff <= 0 {
		cfg.minBackoff = defaultMinBackoff
	}
	if cfg.maxBackoff <= 0 {
		cfg.maxBackoff = defaultMaxBackoff
	}
	return &Source{cfg: cfg, dedupe: events.NewDedupe(0)}
}

func (s *Source) emitHealth(state events.HealthState, err error) {
	if s.cfg.OnHealth == nil {
		return
	}
	ev := events.HealthEvent{State: state, At: time.Now()}
	if err != nil {
		ev.Err = err.Error()
	}
	s.cfg.OnHealth(ev)
}

// Run connects to Hookdeck and dispatches received events to sink until ctx
// is cancelled. It never returns a non-nil error except in response to ctx
// cancellation — any transport failure (session creation, dial, read)
// triggers a reconnect with capped exponential backoff instead of
// propagating up, so a Hookdeck outage never crashes the daemon.
func (s *Source) Run(ctx context.Context, sink events.EventSink) error {
	backoff := s.cfg.minBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		connected, err := s.runOnce(ctx, sink)
		if ctx.Err() != nil {
			return nil
		}
		// A successful connection (even one that later dropped) means the
		// prior backoff no longer reflects current reachability — reset it
		// before applying it below, so a reconnect right after a long
		// healthy stretch is prompt rather than stuck near maxBackoff from
		// whatever ramp-up happened before that stretch began.
		if connected {
			backoff = s.cfg.minBackoff
		}
		s.emitHealth(events.HealthReconnecting, err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if !connected {
			backoff *= 2
			if backoff > s.cfg.maxBackoff {
				backoff = s.cfg.maxBackoff
			}
		}
	}
}

// runOnce creates one Hookdeck CLI session, dials its WebSocket, and reads
// attempt frames until the connection fails or ctx is cancelled. connected
// reports whether the WebSocket handshake completed (used by Run to decide
// whether to reset its backoff), independent of err: a nil err means ctx
// was cancelled (caller stops retrying), and a non-nil err is the transport
// failure that ended this attempt (caller backs off and retries).
func (s *Source) runOnce(ctx context.Context, sink events.EventSink) (connected bool, err error) {
	sessionID, err := createSession(s.cfg.HTTPClient, s.cfg.APIBaseURL, s.cfg.APIKey)
	if err != nil {
		return false, fmt.Errorf("creating hookdeck session: %w", err)
	}

	header := http.Header{}
	header.Set("Websocket-Id", sessionID)
	header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(s.cfg.APIKey+":")))

	conn, _, err := s.cfg.Dialer.DialContext(ctx, s.cfg.WSBaseURL, header)
	if err != nil {
		return false, fmt.Errorf("dialing hookdeck websocket: %w", err)
	}
	defer conn.Close()

	s.emitHealth(events.HealthConnected, nil)

	// ReadMessage has no context parameter; close the connection out from
	// under it on ctx cancellation, the standard gorilla/websocket idiom
	// for context-aware reads. done prevents this goroutine from leaking
	// past runOnce's return on a genuine (non-cancellation) read error.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return true, nil
			}
			return true, fmt.Errorf("reading hookdeck websocket: %w", err)
		}
		s.handleFrame(ctx, sink, conn, msg)
	}
}

// handleFrame processes one raw WebSocket message: only "attempt" frames
// carry forwarded webhooks; anything else (unrecognized frame types, or a
// malformed message) is ignored. Every attempt is acked regardless of
// whether processAttempt accepted or dropped it — the ack completes the
// Hookdeck CLI-session round trip; it is not a statement about GitHub
// signature validity.
func (s *Source) handleFrame(ctx context.Context, sink events.EventSink, conn *websocket.Conn, msg []byte) {
	var frame wsFrame
	if err := json.Unmarshal(msg, &frame); err != nil {
		return
	}
	if frame.Event != wsEventAttempt {
		return
	}
	var attempt attemptBody
	if err := json.Unmarshal(frame.Body, &attempt); err != nil {
		return
	}

	s.processAttempt(ctx, sink, attempt)
	s.ack(conn, attempt)
}

// processAttempt verifies the GitHub signature, dedupes by delivery ID,
// normalizes, and — if all of that succeeds — hands the event to sink.
// Any failure at any step causes the attempt to be silently dropped from
// Pruefer's perspective (GitHub-derived review state, ReviewPR's own
// SHA-idempotency, and the poll-fallback safety net all make a dropped
// event non-fatal).
func (s *Source) processAttempt(ctx context.Context, sink events.EventSink, attempt attemptBody) {
	headers := attempt.Request.Headers
	sig := lookupHeader(headers, "X-Hub-Signature-256")
	if !events.VerifySignature([]byte(attempt.Request.DataString), sig, s.cfg.WebhookSecret) {
		return
	}

	deliveryID := lookupHeader(headers, "X-GitHub-Delivery")
	if s.dedupe.SeenBefore(deliveryID) {
		return
	}

	eventType := lookupHeader(headers, "X-GitHub-Event")
	ev, err := normalizeEvent([]byte(attempt.Request.DataString), eventType, deliveryID, time.Now())
	if err != nil {
		return
	}

	sink.Handle(ctx, ev)
}

// lookupHeader finds key in headers case-insensitively — Hookdeck forwards
// the original header names, and GitHub sends canonical casing, but this
// guards against any intermediary normalization.
func lookupHeader(headers map[string]string, key string) string {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// ack sends the attempt_response frame Hookdeck's CLI-session protocol
// expects for every attempt. A failure to send is not retried — the
// attempt has already been processed; a lost ack only costs a Hookdeck-side
// timeout/retry of the same delivery, which dedupe absorbs on arrival.
func (s *Source) ack(conn *websocket.Conn, attempt attemptBody) {
	body, err := json.Marshal(attemptResponseBody{
		AttemptID: attempt.AttemptID,
		CLIPath:   attempt.CLIPath,
		Status:    http.StatusOK,
	})
	if err != nil {
		return
	}
	data, err := json.Marshal(wsFrame{Event: wsEventAttemptResponse, Body: body})
	if err != nil {
		return
	}
	_ = conn.WriteMessage(websocket.TextMessage, data)
}
