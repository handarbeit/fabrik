package localapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortDir returns a short temp dir: t.TempDir() on macOS can push a socket
// path past sun_path.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

type fakeBackend struct {
	status func(StatusParams) (*StatusResult, error)
	board  func(BoardParams) (*BoardResult, error)
	health func(HealthParams) (*HealthResult, error)
}

func (f fakeBackend) Status(p StatusParams) (*StatusResult, error) { return f.status(p) }
func (f fakeBackend) Board(p BoardParams) (*BoardResult, error)    { return f.board(p) }
func (f fakeBackend) Health(p HealthParams) (*HealthResult, error) { return f.health(p) }

func echoBackend() fakeBackend {
	return fakeBackend{
		status: func(p StatusParams) (*StatusResult, error) {
			if p.Issue == "missing" {
				return nil, Errorf(CodeNotFound, "no such issue")
			}
			if p.Issue == "panic" {
				panic("boom")
			}
			return &StatusResult{Issue: p.Issue}, nil
		},
		board: func(p BoardParams) (*BoardResult, error) { return &BoardResult{View: p.View}, nil },
		health: func(HealthParams) (*HealthResult, error) {
			return &HealthResult{Version: "test"}, nil
		},
	}
}

func startServer(t *testing.T, be Backend) *Server {
	t.Helper()
	path := filepath.Join(shortDir(t), "s.sock")
	srv := NewServer(path, be, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestRoundTripAndErrors(t *testing.T) {
	srv := startServer(t, echoBackend())
	ctx := context.Background()

	var hello map[string]any
	if err := Call(ctx, srv.Path(), MethodHello, nil, &hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if hello["protocol_version"] != float64(ProtocolVersion) {
		t.Errorf("hello = %v", hello)
	}

	var st StatusResult
	if err := Call(ctx, srv.Path(), MethodStatus, StatusParams{Issue: "o/r#4"}, &st); err != nil || st.Issue != "o/r#4" {
		t.Fatalf("status: %v %+v", err, st)
	}

	var br BoardResult
	if err := Call(ctx, srv.Path(), MethodBoard, BoardParams{}, &br); err != nil || br.View != ViewAttention {
		t.Fatalf("board default view: %v %+v", err, br)
	}
	if err := Call(ctx, srv.Path(), MethodBoard, BoardParams{View: "bogus"}, &br); err == nil {
		t.Fatal("unknown view must be rejected")
	}

	err := Call(ctx, srv.Path(), MethodStatus, StatusParams{Issue: "missing"}, &st)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
	err = Call(ctx, srv.Path(), MethodStatus, StatusParams{Issue: "panic"}, &st)
	if !errors.As(err, &pe) || pe.Code != CodeInternal {
		t.Fatalf("panic must become internal, got %v", err)
	}
	// The daemon survived the panic.
	var hr HealthResult
	if err := Call(ctx, srv.Path(), MethodHealth, nil, &hr); err != nil || hr.Version != "test" {
		t.Fatalf("health after panic: %v %+v", err, hr)
	}
	err = Call(ctx, srv.Path(), "nope", nil, nil)
	if !errors.As(err, &pe) || pe.Code != CodeUnknownMethod {
		t.Fatalf("want unknown_method, got %v", err)
	}
}

func TestNoDaemon(t *testing.T) {
	err := Call(context.Background(), filepath.Join(shortDir(t), "none.sock"), MethodHealth, nil, nil)
	if !IsNoDaemon(err) || !strings.Contains(err.Error(), "Fabrik daemon not running at") {
		t.Fatalf("want NoDaemonError, got %v", err)
	}
}

func TestSocketModeAndTCPUnreachable(t *testing.T) {
	srv := startServer(t, echoBackend())
	fi, err := os.Lstat(srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode = %v, want srw-------", fi.Mode())
	}
	// It is a Unix socket, not a TCP listener: nothing answers on loopback.
	if ln := srv.ln; ln.Addr().Network() != "unix" {
		t.Fatalf("listener network = %s, want unix", ln.Addr().Network())
	}
}

func TestStaleSocketReplacedAndRegularFileRefused(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "s.sock")

	// Leave a real stale socket file behind (a listener that is never closed
	// cleanly: unlink-on-close disabled, like a crashed process).
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("setup: stale socket not left behind: %v", err)
	}

	srv := NewServer(path, echoBackend(), nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("stale socket should be replaced: %v", err)
	}
	defer srv.Close()
	if err := Call(context.Background(), path, MethodHealth, nil, nil); err != nil {
		t.Fatalf("replaced socket must serve: %v", err)
	}

	// A regular file at the path is never removed.
	reg := filepath.Join(dir, "regular")
	if err := os.WriteFile(reg, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(reg); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("Listen over a regular file must refuse, got %v", err)
	}
	if b, _ := os.ReadFile(reg); string(b) != "precious" {
		t.Fatal("regular file was modified")
	}
}

func TestCloseUnlinksAndIsIdempotent(t *testing.T) {
	srv := startServer(t, echoBackend())
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(srv.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket file should be gone after Close, Lstat err=%v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSocketPathFallbackAndDistinct(t *testing.T) {
	short := shortDir(t)
	if r, err := filepath.EvalSymlinks(short); err == nil {
		short = r // SocketPath resolves symlinks (/tmp -> /private/tmp on macOS)
	}
	if got := SocketPath(short); got != filepath.Join(short, ".fabrik", "state", SocketName) {
		t.Errorf("short dir path = %s", got)
	}
	long := filepath.Join(short, strings.Repeat("d", 120))
	p := SocketPath(long)
	if len(p) > maxSocketPath {
		t.Errorf("fallback path too long (%d): %s", len(p), p)
	}
	if !strings.HasPrefix(p, fallbackRoot+"/") {
		t.Errorf("fallback should live under the fixed %s, got %s", fallbackRoot, p)
	}
	if p != SocketPath(long) {
		t.Error("fallback path must be deterministic")
	}
	if SocketPath(long) == SocketPath(long+"x") {
		t.Error("distinct directories must get distinct sockets")
	}
	// The fallback path is bindable end to end.
	srv := NewServer(p, echoBackend(), nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("binding fallback path: %v", err)
	}
	srv.Close()
	fi, err := os.Lstat(filepath.Dir(p))
	if err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("fallback dir must be private, got %v %v", fi, err)
	}
}

func TestPrepareDirTightensExistingFallbackDir(t *testing.T) {
	old := fallbackRoot
	fallbackRoot = shortDir(t)
	t.Cleanup(func() { fallbackRoot = old })
	dir := fallbackDir()
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareDir(dir); err != nil {
		t.Fatalf("prepareDir: %v", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("existing fallback dir must be tightened to 0700, got %v %v", fi.Mode().Perm(), err)
	}

	// A non-fallback directory (the primary state dir) is left as it is.
	other := filepath.Join(shortDir(t), "state")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareDir(other); err != nil {
		t.Fatalf("prepareDir: %v", err)
	}
	if fi, _ := os.Lstat(other); fi.Mode().Perm() != 0o755 {
		t.Errorf("non-fallback dir must not be chmodded, got %v", fi.Mode().Perm())
	}
}

func TestConnectionCapRejectsExtras(t *testing.T) {
	srv := startServer(t, echoBackend())
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < MaxConns; i++ {
		c, err := net.Dial("unix", srv.Path())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	// Wait until the server has registered all of them: a request on each works.
	for _, c := range held {
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write([]byte(`{"id":"1","method":"hello"}` + "\n"))
		if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
			t.Fatalf("held conn not served: %v", err)
		}
	}
	extra, err := net.Dial("unix", srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(extra).ReadString('\n')
	if err != nil {
		t.Fatalf("extra conn should get a busy frame: %v", err)
	}
	var f Frame
	if json.Unmarshal([]byte(line), &f) != nil || f.Error == nil || f.Error.Code != CodeBusy {
		t.Fatalf("want busy error frame, got %q", line)
	}
}

func TestOversizeLineRejected(t *testing.T) {
	srv := startServer(t, echoBackend())
	c, err := net.Dial("unix", srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	go c.Write([]byte(strings.Repeat("x", MaxLineBytes+10) + "\n"))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("expected an error frame: %v", err)
	}
	var f Frame
	if json.Unmarshal([]byte(line), &f) != nil || f.Error == nil || f.Error.Code != CodeBadRequest {
		t.Fatalf("want bad_request, got %q", line)
	}
}

func TestMalformedFrameDoesNotKillConnection(t *testing.T) {
	srv := startServer(t, echoBackend())
	c, err := net.Dial("unix", srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	c.Write([]byte("not json\n"))
	if line, _ := r.ReadString('\n'); !strings.Contains(line, CodeBadRequest) {
		t.Fatalf("want bad_request, got %q", line)
	}
	c.Write([]byte(`{"id":"9","method":"hello"}` + "\n"))
	line, err := r.ReadString('\n')
	if err != nil || !strings.Contains(line, `"id":"9"`) {
		t.Fatalf("connection should keep serving: %q %v", line, err)
	}
}

// A server-initiated, id-less frame shares one connection with request/response
// traffic: the framing leaves room for the push stream the next issue adds.
func TestServerInitiatedFrameInterleaves(t *testing.T) {
	var (
		mu   sync.Mutex
		sess *Session
		got  = make(chan struct{})
	)
	path := filepath.Join(shortDir(t), "s.sock")
	srv := NewServer(path, echoBackend(), nil)
	srv.OnSession = func(s *Session) {
		mu.Lock()
		sess = s
		mu.Unlock()
		close(got)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	<-got
	mu.Lock()
	s := sess
	mu.Unlock()

	if err := s.Send(Frame{Event: "item.changed", Sub: "s1", Params: json.RawMessage(`{"issue":"o/r#1"}`)}); err != nil {
		t.Fatal(err)
	}
	c.Write([]byte(`{"id":"1","method":"hello"}` + "\n"))

	r := bufio.NewReader(c)
	var frames []Frame
	for i := 0; i < 2; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		var f Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, f)
	}
	if !frames[0].IsServerInitiated() || frames[0].Sub != "s1" {
		t.Errorf("first frame should be the push, got %+v", frames[0])
	}
	if !frames[1].IsResponse() || frames[1].ID != "1" {
		t.Errorf("second frame should be the response, got %+v", frames[1])
	}
}

func TestKnownMarshalling(t *testing.T) {
	type wrap struct {
		A Known[int]       `json:"a"`
		B Known[time.Time] `json:"b"`
		C MilestoneField   `json:"c"`
		D MilestoneField   `json:"d"`
		E MilestoneField   `json:"e"`
		F LimitMax         `json:"f"`
		G LimitMax         `json:"g"`
		H LimitMax         `json:"h"`
	}
	w := wrap{
		A: Some(3),
		B: KnownTime(time.Time{}),
		C: MilestoneField{State: MilestoneSet, Title: "v1", Number: 2},
		D: MilestoneField{State: MilestoneNone},
		E: MilestoneField{},
		F: LimitMax{Kind: "limit", Value: 5},
		G: LimitMax{Kind: "unlimited"},
		H: LimitMax{},
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":3,"b":"unknown","c":{"title":"v1","number":2},"d":"none","e":"unknown","f":5,"g":"unlimited","h":"unknown"}`
	if string(b) != want {
		t.Fatalf("marshal =\n%s\nwant\n%s", b, want)
	}
	var back wrap
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.A.Valid || back.A.V != 3 || back.B.Valid || back.C.Title != "v1" || back.D.State != MilestoneNone ||
		back.E.State != MilestoneUnknown || back.F.Value != 5 || back.G.Kind != "unlimited" || back.H.Kind != "unknown" {
		t.Fatalf("round trip lost data: %+v", back)
	}
	// A zero Known must never read as a healthy default.
	var zero Known[bool]
	if zb, _ := json.Marshal(zero); string(zb) != `"unknown"` {
		t.Errorf("zero Known marshals to %s", zb)
	}
}

func TestCallHonorsContextDeadline(t *testing.T) {
	// A server that accepts and never answers: Call must return, not hang.
	path := filepath.Join(shortDir(t), "mute.sock")
	ln, err := net.Listen("unix", path)
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
			defer c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = Call(ctx, path, MethodHealth, nil, nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Call hung for %v", time.Since(start))
	}
}

// The fallback must not depend on $TMPDIR: a daemon and a Claude-launched
// `fabrik mcp` need not share an environment.
func TestSocketPathFallbackIgnoresTMPDIR(t *testing.T) {
	long := filepath.Join(shortDir(t), strings.Repeat("d", 120))
	t.Setenv("TMPDIR", "/var/tmp/one")
	a := SocketPath(long)
	t.Setenv("TMPDIR", "/var/tmp/two")
	b := SocketPath(long)
	if a != b {
		t.Fatalf("fallback path depends on TMPDIR: %s vs %s", a, b)
	}
	if !strings.HasPrefix(a, "/tmp/fabrik-") {
		t.Errorf("fallback = %s, want under /tmp/fabrik-<uid>", a)
	}
}

// Two spellings of one directory (a symlink, /tmp vs /private/tmp) must agree.
func TestSocketPathResolvesSymlinks(t *testing.T) {
	real := filepath.Join(shortDir(t), strings.Repeat("d", 120))
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Skipf("cannot create long dir: %v", err)
	}
	link := filepath.Join(shortDir(t), "ln")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if a, b := SocketPath(real), SocketPath(link); a != b {
		t.Fatalf("symlinked spelling disagrees: %s vs %s", a, b)
	}
}

func TestListenRefusesOverlongPath(t *testing.T) {
	p := filepath.Join(shortDir(t), strings.Repeat("s", 120)+".sock")
	if _, err := Listen(p); err == nil || !strings.Contains(err.Error(), "sun_path") {
		t.Fatalf("Listen(%d-byte path) = %v, want a sun_path error", len(p), err)
	}
}
