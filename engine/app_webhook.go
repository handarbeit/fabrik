package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/handarbeit/fabrik/internal/events"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/tui"
)

// appWebhookTransport is the direct App webhook transport (#2142, ADR 2142):
// the daemon receives a GitHub App's own webhook straight from an HTTP request
// the operator's tunnel or proxy forwards to it, with no third-party relay.
//
// It is an ingestTransport and nothing more: it verifies X-Hub-Signature-256
// against the App's static webhook secret and hands a delivery to the engine's
// intake sink. Dedupe, the fast ack, echo attribution, reconnect catch-up and
// the body cap's value all belong to the intake and are reused, not repeated
// (the one thing it must do itself is read at most webhookMaxBodyBytes+1 so an
// oversized body is answered 413 before any verification).
//
// The listener is injected as a factory (listenFn, called inside Start) so a
// different listener source (the embedded tsnet of #2143) reuses the whole
// transport unchanged and a bind failure is an ordinary Start error, which
// attachTransport turns into poll-only. The transport speaks plain HTTP: TLS is
// the fronting proxy's job.
type appWebhookTransport struct {
	name     string
	path     string
	listenFn func() (net.Listener, error)
	logFn    func(issueNumber int, tag, format string, args ...any)
	emitFn   func(tui.Event)

	// secret is the App's webhook secret. It is only ever used as the HMAC key
	// and is never logged.
	secret string

	// onDrift, when set, is called (outside any lock) with true when a
	// signature-failure streak reaches the threshold and false on recovery.
	onDrift func(active bool)

	sigDrift events.SigDrift

	// driftMu serialises driftTransition so the drift callback is never
	// invoked out of order by concurrent requests; driftReported is the last
	// state it reported.
	driftMu       sync.Mutex
	driftReported bool

	rejectedSigs atomic.Int64
	malformed    atomic.Int64
	oversized    atomic.Int64

	stopOnce sync.Once

	mu             sync.Mutex
	sink           func(delivery)
	reconnectFn    func() // stored for interface parity; never called (FR-017)
	coverageFn     func()
	srv            *http.Server
	ln             net.Listener
	listening      bool
	cacheDrift     bool
	lastState      WebhookHealthState
	lastDeliveryAt time.Time
	lastSigWarnAt  time.Time
	eventCounts    map[string]int
	managedRepos   []string
	installNote    string
}

// App webhook server timeouts: long enough for GitHub to upload a body at the
// cap, short enough to bound a slow client behind a public route.
const (
	appWebhookReadHeaderTimeout = 10 * time.Second
	appWebhookReadTimeout       = 2 * time.Minute
	// The write deadline runs from the end of the request-header read, not from
	// the end of the body, so it must cover the whole body upload (ReadTimeout)
	// plus time to write the response; otherwise a slow 25 MiB upload would be
	// accepted and then its 2xx/413 could not be written.
	appWebhookWriteTimeout  = appWebhookReadTimeout + 30*time.Second
	appWebhookIdleTimeout   = 2 * time.Minute
	appWebhookShutdownGrace = 5 * time.Second
	// appWebhookSigWarnInterval rate-limits the bad-signature log line: the
	// listener is exposed through a public route.
	appWebhookSigWarnInterval = 30 * time.Second
)

// newAppWebhookTransport builds a transport named name (the delivery's
// Transport label and the health row's name) that serves path on the listener
// listenFn returns, verifying with secret.
func newAppWebhookTransport(
	name, path string,
	listenFn func() (net.Listener, error),
	secret string,
	logFn func(issueNumber int, tag, format string, args ...any),
	emitFn func(tui.Event),
) *appWebhookTransport {
	return &appWebhookTransport{
		name:        name,
		path:        path,
		listenFn:    listenFn,
		secret:      secret,
		logFn:       logFn,
		emitFn:      emitFn,
		eventCounts: make(map[string]int),
		lastState:   WebhookStreamUnhealthy,
	}
}

// Name implements ingestTransport.
func (t *appWebhookTransport) Name() string { return t.name }

// SetSink implements ingestTransport.
func (t *appWebhookTransport) SetSink(sink func(delivery)) {
	t.mu.Lock()
	t.sink = sink
	t.mu.Unlock()
}

// SetReconnectHandler implements ingestTransport. A direct receiver has no
// connection to re-establish and the reconcile ticker is unchanged (FR-017), so
// the handler is stored and never called.
func (t *appWebhookTransport) SetReconnectHandler(fn func()) {
	t.mu.Lock()
	t.reconnectFn = fn
	t.mu.Unlock()
}

// SetCoverageCheck installs the installation-coverage re-check.
func (t *appWebhookTransport) SetCoverageCheck(fn func()) {
	t.mu.Lock()
	t.coverageFn = fn
	t.mu.Unlock()
}

// SetDriftHandler installs the callback for signature-drift transitions.
func (t *appWebhookTransport) SetDriftHandler(fn func(active bool)) { t.onDrift = fn }

// Start binds the listener and serves in the background. A bind failure is
// returned and leaves the transport not listening.
func (t *appWebhookTransport) Start(context.Context) error {
	ln, err := t.listenFn()
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(t.handle),
		ReadHeaderTimeout: appWebhookReadHeaderTimeout,
		ReadTimeout:       appWebhookReadTimeout,
		WriteTimeout:      appWebhookWriteTimeout,
		IdleTimeout:       appWebhookIdleTimeout,
	}
	t.mu.Lock()
	t.srv, t.ln, t.listening = srv, ln, true
	t.mu.Unlock()
	t.recomputeState("")
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.logFn(0, t.name, "listener stopped: %v\n", err)
			t.mu.Lock()
			t.listening = false
			t.mu.Unlock()
			t.recomputeState("listener stopped")
		}
	}()
	return nil
}

// Addr is the bound listener address ("" before Start).
func (t *appWebhookTransport) Addr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ln == nil {
		return ""
	}
	return t.ln.Addr().String()
}

// Stop shuts the server down, giving in-flight requests a short grace.
func (t *appWebhookTransport) Stop() {
	t.stopOnce.Do(func() {
		t.mu.Lock()
		srv := t.srv
		t.listening = false
		t.mu.Unlock()
		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), appWebhookShutdownGrace)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				_ = srv.Close()
			}
		}
		t.recomputeState("stopped")
	})
}

// handle is the HTTP handler. Check order is fixed: path (404), method (405),
// capped read (413, not a signature failure), signature (401, counted), JSON
// (400, not counted), ping (acknowledged, not submitted), then the sink (200).
func (t *appWebhookTransport) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != t.path {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// A declared length above the cap is answered 413 without reading the body
	// at all (same status, still before verification, still not a signature
	// failure). A chunked or understated body falls through to the capped read.
	if r.ContentLength > webhookMaxBodyBytes {
		t.oversized.Add(1)
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxBodyBytes+1))
	if err != nil {
		t.malformed.Add(1)
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}
	if len(body) > webhookMaxBodyBytes {
		t.oversized.Add(1)
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	if !events.VerifySignature(body, r.Header.Get("X-Hub-Signature-256"), t.secret) {
		t.rejectedSigs.Add(1)
		t.warnBadSignature()
		if t.sigDrift.RecordFailure() {
			t.driftTransition()
		}
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	// A verified request proves the secret is right, whatever else is wrong.
	if t.sigDrift.RecordSuccess() {
		t.driftTransition()
	}

	if !json.Valid(body) {
		t.malformed.Add(1)
		t.logFn(0, t.name, "rejected a correctly signed request whose body is not JSON — set the GitHub App's webhook content type to application/json\n")
		http.Error(w, "body is not JSON; set the App's webhook content type to application/json", http.StatusBadRequest)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	if eventType == "ping" {
		t.logFn(0, t.name, "received GitHub ping (delivery %s) — signature verified\n", r.Header.Get("X-GitHub-Delivery"))
		w.WriteHeader(http.StatusOK)
		return
	}

	t.mu.Lock()
	sink := t.sink
	if sink != nil {
		t.lastDeliveryAt = time.Now()
		t.eventCounts[eventType]++
	}
	t.mu.Unlock()
	if sink == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	t.emitCurrentState()
	sink(delivery{
		ID:        r.Header.Get("X-GitHub-Delivery"),
		EventType: eventType,
		Payload:   body,
		Transport: t.name,
	})
	w.WriteHeader(http.StatusOK)
}

// warnBadSignature logs a rejected signature at most once per interval.
func (t *appWebhookTransport) warnBadSignature() {
	t.mu.Lock()
	now := time.Now()
	log := now.Sub(t.lastSigWarnAt) >= appWebhookSigWarnInterval
	if log {
		t.lastSigWarnAt = now
	}
	t.mu.Unlock()
	if log {
		t.logFn(0, t.name, "rejected a request with a missing or invalid X-Hub-Signature-256 (check the App's webhook secret)\n")
	}
}

// driftTransition reports the tracker's current drift state to the operator
// (log line and onDrift callback) and re-derives health. RecordFailure and
// RecordSuccess decide their transition atomically, but their callers reach
// this function on separate request goroutines, so it is serialised and always
// reports the tracker's present state (not the state the caller saw): a late
// "drift began" can never overwrite a recovery that already happened, and a
// transition that has since been reversed is not reported twice.
func (t *appWebhookTransport) driftTransition() {
	t.driftMu.Lock()
	active := t.sigDrift.Active()
	changed := active != t.driftReported
	if changed {
		t.driftReported = active
		if active {
			t.logFn(0, t.name, "signature verification drift: %d consecutive failures with no interleaved success — "+
				"the App's webhook secret probably does not match the daemon's\n", events.SignatureDriftThreshold)
		} else {
			t.logFn(0, t.name, "signature verification recovered after a drift episode\n")
		}
		if t.onDrift != nil {
			t.onDrift(active)
		}
	}
	t.driftMu.Unlock()
	if !changed {
		return
	}
	reason := ""
	if active {
		reason = "signature verification drift"
	}
	t.recomputeState(reason)
}

// state derives the externally reported health state from the independently
// tracked conditions, so no single hint can mask another.
func (t *appWebhookTransport) state() WebhookHealthState {
	t.mu.Lock()
	listening, cacheDrift := t.listening, t.cacheDrift
	t.mu.Unlock()
	if !listening || cacheDrift || t.sigDrift.Active() {
		return WebhookStreamUnhealthy
	}
	return WebhookStreamHealthy
}

func (t *appWebhookTransport) recomputeState(reason string) {
	newState := t.state()
	t.mu.Lock()
	prev := t.lastState
	t.lastState = newState
	t.mu.Unlock()
	if prev == newState {
		return
	}
	if reason != "" {
		t.logFn(0, t.name, "health state: %s → %s (%s)\n", prev, newState, reason)
	} else {
		t.logFn(0, t.name, "health state: %s → %s\n", prev, newState)
	}
	t.emitCurrentState()
}

func (t *appWebhookTransport) emitCurrentState() {
	if t.emitFn == nil {
		return
	}
	state := string(t.state())
	t.mu.Lock()
	counts := make(map[string]int, len(t.eventCounts))
	for k, v := range t.eventCounts {
		counts[k] = v
	}
	note := t.installNote
	t.mu.Unlock()
	t.emitFn(tui.WebhookStatusEvent{State: state, EventCounts: counts, CoverageNote: &note})
}

// IsHealthyOrStartingUp implements ingestTransport.
func (t *appWebhookTransport) IsHealthyOrStartingUp() bool {
	return t.state() == WebhookStreamHealthy
}

// IsDisabled implements ingestTransport: there is no circuit breaker here.
func (t *appWebhookTransport) IsDisabled() bool { return false }

// TransitionHealth implements ingestTransport. A reconcile "healthy" hint is
// one input to the derived state, so it cannot clear signature drift or make a
// dead listener look alive.
func (t *appWebhookTransport) TransitionHealth(healthy bool, reason string) {
	t.mu.Lock()
	t.cacheDrift = !healthy
	t.mu.Unlock()
	t.recomputeState(reason)
}

// SubscribesTo implements ingestTransport: an App-level webhook receives every
// event type the App is subscribed to.
func (t *appWebhookTransport) SubscribesTo(string) bool { return true }

// UpdateRepos implements ingestTransport; the set feeds only the coverage check.
func (t *appWebhookTransport) UpdateRepos(repos map[string]bool) {
	list := sortedRepoList(repos)
	t.mu.Lock()
	t.managedRepos = list
	t.mu.Unlock()
}

// ManagedRepos is a sorted snapshot of the managed-repo set.
func (t *appWebhookTransport) ManagedRepos() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.managedRepos...)
}

// CheckCoverage implements ingestTransport.
func (t *appWebhookTransport) CheckCoverage() {
	t.mu.Lock()
	fn := t.coverageFn
	t.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// setInstallationCoverageNote records the installation-coverage result and
// re-emits the current state, mirroring hookdeckManager.
func (t *appWebhookTransport) setInstallationCoverageNote(missing []string, truncated bool) {
	var note string
	if len(missing) > 0 {
		note = fmt.Sprintf("installation missing: %d repo(s) (%s)", len(missing), strings.Join(missing, ", "))
		if truncated {
			note += " [unreliable: installation-repo listing was truncated by the pagination ceiling; some of these may actually be granted]"
		}
	}
	t.mu.Lock()
	changed := t.installNote != note
	t.installNote = note
	t.mu.Unlock()
	if changed {
		t.emitCurrentState()
	}
}

// HealthRow implements ingestTransport.
func (t *appWebhookTransport) HealthRow() localapi.TransportHealth {
	t.mu.Lock()
	listening, last := t.listening, t.lastDeliveryAt
	t.mu.Unlock()
	state := "not listening"
	switch {
	case t.sigDrift.Active():
		state = "signature drift"
	case listening:
		state = "listening"
	}
	return localapi.TransportHealth{
		Name:               t.name,
		State:              state,
		LastDeliveryAt:     localapi.KnownTime(last),
		RejectedSignatures: localapi.Some(t.rejectedSigs.Load()),
		MalformedRequests:  localapi.Some(t.malformed.Load()),
		OversizedRequests:  localapi.Some(t.oversized.Load()),
	}
}

var _ ingestTransport = (*appWebhookTransport)(nil)
var _ appCoverageTransport = (*appWebhookTransport)(nil)
