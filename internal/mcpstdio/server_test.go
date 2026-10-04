package mcpstdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
)

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// stubBackend answers the three daemon methods with recognisable payloads.
type stubBackend struct {
	lastStatus localapi.StatusParams
	lastBoard  localapi.BoardParams
}

func (b *stubBackend) Status(p localapi.StatusParams) (*localapi.StatusResult, error) {
	b.lastStatus = p
	if p.Issue == "missing" {
		return nil, localapi.Errorf(localapi.CodeNotFound, "no such issue")
	}
	return &localapi.StatusResult{Issue: p.Issue, Title: "stub title"}, nil
}
func (b *stubBackend) Board(p localapi.BoardParams) (*localapi.BoardResult, error) {
	b.lastBoard = p
	return &localapi.BoardResult{View: p.View, StallThresholdSeconds: p.StallThresholdSeconds}, nil
}
func (b *stubBackend) Health(localapi.HealthParams) (*localapi.HealthResult, error) {
	return &localapi.HealthResult{Version: "stub-v"}, nil
}

// session drives a Server with scripted stdin and returns its stdout lines.
func session(t *testing.T, socket string, requests ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	var errBuf bytes.Buffer
	srv := &Server{
		SocketPath: socket, Version: "v-test",
		In:  strings.NewReader(strings.Join(requests, "\n") + "\n"),
		Out: &out, Err: &errBuf,
		CallTimeout: 3 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Serve hung")
	}

	// stdout must carry only valid JSON-RPC frames, one per line.
	var frames []map[string]any
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var f map[string]any
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("stdout carries a non-JSON line %q: %v", sc.Text(), err)
		}
		if f["jsonrpc"] != "2.0" {
			t.Fatalf("frame without jsonrpc 2.0: %v", f)
		}
		frames = append(frames, f)
	}
	return frames
}

func byID(frames []map[string]any) map[string]map[string]any {
	m := map[string]map[string]any{}
	for _, f := range frames {
		b, _ := json.Marshal(f["id"])
		m[string(b)] = f
	}
	return m
}

func callTool(id int, name, args string) string {
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func toolText(t *testing.T, f map[string]any) (string, bool) {
	t.Helper()
	res, ok := f["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", f)
	}
	content := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("want one content block, got %v", content)
	}
	isErr, _ := res["isError"].(bool)
	return content[0].(map[string]any)["text"].(string), isErr
}

func TestInitializeToolsListAndOneCallEach(t *testing.T) {
	be := &stubBackend{}
	srv := localapi.NewServer(filepath.Join(shortDir(t), "s.sock"), be, nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	frames := session(t, srv.Path(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		callTool(4, ToolStatus, `{"issue":"owner/repo#7"}`),
		callTool(5, ToolBoard, `{"view":"flow","filter":{"milestone":"v1","has_open_blockers":true},"stall_threshold_minutes":45}`),
		callTool(6, ToolHealth, `{}`),
	)
	ids := byID(frames)

	// The notification gets no response: 6 requests, 6 frames.
	if len(frames) != 6 {
		t.Fatalf("want 6 frames (no reply to the notification), got %d: %v", len(frames), frames)
	}

	init := ids["1"]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" {
		t.Errorf("initialize must echo a supported client version, got %v", init["protocolVersion"])
	}
	if si := init["serverInfo"].(map[string]any); si["name"] != "fabrik" || si["version"] != "v-test" {
		t.Errorf("serverInfo = %v", si)
	}
	if caps := init["capabilities"].(map[string]any); caps["tools"] == nil {
		t.Errorf("capabilities = %v", caps)
	}
	if _, ok := ids["2"]["result"].(map[string]any); !ok {
		t.Errorf("ping must return an empty result object: %v", ids["2"])
	}

	tools := ids["3"]["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tl := range tools {
		m := tl.(map[string]any)
		names = append(names, m["name"].(string))
		if m["description"] == "" || m["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("tool %v is missing a description or object schema", m["name"])
		}
		if ann := m["annotations"].(map[string]any); ann["readOnlyHint"] != true {
			t.Errorf("tool %v must be annotated read-only", m["name"])
		}
	}
	if strings.Join(names, ",") != "fabrik_status,fabrik_board,fabrik_health" {
		t.Errorf("tools = %v", names)
	}

	text, isErr := toolText(t, ids["4"])
	if isErr || !strings.Contains(text, `"issue": "owner/repo#7"`) || !strings.Contains(text, "stub title") {
		t.Errorf("status call: err=%v text=%s", isErr, text)
	}
	if be.lastStatus.Issue != "owner/repo#7" {
		t.Errorf("daemon saw %+v", be.lastStatus)
	}

	text, isErr = toolText(t, ids["5"])
	if isErr || !strings.Contains(text, `"view": "flow"`) {
		t.Errorf("board call: err=%v text=%s", isErr, text)
	}
	if be.lastBoard.View != "flow" || be.lastBoard.Filter == nil || be.lastBoard.Filter.Milestone != "v1" ||
		!be.lastBoard.Filter.HasOpenBlockers || be.lastBoard.StallThresholdSeconds != 45*60 {
		t.Errorf("daemon saw %+v (filter %+v)", be.lastBoard, be.lastBoard.Filter)
	}

	text, isErr = toolText(t, ids["6"])
	if isErr || !strings.Contains(text, "stub-v") {
		t.Errorf("health call: err=%v text=%s", isErr, text)
	}
}

// With no daemon listening every tool returns a tool error naming the path,
// promptly — it must not hang or crash the session.
func TestNoDaemonIsAToolErrorNotAHang(t *testing.T) {
	sock := filepath.Join(shortDir(t), "absent.sock")
	start := time.Now()
	frames := session(t, sock,
		callTool(1, ToolStatus, `{"issue":"1"}`),
		callTool(2, ToolBoard, `{}`),
		callTool(3, ToolHealth, `{}`),
		`{"jsonrpc":"2.0","id":4,"method":"ping"}`,
	)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	ids := byID(frames)
	for _, id := range []string{"1", "2", "3"} {
		text, isErr := toolText(t, ids[id])
		if !isErr || !strings.Contains(text, "Fabrik daemon not running at "+sock) {
			t.Errorf("call %s: isError=%v text=%q", id, isErr, text)
		}
	}
	if _, ok := ids["4"]["result"]; !ok {
		t.Errorf("session must keep working after tool errors: %v", ids["4"])
	}
}

// A socket that accepts and never answers is bounded by CallTimeout.
func TestWedgedDaemonIsBoundedByTimeout(t *testing.T) {
	sock := filepath.Join(shortDir(t), "wedged.sock")
	release := make(chan struct{})
	srv := localapi.NewServer(sock, blockingBackend{release: release}, nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	defer close(release) // runs first: unblocks the stuck handler so Close can return

	var out, errBuf bytes.Buffer
	s := &Server{
		SocketPath: sock, In: strings.NewReader(callTool(1, ToolHealth, `{}`) + "\n"),
		Out: &out, Err: &errBuf, CallTimeout: 300 * time.Millisecond,
	}
	start := time.Now()
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("wedged daemon hung the session for %v", time.Since(start))
	}
	if !strings.Contains(out.String(), `"isError":true`) {
		t.Errorf("want an isError result, got %s", out.String())
	}
}

// blockingBackend never answers until release is closed.
type blockingBackend struct{ release chan struct{} }

func (b blockingBackend) Status(localapi.StatusParams) (*localapi.StatusResult, error) {
	<-b.release
	return nil, nil
}
func (b blockingBackend) Board(localapi.BoardParams) (*localapi.BoardResult, error) {
	<-b.release
	return nil, nil
}
func (b blockingBackend) Health(localapi.HealthParams) (*localapi.HealthResult, error) {
	<-b.release
	return nil, nil
}

func TestProtocolErrorsAndToolArgumentErrors(t *testing.T) {
	be := &stubBackend{}
	srv := localapi.NewServer(filepath.Join(shortDir(t), "s.sock"), be, nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	frames := session(t, srv.Path(),
		`{"jsonrpc":"2.0","id":1,"method":"nope/nothing"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fabrik_unknown","arguments":{}}}`,
		callTool(3, ToolStatus, `{}`),
		callTool(4, ToolStatus, `{"issue":"1","bogus":true}`),
		callTool(5, ToolBoard, `{"view":"sideways"}`),
		callTool(6, ToolStatus, `{"issue":"missing"}`),
		`{"jsonrpc":"2.0","method":"nope/notification"}`,
		`not json at all`,
		`[{"jsonrpc":"2.0","id":9,"method":"ping"}]`,
	)
	ids := byID(frames)
	code := func(f map[string]any) float64 { return f["error"].(map[string]any)["code"].(float64) }

	if code(ids["1"]) != -32601 {
		t.Errorf("unknown method: %v", ids["1"])
	}
	if code(ids["2"]) != -32602 {
		t.Errorf("unknown tool should be a protocol error: %v", ids["2"])
	}
	for _, id := range []string{"3", "4", "5"} {
		if text, isErr := toolText(t, ids[id]); !isErr || !strings.Contains(text, "invalid arguments") {
			t.Errorf("call %s: want an invalid-arguments tool error, got isError=%v %q", id, isErr, text)
		}
	}
	if text, isErr := toolText(t, ids["6"]); !isErr || !strings.Contains(text, "not_found") {
		t.Errorf("daemon not_found should surface as a tool error: %v %q", isErr, text)
	}
	// Parse error and batch both answer with id null; the notification gets nothing.
	nulls := 0
	for _, f := range frames {
		if f["id"] == nil && f["error"] != nil {
			nulls++
		}
	}
	if nulls != 2 {
		t.Errorf("want 2 id-null errors (parse, batch), got %d in %v", nulls, frames)
	}
	if len(frames) != 8 {
		t.Errorf("frames = %d, want 8 (6 requests + parse error + batch error): %v", len(frames), frames)
	}
}

// A large pretty-printed result must be one valid line per frame (no embedded
// raw newlines in the JSON-RPC envelope).
func TestResponsesAreSingleLineFrames(t *testing.T) {
	be := &stubBackend{}
	srv := localapi.NewServer(filepath.Join(shortDir(t), "s.sock"), be, nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var out bytes.Buffer
	s := &Server{SocketPath: srv.Path(), In: strings.NewReader(callTool(1, ToolStatus, `{"issue":"1"}`) + "\n"), Out: &out, Err: io.Discard, CallTimeout: 3 * time.Second}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimRight(out.String(), "\n"), "\n"); n != 0 {
		t.Fatalf("frame spans %d extra lines: %q", n, out.String())
	}
}
