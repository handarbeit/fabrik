package engine

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/events"
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

// startDirectReceiver starts a loopback receiver (no sink wired; only the coverage surface is used).
func startDirectReceiver(t *testing.T) *appWebhookTransport {
	t.Helper()
	tr := newAppWebhookTransport("direct", directTestPath,
		func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		directTestSecret, func(int, string, string, ...any) {}, nil)
	if err := tr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(tr.Stop)
	return tr
}

// Coverage generalised to the direct transport: a managed repo the installation
// does not grant is warned about under the transport's own panel type.
func TestDirectTransport_InstallationCoverage(t *testing.T) {
	isolateWarnings(t)
	orig := fetchInstallationRepositoriesFn
	defer func() { fetchInstallationRepositoriesFn = orig }()
	granted := []string{"a/one"}
	fetchInstallationRepositoriesFn = func(string, string) ([]string, bool, error) { return granted, false, nil }
	h := startDirectReceiver(t)
	h.UpdateRepos(map[string]bool{"a/one": true, "b/two": true})
	e := &Engine{hostClient: gh.NewClient("fake-installation-token")}
	spec := appCoverageSpec{Tag: "direct", PanelType: "direct_coverage", Ref: "#2142"}

	e.checkAppInstallationCoverage(h, spec)
	if k := warningKeys(t); len(k) != 1 || !k["direct_coverage:repos"] {
		t.Fatalf("keys = %v", k)
	}
	granted = []string{"a/one", "b/two"}
	e.checkAppInstallationCoverage(h, spec)
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
	// US4.2: the failed listener is still a health row, "not listening", while
	// the aggregate mode stays "off" (nothing is attached).
	rows := eng.transports.HealthRows()
	if len(rows) != 1 || rows[0].Name != "direct" || rows[0].State != "not listening" {
		t.Fatalf("rows after a bind failure = %#v, want one direct row, not listening", rows)
	}
	if m := eng.transports.Mode(); m != "off" {
		t.Fatalf("mode = %q, want off (failed transports are not attached)", m)
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

// The drift panel entry is persisted; a restarted daemon has no streak, so the
// in-memory tracker would never resolve it. A successful start clears it.
func TestDirectTransport_StartClearsStaleDriftEntry(t *testing.T) {
	eng, cleanup := startDirectEngine(t, "127.0.0.1:0")
	tr := directFromEngine(t, eng)
	h := &directHarness{tr: tr, url: "http://" + tr.Addr() + directTestPath, logs: &syncLog{}}
	for i := 0; i < events.SignatureDriftThreshold; i++ {
		h.post(t, []byte(`{"a":1}`), "sha256=bad", "issues", "f")
	}
	if k := warningKeys(t); !k["direct_signature_drift:direct"] {
		t.Fatalf("keys = %v", k)
	}
	cleanup() // the "restart": the panel entry outlives the process

	eng2 := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng2.fabrikDir = t.TempDir()
	eng2.cfg.EventSource = EventSourceDirect
	eng2.cfg.DirectListen = "127.0.0.1:0"
	eng2.cfg.AppWebhookPath = directTestPath
	cache, _ := eng2.UseBoardCacheForTest()
	cleanup2 := eng2.setUpIngestion(context.Background(), cache)
	defer cleanup2()
	if eng2.transports.Empty() {
		t.Fatal("second start should have bound")
	}
	if k := warningKeys(t); k["direct_signature_drift:direct"] {
		t.Fatalf("stale drift entry survived a successful start: %v", k)
	}
}
