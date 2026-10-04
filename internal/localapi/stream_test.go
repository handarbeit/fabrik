package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
)

type eventLog struct {
	mu  sync.Mutex
	got []channelevents.Event
	ch  chan channelevents.Event
}

func newEventLog() *eventLog { return &eventLog{ch: make(chan channelevents.Event, 100)} }
func (l *eventLog) add(ev channelevents.Event) {
	l.mu.Lock()
	l.got = append(l.got, ev)
	l.mu.Unlock()
	l.ch <- ev
}
func (l *eventLog) next(t *testing.T) channelevents.Event {
	t.Helper()
	select {
	case ev := <-l.ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for streamed event")
		return channelevents.Event{}
	}
}

type stateLog struct {
	mu sync.Mutex
	ch chan StreamState
}

func newStateLog() *stateLog           { return &stateLog{ch: make(chan StreamState, 100)} }
func (s *stateLog) add(st StreamState) { s.ch <- st }
func (s *stateLog) next(t *testing.T) StreamState {
	t.Helper()
	select {
	case st := <-s.ch:
		return st
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream state")
		return StreamState{}
	}
}

func newHub(t *testing.T, dir string) *channelevents.Hub {
	t.Helper()
	h, err := channelevents.Open(channelevents.Options{Dir: dir, MinDigest: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func serveOn(t *testing.T, path string, hub *channelevents.Hub) *Server {
	t.Helper()
	srv := NewServer(path, echoBackend(), nil)
	srv.Streamer = hubStreamer{hub}
	srv.HeartbeatInterval = 50 * time.Millisecond
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestStreamReconnectsAfterDaemonRestartAndReceivesHeldThenLive(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s.sock")
	state := t.TempDir()

	hub := newHub(t, state)
	srv := serveOn(t, sock, hub)
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "X"}); err != nil {
		t.Fatal(err)
	}

	evs, sts := newEventLog(), newStateLog()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunStream(ctx, StreamOptions{
			Path: sock, Subscriber: "X", OnEvent: evs.add, OnState: sts.add,
			MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
		})
	}()
	defer func() { cancel(); <-done }()

	if st := sts.next(t); !st.Connected {
		t.Fatalf("first state: %+v", st)
	}
	hub.Publish(channelevents.Event{Type: channelevents.Merged, Repo: "o/r", Issue: 1, Content: "live 1"})
	if ev := evs.next(t); ev.Issue != 1 {
		t.Fatalf("got %+v", ev)
	}

	// Simulated daemon restart: socket closed, hub reopened from disk.
	srv.Close()
	hub.Close()
	if st := sts.next(t); st.Connected {
		t.Fatalf("expected a disconnect, got %+v", st)
	}
	hub2 := newHub(t, state)
	defer hub2.Close()
	// Three events occur while no session is attached; they are held.
	for i := 2; i <= 4; i++ {
		hub2.Publish(channelevents.Event{Type: channelevents.Merged, Repo: "o/r", Issue: i, Content: "held"})
	}
	srv2 := serveOn(t, sock, hub2)
	defer srv2.Close()

	if st := sts.next(t); !st.Connected {
		t.Fatalf("expected reconnect, got %+v", st)
	}
	for i := 2; i <= 4; i++ {
		if ev := evs.next(t); ev.Issue != i {
			t.Fatalf("held event %d arrived as %d", i, ev.Issue)
		}
	}
	hub2.Publish(channelevents.Event{Type: channelevents.Merged, Repo: "o/r", Issue: 5, Content: "live 2"})
	if ev := evs.next(t); ev.Issue != 5 {
		t.Fatalf("live event after held: %+v", ev)
	}
}

func TestStreamReportsDownOncePerOutageWhenNoDaemon(t *testing.T) {
	sock := filepath.Join(shortDir(t), "none.sock")
	sts := newStateLog()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunStream(ctx, StreamOptions{Path: sock, Subscriber: "X", OnEvent: func(channelevents.Event) {}, OnState: sts.add,
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	}()
	st := sts.next(t)
	if st.Connected || !IsNoDaemon(st.Err) {
		t.Fatalf("got %+v", st)
	}
	select {
	case again := <-sts.ch:
		t.Fatalf("a second down report during one outage: %+v", again)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	<-done
}

func TestStreamStopsWhenSuperseded(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s.sock")
	hub := newHub(t, t.TempDir())
	defer hub.Close()
	srv := serveOn(t, sock, hub)
	defer srv.Close()
	hub.Subscribe(channelevents.Subscription{Subscriber: "X"})

	sts := newStateLog()
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunStream(context.Background(), StreamOptions{Path: sock, Subscriber: "X", OnEvent: func(channelevents.Event) {}, OnState: sts.add,
			MinBackoff: 10 * time.Millisecond})
	}()
	if st := sts.next(t); !st.Connected {
		t.Fatalf("%+v", st)
	}
	rawAttach(t, sock, "X") // a second session takes the name
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunStream should return once superseded")
	}
}

func TestStreamRetriesBusyRefusal(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s.sock")
	hub := newHub(t, t.TempDir())
	defer hub.Close()
	srv := serveOn(t, sock, hub)
	defer srv.Close()

	// Fill the connection cap with idle connections.
	var held []net.Conn
	for i := 0; i < MaxConns; i++ {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)

	sts := newStateLog()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunStream(ctx, StreamOptions{Path: sock, Subscriber: "X", OnEvent: func(channelevents.Event) {}, OnState: sts.add,
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond})
	}()
	defer func() { cancel(); <-done }()

	if st := sts.next(t); st.Connected {
		t.Fatalf("expected busy refusal first, got %+v", st)
	}
	held[0].Close() // free one slot
	if st := sts.next(t); !st.Connected {
		t.Fatalf("expected the client to retry into the freed slot, got %+v", st)
	}
}

func TestStreamDegradesAgainstV1Daemon(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s.sock")
	srv := NewServer(sock, echoBackend(), nil) // no Streamer: hello lacks attach
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	sts := newStateLog()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunStream(ctx, StreamOptions{Path: sock, Subscriber: "X", OnEvent: func(channelevents.Event) {}, OnState: sts.add,
			MinBackoff: 10 * time.Millisecond})
	}()
	st := sts.next(t)
	if st.Connected || !st.Unsupported {
		t.Fatalf("got %+v", st)
	}
	cancel()
	<-done
}

func TestStreamWatchdogDropsSilentConnection(t *testing.T) {
	// A server that completes the handshake then goes silent (no heartbeats)
	// must be abandoned after 3 missed beats.
	sock := filepath.Join(shortDir(t), "silent.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sess := newSession(c)
				buf := make([]byte, 4096)
				for i := 0; i < 2; i++ {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					var req Frame
					_ = unmarshal(buf[:n], &req)
					if req.Method == MethodHello {
						sess.Send(Frame{ID: req.ID, Result: []byte(`{"protocol_version":2,"methods":["attach"]}`)})
					} else {
						sess.Send(Frame{ID: req.ID, Result: []byte(`{"heartbeat_ms":30}`)})
					}
				}
				time.Sleep(5 * time.Second) // silent
			}(c)
		}
	}()
	sts := newStateLog()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunStream(ctx, StreamOptions{Path: sock, Subscriber: "X", OnEvent: func(channelevents.Event) {}, OnState: sts.add,
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	}()
	defer func() { cancel(); <-done }()
	if st := sts.next(t); !st.Connected {
		t.Fatalf("%+v", st)
	}
	start := time.Now()
	if st := sts.next(t); st.Connected {
		t.Fatalf("%+v", st)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("watchdog too slow")
	}
}

func unmarshal(b []byte, f *Frame) error { return json.Unmarshal(bytes.TrimSpace(b), f) }
