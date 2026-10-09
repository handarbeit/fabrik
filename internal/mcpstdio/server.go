// Package mcpstdio is a minimal, stdlib-only Model Context Protocol server over
// stdio (newline-delimited JSON-RPC 2.0) that proxies three read tools to the
// Fabrik daemon's local socket (#1967, ADR-1966-a), plus the subscribe tools
// and the Claude Code Channels push of #1968 (ADR-1966-b, channel.go).
//
// stdout is the protocol channel: this package writes nothing to Out except
// JSON-RPC frames, one per line. Diagnostics go to Err.
//
// A tool failure — including "no daemon is listening" — is a tool *result* with
// isError set, not a JSON-RPC error, so the model sees and can act on the
// message; JSON-RPC errors are reserved for protocol problems (malformed
// frames, unknown methods, unknown tools). Every daemon call is bounded by
// CallTimeout, so a wedged or absent daemon can never hang the session.
package mcpstdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// Latest first. initialize echoes the client's version when it is one of these.
var supportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// DefaultCallTimeout bounds one proxied daemon call.
const DefaultCallTimeout = 20 * time.Second

// maxLineBytes bounds one inbound JSON-RPC line.
const maxLineBytes = 4 << 20

// CallFunc performs one daemon call. localapi.Call is the production value.
type CallFunc func(ctx context.Context, socketPath, method string, params, result any) error

// Server is the MCP server. Zero values for Call and CallTimeout select the
// production defaults.
type Server struct {
	// SocketPath is the daemon socket every tool call dials.
	SocketPath string
	// Version is reported as serverInfo.version.
	Version string
	// In and Out are the protocol channel (stdin/stdout); Err receives diagnostics.
	In  io.Reader
	Out io.Writer
	Err io.Writer

	Call        CallFunc
	CallTimeout time.Duration

	// Subscriber is the stable name this shim attaches to the daemon under (the
	// operator's topic or session name — never a PID). Empty disables push:
	// the read tools work unchanged and fabrik_subscribe reports why it cannot
	// default a name. #1968.
	Subscriber string
	// Stream runs the held connection; nil selects localapi.RunStream.
	Stream StreamFunc
	// UnreachableGrace overrides DefaultUnreachableGrace.
	UnreachableGrace time.Duration

	wmu        sync.Mutex
	streamOnce sync.Once
	streamWG   sync.WaitGroup
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// JSON-RPC error codes.
const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
)

func (s *Server) logf(format string, args ...any) {
	if s.Err != nil {
		fmt.Fprintf(s.Err, "fabrik mcp: "+format+"\n", args...)
	}
}

// Serve reads requests until In reaches EOF or ctx is cancelled, handling each
// in its own goroutine so a slow tool call never blocks ping or a second
// call. It returns after every in-flight request has been answered.
func (s *Server) Serve(ctx context.Context) error {
	if s.Call == nil {
		s.Call = localapi.Call
	}
	if s.CallTimeout <= 0 {
		s.CallTimeout = DefaultCallTimeout
	}
	// The held stream ends with Serve: on stdin EOF or cancellation it is
	// stopped and joined before Serve returns.
	sctx, stopStream := context.WithCancel(ctx)
	defer s.streamWG.Wait()
	defer stopStream()
	var wg sync.WaitGroup
	defer wg.Wait()

	br := bufio.NewReaderSize(s.In, 64*1024)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := readLine(br)
		if len(bytes.TrimSpace(line)) > 0 {
			line := line
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.handleLine(sctx, line)
			}()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading stdin: %w", err)
		}
	}
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLineBytes {
			return nil, fmt.Errorf("line exceeds %d bytes", maxLineBytes)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buf, err
	}
}

func (s *Server) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.logf("encoding response: %v", err)
		return
	}
	b = append(b, '\n')
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.Out.Write(b); err != nil {
		s.logf("writing response: %v", err)
	}
}

func (s *Server) reply(id json.RawMessage, result any) {
	s.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) fail(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	s.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) handleLine(ctx context.Context, line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) > 0 && line[0] == '[' {
		s.fail(nil, errInvalidRequest, "batch requests are not supported")
		return
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.fail(nil, errParse, "parse error: "+err.Error())
		return
	}
	isNotification := len(req.ID) == 0
	if req.Method == "" {
		if !isNotification {
			s.fail(req.ID, errInvalidRequest, "missing method")
		}
		return
	}

	switch req.Method {
	case "initialize":
		s.reply(req.ID, s.initialize(req.Params))
	case "ping":
		s.reply(req.ID, struct{}{})
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": toolDefs()})
	case "tools/call":
		s.toolsCall(ctx, req)
	case "notifications/initialized":
		s.startStream(ctx)
	default:
		if isNotification {
			return // notifications/cancelled, ...: nothing to do
		}
		s.fail(req.ID, errMethodNotFound, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	version := supportedProtocolVersions[0]
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil {
		for _, v := range supportedProtocolVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
			// Claude Code Channels (research preview): this server may push
			// notifications/claude/channel into the session (#1968).
			"experimental": map[string]any{"claude/channel": map[string]any{}},
		},
		"serverInfo": map[string]any{"name": "fabrik", "version": s.Version},
		"instructions": "View of a running Fabrik daemon's in-memory state, served from the daemon's cache at no GitHub cost (fabrik_status, fabrik_board, fabrik_health). " +
			"Every response states how fresh it is (as_of); a value the daemon cannot vouch for is the string \"unknown\", never a default. " +
			"Start with fabrik_board (attention) to see what needs looking at, then fabrik_status for one issue. " +
			"Four action tools (fabrik_promote, fabrik_set_autonomy, fabrik_revalidate, fabrik_clear_claude_limit) make the daemon write to GitHub and leave a Fabrik audit comment naming this session's --subscriber name; they refuse, with the current state, when a precondition does not hold. " +
			"There is no tool to lift a pause or to comment: answer a paused item with a comment on the issue itself." + channelInstructions,
	}
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textResult(text string, isError bool) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: text}}, IsError: isError}
}

func (s *Server) toolsCall(ctx context.Context, req request) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		s.fail(req.ID, errInvalidParams, "tools/call needs params {name, arguments}")
		return
	}

	method, params, err := buildCall(p.Name, p.Arguments, s.Subscriber)
	if err != nil {
		var ue *unknownToolError
		if errors.As(err, &ue) {
			s.fail(req.ID, errInvalidParams, err.Error())
			return
		}
		s.reply(req.ID, textResult("invalid arguments: "+err.Error(), true))
		return
	}

	cctx, cancel := context.WithTimeout(ctx, s.CallTimeout)
	defer cancel()
	var result json.RawMessage
	if err := s.Call(cctx, s.SocketPath, method, params, &result); err != nil {
		s.reply(req.ID, textResult(describeToolError(p.Name, err), true))
		return
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, result, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(result)
	}
	res := textResult(pretty.String(), false)
	if p.Name == ToolSubscribe {
		// The daemon cannot know whether this session loaded the shim as a channel;
		// say so rather than let a successful subscribe imply delivery.
		res.Content = append(res.Content, toolContent{Type: "text", Text: subscribeNote(s.Subscriber)})
	}
	s.reply(req.ID, res)
}

func subscribeNote(attached string) string {
	note := "Note: Fabrik cannot tell whether this session receives channel pushes. They appear only if Claude Code was started with Channels enabled for this server " +
		"(research preview: claude --dangerously-load-development-channels server:fabrik; Team/Enterprise orgs must also allow channels). Otherwise events are dropped silently."
	if attached == "" {
		note += " This server was launched without --subscriber, so it holds no push connection: events for this subscription are held by the daemon and delivered once a server with that name attaches."
	}
	return note
}

// describeToolError is describeCallError plus the action-specific cases: a
// daemon that predates actions answers unknown_method, which is reported as
// "does not support actions" rather than an opaque protocol code.
func describeToolError(tool string, err error) string {
	var pe *localapi.Error
	if IsActionTool(tool) && errors.As(err, &pe) && pe.Code == localapi.CodeUnknownMethod {
		return "this Fabrik daemon does not support overseer actions (it predates them or has them disabled); upgrade and restart it, or make the change as a human on GitHub"
	}
	return describeCallError(err)
}

// describeCallError turns a daemon-call failure into the text the model sees.
func describeCallError(err error) string {
	var nd *localapi.NoDaemonError
	if errors.As(err, &nd) {
		return nd.Error() + ". Start the daemon (fabrik) in that directory, or point this server at the right one with --dir."
	}
	var pe *localapi.Error
	if errors.As(err, &pe) {
		if pe.Code == localapi.CodeRefused {
			// A refusal carries the item's current state; show it so the caller
			// can see why without a second call.
			msg := "refused: " + pe.Message
			if len(pe.Data) > 0 {
				var pretty bytes.Buffer
				if json.Indent(&pretty, pe.Data, "", "  ") == nil {
					msg += "\n\ncurrent state:\n" + pretty.String()
				}
			}
			return msg
		}
		return fmt.Sprintf("fabrik daemon error (%s): %s", pe.Code, pe.Message)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the Fabrik daemon did not answer in time"
	}
	return "fabrik daemon call failed: " + err.Error()
}
