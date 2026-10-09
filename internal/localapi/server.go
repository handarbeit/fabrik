package localapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
)

// Server limits. They bound what a hung or abusive client can cost the daemon.
const (
	// MaxConns is the concurrent connection cap; extras get a busy error frame.
	MaxConns = 32
	// WriteTimeout is the per-frame write deadline: a client that never reads
	// cannot block a handler (or, later, a pusher) indefinitely.
	WriteTimeout = 5 * time.Second
	// DefaultIdleTimeout is how long a connection may sit without sending a
	// frame. An attached (streaming) connection is exempt in practice: every
	// heartbeat or push the server writes successfully moves the deadline.
	DefaultIdleTimeout = 10 * time.Minute
	// DefaultHeartbeat is the cadence of server heartbeats on an attached
	// connection. A failed heartbeat write (the 5 s write deadline) reaps a dead
	// peer within one interval instead of at the idle timeout.
	DefaultHeartbeat = 30 * time.Second
)

// Server serves Backend on a Unix socket.
type Server struct {
	path string
	be   Backend
	logf func(format string, args ...any)

	// OnSession, when set before Start, is called for every accepted
	// connection, before its first frame is read. It lets a future streaming
	// layer keep the Session to push server-initiated frames.
	OnSession func(*Session)

	// Streamer, when set before Start, enables protocol v2's subscribe /
	// unsubscribe / attach methods.
	Streamer Streamer
	// Actor, when set before Start, enables the mutating action methods
	// (#1969). It is dispatched by handleAction, separately from the read
	// switch in handle, so a server without one answers them unknown_method
	// and the read path can never mutate.
	Actor Actor
	// IdleTimeout and HeartbeatInterval default to the constants above; tests
	// shorten them.
	IdleTimeout       time.Duration
	HeartbeatInterval time.Duration

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	sem    chan struct{}
	wg     sync.WaitGroup
}

// NewServer builds a Server for the given socket path. logf may be nil.
func NewServer(path string, be Backend, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{
		path:  path,
		be:    be,
		logf:  logf,
		conns: make(map[net.Conn]struct{}),
		sem:   make(chan struct{}, MaxConns),
	}
}

// Path returns the socket path.
func (s *Server) Path() string { return s.path }

// Listen binds the socket at path with mode 0600, replacing a stale socket file
// left by a previous process (SIGHUP/self-upgrade re-exec or a crash). It only
// ever removes a file whose type is socket — a regular file or directory at the
// path is an error, never deleted. The caller must already hold the instance
// lock: that is what makes a pre-existing socket stale rather than live.
func Listen(path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is %d bytes, over the %d-byte sun_path limit", path, len(path), maxSocketPath)
	}
	if err := prepareDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace %s: it exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspecting %s: %w", path, err)
	}

	var ln net.Listener
	if err := withUmask(0o177, func() error {
		var lerr error
		ln, lerr = net.Listen("unix", path)
		return lerr
	}); err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	// Explicit chmod too: the umask guarantees the bind itself was 0600, this
	// pins the mode regardless of platform umask semantics.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return ln, nil
}

// Start binds the socket and serves in the background. A returned error means
// the API is unavailable; callers treat it as non-fatal.
func (s *Server) Start() error {
	ln, err := Listen(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return errors.New("server already closed")
	}
	s.ln = ln
	s.mu.Unlock()

	s.wg.Add(1)
	go s.acceptLoop(ln)
	return nil
}

// Close stops accepting, closes every connection, waits for handlers and
// removes the socket file. Idempotent and safe to call concurrently.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	var err error
	if ln != nil {
		err = ln.Close() // a Unix listener unlinks its socket file on Close
	}
	for _, c := range conns {
		c.Close()
	}
	s.wg.Wait()
	if ln != nil {
		// Belt and braces: remove the file if (and only if) it is still a socket.
		if fi, lerr := os.Lstat(s.path); lerr == nil && fi.Mode()&os.ModeSocket != 0 {
			os.Remove(s.path)
		}
	}
	return err
}

func (s *Server) acceptLoop(ln net.Listener) {
	defer s.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logf("[localapi] accept: %v\n", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case s.sem <- struct{}{}:
		default:
			// Over the cap: tell the client why, then drop it.
			sess := newSession(c)
			sess.Send(Frame{Error: Errorf(CodeBusy, "too many connections (max %d)", MaxConns)})
			c.Close()
			continue
		}
		if !s.track(c) {
			<-s.sem
			c.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.sem }()
			defer s.untrack(c)
			s.serveConn(c)
		}()
	}
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	c.Close()
}

// Session is one client connection. Send is safe for concurrent use, so a
// server-initiated frame can interleave with request/response traffic.
type Session struct {
	conn net.Conn
	wmu  sync.Mutex
	done chan struct{}

	// idle, when non-zero, makes every successful Send push the read deadline
	// out by that much: a push-only subscriber sends nothing, so the server's
	// own writes are what keep its connection alive (R1a).
	kmu  sync.Mutex
	idle time.Duration
}

func newSession(c net.Conn) *Session { return &Session{conn: c, done: make(chan struct{})} }

// Close ends the connection.
func (s *Session) Close() error { return s.conn.Close() }

func (s *Session) setKeepalive(idle time.Duration) {
	s.kmu.Lock()
	s.idle = idle
	s.kmu.Unlock()
}

// Done is closed when the connection ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Send writes one frame as a single line under a write deadline.
func (s *Session) Send(f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encoding frame: %w", err)
	}
	b = append(b, '\n')
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
	_, err = s.conn.Write(b)
	if err == nil {
		s.kmu.Lock()
		idle := s.idle
		s.kmu.Unlock()
		if idle > 0 {
			s.conn.SetReadDeadline(time.Now().Add(idle))
		}
	}
	return err
}

func (s *Server) idleTimeout() time.Duration {
	if s.IdleTimeout > 0 {
		return s.IdleTimeout
	}
	return DefaultIdleTimeout
}

func (s *Server) heartbeatInterval() time.Duration {
	if s.HeartbeatInterval > 0 {
		return s.HeartbeatInterval
	}
	return DefaultHeartbeat
}

func (s *Server) serveConn(c net.Conn) {
	sess := newSession(c)
	defer close(sess.done)
	var detach func()
	defer func() {
		if detach != nil {
			detach()
		}
	}()
	if s.OnSession != nil {
		s.OnSession(sess)
	}
	r := bufio.NewReaderSize(c, 64*1024)
	for {
		c.SetReadDeadline(time.Now().Add(s.idleTimeout()))
		line, err := readLine(r, MaxLineBytes)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				sess.Send(Frame{Error: Errorf(CodeBadRequest, "request exceeds %d bytes", MaxLineBytes)})
			}
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var req Frame
		if err := json.Unmarshal(line, &req); err != nil {
			if sess.Send(Frame{Error: Errorf(CodeBadRequest, "malformed frame: %v", err)}) != nil {
				return
			}
			continue
		}
		if !req.IsRequest() {
			if sess.Send(Frame{ID: req.ID, Error: Errorf(CodeBadRequest, "frame is not a request (need id and method)")}) != nil {
				return
			}
			continue
		}
		if req.Method == MethodAttach {
			if detach != nil {
				if sess.Send(Frame{ID: req.ID, Error: Errorf(CodeBadRequest, "connection is already attached")}) != nil {
					return
				}
				continue
			}
			resp, bind := s.handleAttach(req, sess)
			if sess.Send(resp) != nil {
				return
			}
			if bind != nil {
				detach = bind()
			}
			continue
		}
		if sess.Send(s.handle(req)) != nil {
			return
		}
	}
}

// sessionSink adapts a Session to the hub's Sink: events become "channel"
// frames; a supersede notice is sent and the connection closed so the displaced
// client stops reconnecting.
type sessionSink struct {
	sess       *Session
	subscriber string
}

func (k sessionSink) Deliver(ev channelevents.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encoding event: %w", err)
	}
	if err := k.sess.Send(Frame{Event: EventChannel, Sub: k.subscriber, Params: b}); err != nil {
		// A failed (possibly partial) write leaves the stream unusable and heartbeats
		// would keep it looking healthy: end the connection so the client re-attaches
		// and the unremoved queue entry is redelivered.
		k.sess.Close()
		return err
	}
	return nil
}

func (k sessionSink) Superseded() {
	k.sess.Send(Frame{Event: EventSuperseded, Sub: k.subscriber})
	k.sess.Close()
}

// handleAttach validates an attach and builds its acknowledgement. The returned
// bind func runs in serveConn only after the acknowledgement has been written,
// so no push or heartbeat can precede it; it attaches the session, starts the
// heartbeat and returns the detach func. bind is nil when the request is
// refused.
func (s *Server) handleAttach(req Frame, sess *Session) (Frame, func() func()) {
	if s.Streamer == nil {
		return Frame{ID: req.ID, Error: Errorf(CodeUnknownMethod, "unknown method %q", req.Method)}, nil
	}
	var p AttachParams
	if e := decodeParams(req.Params, &p); e != nil {
		return Frame{ID: req.ID, Error: e}, nil
	}
	if err := channelevents.ValidateSubscriberName(p.Subscriber); err != nil {
		return Frame{ID: req.ID, Error: Errorf(CodeBadRequest, "%v", err)}, nil
	}
	idle, hb := s.idleTimeout(), s.heartbeatInterval()
	res, _ := json.Marshal(AttachResult{
		Subscriber:        p.Subscriber,
		HeartbeatMillis:   hb.Milliseconds(),
		IdleTimeoutMillis: idle.Milliseconds(),
	})
	bind := func() func() {
		sess.setKeepalive(idle)
		detach, err := s.Streamer.Attach(p, sessionSink{sess: sess, subscriber: p.Subscriber})
		if err != nil {
			s.logf("[localapi] attach %q: %v\n", p.Subscriber, err)
			sess.Close()
			return nil
		}
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(hb)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					// A successful write also extends the read deadline; a
					// failed one (dead peer, 5 s write deadline) ends the
					// connection now rather than at the idle timeout.
					if sess.Send(Frame{Event: EventHeartbeat, Sub: p.Subscriber}) != nil {
						sess.Close()
						return
					}
				case <-stop:
					return
				case <-sess.done:
					return
				}
			}
		}()
		return func() {
			close(stop)
			if detach != nil {
				detach()
			}
		}
	}
	return Frame{ID: req.ID, Result: res}, bind
}

var errLineTooLong = errors.New("line too long")

// readLine reads one '\n'-terminated line of at most max bytes.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > max {
			return nil, errLineTooLong
		}
		switch {
		case err == nil:
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(buf) > 0:
			return buf, nil
		default:
			return nil, err
		}
	}
}

// handle answers one request. A panic in the backend becomes an internal error
// for that request only; it never takes the daemon down.
func (s *Server) handle(req Frame) (resp Frame) {
	resp.ID = req.ID
	defer func() {
		if p := recover(); p != nil {
			s.logf("[localapi] panic serving %q: %v\n", req.Method, p)
			resp = Frame{ID: req.ID, Error: Errorf(CodeInternal, "internal error serving %s", req.Method)}
		}
	}()

	if IsActionMethod(req.Method) {
		return s.handleAction(req)
	}

	var result any
	var err error
	switch req.Method {
	case MethodHello:
		methods := []string{MethodHello, MethodStatus, MethodBoard, MethodHealth}
		if s.Streamer != nil {
			methods = append(methods, MethodSubscribe, MethodUnsubscribe, MethodAttach)
		}
		if s.Actor != nil {
			methods = append(methods, ActionMethods()...)
		}
		result = map[string]any{
			"protocol_version": ProtocolVersion,
			"methods":          methods,
		}
	case MethodSubscribe:
		if s.Streamer == nil {
			return Frame{ID: req.ID, Error: Errorf(CodeUnknownMethod, "unknown method %q", req.Method)}
		}
		var p SubscribeParams
		if e := decodeParams(req.Params, &p); e != nil {
			return Frame{ID: req.ID, Error: e}
		}
		result, err = s.Streamer.Subscribe(p)
	case MethodUnsubscribe:
		if s.Streamer == nil {
			return Frame{ID: req.ID, Error: Errorf(CodeUnknownMethod, "unknown method %q", req.Method)}
		}
		var p UnsubscribeParams
		if e := decodeParams(req.Params, &p); e != nil {
			return Frame{ID: req.ID, Error: e}
		}
		result, err = s.Streamer.Unsubscribe(p)
	case MethodStatus:
		var p StatusParams
		if e := decodeParams(req.Params, &p); e != nil {
			return Frame{ID: req.ID, Error: e}
		}
		result, err = s.be.Status(p)
	case MethodBoard:
		var p BoardParams
		if e := decodeParams(req.Params, &p); e != nil {
			return Frame{ID: req.ID, Error: e}
		}
		if p.View == "" {
			p.View = ViewAttention
		}
		if p.View != ViewAttention && p.View != ViewFlow {
			return Frame{ID: req.ID, Error: Errorf(CodeBadRequest, "unknown view %q (want %q or %q)", p.View, ViewAttention, ViewFlow)}
		}
		result, err = s.be.Board(p)
	case MethodHealth:
		result, err = s.be.Health(HealthParams{})
	default:
		return Frame{ID: req.ID, Error: Errorf(CodeUnknownMethod, "unknown method %q", req.Method)}
	}
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) {
			return Frame{ID: req.ID, Error: pe}
		}
		return Frame{ID: req.ID, Error: Errorf(CodeInternal, "%v", err)}
	}
	b, merr := json.Marshal(result)
	if merr != nil {
		return Frame{ID: req.ID, Error: Errorf(CodeInternal, "encoding result: %v", merr)}
	}
	resp.Result = b
	return resp
}

// handleAction serves the mutating method set. It is the only place an Actor
// is reached. With no Actor every action method is unknown. A refusal or other
// *Error from the actor is sent as-is; panics are recovered by handle's defer.
func (s *Server) handleAction(req Frame) Frame {
	if s.Actor == nil {
		return Frame{ID: req.ID, Error: Errorf(CodeUnknownMethod, "unknown method %q", req.Method)}
	}
	var result any
	var err error
	switch req.Method {
	case MethodPromote:
		var p PromoteParams
		if e := decodeParams(req.Params, &p); e != nil {
			return Frame{ID: req.ID, Error: e}
		}
		result, err = s.Actor.Promote(p)
	case MethodSetAutonomy:
		var p SetAutonomyParams
		if e := decodeParams(req.Params, &p); e != nil {
			return Frame{ID: req.ID, Error: e}
		}
		result, err = s.Actor.SetAutonomy(p)
	case MethodRevalidate:
		var p RevalidateParams
		if e := decodeParams(req.Params, &p); e != nil {
			return Frame{ID: req.ID, Error: e}
		}
		result, err = s.Actor.Revalidate(p)
	case MethodClearClaudeLimit:
		var p ClearClaudeLimitParams
		if e := decodeParams(req.Params, &p); e != nil {
			return Frame{ID: req.ID, Error: e}
		}
		result, err = s.Actor.ClearClaudeLimit(p)
	default:
		return Frame{ID: req.ID, Error: Errorf(CodeUnknownMethod, "unknown method %q", req.Method)}
	}
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) {
			return Frame{ID: req.ID, Error: pe}
		}
		return Frame{ID: req.ID, Error: Errorf(CodeInternal, "%v", err)}
	}
	b, merr := json.Marshal(result)
	if merr != nil {
		return Frame{ID: req.ID, Error: Errorf(CodeInternal, "encoding result: %v", merr)}
	}
	return Frame{ID: req.ID, Result: b}
}

func decodeParams(raw json.RawMessage, into any) *Error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return Errorf(CodeBadRequest, "bad params: %v", err)
	}
	return nil
}
