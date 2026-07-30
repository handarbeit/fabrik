package hookdeck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DefaultAPIBaseURL is Hookdeck's production REST API base URL, versioned
// per the verified upstream protocol (see protocol.go).
const DefaultAPIBaseURL = "https://api.hookdeck.com/2025-07-01"

// DefaultWSBaseURL is Hookdeck's production WebSocket endpoint for CLI
// sessions.
const DefaultWSBaseURL = "wss://ws.hookdeck.com"

// createSession creates a new Hookdeck CLI session scoped to apiKey via
// HTTP Basic auth (username=apiKey, empty password), returning the session
// ID used as the Websocket-Id dial header.
func createSession(httpClient *http.Client, baseURL, apiKey string) (string, error) {
	reqBody, err := json.Marshal(createSessionRequest{WebhookIDs: []string{}})
	if err != nil {
		return "", fmt.Errorf("marshaling cli-session request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/cli-sessions", bytes.NewReader(reqBody))
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

	var out createSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding cli-session response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("cli-session response missing id")
	}
	return out.ID, nil
}
