package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/events"
	"github.com/handarbeit/fabrik/tui"
)

const (
	directTestSecret = "direct-test-secret-value"
	directTestPath   = "/fabrik-test"
)

func signDirect(body []byte) string {
	m := hmac.New(sha256.New, []byte(directTestSecret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

type directHarness struct {
	tr   *appWebhookTransport
	mu   sync.Mutex
	got  []delivery
	url  string
	logs *syncLog
}

type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.WriteString(strings.TrimSpace(fmt.Sprintf(format, args...)) + "\n")
}
func (l *syncLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func (h *directHarness) deliveries() []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]delivery(nil), h.got...)
}

func startDirect(t *testing.T) *directHarness {
	t.Helper()
	h := &directHarness{logs: &syncLog{}}
	h.tr = newAppWebhookTransport("direct", directTestPath,
		func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		directTestSecret,
		func(_ int, _ string, format string, args ...any) { h.logs.add(format, args...) },
		func(tui.Event) {})
	h.tr.SetSink(func(d delivery) {
		h.mu.Lock()
		h.got = append(h.got, d)
		h.mu.Unlock()
	})
	if err := h.tr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(h.tr.Stop)
	h.url = "http://" + h.tr.Addr() + directTestPath
	return h
}

func (h *directHarness) post(t *testing.T, body []byte, sig, event, id string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.url, bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestDirectTransport_ValidDeliveryIsSubmittedAndAcked(t *testing.T) {
	h := startDirect(t)
	body := []byte(`{"action":"opened","issue":{"number":1}}`)
	resp := h.post(t, body, signDirect(body), "issues", "d-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := h.deliveries()
	if len(got) != 1 || got[0].ID != "d-1" || got[0].EventType != "issues" || got[0].Transport != "direct" || string(got[0].Payload) != string(body) {
		t.Fatalf("deliveries = %#v", got)
	}
	row := h.tr.HealthRow()
	if row.Name != "direct" || row.State != "listening" || !row.LastDeliveryAt.Valid {
		t.Fatalf("row = %#v", row)
	}
}

// The transport adds no dedupe of its own: the same ID submitted twice reaches
// the sink twice, and the engine's ledger is what drops the second.
func TestDirectTransport_AddsNoDedupe(t *testing.T) {
	h := startDirect(t)
	body := []byte(`{"action":"opened"}`)
	for i := 0; i < 2; i++ {
		if r := h.post(t, body, signDirect(body), "issues", "same"); r.StatusCode != 200 {
			t.Fatalf("status %d", r.StatusCode)
		}
	}
	if n := len(h.deliveries()); n != 2 {
		t.Fatalf("sink received %d, want 2 (dedupe is the intake's)", n)
	}
}

func TestDirectTransport_BadRequests(t *testing.T) {
	h := startDirect(t)
	good := []byte(`{"a":1}`)

	t.Run("wrong signature 401", func(t *testing.T) {
		if r := h.post(t, good, "sha256=deadbeef", "issues", "x"); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d", r.StatusCode)
		}
	})
	t.Run("missing signature 401", func(t *testing.T) {
		if r := h.post(t, good, "", "issues", "x"); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d", r.StatusCode)
		}
	})
	t.Run("non-JSON 400 naming the fix", func(t *testing.T) {
		form := []byte("payload=%7B%22a%22%3A1%7D")
		r := h.post(t, form, signDirect(form), "issues", "x")
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d", r.StatusCode)
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "application/json") || !strings.Contains(h.logs.String(), "application/json") {
			t.Fatalf("response %q / logs %q do not name the content-type fix", b, h.logs.String())
		}
	})
	t.Run("wrong path 404", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+h.tr.Addr()+"/other", bytes.NewReader(good))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("wrong method 405", func(t *testing.T) {
		resp, err := http.Get(h.url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	if n := len(h.deliveries()); n != 0 {
		t.Fatalf("%d bad requests reached the sink", n)
	}
	row := h.tr.HealthRow()
	if row.RejectedSignatures.V != 2 || row.MalformedRequests.V != 1 || row.OversizedRequests.V != 0 {
		t.Fatalf("counts = %d/%d/%d, want 2/1/0", row.RejectedSignatures.V, row.MalformedRequests.V, row.OversizedRequests.V)
	}
}

// 413 comes before verification (an unsigned oversized body is 413, not 401),
// is not a signature failure, and a body exactly at the cap is accepted.
func TestDirectTransport_BodyCap(t *testing.T) {
	h := startDirect(t)
	atCap := append([]byte(`"`), bytes.Repeat([]byte("a"), webhookMaxBodyBytes-2)...)
	atCap = append(atCap, '"')
	if len(atCap) != webhookMaxBodyBytes {
		t.Fatalf("fixture is %d bytes", len(atCap))
	}
	if r := h.post(t, atCap, signDirect(atCap), "issues", "cap"); r.StatusCode != http.StatusOK {
		t.Fatalf("body exactly at the cap: status %d, want 200", r.StatusCode)
	}
	over := append(atCap, ' ')
	for i := 0; i < events.SignatureDriftThreshold+2; i++ {
		if r := h.post(t, over, "", "issues", "over"); r.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized: status %d, want 413 before verification", r.StatusCode)
		}
	}
	row := h.tr.HealthRow()
	if row.OversizedRequests.V != int64(events.SignatureDriftThreshold+2) || row.RejectedSignatures.V != 0 {
		t.Fatalf("row = %#v", row)
	}
	if row.State != "listening" {
		t.Fatalf("oversized requests must not advance signature drift; state = %q", row.State)
	}
}

func TestDirectTransport_Ping(t *testing.T) {
	h := startDirect(t)
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	if r := h.post(t, body, signDirect(body), "ping", "p1"); r.StatusCode != http.StatusOK {
		t.Fatalf("good ping: status %d", r.StatusCode)
	}
	if n := len(h.deliveries()); n != 0 {
		t.Fatalf("ping was submitted (%d)", n)
	}
	if !strings.Contains(h.logs.String(), "ping") {
		t.Fatalf("ping not logged: %q", h.logs.String())
	}
	if h.tr.HealthRow().LastDeliveryAt.Valid {
		t.Fatal("a ping must not count as a delivery")
	}
	if r := h.post(t, body, "sha256=bad", "ping", "p2"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad ping: status %d, want 401", r.StatusCode)
	}
}

func TestDirectTransport_DriftAndRecovery(t *testing.T) {
	h := startDirect(t)
	var transitions []bool
	var tmu sync.Mutex
	h.tr.SetDriftHandler(func(active bool) { tmu.Lock(); transitions = append(transitions, active); tmu.Unlock() })
	body := []byte(`{"a":1}`)

	for i := 0; i < events.SignatureDriftThreshold-1; i++ {
		h.post(t, body, "sha256=bad", "issues", "f")
	}
	if !h.tr.IsHealthyOrStartingUp() || h.tr.HealthRow().State != "listening" {
		t.Fatal("drift reported before the threshold")
	}
	// A good delivery between failures resets the streak.
	h.post(t, body, signDirect(body), "issues", "ok")
	for i := 0; i < events.SignatureDriftThreshold-1; i++ {
		h.post(t, body, "sha256=bad", "issues", "f")
	}
	tmu.Lock()
	n := len(transitions)
	tmu.Unlock()
	if n != 0 {
		t.Fatalf("drift fired despite an interleaved success: %v", transitions)
	}
	h.post(t, body, "sha256=bad", "issues", "f") // the threshold'th consecutive failure
	if h.tr.IsHealthyOrStartingUp() || h.tr.HealthRow().State != "signature drift" {
		t.Fatalf("not in drift: healthy=%v row=%#v", h.tr.IsHealthyOrStartingUp(), h.tr.HealthRow())
	}
	// A reconcile "healthy" hint must not mask drift.
	h.tr.TransitionHealth(true, "")
	if h.tr.IsHealthyOrStartingUp() || h.tr.HealthRow().State != "signature drift" {
		t.Fatal("a healthy reconcile hint masked signature drift")
	}
	h.post(t, body, signDirect(body), "issues", "ok2")
	if !h.tr.IsHealthyOrStartingUp() || h.tr.HealthRow().State != "listening" {
		t.Fatalf("did not recover: %#v", h.tr.HealthRow())
	}
	tmu.Lock()
	defer tmu.Unlock()
	if len(transitions) != 2 || !transitions[0] || transitions[1] {
		t.Fatalf("transitions = %v, want [true false]", transitions)
	}
}

func TestDirectTransport_CacheDriftHintAffectsAggregateNotRowName(t *testing.T) {
	h := startDirect(t)
	h.tr.TransitionHealth(false, "drift found")
	if h.tr.IsHealthyOrStartingUp() {
		t.Fatal("cache drift should make the aggregate unhealthy")
	}
	if got := h.tr.HealthRow().State; got != "listening" {
		t.Fatalf("row state = %q, want listening", got)
	}
	h.tr.TransitionHealth(true, "")
	if !h.tr.IsHealthyOrStartingUp() {
		t.Fatal("did not recover")
	}
}

// Concurrent requests: -race clean, and the drift transitions fire exactly once.
func TestDirectTransport_ConcurrentRequests(t *testing.T) {
	h := startDirect(t)
	var activations atomic.Int32
	h.tr.SetDriftHandler(func(active bool) {
		if active {
			activations.Add(1)
		}
	})
	body := []byte(`{"a":1}`)
	var wg sync.WaitGroup
	for i := 0; i < events.SignatureDriftThreshold*4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, directTestPath, bytes.NewReader(body))
			req.Header.Set("X-Hub-Signature-256", "sha256=bad")
			h.tr.handle(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if activations.Load() != 1 {
		t.Fatalf("drift activations = %d, want 1", activations.Load())
	}
}

// Concurrent failures and successes: whatever the interleaving, the last
// reported drift state equals the tracker's final state (no stale warning after
// recovery) and callbacks alternate.
func TestDirectTransport_DriftCallbacksStayOrderedUnderConcurrency(t *testing.T) {
	h := startDirect(t)
	var mu sync.Mutex
	var seen []bool
	h.tr.SetDriftHandler(func(active bool) {
		mu.Lock()
		seen = append(seen, active)
		mu.Unlock()
	})
	body := []byte(`{"a":1}`)
	good := signDirect(body)
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		for i := 0; i < events.SignatureDriftThreshold*2; i++ {
			sig := "sha256=bad"
			if i%7 == 0 {
				sig = good
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, directTestPath, bytes.NewReader(body))
				req.Header.Set("X-Hub-Signature-256", sig)
				h.tr.handle(rec, req)
			}()
		}
		wg.Wait()
		mu.Lock()
		last := len(seen) > 0 && seen[len(seen)-1]
		for i := 1; i < len(seen); i++ {
			if seen[i] == seen[i-1] {
				t.Fatalf("round %d: drift callbacks repeated a state: %v", round, seen)
			}
		}
		mu.Unlock()
		if last != h.tr.sigDrift.Active() {
			t.Fatalf("round %d: last reported drift=%v, tracker active=%v", round, last, h.tr.sigDrift.Active())
		}
	}
}

func TestDirectTransport_ListenFailureIsAStartError(t *testing.T) {
	boom := errors.New("address already in use")
	tr := newAppWebhookTransport("direct", directTestPath, func() (net.Listener, error) { return nil, boom }, directTestSecret,
		func(int, string, string, ...any) {}, nil)
	if err := tr.Start(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Start err = %v", err)
	}
	if row := tr.HealthRow(); row.State != "not listening" {
		t.Fatalf("state = %q, want not listening", row.State)
	}
	if tr.IsHealthyOrStartingUp() {
		t.Fatal("a transport that never bound must not widen the idle cap")
	}
	tr.Stop() // must not panic with no server
}

func TestDirectTransport_StopStopsListening(t *testing.T) {
	h := startDirect(t)
	addr := h.tr.Addr()
	h.tr.Stop()
	if row := h.tr.HealthRow(); row.State != "not listening" {
		t.Fatalf("state after Stop = %q", row.State)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("listener still accepting after Stop")
	}
}

func TestDirectTransport_NameIsConstructorSet(t *testing.T) {
	tr := newAppWebhookTransport("tailscale", "/x", nil, "s", func(int, string, string, ...any) {}, nil)
	if tr.Name() != "tailscale" || tr.HealthRow().Name != "tailscale" {
		t.Fatal("name not taken from the constructor")
	}
}

// Coverage generalised to the direct transport: a managed repo the installation
// does not grant is warned about under the transport's own panel type.
func TestDirectTransport_InstallationCoverage(t *testing.T) {
	isolateWarnings(t)
	orig := fetchInstallationRepositoriesFn
	defer func() { fetchInstallationRepositoriesFn = orig }()
	granted := []string{"a/one"}
	fetchInstallationRepositoriesFn = func(string, string) ([]string, bool, error) { return granted, false, nil }
	h := startDirect(t)
	h.tr.UpdateRepos(map[string]bool{"a/one": true, "b/two": true})
	e := &Engine{hostClient: gh.NewClient("fake-installation-token")}
	spec := appCoverageSpec{Tag: "direct", PanelType: "direct_coverage", Ref: "#2142"}

	e.checkAppInstallationCoverage(h.tr, spec)
	if k := warningKeys(t); len(k) != 1 || !k["direct_coverage:repos"] {
		t.Fatalf("keys = %v", k)
	}
	granted = []string{"a/one", "b/two"}
	e.checkAppInstallationCoverage(h.tr, spec)
	if k := warningKeys(t); len(k) != 0 {
		t.Fatalf("not cleared: %v", k)
	}
}

// startDirectEngine builds an engine configured for event_source: direct and runs
// its ingestion setup against a real loopback port.
func startDirectEngine(t *testing.T, listen string) (*Engine, func()) {
	t.Helper()
	isolateWarnings(t)
	t.Setenv(DefaultAppWebhookSecretEnv, directTestSecret)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()
	eng.cfg.EventSource = EventSourceDirect
	eng.cfg.DirectListen = listen
	eng.cfg.AppWebhookPath = directTestPath
	cache, _ := eng.UseBoardCacheForTest()
	cleanup := eng.setUpIngestion(context.Background(), cache)
	return eng, cleanup
}

func directFromEngine(t *testing.T, eng *Engine) *appWebhookTransport {
	t.Helper()
	for _, tr := range eng.transports.snapshot() {
		if a, ok := tr.(*appWebhookTransport); ok {
			return a
		}
	}
	t.Fatal("no direct transport attached")
	return nil
}

// SC-001: a correctly signed issues payload POSTed to the listen address is
// applied to the board cache through the real intake, and a duplicate delivery
// ID is dropped by the engine's ledger.
func TestDirectTransport_EndToEndIntoBoardCache(t *testing.T) {
	eng, cleanup := startDirectEngine(t, "127.0.0.1:0")
	defer cleanup()
	tr := directFromEngine(t, eng)
	h := &directHarness{tr: tr, url: "http://" + tr.Addr() + directTestPath, logs: &syncLog{}}

	body := []byte(`{"action":"opened","repository":{"full_name":"o/r"},"issue":{"number":77,"node_id":"I_77","title":"direct works","labels":[{"name":"direct-e2e"}],"assignees":[],"milestone":null,"updated_at":"2026-10-22T03:00:00Z"}}`)
	if r := h.post(t, body, signDirect(body), "issues", "e2e-1"); r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	eng.intake.Drain()
	snap, ok := eng.store.Peek("o/r", 77)
	if !ok || len(snap.Labels()) != 1 || snap.Labels()[0] != "direct-e2e" {
		t.Fatalf("issue not applied to the board cache (ok=%v)", ok)
	}
	if eng.intake.ledger.Len() != 1 {
		t.Fatalf("ledger = %d, want 1", eng.intake.ledger.Len())
	}
	// The same delivery ID again: acked, but the ledger drops it.
	if r := h.post(t, body, signDirect(body), "issues", "e2e-1"); r.StatusCode != http.StatusOK {
		t.Fatalf("duplicate status %d", r.StatusCode)
	}
	eng.intake.Drain()
	if eng.intake.ledger.Len() != 1 {
		t.Fatalf("duplicate was recorded: ledger = %d", eng.intake.ledger.Len())
	}
	// The health surface lists the row.
	rows := eng.transports.HealthRows()
	if len(rows) != 1 || rows[0].Name != "direct" || rows[0].State != "listening" {
		t.Fatalf("rows = %#v", rows)
	}
}

// SC-005: a listen failure leaves the daemon poll-only with a panel entry naming
// the address; a later successful start resolves it.
func TestDirectTransport_ListenFailureIsPollOnlyWithPanelEntry(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := blocker.Addr().String()

	eng, cleanup := startDirectEngine(t, addr)
	defer cleanup()
	if !eng.transports.Empty() {
		t.Fatal("a transport that could not bind must not be attached")
	}
	if eng.echo != nil || eng.intake != nil {
		t.Fatal("poll-only: no echo registry or intake")
	}
	if k := warningKeys(t); !k["direct_listen:"+addr] {
		t.Fatalf("keys = %v, want direct_listen:%s", k, addr)
	}

	// Free the port and start again: the entry resolves.
	blocker.Close()
	eng2 := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng2.fabrikDir = t.TempDir()
	eng2.cfg.EventSource = EventSourceDirect
	eng2.cfg.DirectListen = addr
	cleanup2 := eng2.setUpIngestion(context.Background(), nil)
	defer cleanup2()
	if eng2.transports.Empty() {
		t.Fatal("second start should have bound")
	}
	if k := warningKeys(t); k["direct_listen:"+addr] {
		t.Fatalf("listen entry not resolved: %v", k)
	}
}

// A malformed address is a runtime listen failure too (FR-015), not a hard refusal.
func TestDirectTransport_MalformedAddressIsPollOnly(t *testing.T) {
	eng, cleanup := startDirectEngine(t, "not-an-address")
	defer cleanup()
	if !eng.transports.Empty() {
		t.Fatal("must be poll-only")
	}
	if k := warningKeys(t); !k["direct_listen:not-an-address"] {
		t.Fatalf("keys = %v", k)
	}
}

// SC-002: a streak at the threshold writes the panel entry; a good delivery clears it.
func TestDirectTransport_DriftPanelEntry(t *testing.T) {
	eng, cleanup := startDirectEngine(t, "127.0.0.1:0")
	defer cleanup()
	tr := directFromEngine(t, eng)
	h := &directHarness{tr: tr, url: "http://" + tr.Addr() + directTestPath, logs: &syncLog{}}
	body := []byte(`{"a":1}`)
	for i := 0; i < events.SignatureDriftThreshold; i++ {
		h.post(t, body, "sha256=bad", "issues", "f")
	}
	if k := warningKeys(t); !k["direct_signature_drift:direct"] {
		t.Fatalf("keys = %v", k)
	}
	h.post(t, body, signDirect(body), "issues", "ok")
	if k := warningKeys(t); k["direct_signature_drift:direct"] {
		t.Fatalf("drift entry not resolved: %v", k)
	}
}
