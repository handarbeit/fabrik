package hookdeck

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/handarbeit/fabrik/pruefer/events"
)

const testWebhookSecret = "test-webhook-secret"

func signBody(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// recordingSink is a test events.EventSink that records every Handle call.
type recordingSink struct {
	mu     sync.Mutex
	events []events.GitHubEvent
}

func (r *recordingSink) Handle(ctx context.Context, ev events.GitHubEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *recordingSink) snapshot() []events.GitHubEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.GitHubEvent, len(r.events))
	copy(out, r.events)
	return out
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func waitUntilCount(t *testing.T, sink *recordingSink, n int) {
	t.Helper()
	waitUntil(t, 2*time.Second, func() bool { return sink.count() >= n })
}

// mockHookdeckServer serves both the cli-sessions REST endpoint and the
// WebSocket dial target from a single httptest.Server — close enough to a
// real Hookdeck deployment for Source's tests, which only care about the
// two request shapes independently. Each successful WebSocket upgrade is
// pushed onto conns so tests can script attempt frames and read acks.
type mockHookdeckServer struct {
	srv          *httptest.Server
	sessionCalls int32
	// failSessionsUntil, when > 0, makes the cli-sessions handler return a
	// 500 for the first N calls before succeeding.
	failSessionsUntil int32
	// sessionRequestBlock, when non-nil, makes the cli-sessions handler
	// block (simulating a TCP-connected-but-non-responding endpoint)
	// until the channel is received from or closed.
	sessionRequestBlock <-chan struct{}
	conns               chan *websocket.Conn
}

func newMockHookdeckServer(t *testing.T) *mockHookdeckServer {
	t.Helper()
	m := &mockHookdeckServer{conns: make(chan *websocket.Conn, 10)}
	mux := http.NewServeMux()
	mux.HandleFunc("/cli-sessions", func(w http.ResponseWriter, r *http.Request) {
		if m.sessionRequestBlock != nil {
			select {
			case <-m.sessionRequestBlock:
			case <-r.Context().Done():
				return
			}
		}
		n := atomic.AddInt32(&m.sessionCalls, 1)
		if n <= atomic.LoadInt32(&m.failSessionsUntil) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createSessionResponse{ID: "sess-" + strconv.Itoa(int(n))})
	})
	upgrader := websocket.Upgrader{}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		m.conns <- conn
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockHookdeckServer) wsURL() string {
	return "ws" + strings.TrimPrefix(m.srv.URL, "http")
}

func (m *mockHookdeckServer) nextConn(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-m.conns:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a websocket connection")
		return nil
	}
}

func testConfig(m *mockHookdeckServer) Config {
	return Config{
		APIKey:        "test-key",
		WebhookSecret: testWebhookSecret,
		APIBaseURL:    m.srv.URL,
		WSBaseURL:     m.wsURL(),
		minBackoff:    10 * time.Millisecond,
		maxBackoff:    50 * time.Millisecond,
	}
}

func prOpenedBody(t *testing.T, prNumber int) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"action":       "opened",
		"pull_request": map[string]interface{}{"number": prNumber},
		"repository": map[string]interface{}{
			"name":  "fabrik",
			"owner": map[string]interface{}{"login": "handarbeit"},
		},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return body
}

func sendAttempt(t *testing.T, conn *websocket.Conn, attemptID, deliveryID string, body []byte, sig string) {
	t.Helper()
	attempt := attemptBody{
		CLIPath:   "/",
		EventID:   "evt-" + attemptID,
		AttemptID: attemptID,
		WebhookID: "wh-1",
		Request: attemptRequest{
			Method:     "POST",
			DataString: string(body),
			Headers: map[string]string{
				"X-Hub-Signature-256": sig,
				"X-GitHub-Event":      "pull_request",
				"X-GitHub-Delivery":   deliveryID,
			},
		},
	}
	bodyBytes, err := json.Marshal(attempt)
	if err != nil {
		t.Fatalf("marshal attempt: %v", err)
	}
	frameBytes, err := json.Marshal(wsFrame{Event: wsEventAttempt, Body: bodyBytes})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, frameBytes); err != nil {
		t.Fatalf("write attempt: %v", err)
	}
}

// sendRawAttempt sends an attempt frame built from raw JSON rather than
// through the attemptBody struct, letting a test hand-craft a headers shape
// (e.g. http.Header-style arrays) that flexHeaders's Go struct field would
// never produce on marshal.
func sendRawAttempt(t *testing.T, conn *websocket.Conn, attemptID string, body []byte, headersJSON string) {
	t.Helper()
	bodyJSON := fmt.Sprintf(
		`{"cli_path":"/","event_id":"evt-%s","attempt_id":%q,"webhook_id":"wh-1","request":{"method":"POST","timeout":5,"data_string":%s,"headers":%s}}`,
		attemptID, attemptID, mustMarshalString(t, string(body)), headersJSON,
	)
	frameBytes, err := json.Marshal(wsFrame{Event: wsEventAttempt, Body: json.RawMessage(bodyJSON)})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, frameBytes); err != nil {
		t.Fatalf("write attempt: %v", err)
	}
}

func mustMarshalString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal string: %v", err)
	}
	return string(b)
}

func readAck(t *testing.T, conn *websocket.Conn) attemptResponseBody {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("reading ack: %v", err)
	}
	var frame wsFrame
	if err := json.Unmarshal(msg, &frame); err != nil {
		t.Fatalf("unmarshal ack frame: %v", err)
	}
	if frame.Event != wsEventAttemptResponse {
		t.Fatalf("ack frame event = %q, want %q", frame.Event, wsEventAttemptResponse)
	}
	var resp attemptResponseBody
	if err := json.Unmarshal(frame.Body, &resp); err != nil {
		t.Fatalf("unmarshal ack body: %v", err)
	}
	return resp
}

func runSourceInBackground(t *testing.T, src *Source, sink events.EventSink) (cancel func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		src.Run(ctx, sink)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Source.Run did not return after ctx cancellation")
		}
	})
	return cancel
}

func TestSource_DispatchesValidEvent(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	src := NewSource(testConfig(m))
	runSourceInBackground(t, src, sink)

	conn := m.nextConn(t)
	body := prOpenedBody(t, 42)
	sendAttempt(t, conn, "attempt-1", "delivery-1", body, signBody(body, testWebhookSecret))

	waitUntilCount(t, sink, 1)
	ack := readAck(t, conn)
	if ack.AttemptID != "attempt-1" {
		t.Errorf("ack AttemptID = %q, want 'attempt-1'", ack.AttemptID)
	}
	if ack.Status != http.StatusOK {
		t.Errorf("ack Status = %d, want 200", ack.Status)
	}

	got := sink.snapshot()[0]
	if got.Owner != "handarbeit" || got.Repo != "fabrik" || got.ResourceID != "42" || got.Action != "opened" {
		t.Errorf("unexpected event: %+v", got)
	}
	if got.DeliveryID != "delivery-1" || got.EventType != "pull_request" {
		t.Errorf("unexpected event metadata: %+v", got)
	}
}

func TestSource_DedupesDuplicateDeliveryID(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	src := NewSource(testConfig(m))
	runSourceInBackground(t, src, sink)

	conn := m.nextConn(t)
	body := prOpenedBody(t, 7)
	sig := signBody(body, testWebhookSecret)
	sendAttempt(t, conn, "attempt-1", "delivery-dup", body, sig)
	readAck(t, conn)
	sendAttempt(t, conn, "attempt-2", "delivery-dup", body, sig)
	readAck(t, conn)

	// Give a moment for a wrongly-dispatched second Handle call to land
	// before asserting the negative.
	time.Sleep(50 * time.Millisecond)
	if got := sink.count(); got != 1 {
		t.Errorf("sink.count() = %d, want 1 (duplicate delivery ID must be deduped)", got)
	}
}

// TestSource_MalformedRetryDoesNotBurnDeliveryID guards against dedupe
// marking a delivery ID as seen before normalization has proven the
// payload well-formed: a malformed first attempt must not cause Hookdeck's
// well-formed retry of the very same delivery ID to be silently dropped.
func TestSource_MalformedRetryDoesNotBurnDeliveryID(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	src := NewSource(testConfig(m))
	runSourceInBackground(t, src, sink)

	conn := m.nextConn(t)
	malformed := []byte("not valid json")
	sendAttempt(t, conn, "attempt-1", "delivery-retry", malformed, signBody(malformed, testWebhookSecret))
	readAck(t, conn)

	// Give a moment for a wrongly-dispatched Handle call to land before
	// asserting the malformed attempt was dropped.
	time.Sleep(50 * time.Millisecond)
	if got := sink.count(); got != 0 {
		t.Fatalf("sink.count() = %d after malformed attempt, want 0", got)
	}

	body := prOpenedBody(t, 7)
	sendAttempt(t, conn, "attempt-2", "delivery-retry", body, signBody(body, testWebhookSecret))
	readAck(t, conn)

	waitUntilCount(t, sink, 1)
}

// TestSource_TolerateArrayValuedHeaders guards against a Hookdeck attempt
// frame whose headers use an http.Header-style array-per-key shape instead
// of the flat string-per-key shape normally observed: a per-key shape
// surprise must not fail decoding of the whole frame (which would silently
// drop 100% of event-driven ingestion while transport health still reports
// connected) — the first array element is used, same as any header GitHub
// never sends multi-valued.
func TestSource_TolerateArrayValuedHeaders(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	src := NewSource(testConfig(m))
	runSourceInBackground(t, src, sink)

	conn := m.nextConn(t)
	body := prOpenedBody(t, 11)
	sig := signBody(body, testWebhookSecret)
	headersJSON := fmt.Sprintf(
		`{"X-Hub-Signature-256":%q,"X-GitHub-Event":["pull_request","extra"],"X-GitHub-Delivery":["delivery-array"]}`,
		sig,
	)
	sendRawAttempt(t, conn, "attempt-1", body, headersJSON)
	readAck(t, conn)

	waitUntilCount(t, sink, 1)
	got := sink.snapshot()[0]
	if got.DeliveryID != "delivery-array" || got.EventType != "pull_request" {
		t.Errorf("unexpected event metadata from array-valued headers: %+v", got)
	}
}

func TestSource_DropsInvalidSignature(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	src := NewSource(testConfig(m))
	runSourceInBackground(t, src, sink)

	conn := m.nextConn(t)
	body := prOpenedBody(t, 9)
	sendAttempt(t, conn, "attempt-1", "delivery-bad-sig", body, "sha256="+strings.Repeat("00", 32))

	// The attempt must still be acked at the Hookdeck transport level even
	// though it's dropped from Pruefer's own processing.
	ack := readAck(t, conn)
	if ack.AttemptID != "attempt-1" {
		t.Errorf("ack AttemptID = %q, want 'attempt-1'", ack.AttemptID)
	}

	time.Sleep(50 * time.Millisecond)
	if got := sink.count(); got != 0 {
		t.Errorf("sink.count() = %d, want 0 (invalid signature must be dropped)", got)
	}
}

// captureLogs redirects the package-level Logf hook to a slice for the
// duration of the test, restoring it on cleanup.
func captureLogs(t *testing.T) *[]string {
	t.Helper()
	var logs []string
	var mu sync.Mutex
	old := Logf
	Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { Logf = old })
	return &logs
}

// TestSource_ProcessAttempt_DistinguishesMissingFromInvalidSignature
// guards against a missing X-Hub-Signature-256 header (a Hookdeck-side
// forwarding problem) being logged identically to a present-but-wrong
// signature (a misconfigured hookdeck.webhook_secret_env) — the two need
// different operator fixes, so they must produce distinguishable log
// messages.
func TestSource_ProcessAttempt_DistinguishesMissingFromInvalidSignature(t *testing.T) {
	body := prOpenedBody(t, 1)

	t.Run("missing header", func(t *testing.T) {
		logs := captureLogs(t)
		src := NewSource(Config{WebhookSecret: testWebhookSecret})
		sink := &recordingSink{}
		src.processAttempt(context.Background(), sink, attemptBody{
			Request: attemptRequest{
				DataString: string(body),
				Headers: map[string]string{
					"X-GitHub-Event":    "pull_request",
					"X-GitHub-Delivery": "d-missing-sig",
				},
			},
		})
		if sink.count() != 0 {
			t.Fatalf("sink.count() = %d, want 0", sink.count())
		}
		joined := strings.Join(*logs, "\n")
		if !strings.Contains(joined, "no X-Hub-Signature-256 header") {
			t.Errorf("logs = %v, want a message about a missing signature header", *logs)
		}
		if strings.Contains(joined, "invalid GitHub webhook signature") {
			t.Errorf("logs = %v, missing-header case must not log the wrong-signature message", *logs)
		}
	})

	t.Run("present but wrong", func(t *testing.T) {
		logs := captureLogs(t)
		src := NewSource(Config{WebhookSecret: testWebhookSecret})
		sink := &recordingSink{}
		src.processAttempt(context.Background(), sink, attemptBody{
			Request: attemptRequest{
				DataString: string(body),
				Headers: map[string]string{
					"X-Hub-Signature-256": "sha256=" + strings.Repeat("00", 32),
					"X-GitHub-Event":      "pull_request",
					"X-GitHub-Delivery":   "d-wrong-sig",
				},
			},
		})
		if sink.count() != 0 {
			t.Fatalf("sink.count() = %d, want 0", sink.count())
		}
		joined := strings.Join(*logs, "\n")
		if !strings.Contains(joined, "invalid GitHub webhook signature") {
			t.Errorf("logs = %v, want a message about an invalid signature", *logs)
		}
		if strings.Contains(joined, "no X-Hub-Signature-256 header") {
			t.Errorf("logs = %v, wrong-signature case must not log the missing-header message", *logs)
		}
	})
}

// TestSource_ProcessAttempt_LogsNormalizationFailure guards against a
// malformed-body (or unexpected-content-type) delivery being dropped with
// zero logging, unlike every sibling failure path in processAttempt
// (invalid signature, missing signature header) — a sustained protocol
// drift here would otherwise silently stop all event-driven ingestion while
// transport health still reports connected.
func TestSource_ProcessAttempt_LogsNormalizationFailure(t *testing.T) {
	logs := captureLogs(t)
	src := NewSource(Config{WebhookSecret: testWebhookSecret})
	sink := &recordingSink{}
	body := []byte("not valid json")
	src.processAttempt(context.Background(), sink, attemptBody{
		Request: attemptRequest{
			DataString: string(body),
			Headers: map[string]string{
				"X-Hub-Signature-256": signBody(body, testWebhookSecret),
				"X-GitHub-Event":      "pull_request",
				"X-GitHub-Delivery":   "d-malformed-body",
			},
		},
	})
	if sink.count() != 0 {
		t.Fatalf("sink.count() = %d, want 0 (malformed body must be dropped)", sink.count())
	}
	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, "normalizing webhook payload") {
		t.Errorf("logs = %v, want a message about the normalization failure", *logs)
	}
}

func TestSource_ReconnectsAfterConnectionDrop(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}

	var mu sync.Mutex
	var health []events.HealthEvent
	cfg := testConfig(m)
	cfg.OnHealth = func(ev events.HealthEvent) {
		mu.Lock()
		health = append(health, ev)
		mu.Unlock()
	}
	src := NewSource(cfg)
	runSourceInBackground(t, src, sink)

	conn1 := m.nextConn(t)
	// HealthConnected only fires once the session proves itself alive by
	// reading a frame (see source.go's runOnce), so send one here before
	// dropping the connection — otherwise this connection never counts as
	// "connected" and the assertion below couldn't distinguish a real
	// reconnect from a source that never got past the handshake.
	body1 := prOpenedBody(t, 2)
	sendAttempt(t, conn1, "attempt-0", "delivery-before-drop", body1, signBody(body1, testWebhookSecret))
	waitUntilCount(t, sink, 1)
	conn1.Close() // simulate a connection drop

	conn2 := m.nextConn(t) // Source must reconnect
	body := prOpenedBody(t, 1)
	sendAttempt(t, conn2, "attempt-1", "delivery-after-reconnect", body, signBody(body, testWebhookSecret))
	waitUntilCount(t, sink, 2)

	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		connected := 0
		for _, h := range health {
			if h.State == events.HealthConnected {
				connected++
			}
		}
		return connected >= 2
	})
}

func TestSource_SessionCreationFailureRetriesWithoutPanic(t *testing.T) {
	m := newMockHookdeckServer(t)
	atomic.StoreInt32(&m.failSessionsUntil, 2) // first 2 session creations fail
	sink := &recordingSink{}
	src := NewSource(testConfig(m))
	runSourceInBackground(t, src, sink)

	// A connection only appears once session creation has succeeded twice
	// past failure and Source has retried through the backoff without
	// panicking.
	m.nextConn(t)
}

func TestSource_RunOnce_BoundsHangingSessionCreation(t *testing.T) {
	m := newMockHookdeckServer(t)
	neverReleased := make(chan struct{})
	m.sessionRequestBlock = neverReleased
	t.Cleanup(func() { close(neverReleased) })

	sink := &recordingSink{}
	cfg := testConfig(m)
	cfg.connectTimeout = 100 * time.Millisecond
	src := NewSource(cfg)

	// A generous outer ctx timeout, well past connectTimeout — if
	// connectTimeout weren't bounding the session-creation call
	// independently, runOnce would hang until this ctx expires instead,
	// which the elapsed-time assertion below would catch.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	connected, err := src.runOnce(ctx, sink)
	elapsed := time.Since(start)

	if connected {
		t.Error("connected = true, want false: session creation never completed")
	}
	if err == nil {
		t.Error("err = nil, want non-nil: a hung session-creation call should surface as a timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("runOnce took %s to return, want well under the 5s outer ctx timeout — connectTimeout (100ms) should have bounded session creation independently", elapsed)
	}
}

func TestSource_RunOnce_ReadDeadlineDetectsSilentlyDeadConnection(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	cfg := testConfig(m)
	cfg.pongWait = 100 * time.Millisecond
	cfg.pingPeriod = 20 * time.Millisecond
	src := NewSource(cfg)

	// A generous outer ctx timeout — if the read deadline weren't applied,
	// ReadMessage would block until this ctx expires instead (or forever,
	// against a real non-responding server), which the elapsed-time
	// assertion below would catch.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resultCh := make(chan runOnceResult, 1)
	go func() {
		connected, err := src.runOnce(ctx, sink)
		resultCh <- runOnceResult{connected, err}
	}()

	// Accept the WebSocket upgrade but never send anything on it — no
	// frame, no pong, no close — simulating a connection that died
	// silently (e.g. a NAT timeout dropping packets with no FIN/RST).
	m.nextConn(t)

	start := time.Now()
	select {
	case res := <-resultCh:
		if res.err == nil {
			t.Error("err = nil, want non-nil: a silently-dead connection should surface as a read-deadline error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runOnce did not return after the read deadline should have elapsed")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("runOnce took %s to detect the dead connection, want well under the 5s outer ctx timeout — pongWait (100ms) should have bounded it", elapsed)
	}
}

type runOnceResult struct {
	connected bool
	err       error
}

func TestSource_RunOnce_NotConnectedOnImmediateDrop(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	src := NewSource(testConfig(m))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resultCh := make(chan runOnceResult, 1)
	go func() {
		connected, err := src.runOnce(ctx, sink)
		resultCh <- runOnceResult{connected, err}
	}()

	// Simulate Hookdeck accepting the WebSocket handshake and then
	// immediately rejecting/dropping the session (e.g. a stale API key) —
	// no attempt frame is ever sent.
	conn := m.nextConn(t)
	conn.Close()

	select {
	case res := <-resultCh:
		if res.connected {
			t.Error("connected = true, want false: a handshake with no successfully-read frame must not count as connected")
		}
		if res.err == nil {
			t.Error("err = nil, want non-nil: the immediate drop should surface as a read error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runOnce did not return after the connection was dropped")
	}
}

func TestSource_RunOnce_NoHealthConnectedOnImmediateDrop(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	cfg := testConfig(m)

	var mu sync.Mutex
	var health []events.HealthEvent
	cfg.OnHealth = func(ev events.HealthEvent) {
		mu.Lock()
		health = append(health, ev)
		mu.Unlock()
	}
	src := NewSource(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resultCh := make(chan runOnceResult, 1)
	go func() {
		connected, err := src.runOnce(ctx, sink)
		resultCh <- runOnceResult{connected, err}
	}()

	// A handshake that's immediately dropped with no frame ever read must
	// not report HealthConnected — Daemon.HealthHandler treats that
	// transition as a signal to run a reconciliation poll, which should
	// only fire once the session has proven itself alive.
	conn := m.nextConn(t)
	conn.Close()

	select {
	case <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("runOnce did not return after the connection was dropped")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, h := range health {
		if h.State == events.HealthConnected {
			t.Errorf("got HealthConnected on a handshake-only drop, want none: %+v", health)
		}
	}
}

func TestSource_RunOnce_ConnectedAfterFirstFrame(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	src := NewSource(testConfig(m))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resultCh := make(chan runOnceResult, 1)
	go func() {
		connected, err := src.runOnce(ctx, sink)
		resultCh <- runOnceResult{connected, err}
	}()

	conn := m.nextConn(t)
	body := prOpenedBody(t, 5)
	sendAttempt(t, conn, "attempt-1", "delivery-1", body, signBody(body, testWebhookSecret))
	readAck(t, conn)
	conn.Close() // drop only after a frame was successfully received

	select {
	case res := <-resultCh:
		if !res.connected {
			t.Error("connected = false, want true: a frame was read successfully before the drop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runOnce did not return after the connection was dropped")
	}
}

func TestSource_RunOnce_ConnectedAfterGracePeriodWithNoFrames(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	cfg := testConfig(m)
	cfg.aliveGracePeriod = 30 * time.Millisecond

	var mu sync.Mutex
	var health []events.HealthEvent
	cfg.OnHealth = func(ev events.HealthEvent) {
		mu.Lock()
		health = append(health, ev)
		mu.Unlock()
	}
	src := NewSource(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resultCh := make(chan runOnceResult, 1)
	go func() {
		connected, err := src.runOnce(ctx, sink)
		resultCh <- runOnceResult{connected, err}
	}()

	conn := m.nextConn(t)
	// Keep the connection open well past aliveGracePeriod without ever
	// sending an attempt frame — simulates a genuinely healthy but idle
	// session (e.g. a quiet period with no forwarded webhooks), which
	// ReadMessage alone could never distinguish from a handshake that's
	// about to be dropped.
	time.Sleep(150 * time.Millisecond)
	conn.Close()

	select {
	case res := <-resultCh:
		if !res.connected {
			t.Error("connected = false, want true: the connection survived past aliveGracePeriod with no error, so it should be proven alive even without a frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runOnce did not return after the connection was dropped")
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, h := range health {
		if h.State == events.HealthConnected {
			found = true
		}
	}
	if !found {
		t.Error("no HealthConnected event emitted despite the connection surviving past aliveGracePeriod")
	}
}

// deadlineRecordingConn wraps a net.Conn to record every SetWriteDeadline
// call — used to verify ack bounds its WriteMessage call, since a stuck
// write (peer stops reading) would otherwise block the single read-loop
// goroutine forever with no way to observe it from the outside.
type deadlineRecordingConn struct {
	net.Conn
	mu             sync.Mutex
	writeDeadlines []time.Time
}

func (c *deadlineRecordingConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadlines = append(c.writeDeadlines, t)
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *deadlineRecordingConn) recordedDeadlines() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Time, len(c.writeDeadlines))
	copy(out, c.writeDeadlines)
	return out
}

func TestSource_Ack_SetsWriteDeadline(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	cfg := testConfig(m)
	cfg.writeWait = 250 * time.Millisecond

	var wrapped *deadlineRecordingConn
	var mu sync.Mutex
	cfg.Dialer = &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			wrapped = &deadlineRecordingConn{Conn: c}
			mu.Unlock()
			return wrapped, nil
		},
	}
	src := NewSource(cfg)
	runSourceInBackground(t, src, sink)

	conn := m.nextConn(t)
	body := prOpenedBody(t, 7)
	before := time.Now()
	sendAttempt(t, conn, "attempt-1", "delivery-1", body, signBody(body, testWebhookSecret))
	waitUntilCount(t, sink, 1)
	readAck(t, conn)

	mu.Lock()
	w := wrapped
	mu.Unlock()
	if w == nil {
		t.Fatal("client-side connection was never captured")
	}
	deadlines := w.recordedDeadlines()
	if len(deadlines) == 0 {
		t.Fatal("ack did not call conn.SetWriteDeadline — a stuck write to a non-reading peer could block the read loop forever")
	}
	maxWant := before.Add(cfg.writeWait + time.Second) // generous slack for scheduling jitter
	for _, d := range deadlines {
		if d.After(maxWant) {
			t.Errorf("recorded write deadline %v is later than expected bound %v (writeWait = %s)", d, maxWant, cfg.writeWait)
		}
	}
}

// failingWriteConn wraps a net.Conn so Write can be switched, after the
// fact, into always failing — used to simulate a broken outbound pipe
// discovered only when the idle ping tries to use it.
type failingWriteConn struct {
	net.Conn
	fail atomic.Bool
}

func (c *failingWriteConn) Write(b []byte) (int, error) {
	if c.fail.Load() {
		return 0, fmt.Errorf("simulated broken pipe")
	}
	return c.Conn.Write(b)
}

// TestSource_PingWriteFailureClosesConnectionPromptly guards against a
// regression where a failed idle-ping write left conn open: the read loop's
// blocked ReadMessage would then only fail once the existing pongWait read
// deadline (up to 60s in production) finally lapsed, instead of failing
// fast the way a ctx-cancellation already does (which explicitly closes
// conn). pongWait is set long here specifically so a pass can only mean the
// ping-failure path itself closed the connection, not the read deadline.
func TestSource_PingWriteFailureClosesConnectionPromptly(t *testing.T) {
	m := newMockHookdeckServer(t)
	sink := &recordingSink{}
	cfg := testConfig(m)
	cfg.pongWait = 5 * time.Second
	cfg.pingPeriod = 20 * time.Millisecond
	cfg.writeWait = 100 * time.Millisecond

	var wrapped *failingWriteConn
	var mu sync.Mutex
	cfg.Dialer = &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			w := &failingWriteConn{Conn: c}
			mu.Lock()
			wrapped = w
			mu.Unlock()
			return w, nil
		},
	}
	src := NewSource(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resultCh := make(chan runOnceResult, 1)
	go func() {
		connected, err := src.runOnce(ctx, sink)
		resultCh <- runOnceResult{connected, err}
	}()

	m.nextConn(t) // handshake completed — dial has returned our wrapped conn

	mu.Lock()
	wrapped.fail.Store(true)
	mu.Unlock()

	start := time.Now()
	select {
	case res := <-resultCh:
		if res.err == nil {
			t.Error("err = nil, want non-nil: a failed ping write should close the connection and surface as a read error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runOnce did not return after the ping write should have failed and closed the connection")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("runOnce took %s to detect the broken write, want well under pongWait (%s) — a failed ping write must close conn so the read loop fails fast instead of waiting out the read deadline", elapsed, cfg.pongWait)
	}
}
