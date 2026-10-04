package localapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
)

// hubStreamer is a minimal Streamer over a real channelevents.Hub.
type hubStreamer struct{ hub *channelevents.Hub }

func (h hubStreamer) Subscribe(p SubscribeParams) (*SubscribeResult, error) {
	sub, err := h.hub.Subscribe(channelevents.Subscription{Subscriber: p.Subscriber, Repos: p.Repos})
	if err != nil {
		return nil, Errorf(CodeBadRequest, "%v", err)
	}
	return &SubscribeResult{Subscription: sub, Subscriptions: h.hub.Subscriptions(p.Subscriber)}, nil
}

func (h hubStreamer) Unsubscribe(p UnsubscribeParams) (*UnsubscribeResult, error) {
	n := h.hub.Unsubscribe(p.Subscriber, p.ID)
	return &UnsubscribeResult{Removed: n, Subscriptions: h.hub.Subscriptions(p.Subscriber)}, nil
}

func (h hubStreamer) Attach(p AttachParams, sink channelevents.Sink) (func(), error) {
	return h.hub.Attach(p.Subscriber, sink), nil
}

func startStreamServer(t *testing.T, idle, hb time.Duration) (*Server, *channelevents.Hub) {
	t.Helper()
	hub, err := channelevents.Open(channelevents.Options{Dir: t.TempDir(), MinDigest: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hub.Close)
	path := shortDir(t) + "/s.sock"
	srv := NewServer(path, echoBackend(), nil)
	srv.Streamer = hubStreamer{hub}
	srv.IdleTimeout, srv.HeartbeatInterval = idle, hb
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, hub
}

// rawAttach dials, attaches as name and returns a frame reader.
func rawAttach(t *testing.T, path, name string) (net.Conn, func(time.Duration) (Frame, bool)) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	params, _ := json.Marshal(AttachParams{Subscriber: name})
	req, _ := json.Marshal(Frame{ID: "1", Method: MethodAttach, Params: params})
	c.Write(append(req, '\n'))
	r := bufio.NewReader(c)
	read := func(d time.Duration) (Frame, bool) {
		c.SetReadDeadline(time.Now().Add(d))
		line, err := r.ReadBytes('\n')
		if err != nil {
			return Frame{}, false
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			t.Fatalf("bad frame %q: %v", line, err)
		}
		return f, true
	}
	ack, ok := read(2 * time.Second)
	if !ok || ack.ID != "1" || ack.Error != nil {
		t.Fatalf("attach ack: %+v ok=%v", ack, ok)
	}
	return c, read
}

func TestHelloAdvertisesStreamingOnlyWithStreamer(t *testing.T) {
	plain := startServer(t, echoBackend())
	var hello struct {
		Version int      `json:"protocol_version"`
		Methods []string `json:"methods"`
	}
	if err := Call(context.Background(), plain.Path(), MethodHello, nil, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Version != 2 || containsStr(hello.Methods, MethodAttach) {
		t.Fatalf("plain server: %+v", hello)
	}
	srv, _ := startStreamServer(t, 0, 0)
	if err := Call(context.Background(), srv.Path(), MethodHello, nil, &hello); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{MethodSubscribe, MethodUnsubscribe, MethodAttach} {
		if !containsStr(hello.Methods, m) {
			t.Errorf("hello missing %s: %v", m, hello.Methods)
		}
	}
	// Without a Streamer the streaming methods are unknown.
	err := Call(context.Background(), plain.Path(), MethodSubscribe, SubscribeParams{Subscriber: "x"}, nil)
	var pe *Error
	if !asErr(err, &pe) || pe.Code != CodeUnknownMethod {
		t.Fatalf("want unknown_method, got %v", err)
	}
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func asErr(err error, target **Error) bool {
	pe, ok := err.(*Error)
	if ok {
		*target = pe
	}
	return ok
}

func TestPushOnlySubscriberSurvivesPastIdleTimeout(t *testing.T) {
	// Idle timeout 300ms, heartbeat 60ms: a connection that sends nothing must
	// outlive several idle periods and still receive a push.
	srv, hub := startStreamServer(t, 300*time.Millisecond, 60*time.Millisecond)
	if err := Call(context.Background(), srv.Path(), MethodSubscribe, SubscribeParams{Subscriber: "X"}, nil); err != nil {
		t.Fatal(err)
	}
	_, read := rawAttach(t, srv.Path(), "X")

	time.Sleep(1000 * time.Millisecond) // > 3 idle timeouts, client sends nothing
	hub.Publish(channelevents.Event{Type: channelevents.Merged, Repo: "o/r", Issue: 1, Content: "merged"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, ok := read(500 * time.Millisecond)
		if !ok {
			t.Fatal("connection dropped (idle timeout not extended by heartbeats)")
		}
		if f.Event == EventChannel {
			var ev channelevents.Event
			json.Unmarshal(f.Params, &ev)
			if ev.Issue != 1 {
				t.Fatalf("wrong event %+v", ev)
			}
			return
		}
		if f.Event != EventHeartbeat {
			t.Fatalf("unexpected frame %+v", f)
		}
	}
	t.Fatal("push never arrived")
}

func TestIdleConnectionWithoutAttachStillDropped(t *testing.T) {
	srv, _ := startStreamServer(t, 150*time.Millisecond, 50*time.Millisecond)
	c, err := net.Dial("unix", srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := bufio.NewReader(c).ReadByte(); err == nil {
		t.Fatal("expected the idle connection to be closed")
	}
}

func TestSecondAttachSupersedesFirst(t *testing.T) {
	srv, hub := startStreamServer(t, 0, 0)
	Call(context.Background(), srv.Path(), MethodSubscribe, SubscribeParams{Subscriber: "X"}, nil)
	_, readA := rawAttach(t, srv.Path(), "X")
	_, readB := rawAttach(t, srv.Path(), "X")

	f, ok := readA(2 * time.Second)
	if !ok || f.Event != EventSuperseded {
		t.Fatalf("first session should be told it was superseded, got %+v ok=%v", f, ok)
	}
	if _, ok := readA(500 * time.Millisecond); ok {
		t.Fatal("superseded connection should be closed")
	}
	hub.Publish(channelevents.Event{Type: channelevents.Merged, Repo: "o/r", Issue: 7, Content: "x"})
	f, ok = readB(2 * time.Second)
	if !ok || f.Event != EventChannel {
		t.Fatalf("second session should receive the push, got %+v", f)
	}
}

func TestDoubleAttachOnOneConnectionRefused(t *testing.T) {
	srv, _ := startStreamServer(t, 0, 0)
	c, _ := rawAttach(t, srv.Path(), "X")
	params, _ := json.Marshal(AttachParams{Subscriber: "Y"})
	req, _ := json.Marshal(Frame{ID: "2", Method: MethodAttach, Params: params})
	c.Write(append(req, '\n'))
	r := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var f Frame
	json.Unmarshal(line, &f)
	if f.ID != "2" || f.Error == nil || f.Error.Code != CodeBadRequest {
		t.Fatalf("got %+v", f)
	}
}

func TestAttachRejectsBadName(t *testing.T) {
	srv, _ := startStreamServer(t, 0, 0)
	err := Call(context.Background(), srv.Path(), MethodAttach, AttachParams{Subscriber: ""}, nil)
	var pe *Error
	if !asErr(err, &pe) || pe.Code != CodeBadRequest {
		t.Fatalf("got %v", err)
	}
}

func TestSubscribeUnsubscribeRoundTrip(t *testing.T) {
	srv, hub := startStreamServer(t, 0, 0)
	var res SubscribeResult
	if err := Call(context.Background(), srv.Path(), MethodSubscribe, SubscribeParams{Subscriber: "X", Repos: []string{"o/r"}}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Subscription.ID == "" || len(hub.Subscriptions("X")) != 1 {
		t.Fatalf("subscribe: %+v", res)
	}
	var un UnsubscribeResult
	if err := Call(context.Background(), srv.Path(), MethodUnsubscribe, UnsubscribeParams{Subscriber: "X"}, &un); err != nil {
		t.Fatal(err)
	}
	if un.Removed != 1 {
		t.Fatalf("unsubscribe: %+v", un)
	}
}
