package pruefer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/handarbeit/fabrik/internal/appwebhook"
	"github.com/handarbeit/fabrik/internal/events"
	"github.com/handarbeit/fabrik/internal/githubauth"
	ptui "github.com/handarbeit/fabrik/pruefer/tui"
)

const (
	srcTestSecret = "whsec-test-DO-NOT-LOG"
	srcTestKey    = "tskey-auth-SECRETVALUE-do-not-log"
	srcTestPath   = "/webhook"
)

// --- fakes -----------------------------------------------------------------

// fakeNode is an appwebhook.Node with no tailnet: the Funnel listener is a
// loopback port.
type fakeNode struct {
	cfg       appwebhook.NodeConfig
	dnsName   string
	upErr     error
	listenErr error
	closed    atomic.Int32
}

func (f *fakeNode) Up(context.Context) (appwebhook.NodeState, error) {
	if f.upErr != nil {
		return appwebhook.NodeState{}, f.upErr
	}
	return appwebhook.NodeState{Connected: true, BackendState: "Running", DNSName: f.dnsName, Funnel: appwebhook.FunnelOK}, nil
}
func (f *fakeNode) ListenFunnel(string) (net.Listener, error) {
	if f.listenErr != nil {
		return nil, f.listenErr
	}
	return net.Listen("tcp", "127.0.0.1:0")
}
func (f *fakeNode) Watch(ctx context.Context, _ func(appwebhook.NodeState)) error {
	<-ctx.Done()
	return nil
}
func (f *fakeNode) Close() error { f.closed.Add(1); return nil }

func installFakeNode(t *testing.T, build func(cfg appwebhook.NodeConfig) *fakeNode) *[]*fakeNode {
	t.Helper()
	var mu sync.Mutex
	var nodes []*fakeNode
	orig := appNodeFactory
	appNodeFactory = func(cfg appwebhook.NodeConfig) (appwebhook.Node, error) {
		n := build(cfg)
		mu.Lock()
		nodes = append(nodes, n)
		mu.Unlock()
		return n, nil
	}
	t.Cleanup(func() { appNodeFactory = orig })
	return &nodes
}

// fakeHookAPI is the App's delivery log and hook configuration.
type fakeHookAPI struct {
	mu          sync.Mutex
	deliveries  []gh.AppHookDelivery
	redelivered []int64
	hookURL     string
}

func (f *fakeHookAPI) ListAppHookDeliveries(cursor string, perPage int) ([]gh.AppHookDelivery, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cursor != "" {
		return nil, "", nil
	}
	return append([]gh.AppHookDelivery(nil), f.deliveries...), "", nil
}
func (f *fakeHookAPI) RedeliverAppHookDelivery(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redelivered = append(f.redelivered, id)
	return nil
}
func (f *fakeHookAPI) FetchAppHookConfig(context.Context) (gh.AppHookConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return gh.AppHookConfig{URL: f.hookURL, ContentType: "json"}, nil
}
func (f *fakeHookAPI) redeliveries() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.redelivered...)
}

type uiLog struct {
	mu  sync.Mutex
	evs []ptui.Event
}

func (u *uiLog) emit(ev ptui.Event) {
	u.mu.Lock()
	u.evs = append(u.evs, ev)
	u.mu.Unlock()
}
func (u *uiLog) all() []ptui.Event {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]ptui.Event(nil), u.evs...)
}

// --- harness ---------------------------------------------------------------

type srcHarness struct {
	t      *testing.T
	client *fakeLister
	d      *Daemon
	src    *appWebhookSource
	ui     *uiLog
	cancel context.CancelFunc
	done   chan struct{} // closed when Run returns
	runErr error
}

func srcConfig(source string) Config {
	return Config{
		EventSource:            source,
		DirectListen:           "127.0.0.1:0",
		AppWebhookPath:         srcTestPath,
		AppWebhookSecretEnv:    DefaultAppWebhookSecretEnv,
		TailscaleHostname:      "pruefer-acme",
		TailscaleAuthKeyEnv:    DefaultTailscaleAuthKeyEnv,
		ReplayFailedDeliveries: true,
	}
}

// startSource runs the real appWebhookSource under a real Daemon and the real
// daemonEventSink, and waits for the listener.
func startSource(t *testing.T, cfg Config, installation int64, mutate func(*appWebhookSource)) *srcHarness {
	t.Helper()
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = []gh.PRDetails{{Number: 1, Author: "alice", HeadSHA: "sha1", State: "open"}}
	d := newTestDaemonForEvents(client)
	d.derived.Installations = []githubauth.DerivedInstallation{{InstallationID: installation, Account: "owner"}}
	ui := &uiLog{}
	d.Emit = ui.emit

	src := newAppWebhookSource(d, cfg, srcTestSecret)
	src.sampleEvery = 20 * time.Millisecond
	if mutate != nil {
		mutate(src)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &srcHarness{t: t, client: client, d: d, src: src, ui: ui, cancel: cancel, done: make(chan struct{})}
	go func() {
		h.runErr = src.Run(ctx, &daemonEventSink{daemon: d})
		close(h.done)
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *srcHarness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Error("source did not stop within 10s of cancel")
	}
}

func (h *srcHarness) waitListening() string {
	h.t.Helper()
	waitUntil(h.t, 5*time.Second, func() bool { return h.src.addr() != "" })
	return h.src.addr()
}

func (h *srcHarness) post(path string, body []byte, sig, event, delivery string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest("POST", "http://"+h.waitListening()+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func sign(body []byte) string {
	m := hmac.New(sha256.New, []byte(srcTestSecret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func prBody(action string, installation int64) []byte {
	return []byte(fmt.Sprintf(`{"action":%q,"pull_request":{"number":1},"repository":{"name":"repo","owner":{"login":"owner"}},"installation":{"id":%d}}`, action, installation))
}

func (h *srcHarness) postPR(delivery, action string, installation int64) *http.Response {
	b := prBody(action, installation)
	return h.post(srcTestPath, b, sign(b), "pull_request", delivery)
}

func (h *srcHarness) transportEvents() []ptui.TransportEvent {
	var out []ptui.TransportEvent
	for _, ev := range h.ui.all() {
		if te, ok := ev.(ptui.TransportEvent); ok {
			out = append(out, te)
		}
	}
	return out
}

func (h *srcHarness) warningEvents() []ptui.TransportWarningEvent {
	var out []ptui.TransportWarningEvent
	for _, ev := range h.ui.all() {
		if te, ok := ev.(ptui.TransportWarningEvent); ok {
			out = append(out, te)
		}
	}
	return out
}

func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var b strings.Builder
	prev := Logf
	Logf = func(_ int, tag, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&b, "[%s] %s", tag, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { Logf = prev })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return b.String()
	}
}

// --- US1 / SC-001 ----------------------------------------------------------

func TestAppSource_Direct_SignedPullRequestDispatchesOneReview(t *testing.T) {
	h := startSource(t, srcConfig(EventSourceDirect), 111, nil)
	if r := h.postPR("d1", "opened", 111); r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	waitUntil(t, 3*time.Second, func() bool { return h.client.submitCallCount() == 1 })

	// The same delivery again is acknowledged and dropped by the dedupe.
	if r := h.postPR("d1", "opened", 111); r.StatusCode != http.StatusOK {
		t.Fatalf("duplicate status %d", r.StatusCode)
	}
	time.Sleep(150 * time.Millisecond)
	if got := h.client.submitCallCount(); got != 1 {
		t.Errorf("submitCallCount() = %d, want 1 after a duplicate delivery ID", got)
	}
}

func TestAppSource_Direct_RejectionsDispatchNothing(t *testing.T) {
	h := startSource(t, srcConfig(EventSourceDirect), 111, nil)
	body := prBody("opened", 111)
	for name, tc := range map[string]struct {
		do   func() *http.Response
		want int
	}{
		"bad signature": {func() *http.Response { return h.post(srcTestPath, body, "sha256=bad", "pull_request", "r1") }, 401},
		"no signature":  {func() *http.Response { return h.post(srcTestPath, body, "", "pull_request", "r2") }, 401},
		"wrong path":    {func() *http.Response { return h.post("/other", body, sign(body), "pull_request", "r3") }, 404},
		"invalid json": {func() *http.Response {
			b := []byte("{not json")
			return h.post(srcTestPath, b, sign(b), "pull_request", "r4")
		}, 400},
		"ping is acked": {func() *http.Response {
			b := []byte(`{"zen":"x"}`)
			return h.post(srcTestPath, b, sign(b), "ping", "r5")
		}, 200},
	} {
		if r := tc.do(); r.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", name, r.StatusCode, tc.want)
		}
	}
	req, _ := http.NewRequest("GET", "http://"+h.waitListening()+srcTestPath, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 405 {
		t.Errorf("GET: %v %v, want 405", resp, err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := h.client.submitCallCount(); got != 0 {
		t.Errorf("submitCallCount() = %d, want 0", got)
	}
}

// --- US2 / SC-002 ----------------------------------------------------------

func TestAppSource_Direct_ForeignInstallationIgnoredWithOneLine(t *testing.T) {
	logs := captureLog(t)
	h := startSource(t, srcConfig(EventSourceDirect), 111, nil)
	if r := h.postPR("foreign-1", "opened", 222); r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	time.Sleep(150 * time.Millisecond)
	if got := h.client.submitCallCount(); got != 0 {
		t.Errorf("submitCallCount() = %d, want 0", got)
	}
	if n := strings.Count(logs(), "foreign-1"); n != 1 {
		t.Errorf("log lines naming the delivery = %d, want 1:\n%s", n, logs())
	}
	// And one for the daemon's own installation still goes through.
	h.postPR("own-1", "opened", 111)
	waitUntil(t, 3*time.Second, func() bool { return h.client.submitCallCount() == 1 })
}

// --- US3 / SC-003 ----------------------------------------------------------

func writeLoggedInState(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// tsnet's state file: a JSON object of base64 values, one of which is a
	// profile whose Config carries a NodeID once login completed.
	prefs, _ := json.Marshal(map[string]any{"Config": map[string]any{"NodeID": "nNODE123"}})
	state, _ := json.Marshal(map[string][]byte{"profile-1234": prefs})
	if err := os.WriteFile(filepath.Join(dir, appwebhook.NodeStateFile), state, 0o600); err != nil {
		t.Fatal(err)
	}
}

func tsConfig(t *testing.T) Config {
	c := srcConfig(EventSourceTailscale)
	c.TailscaleStateDir = filepath.Join(t.TempDir(), "ts")
	return c
}

func TestAppSource_Tailscale_StartsWithKeyAndNeverLogsIt(t *testing.T) {
	logs := captureLog(t)
	t.Setenv(DefaultTailscaleAuthKeyEnv, srcTestKey)
	nodes := installFakeNode(t, func(c appwebhook.NodeConfig) *fakeNode {
		return &fakeNode{cfg: c, dnsName: "pruefer-acme-1.tail1234.ts.net."}
	})
	cfg := tsConfig(t)
	h := startSource(t, cfg, 111, nil)
	h.waitListening()
	if len(*nodes) != 1 {
		t.Fatalf("nodes built = %d", len(*nodes))
	}
	n := (*nodes)[0]
	if n.cfg.Hostname != "pruefer-acme" || n.cfg.AuthKey != srcTestKey || n.cfg.StateDir != cfg.TailscaleStateDir {
		t.Fatalf("node config: hostname %q, key passed %v, dir %q", n.cfg.Hostname, n.cfg.AuthKey == srcTestKey, n.cfg.StateDir)
	}
	if fi, err := os.Stat(n.cfg.StateDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("state dir not created 0700: %v %v", fi, err)
	}
	waitUntil(t, 3*time.Second, func() bool {
		return strings.Contains(logs(), "App webhook URL: https://pruefer-acme-1.tail1234.ts.net"+srcTestPath)
	})
	if out := logs(); strings.Contains(out, srcTestKey) || strings.Contains(out, srcTestSecret) {
		t.Errorf("a secret leaked into the log:\n%s", out)
	}
	// A signed delivery over the Funnel listener dispatches a review.
	h.postPR("ts-1", "synchronize", 111)
	waitUntil(t, 3*time.Second, func() bool { return h.client.submitCallCount() == 1 })
}

// Restart reuses the node from its state directory without an auth key.
func TestAppSource_Tailscale_RestartFromStateNeedsNoAuthKey(t *testing.T) {
	t.Setenv(DefaultTailscaleAuthKeyEnv, "") // no key in the environment
	nodes := installFakeNode(t, func(c appwebhook.NodeConfig) *fakeNode {
		return &fakeNode{cfg: c, dnsName: "pruefer-acme.tail1234.ts.net"}
	})
	cfg := tsConfig(t)
	writeLoggedInState(t, cfg.TailscaleStateDir)

	// The startup refusal accepts it...
	if !appwebhook.StateExists(cfg.TailscaleStateDir) {
		t.Fatal("test state file is not recognised as a logged-in node")
	}
	if err := RefuseAppSourceMisconfigured(cfg, srcTestSecret, "", true); err != nil {
		t.Fatalf("refused a restart from state: %v", err)
	}
	// ...and the node comes up with an empty key and a healthy transport.
	h := startSource(t, cfg, 111, nil)
	h.waitListening()
	if got := (*nodes)[0].cfg.AuthKey; got != "" {
		t.Errorf("node received a key (%d bytes) on a restart with none configured", len(got))
	}
	waitUntil(t, 3*time.Second, func() bool {
		evs := h.transportEvents()
		return len(evs) > 0 && evs[len(evs)-1].State == "listening" && evs[len(evs)-1].Tailnet == "Running"
	})
	h.postPR("restart-1", "opened", 111)
	waitUntil(t, 3*time.Second, func() bool { return h.client.submitCallCount() == 1 })
}

func TestAppSource_Tailscale_EmptyStateAndNoKeyIsRefusedAtStartup(t *testing.T) {
	cfg := tsConfig(t)
	if err := RefuseAppSourceMisconfigured(cfg, srcTestSecret, "", appwebhook.StateExists(cfg.TailscaleStateDir)); err == nil {
		t.Fatal("an empty state directory with no auth key must be refused at startup")
	}
}

// A node that cannot start leaves the daemon on its fallback poll, reported.
func TestAppSource_Tailscale_UpFailureIsReportedAndRunKeepsBlocking(t *testing.T) {
	logs := captureLog(t)
	installFakeNode(t, func(c appwebhook.NodeConfig) *fakeNode {
		return &fakeNode{cfg: c, upErr: errors.New("control plane unreachable")}
	})
	h := startSource(t, tsConfig(t), 111, nil)
	waitUntil(t, 3*time.Second, func() bool {
		for _, w := range h.warningEvents() {
			if w.Active && strings.Contains(w.Title, "could not start") {
				return true
			}
		}
		return false
	})
	waitUntil(t, 3*time.Second, func() bool {
		evs := h.transportEvents()
		return len(evs) > 0 && evs[len(evs)-1].State == "not listening"
	})
	select {
	case <-h.done:
		t.Fatalf("Run returned (%v) on a start failure; it must keep blocking so the daemon stays on the fallback poll", h.runErr)
	case <-time.After(100 * time.Millisecond):
	}
	if !strings.Contains(logs(), "control plane unreachable") {
		t.Errorf("failure cause not logged:\n%s", logs())
	}
}

func TestAppSource_Direct_BindFailureIsReported(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg := srcConfig(EventSourceDirect)
	cfg.DirectListen = busy.Addr().String()
	h := startSource(t, cfg, 111, nil)
	waitUntil(t, 3*time.Second, func() bool {
		for _, w := range h.warningEvents() {
			if w.Active && strings.Contains(w.Title, "could not start") {
				return true
			}
		}
		return false
	})
}

// --- US4: health ------------------------------------------------------------

func TestAppSource_FooterShowsLastDeliveryAndDriftReachesTheFooter(t *testing.T) {
	h := startSource(t, srcConfig(EventSourceDirect), 111, nil)
	h.postPR("hl-1", "opened", 111)
	waitUntil(t, 3*time.Second, func() bool {
		evs := h.transportEvents()
		return len(evs) > 0 && !evs[len(evs)-1].LastDelivery.IsZero()
	})
	last := h.transportEvents()[len(h.transportEvents())-1]
	if last.Name != "direct" || last.State != "listening" {
		t.Errorf("event = %+v", last)
	}

	// A sustained run of failed signatures is signature drift.
	body := prBody("opened", 111)
	for i := 0; i < events.SignatureDriftThreshold+1; i++ {
		h.post(srcTestPath, body, "sha256=bad", "pull_request", fmt.Sprintf("bad-%d", i))
	}
	waitUntil(t, 3*time.Second, func() bool {
		for _, ev := range h.ui.all() {
			if sd, ok := ev.(ptui.SignatureDriftEvent); ok && sd.Active {
				return true
			}
		}
		return false
	})
	waitUntil(t, 3*time.Second, func() bool {
		evs := h.transportEvents()
		return len(evs) > 0 && evs[len(evs)-1].State == "signature drift"
	})
}

func TestAppSource_Tailscale_HookConfigMismatchRaisesBanner(t *testing.T) {
	installFakeNode(t, func(c appwebhook.NodeConfig) *fakeNode {
		return &fakeNode{cfg: c, dnsName: "pruefer-acme.tail1234.ts.net"}
	})
	api := &fakeHookAPI{hookURL: "https://somewhere-else.example/webhook"}
	t.Setenv(DefaultTailscaleAuthKeyEnv, srcTestKey)
	h := startSource(t, tsConfig(t), 111, func(s *appWebhookSource) {
		s.api = api
		s.replayWait = func(context.Context, time.Duration) bool { return false }
		s.livenessWait = func(context.Context, time.Duration) bool { return false }
	})
	h.waitListening()
	waitUntil(t, 3*time.Second, func() bool {
		for _, w := range h.warningEvents() {
			if w.Active && strings.Contains(w.Key, appwebhook.WebhookConfigType) {
				return true
			}
		}
		return false
	})
}

// --- US5 / SC-005: replay ----------------------------------------------------

func TestAppSource_ReplayRequestsOnlyUnseenAndNeverDispatchesOrRecords(t *testing.T) {
	now := time.Now()
	api := &fakeHookAPI{deliveries: []gh.AppHookDelivery{
		{ID: 1001, GUID: "g-unseen", DeliveredAt: now.Add(-time.Minute), StatusCode: 500, Event: "pull_request"},
		{ID: 1002, GUID: "g-seen", DeliveredAt: now.Add(-time.Minute), StatusCode: 500, Event: "pull_request"},
		{ID: 1003, GUID: "g-ok", DeliveredAt: now.Add(-time.Minute), StatusCode: 200, Event: "pull_request"},
	}}
	h := startSource(t, srcConfig(EventSourceDirect), 111, func(s *appWebhookSource) {
		s.api = api
		s.replayWait = func(context.Context, time.Duration) bool { return false } // no follow-up pass
		s.noLiveness = true
		// g-seen was applied by a live delivery whose response GitHub never saw.
		s.dedupe.SeenBefore("g-seen")
	})
	h.waitListening()

	waitUntil(t, 3*time.Second, func() bool { return len(api.redeliveries()) > 0 })
	time.Sleep(100 * time.Millisecond)
	if got := api.redeliveries(); len(got) != 1 || got[0] != 1001 {
		t.Fatalf("redelivery requests = %v, want only [1001] (unseen and failed)", got)
	}
	// FR-006: the pass itself dispatched nothing and recorded nothing.
	if got := h.client.submitCallCount(); got != 0 {
		t.Errorf("the replayer dispatched a review (%d)", got)
	}
	if h.src.dedupe.Seen("g-unseen") {
		t.Error("the replayer recorded the delivery it asked GitHub to redeliver")
	}

	// The redelivered copy re-enters the normal handler and is applied once.
	b := prBody("opened", 111)
	h.post(srcTestPath, b, sign(b), "pull_request", "g-unseen")
	waitUntil(t, 3*time.Second, func() bool { return h.client.submitCallCount() == 1 })
}

func TestAppSource_NoReplayWhenDisabledOrNoAppAPI(t *testing.T) {
	api := &fakeHookAPI{deliveries: []gh.AppHookDelivery{
		{ID: 1, GUID: "g", DeliveredAt: time.Now().Add(-time.Minute), StatusCode: 500, Event: "pull_request"},
	}}
	cfg := srcConfig(EventSourceDirect)
	cfg.ReplayFailedDeliveries = false
	h := startSource(t, cfg, 111, func(s *appWebhookSource) {
		s.api = api
		s.livenessWait = func(context.Context, time.Duration) bool { return false }
	})
	h.waitListening()
	time.Sleep(200 * time.Millisecond)
	if got := api.redeliveries(); len(got) != 0 {
		t.Errorf("replay disabled but requested %v", got)
	}
}

// --- shutdown ---------------------------------------------------------------

func TestAppSource_StopClosesTheNodeAndReturnsNil(t *testing.T) {
	nodes := installFakeNode(t, func(c appwebhook.NodeConfig) *fakeNode {
		return &fakeNode{cfg: c, dnsName: "n.t.ts.net"}
	})
	t.Setenv(DefaultTailscaleAuthKeyEnv, srcTestKey)
	h := startSource(t, tsConfig(t), 111, nil)
	h.waitListening()
	h.cancel()
	select {
	case <-h.done:
		if h.runErr != nil {
			t.Errorf("Run = %v on cancel", h.runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if (*nodes)[0].closed.Load() == 0 {
		t.Error("the tailnet node was not closed on shutdown")
	}
}
