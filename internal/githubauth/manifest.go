package githubauth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultGitHubBaseURL is githubauth's own copy of github/client.go's
// unexported defaultBaseURL — this package intentionally does not import
// github's client internals for an unauthenticated, code-keyed endpoint
// that has nothing in common with Client's authenticated request core.
const defaultGitHubBaseURL = "https://api.github.com"

// defaultAppName is the name prefilled on GitHub's manifest-confirmation
// page. GitHub lets the user rename it there before creating the App, so
// this is a sensible default rather than a configurable option — see the
// Plan's "no manifest-name config override" decision.
const defaultAppName = "pruefer"

// manifestHTTPClient is package-level so tests can leave it as the default
// (they never hit the real GitHub API — only exchangeManifestCode's baseURL
// parameter is overridden, pointed at an httptest server).
var manifestHTTPClient = &http.Client{Timeout: 30 * time.Second}

// buildManifest returns the JSON manifest GitHub's App-creation-from-manifest
// flow expects, scoped to exactly the permissions Pruefer's code paths use
// (matching cmd/pruefer/README.md's manual-setup permission list):
// Metadata: read, Pull requests: write, Contents: read, Issues: read.
// hook_attributes.active is always false and no default_events are
// requested — Pruefer V1 is polling-only (ADR-1113 §1, ADR-032); enabling
// webhook delivery is a separate, out-of-scope future issue this manifest
// must never enable. redirectURL is the loopback callback server's own URL,
// assigned only after it starts listening (see runManifestCallbackServer).
func buildManifest(redirectURL string) map[string]interface{} {
	return map[string]interface{}{
		"name":         defaultAppName,
		"url":          redirectURL,
		"redirect_url": redirectURL,
		"public":       false,
		"default_permissions": map[string]string{
			"metadata":      "read",
			"pull_requests": "write",
			"contents":      "read",
			"issues":        "read",
		},
		"default_events": []string{},
		"hook_attributes": map[string]interface{}{
			"active": false,
		},
	}
}

// ManifestCredentials holds everything GitHub returns from a successful
// app-manifest code exchange (POST /app-manifests/{code}/conversions).
type ManifestCredentials struct {
	AppID         int64
	Slug          string
	PEM           string
	WebhookSecret string
	ClientID      string
	ClientSecret  string
}

// exchangeManifestCode calls the unauthenticated, single-use, code-keyed
// POST /app-manifests/{code}/conversions to retrieve the newly created
// App's credentials. baseURL selects GitHub's API host ("" = production, an
// httptest URL in tests). The code expires after GitHub's fixed window (1
// hour) and can only be exchanged once.
func exchangeManifestCode(baseURL, code string) (ManifestCredentials, error) {
	if baseURL == "" {
		baseURL = defaultGitHubBaseURL
	}
	url := fmt.Sprintf("%s/app-manifests/%s/conversions", baseURL, code)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return ManifestCredentials{}, fmt.Errorf("creating manifest exchange request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := manifestHTTPClient.Do(req)
	if err != nil {
		return ManifestCredentials{}, fmt.Errorf("exchanging manifest code: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ManifestCredentials{}, fmt.Errorf("reading manifest exchange response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return ManifestCredentials{}, fmt.Errorf("GitHub manifest exchange returned %d: %s", resp.StatusCode, string(body))
	}

	var raw struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ManifestCredentials{}, fmt.Errorf("decoding manifest exchange response: %w", err)
	}
	if raw.ID == 0 || raw.PEM == "" {
		return ManifestCredentials{}, fmt.Errorf("manifest exchange response missing id or pem")
	}

	return ManifestCredentials{
		AppID:         raw.ID,
		Slug:          raw.Slug,
		PEM:           raw.PEM,
		WebhookSecret: raw.WebhookSecret,
		ClientID:      raw.ClientID,
		ClientSecret:  raw.ClientSecret,
	}, nil
}
