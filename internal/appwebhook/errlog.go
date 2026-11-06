package appwebhook

import (
	"io"
	"log"
	"strings"
	"time"
)

// App webhook receiver: routing of net/http server errors (#2549, ADR 2549).
//
// A receiver behind a public TLS route (the tailscale Funnel) is probed
// continuously by scanners that abort the handshake. net/http reports each one
// through http.Server.ErrorLog, which falls back to the std logger and so to the
// process's stderr when nil — drawing over the TUI and never reaching the engine
// log. The server therefore gets an ErrorLog backed by serverErrorWriter: a TLS
// handshake failure is only counted (and summarised once per interval); every
// other server error is forwarded to the engine log.

// handshakeLinePrefix is the exact prefix net/http gives a failed TLS handshake
// (net/http/server.go: "http: TLS handshake error from %s: %v"). Only this form
// is counted; anything else is an ordinary server error and is logged.
const handshakeLinePrefix = "http: TLS handshake error from "

// handshakeSummaryInterval is how often the pending handshake-failure count is
// summarised in the engine log. Fixed: operators who need the exact rate read
// the tls_handshake_failures counter.
const handshakeSummaryInterval = time.Minute

// serverErrorWriter is the io.Writer behind the receiver's http.Server.ErrorLog.
type serverErrorWriter struct{ t *Receiver }

// Write classifies one log record (log.Logger calls Write once per Printf).
// It never writes to stderr and never fails.
func (w serverErrorWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\r\n")
	if strings.HasPrefix(line, handshakeLinePrefix) {
		// Counter only: no lock, no state recompute, no warning, no liveness
		// input (FR-006). The summary goroutine reports it.
		w.t.tlsHandshakeFailures.Add(1)
		return len(p), nil
	}
	if line != "" {
		w.t.logFn(0, w.t.name, "http server: %s\n", line)
	}
	return len(p), nil
}

var _ io.Writer = serverErrorWriter{}

// errorLog builds the server's ErrorLog, or nil when the test seam disables it.
func (t *Receiver) errorLog() *log.Logger {
	if t.errorLogDisabled {
		return nil
	}
	return log.New(serverErrorWriter{t: t}, "", 0)
}

// SetErrorLogDisabledForTest skips the ErrorLog assignment in Start (the
// non-vacuity seam: with it on, net/http falls back to the std logger).
func (t *Receiver) SetErrorLogDisabledForTest(disabled bool) {
	t.errorLogDisabled = disabled
}

// SetHandshakeSummaryIntervalForTest overrides the summary interval; call it
// before Start.
func (t *Receiver) SetHandshakeSummaryIntervalForTest(d time.Duration) {
	t.summaryInterval = d
}

// runHandshakeSummary logs one line per interval that saw handshake failures,
// and flushes a pending count once when done closes.
func (t *Receiver) runHandshakeSummary(done <-chan struct{}, interval time.Duration) {
	defer t.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var summarised int64
	flush := func(window string) {
		now := t.tlsHandshakeFailures.Load()
		if n := now - summarised; n > 0 {
			t.logFn(0, t.name, "TLS handshake failures: %d %s (internet probes of the public route; delivery unaffected)\n", n, window)
			summarised = now
		}
	}
	for {
		select {
		case <-ticker.C:
			flush("in the last " + interval.String())
		case <-done:
			flush("since the last summary")
			return
		}
	}
}
