package engine

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tui"
	"github.com/handarbeit/fabrik/warnings"
)

// These tests redirect the process-global std logger (where net/http writes when
// a server has no ErrorLog), so none of them may run in parallel.

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// captureStdLogger points the std logger at a buffer for the test: this is
// where net/http sends server errors when http.Server.ErrorLog is nil, i.e. what
// would reach stderr.
func captureStdLogger(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

// tlsListenFn returns a listener factory serving TLS with httptest's
// certificate, like the Funnel listener.
func tlsListenFn(t *testing.T) func() (net.Listener, error) {
	t.Helper()
	ts := httptest.NewTLSServer(nil)
	cfg := ts.TLS.Clone()
	ts.Close()
	return func() (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		return tls.NewListener(ln, cfg), nil
	}
}

func startErrlogTransport(t *testing.T, name string, listenFn func() (net.Listener, error), configure func(*appWebhookTransport)) (*appWebhookTransport, *syncLog) {
	t.Helper()
	logs := &syncLog{}
	tr := newAppWebhookTransport(name, directTestPath, listenFn, directTestSecret,
		func(_ int, _ string, format string, args ...any) { logs.add(format, args...) },
		func(tui.Event) {})
	if configure != nil {
		configure(tr)
	}
	if err := tr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(tr.Stop)
	return tr, logs
}

// noHealthLines drops the transport's own "health state:" transition lines so
// assertions see only what the ErrorLog path wrote.
func noHealthLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" && !strings.HasPrefix(l, "health state:") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func abortHandshake(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close()
}

func waitForErrlog(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func handshakeLine(n int) string {
	return "http: TLS handshake error from 203.0.113." + string(rune('0'+n%10)) + ":4000: EOF"
}

func TestErrorLog_RealHandshakeFailureIsCountedAndNotPrinted(t *testing.T) {
	for _, name := range []string{"tailscale", "direct"} { // R5: one server for both
		t.Run(name, func(t *testing.T) {
			std := captureStdLogger(t)
			tr, logs := startErrlogTransport(t, name, tlsListenFn(t), nil)
			abortHandshake(t, tr.Addr())
			waitForErrlog(t, "handshake counter", func() bool { return tr.HealthRow().TLSHandshakeFailures.V >= 1 })
			if got := std.String(); got != "" {
				t.Fatalf("std logger (stderr) received %q, want nothing", got)
			}
			if strings.Contains(logs.String(), "handshake error") {
				t.Fatalf("a per-failure line reached the engine log: %q", logs.String())
			}
			if row := tr.HealthRow(); !row.TLSHandshakeFailures.Valid || row.TLSHandshakeFailures.V != 1 {
				t.Fatalf("counter = %#v, want 1", row.TLSHandshakeFailures)
			}
		})
	}
}

// Non-vacuity: without the ErrorLog assignment the same handshake failure lands
// on the std logger, so the stderr assertion above can fail.
func TestErrorLog_NonVacuity_WithoutErrorLogHandshakeReachesStderr(t *testing.T) {
	std := captureStdLogger(t)
	tr, _ := startErrlogTransport(t, "tailscale", tlsListenFn(t), func(tr *appWebhookTransport) {
		tr.SetErrorLogDisabledForTest(true)
	})
	abortHandshake(t, tr.Addr())
	waitForErrlog(t, "std logger line", func() bool { return strings.Contains(std.String(), "TLS handshake error") })
	if tr.HealthRow().TLSHandshakeFailures.V != 0 {
		t.Fatal("counter incremented with the ErrorLog disabled")
	}
}

func TestErrorLog_BurstProducesOneSummary(t *testing.T) {
	std := captureStdLogger(t)
	tr, logs := startErrlogTransport(t, "direct", func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		func(tr *appWebhookTransport) { tr.SetHandshakeSummaryIntervalForTest(300 * time.Millisecond) })
	w := serverErrorWriter{t: tr}
	for i := 0; i < 5; i++ {
		_, _ = w.Write([]byte(handshakeLine(i) + "\n"))
	}
	if got := noHealthLines(logs.String()); got != "" {
		t.Fatalf("a per-failure line was logged: %q", got)
	}
	waitForErrlog(t, "summary line", func() bool { return strings.Contains(logs.String(), "TLS handshake failures") })
	// Two further empty intervals: no second line.
	time.Sleep(700 * time.Millisecond)
	out := logs.String()
	if n := strings.Count(out, "TLS handshake failures"); n != 1 {
		t.Fatalf("summary lines = %d, want 1: %q", n, out)
	}
	if !strings.Contains(out, "failures: 5 ") {
		t.Fatalf("summary does not report 5: %q", out)
	}
	if tr.HealthRow().TLSHandshakeFailures.V != 5 {
		t.Fatalf("counter = %d, want 5", tr.HealthRow().TLSHandshakeFailures.V)
	}
	if std.String() != "" {
		t.Fatalf("stderr received %q", std.String())
	}
}

func TestErrorLog_NoSummaryWithoutFailures(t *testing.T) {
	_, logs := startErrlogTransport(t, "direct", func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		func(tr *appWebhookTransport) { tr.SetHandshakeSummaryIntervalForTest(20 * time.Millisecond) })
	time.Sleep(150 * time.Millisecond)
	if got := noHealthLines(logs.String()); got != "" {
		t.Fatalf("logged with no failures: %q", got)
	}
}

func TestErrorLog_StopFlushesPendingCount(t *testing.T) {
	tr, logs := startErrlogTransport(t, "direct", func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		func(tr *appWebhookTransport) { tr.SetHandshakeSummaryIntervalForTest(time.Hour) })
	w := serverErrorWriter{t: tr}
	for i := 0; i < 3; i++ {
		_, _ = w.Write([]byte(handshakeLine(i)))
	}
	tr.Stop()
	if out := logs.String(); strings.Count(out, "TLS handshake failures: 3 ") != 1 {
		t.Fatalf("final flush missing: %q", out)
	}
	// A late write after Stop must not panic or log a per-failure line.
	_, _ = w.Write([]byte(handshakeLine(9)))
}

func TestErrorLog_OtherServerErrorsReachTheEngineLog(t *testing.T) {
	std := captureStdLogger(t)
	tr, logs := startErrlogTransport(t, "direct", func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }, nil)
	// Through the server's own ErrorLog, as net/http would call it.
	tr.mu.Lock()
	el := tr.srv.ErrorLog
	tr.mu.Unlock()
	if el == nil {
		t.Fatal("server has no ErrorLog")
	}
	el.Printf("http: Accept error: %v; retrying in %v", "too many open files", 5*time.Millisecond)
	el.Printf("http: panic serving 1.2.3.4:5: boom\ngoroutine 1 [running]:\nmain.x()")
	el.Printf("http: Accept error: again")
	out := logs.String()
	if !strings.Contains(out, "http server: http: Accept error: too many open files") {
		t.Fatalf("Accept error missing from engine log: %q", out)
	}
	if !strings.Contains(out, "boom\ngoroutine 1 [running]:") {
		t.Fatalf("multi-line panic not kept as one message: %q", out)
	}
	if strings.Count(out, "Accept error") != 2 {
		t.Fatalf("server errors must not be summarised/rate-limited: %q", out)
	}
	if std.String() != "" {
		t.Fatalf("stderr received %q", std.String())
	}
	if tr.HealthRow().TLSHandshakeFailures.V != 0 {
		t.Fatal("handshake counter changed for a non-handshake error")
	}
	// A line that only mentions TLS is an ordinary server error.
	el.Printf("http: something about TLS handshake error from elsewhere")
	if tr.HealthRow().TLSHandshakeFailures.V != 0 || !strings.Contains(logs.String(), "something about TLS") {
		t.Fatalf("look-alike line misclassified: %q", logs.String())
	}
}

func TestErrorLog_HandshakeNoiseIsNotAHealthSignal(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	tr, _ := startErrlogTransport(t, "tailscale", tlsListenFn(t), nil)
	before := tr.HealthRow()
	stateBefore := tr.state()
	for i := 0; i < 20; i++ {
		_, _ = serverErrorWriter{t: tr}.Write([]byte(handshakeLine(i)))
	}
	after := tr.HealthRow()
	if after.State != before.State || tr.state() != stateBefore || after.Route != nil || before.Route != nil {
		t.Fatalf("health changed: %#v -> %#v", before, after)
	}
	if after.RejectedSignatures != before.RejectedSignatures || after.MalformedRequests != before.MalformedRequests {
		t.Fatalf("sibling counters changed: %#v", after)
	}
	if entries, _ := warnings.Load(); len(entries) != 0 {
		t.Fatalf("warnings raised: %#v", entries)
	}
}

func TestErrorLog_PlainHTTPListenerCountsNothing(t *testing.T) {
	tr, _ := startErrlogTransport(t, "direct", func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }, nil)
	abortHandshake(t, tr.Addr())
	time.Sleep(50 * time.Millisecond)
	if row := tr.HealthRow(); !row.TLSHandshakeFailures.Valid || row.TLSHandshakeFailures.V != 0 {
		t.Fatalf("counter = %#v, want a known 0", row.TLSHandshakeFailures)
	}
}

func TestErrorLog_StopBeforeStartDoesNotHang(t *testing.T) {
	tr := newAppWebhookTransport("direct", directTestPath, func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		directTestSecret, func(int, string, string, ...any) {}, func(tui.Event) {})
	done := make(chan struct{})
	go func() { tr.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop before Start hung")
	}
}

// Handshake failures are counted whatever the source address (the Tailscale
// Funnel shows only two fd7a:: addresses, IPv6 in brackets) and whatever the
// reason net/http appends (community report #2502, seen on a second daemon).
func TestErrorLog_CountsEveryHandshakeReasonAndSourceAddress(t *testing.T) {
	std := captureStdLogger(t)
	tr, logs := startErrlogTransport(t, "tailscale", func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		func(tr *appWebhookTransport) { tr.SetHandshakeSummaryIntervalForTest(time.Hour) })
	lines := []string{
		"http: TLS handshake error from [fd7a:115c:a1e0::1e01:7098]:52393: EOF",
		"http: TLS handshake error from [fd7a:115c:a1e0::401:c29f]:35253: EOF",
		"http: TLS handshake error from [fd7a:115c:a1e0::1e01:7098]:47311: tls: no cipher suite supported by both client and server; client offered: [0x1301 0x1302]",
		"http: TLS handshake error from [fd7a:115c:a1e0::1e01:7098]:35987: tls: client offered only unsupported versions: [302 301]",
	}
	el := log.New(serverErrorWriter{t: tr}, "", 0)
	for i := 0; i < 50; i++ { // a burst of dozens
		for _, l := range lines {
			el.Print(l)
		}
	}
	if got := tr.HealthRow().TLSHandshakeFailures.V; got != int64(50*len(lines)) {
		t.Fatalf("counter = %d, want %d", got, 50*len(lines))
	}
	if got := noHealthLines(logs.String()); got != "" {
		t.Fatalf("handshake lines reached the engine log individually: %q", got)
	}
	if std.String() != "" {
		t.Fatalf("stderr received %q", std.String())
	}
}
