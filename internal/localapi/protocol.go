// Package localapi is the daemon's local read API (#1967, ADR-1966-a): a
// newline-delimited-JSON protocol served on a per-daemon Unix socket, plus the
// client the `fabrik mcp` proxy uses. It is deliberately independent of the
// engine — the engine implements Backend — so cmd and the MCP server can import
// the wire types without importing the engine.
//
// # Framing
//
// One JSON object per line, in both directions, never containing a raw
// newline. A request carries an id and a method; the response echoes the id
// with either a result or an error. A frame with no id is a server-initiated
// message (it carries an event and a sub). Protocol v2 (#1968, ADR-1966-b)
// emits them: after a successful "attach" request the server pushes
// "channel" frames (one pushed event each, in params), periodic "heartbeat"
// frames, and a final "superseded" frame if a newer attach under the same
// subscriber name displaces this connection. The long-lived streaming
// connection shares the socket with request/response traffic without a
// framing change.
package localapi

import (
	"encoding/json"
	"fmt"

	"github.com/handarbeit/fabrik/internal/channelevents"
)

// ProtocolVersion is returned by the "hello" method. Version 2 adds the
// streaming methods; a v1 daemon lacks them, which a client detects from
// hello's "methods" list and degrades to read-only.
const ProtocolVersion = 2

// Methods.
const (
	MethodHello       = "hello"
	MethodStatus      = "status"
	MethodBoard       = "board"
	MethodHealth      = "health"
	MethodSubscribe   = "subscribe"
	MethodUnsubscribe = "unsubscribe"
	MethodAttach      = "attach"
)

// Server-initiated event names (Frame.Event).
const (
	// EventChannel carries one channelevents.Event in Params.
	EventChannel = "channel"
	// EventHeartbeat keeps an attached connection's idle deadline moving and
	// lets either side notice a dead peer; clients swallow it.
	EventHeartbeat = "heartbeat"
	// EventSuperseded tells a displaced session to stop reconnecting.
	EventSuperseded = "superseded"
)

// Error codes.
const (
	CodeBadRequest    = "bad_request"
	CodeUnknownMethod = "unknown_method"
	CodeNotFound      = "not_found"
	CodeAmbiguous     = "ambiguous"
	CodeInternal      = "internal"
	CodeBusy          = "busy"
)

// Frame is one protocol message. Request: ID+Method(+Params). Response: ID plus
// Result or Error. Server-initiated: no ID, Event(+Sub, Params).
type Frame struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	// Event and Sub name a server-initiated message and its subscription.
	Event string `json:"event,omitempty"`
	Sub   string `json:"sub,omitempty"`
}

// IsRequest reports whether f is a client request.
func (f Frame) IsRequest() bool { return f.ID != "" && f.Method != "" }

// IsResponse reports whether f answers a request.
func (f Frame) IsResponse() bool { return f.ID != "" && f.Method == "" }

// IsServerInitiated reports whether f is a server push (no id).
func (f Frame) IsServerInitiated() bool { return f.ID == "" && f.Event != "" }

// Error is a protocol error.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Errorf builds an *Error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// MaxLineBytes bounds one frame (request line) the server will read.
const MaxLineBytes = 1 << 20

// Streamer is the optional streaming half of the daemon API (protocol v2). A
// Backend that also implements it enables subscribe/unsubscribe/attach; the
// three-method Backend contract is unchanged.
type Streamer interface {
	Subscribe(SubscribeParams) (*SubscribeResult, error)
	Unsubscribe(UnsubscribeParams) (*UnsubscribeResult, error)
	// Attach binds sink as the live session for p.Subscriber and returns the
	// detach func the server calls when the connection ends.
	Attach(p AttachParams, sink channelevents.Sink) (detach func(), err error)
}

// Backend serves the three read methods. Every method must be answered purely
// from in-memory state: no GitHub call, ever (R7). A returned *Error is sent
// to the client as-is; any other error becomes CodeInternal.
type Backend interface {
	Status(StatusParams) (*StatusResult, error)
	Board(BoardParams) (*BoardResult, error)
	Health(HealthParams) (*HealthResult, error)
}
