package mcpstdio

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// handshakes are the initialize shapes a client might send. The shim reads
// nothing from clientInfo/capabilities (#2322: no documented signal tells an
// interactive session from `claude -p`), so every shape must behave the same.
var handshakes = map[string]string{
	"protocolVersion only":     `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
	"capabilities+clientInfo":  `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"experimental":{"claude/channel":{}}},"clientInfo":{"name":"claude-code","version":"1"}}}`,
	"print-mode-like, no caps": `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code-print"}}}`,
}

// runWithStream serves one handshake and reports the subscriber names the stream
// attached under.
func runWithStream(t *testing.T, srv *Server, initialize string) []string {
	t.Helper()
	var names []string
	srv.Stream = func(ctx context.Context, o localapi.StreamOptions) error {
		names = append(names, o.Subscriber)
		<-ctx.Done()
		return nil
	}
	srv.In = strings.NewReader(initialize + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	srv.Out = io.Discard
	srv.Err = io.Discard
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	return names
}

// The shared OS-username default never starts a stream; the explicit sources do.
func TestStreamStartsOnlyForExplicitNameSources(t *testing.T) {
	for hs, init := range handshakes {
		for _, tc := range []struct {
			src  NameSource
			want int
		}{
			{SourceDefault, 0},
			{SourceFlag, 1},
			{SourceEnv, 1},
			{SourceTopic, 1},
		} {
			names := runWithStream(t, &Server{SocketPath: "/nonexistent", Subscriber: "alice", SubscriberSource: tc.src, Version: "v"}, init)
			if len(names) != tc.want {
				t.Errorf("%s, source %d: stream started %d times, want %d", hs, tc.src, len(names), tc.want)
			}
			if len(names) == 1 && names[0] != "alice" {
				t.Errorf("%s, source %d: attached as %q", hs, tc.src, names[0])
			}
		}
	}
}

// The default still names the action tools' audit comments.
func TestDefaultNameSignsActions(t *testing.T) {
	srv, act := startActionDaemon(t, true)
	var out bytes.Buffer
	s := &Server{
		SocketPath: srv.Path(), Version: "v", Subscriber: "alice", SubscriberSource: SourceDefault,
		In:  strings.NewReader(callTool(1, ToolPromote, `{"issue":"5","to":"Specify"}`) + "\n"),
		Out: &out, Err: io.Discard, CallTimeout: 3 * time.Second,
	}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(act.promote) != 1 || act.promote[0].Subscriber != "alice" {
		t.Fatalf("the action must be attributed to the default name: %+v", act.promote)
	}
}

// Only a default-named session's superseded notice carries the remedy.
func TestSupersededNoticeAdviceOnlyForDefault(t *testing.T) {
	for _, tc := range []struct {
		src    NameSource
		advice bool
	}{{SourceDefault, true}, {SourceFlag, false}, {SourceEnv, false}, {SourceTopic, false}} {
		msg := supersededContent("alice", tc.src)
		if !strings.Contains(msg, `"alice" and took over`) {
			t.Errorf("source %d: notice lost its base text: %q", tc.src, msg)
		}
		if got := strings.Contains(msg, "FABRIK_SUBSCRIBER") && strings.Contains(msg, "`topic`"); got != tc.advice {
			t.Errorf("source %d: remedy present = %v, want %v: %q", tc.src, got, tc.advice, msg)
		}
	}

	// Through the reachability path, as the stream reports it.
	var out bytes.Buffer
	srv := &Server{Subscriber: "alice", SubscriberSource: SourceDefault, Out: &out, Err: io.Discard}
	(&reachability{srv: srv, grace: time.Hour}).onState(localapi.StreamState{Superseded: true})
	if !strings.Contains(out.String(), "set FABRIK_SUBSCRIBER") || !strings.Contains(out.String(), "stream-superseded") {
		t.Errorf("superseded notification: %s", out.String())
	}
}

func TestSubscribeNoteNamesTheDefault(t *testing.T) {
	if n := subscribeNote("alice", SourceDefault); !strings.Contains(n, "OS-username default") || !strings.Contains(n, "FABRIK_SUBSCRIBER") {
		t.Errorf("default note: %q", n)
	}
	if n := subscribeNote("", SourceNone); !strings.Contains(n, "could not resolve a subscriber name") {
		t.Errorf("no-name note: %q", n)
	}
	if n := subscribeNote("topic", SourceTopic); strings.Contains(n, "holds no push connection") {
		t.Errorf("an explicit name holds a push connection: %q", n)
	}
}
