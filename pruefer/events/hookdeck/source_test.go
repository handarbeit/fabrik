package hookdeck

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
