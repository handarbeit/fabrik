package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// skewCalls scripts the daemon: hello answers daemonVersion (or fails), every
// other method answers a fixed payload. It counts hello calls.
type skewCalls struct {
	mu            sync.Mutex
	daemonVersion string
	helloErr      error
	hellos        int
	health        string // version in the fabrik_health result
}

func (c *skewCalls) call(_ context.Context, _ string, method string, _ any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var payload any
	switch method {
	case localapi.MethodHello:
		c.hellos++
		if c.helloErr != nil {
			return c.helloErr
		}
		m := map[string]any{"protocol_version": localapi.ProtocolVersion}
		if c.daemonVersion != "" {
			m["daemon_version"] = c.daemonVersion
		}
		payload = m
	case localapi.MethodHealth:
		payload = map[string]any{"version": c.health}
	default:
		payload = map[string]any{"ok": true}
	}
	b, _ := json.Marshal(payload)
	return json.Unmarshal(b, result)
}

func args(tool string) string {
	if tool == ToolStatus {
		return `{"issue":"o/r#1"}`
	}
	return `{}`
}

func skewSession(t *testing.T, shim string, c *skewCalls, tools ...string) [][]string {
	t.Helper()
	var reqs []string
	for i, tool := range tools {
		reqs = append(reqs, callTool(i+1, tool, args(tool)))
	}
	var out bytes.Buffer
	srv := &Server{
		SocketPath: "unused", Version: shim, Call: c.call, CallTimeout: time.Second,
		In:  strings.NewReader(strings.Join(reqs, "\n") + "\n"),
		Out: &out, Err: &bytes.Buffer{},
	}
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	byID := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var f struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("bad frame %q: %v", line, err)
		}
		var texts []string
		for _, c := range f.Result.Content {
			texts = append(texts, c.Text)
		}
		byID[string(f.ID)] = texts
	}
	var res [][]string
	for i := range tools {
		res = append(res, byID[itoa(i+1)])
	}
	return res
}

func hasSkewNote(texts []string) bool {
	for _, s := range texts {
		if strings.Contains(s, "restart the Claude session") {
			return true
		}
	}
	return false
}

func TestSkewNoteShownOnceWhenVersionsDiffer(t *testing.T) {
	c := &skewCalls{daemonVersion: "dev(new)"}
	// Calls are served concurrently, so which result carries the note is not
	// fixed; that exactly one does, naming both builds, is the contract.
	res := skewSession(t, "dev(old)", c, ToolStatus, ToolBoard, ToolStatus)
	notes := 0
	for _, r := range res {
		for _, text := range r {
			if strings.Contains(text, "restart the Claude session") {
				notes++
				if !strings.Contains(text, "dev(new)") || !strings.Contains(text, "dev(old)") {
					t.Errorf("note must name both versions: %q", text)
				}
			}
		}
	}
	if notes != 1 {
		t.Fatalf("skew note shown %d times across 3 calls, want exactly 1: %v", notes, res)
	}
}

func TestNoSkewNoteWhenVersionsMatchOrUnknown(t *testing.T) {
	for name, tc := range map[string]struct {
		shim  string
		calls *skewCalls
		// Calls are served concurrently, so the first few can overlap and each ask
		// once; a conclusive no-skew check is then trusted for skewRecheckInterval
		// (TestSkewCheckTrustedForRecheckInterval pins the sequential case), while a
		// failed hello is retried by every call.
		minHellos, maxHellos int
	}{
		"equal":             {"dev(a)", &skewCalls{daemonVersion: "dev(a)"}, 1, 3},
		"old daemon":        {"dev(a)", &skewCalls{}, 1, 3},
		"hello fails":       {"dev(a)", &skewCalls{helloErr: errors.New("boom")}, 3, 3},
		"shim has no build": {"", &skewCalls{daemonVersion: "dev(a)"}, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			res := skewSession(t, tc.shim, tc.calls, ToolStatus, ToolBoard, ToolStatus)
			for i, r := range res {
				if hasSkewNote(r) {
					t.Errorf("call %d: unexpected skew note %v", i+1, r)
				}
				if len(r) != 1 {
					t.Errorf("call %d: want exactly the tool result, got %v", i+1, r)
				}
			}
			if h := tc.calls.hellos; h < tc.minHellos || h > tc.maxHellos {
				t.Errorf("hello calls = %d, want %d..%d", h, tc.minHellos, tc.maxHellos)
			}
		})
	}
}

// With matching builds, sequential tool calls cost one hello within the
// recheck interval, and one more once it has elapsed.
func TestSkewCheckTrustedForRecheckInterval(t *testing.T) {
	c := &skewCalls{daemonVersion: "dev(a)"}
	pr, pw := io.Pipe()
	out := &syncBuffer{}
	var clockMu sync.Mutex
	clock := time.Unix(1_000_000, 0)
	srv := &Server{SocketPath: "unused", Version: "dev(a)", Call: c.call, CallTimeout: time.Second, In: pr, Out: out, Err: &bytes.Buffer{},
		skewNow: func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background()) }()
	hellos := func() int { c.mu.Lock(); defer c.mu.Unlock(); return c.hellos }
	for i := 1; i <= 3; i++ {
		if _, err := io.WriteString(pw, callTool(i, ToolStatus, args(ToolStatus))+"\n"); err != nil {
			t.Fatal(err)
		}
		out.waitLines(t, i)
	}
	if h := hellos(); h != 1 {
		t.Fatalf("hello calls across 3 sequential calls = %d, want 1", h)
	}
	clockMu.Lock()
	clock = clock.Add(skewRecheckInterval + time.Second)
	clockMu.Unlock()
	if _, err := io.WriteString(pw, callTool(4, ToolStatus, args(ToolStatus))+"\n"); err != nil {
		t.Fatal(err)
	}
	out.waitLines(t, 4)
	if h := hellos(); h != 2 {
		t.Fatalf("hello calls after the interval = %d, want 2", h)
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A daemon that upgrades mid-session is noticed on a later call, once.
func TestSkewNoticedWhenDaemonUpgradesMidSession(t *testing.T) {
	c := &skewCalls{daemonVersion: "dev(a)"}
	pr, pw := io.Pipe()
	out := &syncBuffer{}
	send := func(line string) {
		if _, err := io.WriteString(pw, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	var clockMu sync.Mutex
	clock := time.Unix(1_000_000, 0)
	srv := &Server{SocketPath: "unused", Version: "dev(a)", Call: c.call, CallTimeout: time.Second, In: pr, Out: out, Err: &bytes.Buffer{},
		skewNow: func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background()) }()
	send(callTool(1, ToolStatus, args(ToolStatus)))
	out.waitLines(t, 1)
	c.mu.Lock()
	c.daemonVersion = "dev(b)"
	c.mu.Unlock()
	// Within the recheck interval the earlier "no skew" result is trusted, so
	// the upgrade is not seen yet and no hello is spent.
	send(callTool(2, ToolStatus, args(ToolStatus)))
	out.waitLines(t, 2)
	c.mu.Lock()
	hellos := c.hellos
	c.mu.Unlock()
	if hellos != 1 {
		t.Fatalf("hello calls inside the recheck interval = %d, want 1", hellos)
	}
	clockMu.Lock()
	clock = clock.Add(skewRecheckInterval + time.Second)
	clockMu.Unlock()
	send(callTool(3, ToolStatus, args(ToolStatus)))
	send(callTool(4, ToolStatus, args(ToolStatus)))
	out.waitLines(t, 4)
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var notes []int
	for i, l := range lines {
		if strings.Contains(l, "restart the Claude session") {
			notes = append(notes, i)
		}
	}
	// The first two calls ran before the interval elapsed; of the two after it
	// (served concurrently, so in either order) exactly one carries the note.
	if len(notes) != 1 || notes[0] < 2 {
		t.Fatalf("want exactly one note, not on the pre-upgrade result; got lines %v of\n%s", notes, out.String())
	}
}

func TestHealthReportsBothVersions(t *testing.T) {
	c := &skewCalls{health: "dev(a)"}
	res := skewSession(t, "dev(a)", c, ToolHealth)
	if len(res[0]) != 2 || res[0][1] != "Versions — daemon: dev(a), MCP server: dev(a)" {
		t.Fatalf("health result = %v", res[0])
	}
	c = &skewCalls{health: ""}
	res = skewSession(t, "dev(a)", c, ToolHealth)
	if res[0][1] != "Versions — daemon: unknown, MCP server: dev(a)" {
		t.Fatalf("version-less daemon: %v", res[0])
	}
}

// syncBuffer is a bytes.Buffer safe for the Serve goroutine to write while the
// test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) waitLines(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(s.String(), "\n") >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d output lines; have %q", n, s.String())
}
