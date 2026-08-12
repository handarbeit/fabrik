package hookdeck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DefaultAPIBaseURL is Hookdeck's production REST API base URL, versioned
// per the upstream protocol as traced from hookdeck-cli's source (see
// protocol.go, and ADR-1254's amendment on claim strength — this is not a
// published spec and is not exercised against a live session in CI).
const DefaultAPIBaseURL = "https://api.hookdeck.com/2025-07-01"

// DefaultWSBaseURL is Hookdeck's production WebSocket endpoint for CLI
// sessions.
const DefaultWSBaseURL = "wss://ws.hookdeck.com"

// maxSessionResponseBytes bounds the cli-session creation response body —
// see the io.LimitReader call site in createSession's success path.
const maxSessionResponseBytes = 1 << 20 // 1MiB

// createSession creates a new Hookdeck CLI session scoped to apiKey via
// HTTP Basic auth (username=apiKey, empty password), returning the session
// ID used as the Websocket-Id dial header.
//
// The request is bound to ctx — runOnce passes a connectCtx bounded by
// Config.connectTimeout, shared with the WebSocket dial that follows —
// since http.Client has no default timeout of its own and a hang (dead
// TCP, slow DNS, a connected-but-non-responding endpoint) would otherwise
// block indefinitely rather than surfacing as a retryable error.
func createSession(ctx context.Context, httpClient *http.Client, baseURL, apiKey string) (string, error) {
	reqBody, err := json.Marshal(createSessionRequest{WebhookIDs: []string{}})
	if err != nil {
		return "", fmt.Errorf("marshaling cli-session request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/cli-sessions", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("building cli-session request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(apiKey, "")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("creating cli-session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("creating cli-session: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	// Bounded like the error path above: an HTTP 200 from a misbehaving or
	// compromised endpoint carrying an oversized body would otherwise force
	// unbounded allocation in Decode before it ever returns.
	var out createSessionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSessionResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding cli-session response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("cli-session response missing id")
	}
	return out.ID, nil
}
