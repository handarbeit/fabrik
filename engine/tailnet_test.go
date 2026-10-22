package engine

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// fakeTailnet is a tailnetNode with no tailnet: the Funnel listener is a
// loopback port, and the test drives connection changes by hand.
type fakeTailnet struct {
	cfg       tailnetNodeConfig
	dnsName   string
	upErr     error
	listenErr error

	closed   atomic.Int32
	upCalled atomic.Int32

	mu      sync.Mutex
	watchFn func(tailnetState)
	ready   chan struct{}
}

func newFakeTailnet(cfg tailnetNodeConfig, dns string) *fakeTailnet {
	return &fakeTailnet{cfg: cfg, dnsName: dns, ready: make(chan struct{})}
}

func (f *fakeTailnet) Up(context.Context) (tailnetState, error) {
	f.upCalled.Add(1)
	if f.upErr != nil {
		return tailnetState{}, f.upErr
	}
	return tailnetState{Connected: true, BackendState: "Running", DNSName: f.dnsName, Funnel: tailnetFunnelOK}, nil
}

func (f *fakeTailnet) ListenFunnel(addr string) (net.Listener, error) {
	if f.listenErr != nil {
		return nil, f.listenErr
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func (f *fakeTailnet) Watch(ctx context.Context, fn func(tailnetState)) error {
	f.mu.Lock()
	f.watchFn = fn
	f.mu.Unlock()
	close(f.ready)
	<-ctx.Done()
	return nil
}

func (f *fakeTailnet) Close() error { f.closed.Add(1); return nil }

func (f *fakeTailnet) emit(st tailnetState) {
	<-f.ready
	f.mu.Lock()
	fn := f.watchFn
	f.mu.Unlock()
	fn(st)
}

// installFakeTailnet replaces the node factory for one test and returns the
// nodes it built.
func installFakeTailnet(t *testing.T, build func(cfg tailnetNodeConfig) *fakeTailnet) *[]*fakeTailnet {
	t.Helper()
	var nodes []*fakeTailnet
	var mu sync.Mutex
	orig, origBuilt := tailnetNodeFactory, tailscaleBuilt
	tailscaleBuilt = true
	tailnetNodeFactory = func(cfg tailnetNodeConfig) (tailnetNode, error) {
		n := build(cfg)
		mu.Lock()
		nodes = append(nodes, n)
		mu.Unlock()
		return n, nil
	}
	t.Cleanup(func() { tailnetNodeFactory, tailscaleBuilt = orig, origBuilt })
	return &nodes
}

const tailscaleTestKey = "tskey-auth-SECRETVALUE-do-not-log"

func startTailscaleEngine(t *testing.T) (*Engine, func(), *syncLog) {
	t.Helper()
	isolateWarnings(t)
	t.Setenv(DefaultAppWebhookSecretEnv, directTestSecret)
	t.Setenv(DefaultTailscaleAuthKeyEnv, tailscaleTestKey)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()
	eng.cfg.EventSource = EventSourceTailscale
	eng.cfg.TailscaleHostname = "fabrik-models"
	eng.cfg.AppWebhookPath = directTestPath
	logs := &syncLog{}
	eng.SetLogSinkForTest(func(_ int, tag, msg string) { logs.add("[%s] %s", tag, msg) })
	// No GitHub App in the test engine: R7 and coverage must not reach for one.
	orig := fetchAppHookConfigFn
	fetchAppHookConfigFn = func(*Engine) (gh.AppHookConfig, error) { return gh.AppHookConfig{}, errors.New("no app") }
	t.Cleanup(func() { fetchAppHookConfigFn = orig })
	cache, _ := eng.UseBoardCacheForTest()
	cleanup := eng.setUpIngestion(context.Background(), cache)
	return eng, cleanup, logs
}

func tailscaleFromEngine(t *testing.T, eng *Engine) *appWebhookTransport {
	t.Helper()
	for _, tr := range eng.transports.snapshot() {
		if a, ok := tr.(*appWebhookTransport); ok && a.Name() == "tailscale" {
			return a
		}
	}
	t.Fatal("no tailscale transport attached")
	return nil
}

// US1, SC-001 (seam-level): the daemon logs the Funnel URL built from the name
// the node REPORTS (tsnet suffixes a taken hostname), and a signed delivery to
// the listener is applied through the real intake. The auth key is never logged
// and reaches only the node.
func TestTailscale_StartsLogsReportedURLAndAppliesDelivery(t *testing.T) {
	nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet {
		return newFakeTailnet(cfg, "fabrik-models-1.tail1234.ts.net.")
	})
	eng, cleanup, logs := startTailscaleEngine(t)
	defer cleanup()
	if len(*nodes) != 1 {
		t.Fatalf("nodes built = %d", len(*nodes))
	}
	n := (*nodes)[0]
	if n.cfg.Hostname != "fabrik-models" || n.cfg.AuthKey != tailscaleTestKey {
		t.Fatalf("node config = hostname %q, key passed %v", n.cfg.Hostname, n.cfg.AuthKey == tailscaleTestKey)
	}
	if want := filepath.Join(eng.fabrikDir, DefaultTailscaleStateDir); n.cfg.StateDir != want {
		t.Fatalf("state dir = %q, want %q", n.cfg.StateDir, want)
	}
	if fi, err := os.Stat(n.cfg.StateDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir not created 0700: %v %v", fi, err)
	}
	out := logs.String()
	if want := "https://fabrik-models-1.tail1234.ts.net" + directTestPath; strings.Count(out, "App webhook URL: "+want) != 1 {
		t.Fatalf("startup log lacks exactly one URL line %q:\n%s", want, out)
	}
	if strings.Contains(out, tailscaleTestKey) {
		t.Fatalf("auth key leaked into the log:\n%s", out)
	}

	tr := tailscaleFromEngine(t, eng)
	h := &directHarness{tr: tr, url: "http://" + tr.Addr() + directTestPath, logs: &syncLog{}}
	body := []byte(`{"action":"opened","repository":{"full_name":"o/r"},"issue":{"number":91,"node_id":"I_91","title":"ts works","labels":[{"name":"ts-e2e"}],"assignees":[],"milestone":null,"updated_at":"2026-10-22T03:00:00Z"}}`)
	if r := h.post(t, body, signDirect(body), "issues", "ts-1"); r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	eng.intake.Drain()
	if snap, ok := eng.store.Peek("o/r", 91); !ok || len(snap.Labels()) != 1 || snap.Labels()[0] != "ts-e2e" {
		t.Fatalf("delivery not applied (ok=%v)", ok)
	}
	// A bad signature is the shared transport's 401, nothing tailscale-specific.
	if r := h.post(t, body, "sha256=bad", "issues", "ts-2"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad signature status %d", r.StatusCode)
	}
	rows := eng.transports.HealthRows()
	if len(rows) != 1 || rows[0].Name != "tailscale" || rows[0].State != "listening" ||
		rows[0].Tailnet.V != "Running" || rows[0].Funnel.V != tailnetFunnelOK {
		t.Fatalf("rows = %#v", rows)
	}
}

// AC2: both App-webhook event sources build the very same transport type, and
// the tailscale file constructs it only through newAppWebhookTransport.
func TestTailscale_SharesTheDirectTransportType(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	eng, cleanup, _ := startTailscaleEngine(t)
	defer cleanup()
	got := tailscaleFromEngine(t, eng)
	direct, cleanupD := startDirectEngine(t, "127.0.0.1:0")
	defer cleanupD()
	d := directFromEngine(t, direct)
	if got == nil || d == nil {
		t.Fatal("missing transport")
	}
	var _ *appWebhookTransport = got
	var _ *appWebhookTransport = d

	src, err := os.ReadFile("tailnet.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "newAppWebhookTransport(") || strings.Contains(string(src), "http.Server{") || strings.Contains(string(src), "VerifySignature") {
		t.Fatal("tailnet.go must reuse newAppWebhookTransport and add no request handling of its own")
	}
}

// US2 / AC5: node state in the state dir means no auth key is read or passed.
func TestTailscale_ExistingStateReusesNodeWithoutAuthKey(t *testing.T) {
	nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	isolateWarnings(t)
	t.Setenv(DefaultAppWebhookSecretEnv, directTestSecret)
	t.Setenv(DefaultTailscaleAuthKeyEnv, "") // unset
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()
	eng.cfg.EventSource = EventSourceTailscale
	eng.cfg.TailscaleHostname = "fabrik-models"
	dir := tailscaleStateDir(eng.cfg, eng.fabrikDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tailnetNodeStateFile), []byte(`{"node":"state"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RefuseTailscaleMisconfigured(eng.cfg, true, directTestSecret, "", tailnetStateExists(dir)); err != nil {
		t.Fatalf("existing state must make the key optional: %v", err)
	}
	cleanup := eng.setUpIngestion(context.Background(), nil)
	defer cleanup()
	if len(*nodes) != 1 || (*nodes)[0].cfg.AuthKey != "" || eng.transports.Empty() {
		t.Fatalf("nodes=%d, attached=%v", len(*nodes), !eng.transports.Empty())
	}
}

// A key in the environment is not handed to a node that already has state.
func TestTailscale_KeyNotPassedWhenStateExists(t *testing.T) {
	nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	isolateWarnings(t)
	t.Setenv(DefaultAppWebhookSecretEnv, directTestSecret)
	t.Setenv(DefaultTailscaleAuthKeyEnv, tailscaleTestKey)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()
	eng.cfg.EventSource = EventSourceTailscale
	eng.cfg.TailscaleHostname = "h"
	dir := tailscaleStateDir(eng.cfg, eng.fabrikDir)
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, tailnetNodeStateFile), []byte("x"), 0o600)
	cleanup := eng.setUpIngestion(context.Background(), nil)
	defer cleanup()
	if (*nodes)[0].cfg.AuthKey != "" {
		t.Fatal("auth key was handed to a node with existing state")
	}
}

// US2.3: Stop closes the listener and the node, once.
func TestTailscale_StopClosesListenerAndNode(t *testing.T) {
	nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	eng, cleanup, _ := startTailscaleEngine(t)
	tr := tailscaleFromEngine(t, eng)
	addr := tr.Addr()
	cleanup()
	if n := (*nodes)[0].closed.Load(); n != 1 {
		t.Fatalf("node closed %d times, want 1", n)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("listener still accepting after Stop")
	}
	tr.Stop() // idempotent
	if n := (*nodes)[0].closed.Load(); n != 1 {
		t.Fatalf("node closed %d times after a second Stop", n)
	}
}

// US3 / R5-adjacent runtime faults: a node that cannot join, or cannot open
// Funnel, leaves the daemon poll-only with a "not listening" row and a panel
// entry, and the half-built node is closed.
func TestTailscale_StartFailuresArePollOnlyWithPanelEntry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fakeTailnet)
		want   string
	}{
		{"up fails", func(f *fakeTailnet) { f.upErr = errors.New("bad key") }, "bad key"},
		{"up times out", func(f *fakeTailnet) { f.upErr = context.DeadlineExceeded }, "did not join the tailnet"},
		{"funnel not permitted", func(f *fakeTailnet) { f.listenErr = errors.New("funnel not enabled") }, "funnel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet {
				f := newFakeTailnet(cfg, "n.t.ts.net")
				tc.mutate(f)
				return f
			})
			eng, cleanup, logs := startTailscaleEngine(t)
			defer cleanup()
			if !eng.transports.Empty() || eng.intake != nil {
				t.Fatal("must be poll-only")
			}
			if k := warningKeys(t); !k["tailscale_listen:fabrik-models"] {
				t.Fatalf("keys = %v", k)
			}
			rows := eng.transports.HealthRows()
			if len(rows) != 1 || rows[0].Name != "tailscale" || rows[0].State != "not listening" {
				t.Fatalf("rows = %#v", rows)
			}
			if (*nodes)[0].closed.Load() != 1 {
				t.Fatal("the node of a failed start must be closed")
			}
			if !strings.Contains(logs.String(), tc.want) {
				t.Fatalf("log lacks %q:\n%s", tc.want, logs.String())
			}
			if strings.Contains(logs.String(), tailscaleTestKey) {
				t.Fatal("auth key leaked")
			}
		})
	}
}

// US4 / AC6: a tailnet drop is unhealthy with a panel entry; recovery clears it
// and calls the reconnect handler exactly once; the first connect never does.
func TestTailscale_DropAndRecovery(t *testing.T) {
	nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	eng, cleanup, _ := startTailscaleEngine(t)
	defer cleanup()
	tr := tailscaleFromEngine(t, eng)
	n := (*nodes)[0]
	var reconnects atomic.Int32
	tr.SetReconnectHandler(func() { reconnects.Add(1) })

	if reconnects.Load() != 0 || !tr.IsHealthyOrStartingUp() {
		t.Fatal("the first connect must not reconnect and must be healthy")
	}
	n.emit(tailnetState{Connected: false, BackendState: "Stopped", Funnel: tailnetFunnelUnknown})
	if tr.IsHealthyOrStartingUp() {
		t.Fatal("a tailnet drop must be unhealthy")
	}
	if row := tr.HealthRow(); row.State != "tailnet disconnected" || row.Tailnet.V != "Stopped" {
		t.Fatalf("row = %#v", row)
	}
	if k := warningKeys(t); !k["tailscale_connection:fabrik-models"] {
		t.Fatalf("keys = %v", k)
	}
	// A reconcile "healthy" hint must not mask the drop.
	tr.TransitionHealth(true, "reconcile ok")
	if tr.IsHealthyOrStartingUp() {
		t.Fatal("a reconcile hint masked a tailnet drop")
	}
	n.emit(tailnetState{Connected: false, BackendState: "Starting"}) // still down: no handler
	if reconnects.Load() != 0 {
		t.Fatal("handler called while still down")
	}
	n.emit(tailnetState{Connected: true, BackendState: "Running", Funnel: tailnetFunnelOK})
	if got := reconnects.Load(); got != 1 {
		t.Fatalf("reconnect handler calls = %d, want 1", got)
	}
	if !tr.IsHealthyOrStartingUp() {
		t.Fatal("recovery must be healthy")
	}
	if k := warningKeys(t); k["tailscale_connection:fabrik-models"] {
		t.Fatalf("panel entry not cleared: %v", k)
	}
	n.emit(tailnetState{Connected: true, BackendState: "Running"}) // steady: nothing more
	if reconnects.Load() != 1 {
		t.Fatal("steady state re-fired the handler")
	}
}

// Transport-level: a connection that is down from its very first report and
// then comes up is a first connect, not a recovery.
func TestTailscale_FirstConnectAfterInitialDownIsNotARecovery(t *testing.T) {
	tr := newAppWebhookTransport("tailscale", "/x", nil, "s", func(int, string, string, ...any) {}, nil)
	var calls int
	tr.SetReconnectHandler(func() { calls++ })
	tr.SetNetworkCondition(false, "NeedsLogin", tailnetFunnelUnknown)
	tr.SetNetworkCondition(true, "Running", tailnetFunnelOK)
	if calls != 0 {
		t.Fatalf("calls = %d", calls)
	}
	tr.SetNetworkCondition(false, "Stopped", tailnetFunnelUnknown)
	tr.SetNetworkCondition(true, "Running", tailnetFunnelOK)
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 after a real recovery", calls)
	}
}

// A direct transport reports unknown tailnet/funnel values.
func TestDirectTransport_HealthRowHasNoTailnetFields(t *testing.T) {
	h := startDirect(t)
	row := h.tr.HealthRow()
	if row.Tailnet.Valid || row.Funnel.Valid {
		t.Fatalf("direct row carries tailnet fields: %#v", row)
	}
}

func TestFunnelWebhookURL(t *testing.T) {
	for _, tc := range []struct{ dns, path, want string }{
		{"fabrik-models.tail1.ts.net.", "/webhook", "https://fabrik-models.tail1.ts.net/webhook"},
		{"a.b.ts.net", "/x/y", "https://a.b.ts.net/x/y"},
		{"", "/webhook", ""},
	} {
		if got := funnelWebhookURL(tc.dns, tc.path); got != tc.want {
			t.Errorf("funnelWebhookURL(%q,%q) = %q, want %q", tc.dns, tc.path, got, tc.want)
		}
	}
}

func TestRedactTailnetLog(t *testing.T) {
	in := "To authenticate, visit: https://login.tailscale.com/a/abc123def\n"
	got := redactTailnetLog(in)
	if strings.Contains(got, "abc123def") || !strings.Contains(got, "[login URL withheld]") {
		t.Fatalf("got %q", got)
	}
}

func TestTailnetStateExists(t *testing.T) {
	dir := t.TempDir()
	if tailnetStateExists(dir) {
		t.Fatal("empty dir has no state")
	}
	p := filepath.Join(dir, tailnetNodeStateFile)
	_ = os.WriteFile(p, nil, 0o600)
	if tailnetStateExists(dir) {
		t.Fatal("an empty state file is not node state")
	}
	_ = os.WriteFile(p, []byte("x"), 0o600)
	if !tailnetStateExists(dir) {
		t.Fatal("non-empty state file is node state")
	}
	if tailnetStateExists(filepath.Join(dir, "missing")) {
		t.Fatal("missing dir")
	}
}

func TestTailscaleStateDirResolution(t *testing.T) {
	if got := tailscaleStateDir(Config{}, "/fab"); got != "/fab/.fabrik/state/tailscale" {
		t.Fatalf("default = %q", got)
	}
	if got := tailscaleStateDir(Config{TailscaleStateDir: "ts"}, "/fab"); got != "/fab/ts" {
		t.Fatalf("relative = %q", got)
	}
	if got := tailscaleStateDir(Config{TailscaleStateDir: "/abs/ts"}, "/fab"); got != "/abs/ts" {
		t.Fatalf("absolute = %q", got)
	}
}

// R4/AC: only the tagged file may import tailscale.com, so the default build's
// compiled graph is untouched.
func TestOnlyTheTaggedFileImportsTailscale(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "tailnet_tsnet.go" {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"tailscale.com`) {
			t.Errorf("%s imports tailscale.com; only tailnet_tsnet.go (build tag tailscale) may", name)
		}
	}
	b, err := os.ReadFile("tailnet_tsnet.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "//go:build tailscale") {
		t.Error("tailnet_tsnet.go must carry //go:build tailscale")
	}
}

// Self-upgrade would replace a tagged binary with the untagged release asset (or
// rebuild without the tag); it is skipped, once and loudly, for a tagged
// tailscale daemon, and is untouched for every other configuration.
func TestSelfUpgradeSkippedForTaggedTailscaleDaemon(t *testing.T) {
	orig := tailscaleBuilt
	defer func() { tailscaleBuilt = orig }()
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	logs := &syncLog{}
	eng.SetLogSinkForTest(func(_ int, tag, msg string) { logs.add("[%s] %s", tag, msg) })

	tailscaleBuilt = true
	eng.cfg.EventSource = EventSourceTailscale
	eng.cfg.Version = "v0.0.1"
	for i := 0; i < 3; i++ {
		eng.checkAndUpgrade() // would dereference a nil release client if it proceeded
	}
	if n := strings.Count(logs.String(), "self-upgrade is disabled"); n != 1 {
		t.Fatalf("notice logged %d times:\n%s", n, logs.String())
	}
	if !eng.selfUpgradeWouldDropTailscale() {
		t.Fatal("guard should hold")
	}

	eng.cfg.EventSource = EventSourceDirect
	if eng.selfUpgradeWouldDropTailscale() {
		t.Fatal("direct must upgrade as before")
	}
	eng.cfg.EventSource = EventSourceTailscale
	tailscaleBuilt = false
	if eng.selfUpgradeWouldDropTailscale() {
		t.Fatal("an untagged binary has nothing to lose")
	}
}
