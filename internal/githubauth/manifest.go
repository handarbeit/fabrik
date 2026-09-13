package githubauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// defaultGitHubBaseURL is githubauth's own copy of github/client.go's
// unexported defaultBaseURL — this package intentionally does not import
// github's client internals for an unauthenticated, code-keyed endpoint
// that has nothing in common with Client's authenticated request core.
const defaultGitHubBaseURL = "https://api.github.com"

// maxManifestExchangeResponseBytes caps how much of the manifest-exchange
// HTTP response exchangeManifestCode will read. A real GitHub App's
// credentials (PEM, secrets, slug) fit comfortably in a few KB; this is
// generous headroom against an operator-configured BaseURL pointed at a
// misbehaving or untrusted endpoint, not a tight production-shaped limit.
const maxManifestExchangeResponseBytes = 1 << 20 // 1 MiB

// defaultAppName is the name prefilled on GitHub's manifest-confirmation
// page. GitHub lets the user rename it there before creating the App, so
// this is a sensible default rather than a configurable option — see the
// Plan's "no manifest-name config override" decision.
const defaultAppName = "pruefer"

// defaultAppHomepageURL is the manifest's "url" field — the App's public
// homepage link, shown on its GitHub App settings page. This is distinct
// from "redirect_url" (the loopback callback), which stops existing the
// moment the local manifest-flow server shuts down; reusing it here would
// leave the created App's homepage permanently dead. No config knob for
// this, matching the "no manifest-name config override" precedent above —
// GitHub's own create page lets the user edit it before creating the App.
const defaultAppHomepageURL = "https://github.com/handarbeit/fabrik"

// manifestHTTPClient is package-level so tests can leave it as the default
// (they never hit the real GitHub API — only exchangeManifestCode's baseURL
// parameter is overridden, pointed at an httptest server).
var manifestHTTPClient = &http.Client{Timeout: 30 * time.Second}

// requiredPermissions is the single source of truth for exactly the
// permissions Pruefer's code paths use (matching cmd/pruefer/README.md's
// manual-setup permission list): Metadata: read, Pull requests: write,
// Contents: read, Issues: write. Both buildManifest (what a freshly
// manifest-created App requests) and PrueferRequiredPermissions (what
// internal/githubauth's grant-verification check, #1709, requires an
// installation to have actually granted) read from this one map, so the
// two can no longer independently drift the way they had before #1709 —
// the exact defect (issues: write requested by neither the manifest nor
// any check, while the hand-created live App silently held issues: read)
// this issue exists to catch earlier next time.
//
// Issues is "write", not "read": pruefer/comment.go's
// AcknowledgeForceReview/MarkForceReviewsProcessed POST to
// /repos/{owner}/{repo}/issues/comments/{id}/reactions (github/comments.go's
// AddCommentReaction) to leave the eyes/rocket acknowledgment on a
// "/pruefer review" comment — GitHub's Issue Comments API requires "issues:
// write" to create a reaction, not "read"; "read" only covers listing
// comments.
var requiredPermissions = map[string]string{
	"metadata":      "read",
	"pull_requests": "write",
	"contents":      "read",
	"issues":        "write",
}

// PrueferRequiredPermissions returns the permission set Pruefer's code paths
// require an installation to have actually granted — the same set
// buildManifest requests for a freshly created App (see requiredPermissions'
// doc comment for why they're one map, not two). Intended as the
// Options.RequiredPermissions value Pruefer's own execute.go passes into
// Reconcile for R2's grant-verification check. Returns a fresh copy so a
// caller mutating the result can never corrupt this package's own copy.
func PrueferRequiredPermissions() map[string]string {
	out := make(map[string]string, len(requiredPermissions))
	for k, v := range requiredPermissions {
		out[k] = v
	}
	return out
}

// buildManifest returns the JSON manifest GitHub's App-creation-from-manifest
// flow expects, scoped to requiredPermissions (see its doc comment).
// hook_attributes.active is always false and no default_events are
// requested — Pruefer V1 is polling-only (ADR-1113 §1, ADR-032); enabling
// webhook delivery is a separate, out-of-scope future issue this manifest
// must never enable. redirectURL is the loopback callback server's own URL,
// assigned only after it starts listening (see runManifestCallbackServer).
func buildManifest(redirectURL string) map[string]interface{} {
	return map[string]interface{}{
		"name":                defaultAppName,
		"url":                 defaultAppHomepageURL,
		"redirect_url":        redirectURL,
		"public":              false,
		"default_permissions": PrueferRequiredPermissions(),
		"default_events":      []string{},
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
// hour) and can only be exchanged once. ctx is honored via
// http.NewRequestWithContext so a caller-driven cancellation (e.g. shutdown
// mid-exchange) aborts the request promptly instead of riding out
// manifestHTTPClient's full 30s timeout.
func exchangeManifestCode(ctx context.Context, baseURL, code string) (ManifestCredentials, error) {
	if baseURL == "" {
		baseURL = defaultGitHubBaseURL
	}
	// code comes from an untrusted /callback query parameter (its state
	// check only proves it arrived from a plausible completion attempt, not
	// that it's well-formed) — path-escape it so a value containing "/",
	// "?", or "#" can't redirect this request to a different path/query.
	reqURL := fmt.Sprintf("%s/app-manifests/%s/conversions", baseURL, url.PathEscape(code))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return ManifestCredentials{}, fmt.Errorf("creating manifest exchange request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := manifestHTTPClient.Do(req)
	if err != nil {
		return ManifestCredentials{}, fmt.Errorf("exchanging manifest code: %w", err)
	}
	defer resp.Body.Close()

	// baseURL is normally api.github.com, but is operator-configurable
	// (tests point it at an httptest server) — cap the read so a
	// misbehaving or untrusted endpoint can't exhaust memory via an
	// unbounded response body.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestExchangeResponseBytes))
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
	if raw.ID == 0 || raw.PEM == "" || raw.Slug == "" {
		// An empty Slug wouldn't fail loudly downstream — it would silently
		// produce a bot identity of just "[bot]" and a broken
		// "https://github.com/apps//installations/new" guided-install URL.
		// Reject at exchange time instead, alongside the existing id/pem
		// checks.
		return ManifestCredentials{}, fmt.Errorf("manifest exchange response missing id, pem, or slug")
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
