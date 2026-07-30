package hookdeck

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/handarbeit/fabrik/pruefer/events"
)

const (
	defaultMinBackoff     = 1 * time.Second
	defaultMaxBackoff     = 60 * time.Second
	defaultConnectTimeout = 15 * time.Second

	// defaultPongWait is the default read deadline applied to the
	// WebSocket connection, refreshed on every received frame (data or
	// pong). If Hookdeck's connection dies silently — a NAT timeout or a
	// firewall dropping packets without a FIN/RST — ReadMessage would
	// otherwise block forever with no error, and Run's reconnect/backoff
	// loop would never re-trigger. defaultPingPeriod (well under
	// defaultPongWait) sends an idle keepalive so a silently-dead
	// connection is detected within one pongWait window even with no
	// GitHub traffic to keep it alive naturally.
	defaultPongWait   = 60 * time.Second
	defaultPingPeriod = 30 * time.Second

	// defaultAliveGracePeriod bounds how long a connection must survive
	// without error before runOnce treats it as proven alive even with zero
	// received frames — see markConnected in runOnce for why relying on a
	// frame alone would misclassify a genuinely healthy but idle connection
	// (e.g. a quiet period with no forwarded webhooks) as never having
	// connected.
	defaultAliveGracePeriod = 5 * time.Second
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

	// connectTimeout bounds session creation plus the WebSocket dial as a
	// single attempt, independently of the caller's (long-lived) ctx — a
	// TCP-connected-but-non-responding Hookdeck endpoint would otherwise
	// hang the entire reconnect loop indefinitely, since neither
	// http.Client nor the WebSocket dialer has a timeout of its own here.
	// Defaults (15s) when zero; unexported — only tests need to shrink
	// this for speed.
	connectTimeout time.Duration

	// pongWait/pingPeriod bound how long a silently-dead connection (no
	// FIN/RST, e.g. a NAT timeout) can go undetected; both default
	// (60s / 30s) when zero. Unexported — only tests need to shrink these
	// for speed.
	pongWait   time.Duration
	pingPeriod time.Duration

	// aliveGracePeriod bounds how long a connection must survive without
	// error before it's considered proven alive even with zero received
	// frames. Defaults (5s) when zero; unexported — only tests need to
	// shrink this for speed.
	aliveGracePeriod time.Duration
}

// Source implements events.EventSource against Hookdeck's CLI-session
// protocol (see protocol.go for the verified wire shapes). Construct via
// NewSource; the zero value is not usable.
type Source struct {
	cfg    Config
	dedupe *events.Dedupe

	// lastSigWarnAt rate-limits the invalid-signature warn log below —
	// processAttempt runs single-threaded off Source's own WebSocket read
	// loop, so no synchronization is needed to guard it.
	lastSigWarnAt time.Time
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
	if cfg.connectTimeout <= 0 {
		cfg.connectTimeout = defaultConnectTimeout
	}
	if cfg.pongWait <= 0 {
		cfg.pongWait = defaultPongWait
	}
	if cfg.pingPeriod <= 0 {
		cfg.pingPeriod = defaultPingPeriod
	}
	if cfg.aliveGracePeriod <= 0 {
		cfg.aliveGracePeriod = defaultAliveGracePeriod
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
// reports whether the session proved itself alive (see markConnected below
// for what counts as proof, and why a completed handshake alone does not) —
// used by Run to decide whether to reset its backoff. connected is
// independent of err: a nil err means ctx was cancelled (caller stops
// retrying), and a non-nil err is the transport failure that ended this
// attempt (caller backs off and retries).
func (s *Source) runOnce(ctx context.Context, sink events.EventSink) (connected bool, err error) {
	// connectCtx bounds session creation plus the WebSocket dial as a
	// single attempt, independently of ctx (the daemon's long-lived run
	// context) — a TCP-connected-but-non-responding Hookdeck endpoint
	// would otherwise hang here indefinitely, since neither http.Client
	// nor the dialer has a timeout of its own. It is not used past the
	// dial: the read loop below is governed by ctx alone.
	connectCtx, cancel := context.WithTimeout(ctx, s.cfg.connectTimeout)
	defer cancel()

	sessionID, err := createSession(connectCtx, s.cfg.HTTPClient, s.cfg.APIBaseURL, s.cfg.APIKey)
	if err != nil {
		return false, fmt.Errorf("creating hookdeck session: %w", err)
	}

	header := http.Header{}
	header.Set("Websocket-Id", sessionID)
	header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(s.cfg.APIKey+":")))

	conn, _, err := s.cfg.Dialer.DialContext(connectCtx, s.cfg.WSBaseURL, header)
	if err != nil {
		return false, fmt.Errorf("dialing hookdeck websocket: %w", err)
	}
	defer conn.Close()

	// A silently-dead connection (NAT timeout, a firewall dropping packets
	// without a FIN/RST) would otherwise leave ReadMessage blocked forever
	// with no error — Run's reconnect/backoff loop would never re-trigger,
	// and the daemon would degrade to poll-fallback with zero
	// operator-visible signal. The read deadline (refreshed on every
	// received frame, data or pong) plus a periodic idle ping together
	// bound how long such a failure can go undetected to roughly pongWait.
	conn.SetReadDeadline(time.Now().Add(s.cfg.pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(s.cfg.pongWait))
		return nil
	})

	// ReadMessage has no context parameter; close the connection out from
	// under it on ctx cancellation, the standard gorilla/websocket idiom
	// for context-aware reads. done also stops the goroutine below, and
	// prevents it from leaking past runOnce's return on a genuine
	// (non-cancellation) read error.
	done := make(chan struct{})
	defer close(done)

	// proven reports whether this session has demonstrated itself alive —
	// either by successfully reading a frame, or by surviving past
	// aliveGracePeriod without error — used by Run to decide whether to
	// reset its backoff, and to gate the single HealthConnected emission
	// below. A completed handshake alone is not enough evidence, since
	// Hookdeck can accept the handshake and then immediately reject/drop
	// the session (e.g. a stale or invalid API key) without that surfacing
	// as a dial-time error; treating that as proven would reset backoff to
	// its floor every cycle and hammer the session-creation REST endpoint
	// at roughly once a second instead of ramping toward maxBackoff like
	// any other hard failure. But requiring a frame forever would also
	// misclassify a genuinely healthy, merely idle connection (e.g. a
	// quiet period with no forwarded webhooks — gorilla/websocket handles
	// ping/pong control frames internally and never surfaces them via
	// ReadMessage, so an idle session reads nothing at all) as never
	// having connected, ramping backoff toward maxBackoff on every
	// reconnect even though nothing was actually wrong. aliveGracePeriod
	// resolves both: an instant reject can't survive it, but idle-and-
	// healthy does.
	var proven atomic.Bool
	var markConnectedOnce sync.Once
	markConnected := func() {
		markConnectedOnce.Do(func() {
			proven.Store(true)
			s.emitHealth(events.HealthConnected, nil)
		})
	}

	go func() {
		ticker := time.NewTicker(s.cfg.pingPeriod)
		defer ticker.Stop()
		graceTimer := time.NewTimer(s.cfg.aliveGracePeriod)
		defer graceTimer.Stop()
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			case <-done:
				return
			case <-ticker.C:
				if writeErr := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); writeErr != nil {
					return
				}
			case <-graceTimer.C:
				markConnected()
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return true, nil
			}
			return proven.Load(), fmt.Errorf("reading hookdeck websocket: %w", err)
		}
		conn.SetReadDeadline(time.Now().Add(s.cfg.pongWait))
		markConnected()
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
		logf("dropping malformed hookdeck attempt frame: %v\n", err)
		return
	}

	s.processAttempt(ctx, sink, attempt)
	s.ack(conn, attempt)
}

// sigFailureLogInterval rate-limits the invalid-signature warn log in
// processAttempt — a misconfigured secret can fail every single delivery,
// and this is the most security-sensitive check in the pipeline, so it must
// stay visible without flooding the log on a sustained run of failures.
const sigFailureLogInterval = 30 * time.Second

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
		if time.Since(s.lastSigWarnAt) >= sigFailureLogInterval {
			s.lastSigWarnAt = time.Now()
			logf("dropping event: invalid GitHub webhook signature (check hookdeck.webhook_secret_env)\n")
		}
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
