package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

func TestChannelNotificationParamsAreStringMetaWithIdentifierKeys(t *testing.T) {
	ev := channelevents.Event{
		Type: channelevents.ValidateSettled, Repo: "o/r", Issue: 12, Stage: "Validate", PR: 34,
		Content: "o/r#12 settled", CommentURL: "https://x/c",
		Meta: map[string]string{"next": "waiting-for-human", "bad-key": "dropped", "with space": "dropped", "ok_1": "kept"},
	}
	p := channelNotification(ev)
	if p["content"] != "o/r#12 settled" {
		t.Fatalf("content: %v", p["content"])
	}
	meta := p["meta"].(map[string]string)
	for k := range meta {
		if !metaKey(k) {
			t.Errorf("meta key %q would be silently dropped by Claude Code", k)
		}
	}
	want := map[string]string{
		"event": "validate-settled", "repo": "o/r", "issue": "12", "stage": "Validate",
		"pr": "34", "comment": "https://x/c", "next": "waiting-for-human", "ok_1": "kept",
	}
	for k, v := range want {
		if meta[k] != v {
			t.Errorf("meta[%q] = %q, want %q", k, meta[k], v)
		}
	}
	if _, ok := meta["bad-key"]; ok {
		t.Error("hyphenated key must not be sent")
	}
	// Event type names are values, so a hyphenated type is fine.
	if meta["event"] != "validate-settled" {
		t.Error("event value lost")
	}
	// The whole thing must serialise as JSON string values.
	b, _ := json.Marshal(p)
	var back struct {
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
}

func TestSubscribeToolNeedsANameAndPassesExcludeLabelsFaithfully(t *testing.T) {
	// No launch name, no argument: refuse with the fix.
	if _, _, err := buildCall(ToolSubscribe, json.RawMessage(`{}`), ""); err == nil || !strings.Contains(err.Error(), "--subscriber") {
		t.Fatalf("want a clear error naming --subscriber, got %v", err)
	}
	// Launch name is the default; absent exclude_labels stays nil (default exclusions).
	m, p, err := buildCall(ToolSubscribe, json.RawMessage(`{"issues":["7"],"labels":["fabrik:*"],"digest_seconds":60}`), "topic-x")
	if err != nil || m != localapi.MethodSubscribe {
		t.Fatalf("%v %v", m, err)
	}
	sp := p.(localapi.SubscribeParams)
	if sp.Subscriber != "topic-x" || sp.ExcludeLabels != nil || sp.DigestSeconds != 60 || len(sp.Issues) != 1 {
		t.Fatalf("%+v", sp)
	}
	// An explicit empty list opts into every label.
	_, p, _ = buildCall(ToolSubscribe, json.RawMessage(`{"exclude_labels":[]}`), "topic-x")
	if sp := p.(localapi.SubscribeParams); sp.ExcludeLabels == nil || len(*sp.ExcludeLabels) != 0 {
		t.Fatalf("exclude_labels [] must stay non-nil and empty: %+v", sp.ExcludeLabels)
	}
	// An explicit subscriber overrides the launch name.
	_, p, _ = buildCall(ToolSubscribe, json.RawMessage(`{"subscriber":"other"}`), "topic-x")
	if p.(localapi.SubscribeParams).Subscriber != "other" {
		t.Fatal("explicit subscriber ignored")
	}
	// Strict decode: unknown fields are refused.
	if _, _, err := buildCall(ToolSubscribe, json.RawMessage(`{"nope":1}`), "x"); err == nil {
		t.Fatal("unknown argument accepted")
	}
	m, p, err = buildCall(ToolUnsubscribe, json.RawMessage(`{"id":"sub-1"}`), "topic-x")
	if err != nil || m != localapi.MethodUnsubscribe || p.(localapi.UnsubscribeParams).ID != "sub-1" {
		t.Fatalf("%v %v %v", m, p, err)
	}
}

// ---- end-to-end against a real daemon socket and hub ----

type hubStreamer struct{ hub *channelevents.Hub }

func (h hubStreamer) Subscribe(p localapi.SubscribeParams) (*localapi.SubscribeResult, error) {
	sub, err := h.hub.Subscribe(channelevents.Subscription{Subscriber: p.Subscriber, ExcludeLabels: p.ExcludeLabels})
	if err != nil {
		return nil, localapi.Errorf(localapi.CodeBadRequest, "%v", err)
	}
	return &localapi.SubscribeResult{Subscription: sub, Subscriptions: h.hub.Subscriptions(p.Subscriber)}, nil
}
func (h hubStreamer) Unsubscribe(p localapi.UnsubscribeParams) (*localapi.UnsubscribeResult, error) {
	return &localapi.UnsubscribeResult{Removed: h.hub.Unsubscribe(p.Subscriber, p.ID)}, nil
}
func (h hubStreamer) Attach(p localapi.AttachParams, sink channelevents.Sink) (func(), error) {
	return h.hub.Attach(p.Subscriber, sink), nil
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// notifications returns every channel notification written so far.
func (b *syncBuf) notifications() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(b.buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var f map[string]any
		if json.Unmarshal([]byte(line), &f) == nil && f["method"] == ChannelMethod {
			out = append(out, f["params"].(map[string]any))
		}
	}
	return out
}

func waitNotifications(t *testing.T, b *syncBuf, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if got := b.notifications(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("only %d of %d notifications: %v", len(b.notifications()), n, b.notifications())
	return nil
}

func metaOf(n map[string]any) map[string]any { return n["meta"].(map[string]any) }

func openTestHub(t *testing.T, dir string) *channelevents.Hub {
	t.Helper()
	h, err := channelevents.Open(channelevents.Options{Dir: dir, MinDigest: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func startDaemon(t *testing.T, sock string, hub *channelevents.Hub) *localapi.Server {
	t.Helper()
	srv := localapi.NewServer(sock, &stubBackend{}, nil)
	srv.Streamer = hubStreamer{hub}
	srv.HeartbeatInterval = 50 * time.Millisecond
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	return srv
}

// runShim starts a Server on a pipe, performs the initialize handshake and
// returns its captured stdout and a stop func.
func runShim(t *testing.T, sock, subscriber string, grace time.Duration) (*syncBuf, func()) {
	t.Helper()
	pr, pw := io.Pipe()
	out := &syncBuf{}
	srv := &Server{
		SocketPath: sock, Version: "v-test", In: pr, Out: out, Err: io.Discard,
		CallTimeout: 3 * time.Second, Subscriber: subscriber, UnreachableGrace: grace,
	}
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(context.Background()) }()
	io.WriteString(pw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`+"\n")
	io.WriteString(pw, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	stop := func() { pw.Close(); <-done }
	t.Cleanup(func() { pw.Close() })
	return out, stop
}

func TestShimReconnectsAfterDaemonRestartAndDeliversHeldThenLive(t *testing.T) {
	sock := filepath.Join(shortDir(t), "d.sock")
	state := t.TempDir()

	hub := openTestHub(t, state)
	daemon := startDaemon(t, sock, hub)
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "topic"}); err != nil {
		t.Fatal(err)
	}

	out, stop := runShim(t, sock, "topic", 100*time.Millisecond)
	defer stop()

	hub.Publish(channelevents.Event{Type: channelevents.Merged, Repo: "o/r", Issue: 1, Content: "live one", Meta: map[string]string{"pr": "9"}})
	first := waitNotifications(t, out, 1)[0]
	if first["content"] != "live one" || metaOf(first)["event"] != "merged" || metaOf(first)["issue"] != "1" {
		t.Fatalf("first push: %v", first)
	}

	// The daemon restarts: socket closed, state reloaded from disk.
	daemon.Close()
	hub.Close()
	// Down past the grace period -> daemon-unreachable, live in the session.
	un := waitNotifications(t, out, 2)[1]
	if metaOf(un)["event"] != "daemon-unreachable" {
		t.Fatalf("want daemon-unreachable, got %v", un)
	}

	hub2 := openTestHub(t, state)
	defer hub2.Close()
	for i := 2; i <= 4; i++ { // three events while no session is attached
		hub2.Publish(channelevents.Event{Type: channelevents.Merged, Repo: "o/r", Issue: i, Content: "held"})
	}
	daemon2 := startDaemon(t, sock, hub2)
	defer daemon2.Close()

	all := waitNotifications(t, out, 6) // first, unreachable, reachable, then 3 held
	events := []string{}
	issues := []string{}
	for _, n := range all[2:] {
		events = append(events, metaOf(n)["event"].(string))
		if v, ok := metaOf(n)["issue"].(string); ok {
			issues = append(issues, v)
		}
	}
	if events[0] != "daemon-reachable" {
		t.Fatalf("expected daemon-reachable first after reconnect: %v", events)
	}
	if strings.Join(issues, ",") != "2,3,4" {
		t.Fatalf("held events must arrive in order after reconnect: %v (events %v)", issues, events)
	}
	if v := metaOf(all[2])["down_for_seconds"]; v == nil {
		t.Errorf("daemon-reachable should say how long it was down: %v", metaOf(all[2]))
	}

	// And live events keep flowing, with no action from the session.
	hub2.Publish(channelevents.Event{Type: channelevents.Merged, Repo: "o/r", Issue: 5, Content: "live two"})
	got := waitNotifications(t, out, 7)
	if got[len(got)-1]["content"] != "live two" {
		t.Fatalf("live push after reconnect: %v", got[len(got)-1])
	}
}

func TestShimWithoutSubscriberHoldsNoStreamAndKeepsReadTools(t *testing.T) {
	sock := filepath.Join(shortDir(t), "d.sock")
	hub := openTestHub(t, t.TempDir())
	defer hub.Close()
	daemon := startDaemon(t, sock, hub)
	defer daemon.Close()
	hub.Subscribe(channelevents.Subscription{Subscriber: "anyone"})

	out, stop := runShim(t, sock, "", 50*time.Millisecond)
	hub.Publish(channelevents.Event{Type: channelevents.Merged, Repo: "o/r", Issue: 1, Content: "x"})
	time.Sleep(300 * time.Millisecond)
	stop()
	if n := out.notifications(); len(n) != 0 {
		t.Fatalf("a shim with no subscriber name must not push: %v", n)
	}
	if hub.Queued("anyone") != 1 {
		t.Fatalf("the event should be held for its subscriber: Queued=%d", hub.Queued("anyone"))
	}
}

func TestShimDegradesAgainstDaemonWithoutStreaming(t *testing.T) {
	sock := filepath.Join(shortDir(t), "d.sock")
	daemon := localapi.NewServer(sock, &stubBackend{}, nil) // no Streamer: a v1-style daemon
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	out, stop := runShim(t, sock, "topic", 50*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	stop()
	if n := out.notifications(); len(n) != 0 {
		t.Fatalf("no push and no daemon-unreachable against a reachable v1 daemon: %v", n)
	}
}
