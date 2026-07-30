// Package hookdeck implements events.EventSource by speaking Hookdeck's CLI
// session protocol directly: a REST call to create a session, then a single
// WebSocket connection carrying JSON "attempt"/"attempt_response" frames.
// This is a reimplementation, not an embedding of hookdeck-cli's own Go
// transport (pkg/listen/proxy) — that package installs its own SIGTERM
// handler (would compete with pruefer/execute.go's signal.NotifyContext)
// and pulls in logrus plus its own REST/WS client for a wire protocol that
// is small enough to own directly. See adrs/1254-*.md.
//
// No Hookdeck-specific type or concept crosses the events.EventSource
// boundary into review/domain code — this package is purely a transport
// adapter that produces events.GitHubEvent values.
package hookdeck

import "encoding/json"

// createSessionRequest is the body of POST /cli-sessions. An empty
// WebhookIDs means "all connections visible to this API key" — no
// connection-ID configuration is needed on Pruefer's side.
type createSessionRequest struct {
	WebhookIDs []string `json:"webhook_ids"`
}

// createSessionResponse is the response body of POST /cli-sessions.
type createSessionResponse struct {
	ID string `json:"id"`
}

// wsFrame is the outer envelope for every message on the Hookdeck CLI
// session WebSocket, discriminated by Event.
type wsFrame struct {
	Event string          `json:"event"`
	Body  json.RawMessage `json:"body"`
}

const (
	wsEventAttempt         = "attempt"
	wsEventAttemptResponse = "attempt_response"
)

// attemptBody is the payload of an incoming "attempt" frame: one forwarded
// webhook delivery.
type attemptBody struct {
	CLIPath   string         `json:"cli_path"`
	EventID   string         `json:"event_id"`
	AttemptID string         `json:"attempt_id"`
	WebhookID string         `json:"webhook_id"`
	Request   attemptRequest `json:"request"`
}

// attemptRequest carries the original forwarded HTTP request: DataString is
// the raw (not base64-encoded) webhook body, and Headers carries the
// original GitHub request headers (X-Hub-Signature-256, X-GitHub-Event,
// X-GitHub-Delivery).
type attemptRequest struct {
	Method     string      `json:"method"`
	Timeout    int         `json:"timeout"`
	DataString string      `json:"data_string"`
	Headers    flexHeaders `json:"headers"`
}

// flexHeaders decodes an attempt frame's forwarded-header map, tolerating
// either a flat string-per-key shape (the format observed against
// Hookdeck's current CLI-session protocol) or an http.Header-style
// array-per-key shape, in case a future Hookdeck revision or an
// intermediary ever forwards raw multi-value headers that way. A per-key
// shape surprise must not fail decoding of the whole attempt frame — that
// would silently drop 100% of event-driven ingestion while transport
// health still reports connected. lookupHeader only ever needs a single
// value per header of interest (X-Hub-Signature-256, X-GitHub-Event,
// X-GitHub-Delivery), none of which GitHub ever sends multi-valued, so the
// first array element is sufficient.
type flexHeaders map[string]string

func (h *flexHeaders) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make(flexHeaders, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
			continue
		}
		var arr []string
		if err := json.Unmarshal(v, &arr); err == nil && len(arr) > 0 {
			out[k] = arr[0]
			continue
		}
		// Any other per-key shape (null, number, empty array) is skipped
		// rather than failing the frame; lookupHeader treats a missing key
		// the same as an empty value. For X-Hub-Signature-256 specifically,
		// this means a header Hookdeck sent in some unrecognized shape is
		// indistinguishable downstream from one it never sent at all — both
		// surface as source.go's "no X-Hub-Signature-256 header" log line,
		// not "invalid signature". Accepted rather than plumbed through:
		// the operator-facing fix is the same either way (check Hookdeck's
		// source config), and the frame must not fail decoding just because
		// one header looked unusual.
	}
	*h = out
	return nil
}

// attemptResponseBody is the payload of an outgoing "attempt_response"
// frame — the ack sent back over the same socket. No local HTTP hop is
// involved: Pruefer has no server to forward to, so it acks in-process
// immediately after verify+dedupe+normalize+dispatch.
type attemptResponseBody struct {
	AttemptID string `json:"attempt_id"`
	CLIPath   string `json:"cli_path"`
	Status    int    `json:"status"`
	Data      string `json:"data"`
}
