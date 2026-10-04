package localapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
)

// Reconnect backoff bounds for RunStream.
const (
	DefaultMinBackoff = 250 * time.Millisecond
	DefaultMaxBackoff = 30 * time.Second
	// watchdogBeats is how many missed heartbeats make a silent connection
	// "half-open": the client drops it and reconnects.
	watchdogBeats = 3
)

// StreamState is one connection-state transition reported by RunStream.
type StreamState struct {
	// Connected is true once an attach has been acknowledged, false whenever a
	// connection attempt fails or an established one is lost.
	Connected bool
	// Err is why the connection is down (nil when Connected).
	Err error
	// At is when the transition happened.
	At time.Time
	// Unsupported is set (with Connected false) when the daemon is reachable
	// but speaks no streaming protocol (a v1 daemon): push is off and the
	// client keeps probing at the slow backoff in case the daemon is upgraded.
	Unsupported bool
}

// StreamOptions configures RunStream.
type StreamOptions struct {
	// Path is the daemon socket.
	Path string
	// Subscriber is re-registered by an attach on every (re)connect, so held
	// delivery resumes with no action from the session.
	Subscriber string
	// OnEvent receives each pushed event, in order, from one goroutine.
	OnEvent func(channelevents.Event)
	// OnState, when set, receives every connected/disconnected transition. It
	// runs on the stream goroutine and must not block.
	OnState func(StreamState)

	MinBackoff, MaxBackoff time.Duration
	Logf                   func(format string, args ...any)
}

// errSuperseded ends RunStream: a newer attach under the same name displaced
// this one, so reconnecting would only fight it.
var errSuperseded = errors.New("superseded by a newer session with the same subscriber name")

// errUnsupported marks a daemon that does not speak protocol v2 streaming.
var errUnsupported = errors.New("daemon does not support streaming (protocol < 2)")

// RunStream holds a connection to the daemon socket open, attaches as
// opts.Subscriber and delivers pushed events to opts.OnEvent. It reconnects
// with jittered exponential backoff after a daemon restart (the SIGHUP re-exec
// closes the socket), a busy refusal or a silent half-open connection, and
// re-attaches every time. It returns nil when ctx ends or the session was
// superseded, and an error only for invalid options.
func RunStream(ctx context.Context, opts StreamOptions) error {
	if opts.Path == "" || opts.Subscriber == "" || opts.OnEvent == nil {
		return errors.New("RunStream: Path, Subscriber and OnEvent are required")
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = DefaultMinBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultMaxBackoff
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	report := func(st StreamState) {
		if opts.OnState != nil {
			st.At = time.Now()
			opts.OnState(st)
		}
	}

	backoff := opts.MinBackoff
	down := false // a down transition has been reported and no attach has succeeded since
	for ctx.Err() == nil {
		attached := false
		err := streamOnce(ctx, opts, func() {
			attached, down = true, false
			report(StreamState{Connected: true})
		})
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errSuperseded) {
			opts.Logf("[stream] %v\n", err)
			return nil
		}
		unsupported := errors.Is(err, errUnsupported)
		if attached {
			backoff = opts.MinBackoff
		}
		// One down report per outage, not one per retry.
		if !down {
			down = true
			report(StreamState{Connected: false, Err: err, Unsupported: unsupported})
		}
		wait := jitter(backoff)
		if unsupported {
			wait = jitter(opts.MaxBackoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		if backoff *= 2; backoff > opts.MaxBackoff {
			backoff = opts.MaxBackoff
		}
	}
	return nil
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.5 + rand.Float64()))
}

// streamOnce runs one connection: dial, hello, attach, then read frames until
// the connection fails. onAttached fires after the attach acknowledgement.
func streamOnce(ctx context.Context, opts StreamOptions, onAttached func()) error {
	d := net.Dialer{Timeout: DialTimeout}
	conn, err := d.DialContext(ctx, "unix", opts.Path)
	if err != nil {
		return &NoDaemonError{Path: opts.Path, Err: err}
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	r := bufio.NewReaderSize(conn, 64*1024)
	roundTrip := func(id, method string, params, result any) error {
		req := Frame{ID: id, Method: method}
		if params != nil {
			b, err := json.Marshal(params)
			if err != nil {
				return err
			}
			req.Params = b
		}
		b, err := json.Marshal(req)
		if err != nil {
			return err
		}
		conn.SetDeadline(time.Now().Add(DialTimeout + 2*time.Second))
		if _, err := conn.Write(append(b, '\n')); err != nil {
			return fmt.Errorf("writing %s: %w", method, err)
		}
		for {
			line, err := readLine(r, 16*MaxLineBytes)
			if err != nil {
				return fmt.Errorf("reading %s response: %w", method, err)
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var f Frame
			if err := json.Unmarshal(line, &f); err != nil {
				return fmt.Errorf("malformed frame: %w", err)
			}
			if f.IsServerInitiated() {
				continue
			}
			if f.ID != id && f.Error == nil {
				continue
			}
			if f.Error != nil {
				return f.Error
			}
			if result != nil && len(f.Result) > 0 {
				return json.Unmarshal(f.Result, result)
			}
			return nil
		}
	}

	var hello struct {
		Version int      `json:"protocol_version"`
		Methods []string `json:"methods"`
	}
	if err := roundTrip("h", MethodHello, nil, &hello); err != nil {
		return err
	}
	supported := false
	for _, m := range hello.Methods {
		if m == MethodAttach {
			supported = true
		}
	}
	if hello.Version < 2 || !supported {
		return errUnsupported
	}
	var ack AttachResult
	if err := roundTrip("a", MethodAttach, AttachParams{Subscriber: opts.Subscriber}, &ack); err != nil {
		return err
	}
	onAttached()

	hb := time.Duration(ack.HeartbeatMillis) * time.Millisecond
	if hb <= 0 {
		hb = DefaultHeartbeat
	}
	for {
		conn.SetReadDeadline(time.Now().Add(watchdogBeats * hb))
		line, err := readLine(r, 16*MaxLineBytes)
		if err != nil {
			return fmt.Errorf("stream read: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			return fmt.Errorf("malformed stream frame: %w", err)
		}
		switch {
		case f.Error != nil && f.ID == "":
			return f.Error // e.g. a busy refusal
		case f.Event == EventHeartbeat:
		case f.Event == EventSuperseded:
			return errSuperseded
		case f.Event == EventChannel:
			var ev channelevents.Event
			if err := json.Unmarshal(f.Params, &ev); err != nil {
				return fmt.Errorf("malformed channel event: %w", err)
			}
			opts.OnEvent(ev)
		}
	}
}
