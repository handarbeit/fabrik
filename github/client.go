package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// Logf is an optional package-level logger callback. When non-nil, internal
// retry/diagnostic warnings (e.g. project board indexer mismatches) are routed
// here. Engine wires this to its own logf during construction so messages land
// in fabrik.log; tests that don't set it get silent retries.
var Logf func(issueNumber int, tag, format string, args ...any)

// logf is the package-internal helper that fans out to Logf if set, otherwise
// silent. Always pass issueNumber=0 for non-issue-scoped messages.
func logf(issueNumber int, tag, format string, args ...any) {
	if Logf != nil {
		Logf(issueNumber, tag, format, args...)
	}
}

// Client is a GitHub GraphQL API client.
type Client struct {
	token      string
	baseURL    string
	graphqlURL string
	httpClient *http.Client

	mu           sync.Mutex
	restStats    RateLimitStats
	graphqlStats RateLimitStats

	mergeStrategy string
}

// SetMergeStrategy configures the merge method MergePR attempts first
// ("MERGE", "SQUASH", or "REBASE", case-insensitive). An empty string leaves
// MergePR defaulting to "merge". Safe to call concurrently with MergePR.
func (c *Client) SetMergeStrategy(strategy string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergeStrategy = strategy
}

// MergeStrategy returns the currently configured merge strategy, safe for
// concurrent use alongside SetMergeStrategy.
func (c *Client) MergeStrategy() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mergeStrategy
}

// SetToken replaces the client's bearer token. Safe to call concurrently
// with in-flight requests. Needed by Pruefer's GitHub App installation-token
// refresh loop, where the token expires roughly hourly and must be swapped
// in place without reconstructing the client (see ADR-1113).
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
}

// Token returns the client's current bearer token, safe for concurrent use
// alongside SetToken.
func (c *Client) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

func NewClient(token string) *Client {
	return &Client{
		token:      token,
		baseURL:    defaultBaseURL,
		graphqlURL: defaultBaseURL + "/graphql",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewClientWithBaseURL creates a client with a custom base URL. An empty
// baseURL falls back to defaultBaseURL (the production API host), matching
// NewClient and the appRequest convention — a caller passing "" means "the
// real GitHub API", not a hostless client. Without this, production callers
// (e.g. pruefer's mintAuth, which passes "" for the non-test case) built
// clients whose requests hit "/repos/..." with no scheme/host ("unsupported
// protocol scheme"). Tests always pass an httptest URL, so they never caught it.
//
// The GraphQL endpoint is always derived as baseURL+"/graphql" — this matches
// github.com, where REST and GraphQL share a host. It does NOT match GitHub
// Enterprise Server, where REST lives at <host>/api/v3 and GraphQL at
// <host>/api/graphql (not a path suffix of the REST base). GHES callers must
// use NewClientForHost instead.
func NewClientWithBaseURL(token, baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		token:      token,
		baseURL:    baseURL,
		graphqlURL: baseURL + "/graphql",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewClientForHost creates a client targeting a GitHub Enterprise Server
// instance identified by a bare hostname (e.g. "github.example.com"). Unlike
// NewClientWithBaseURL, REST and GraphQL endpoints are derived independently
// per GHES's actual layout: REST at https://<host>/api/v3, GraphQL at
// https://<host>/api/graphql. host is defensively normalized — any
// "http://"/"https://" scheme prefix and any trailing "/" are stripped before
// the URLs are built, so callers may pass either a bare host or a full URL.
func NewClientForHost(token, host string) *Client {
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimSuffix(host, "/")
	return &Client{
		token:      token,
		baseURL:    "https://" + host + "/api/v3",
		graphqlURL: "https://" + host + "/api/graphql",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// parseRateLimitHeaders extracts GitHub rate limit headers from an HTTP response.
// Returns a RateLimitStats with numeric fields at zero and Reset at the zero time if
// the headers are absent or malformed; UpdatedAt is always set to the current time.
func parseRateLimitHeaders(h http.Header) RateLimitStats {
	limit, _ := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	remaining, _ := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	used, _ := strconv.Atoi(h.Get("X-RateLimit-Used"))
	var reset time.Time
	if unix, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && unix > 0 {
		reset = time.Unix(unix, 0)
	}
	return RateLimitStats{
		Limit:     limit,
		Remaining: remaining,
		Used:      used,
		Reset:     reset,
		UpdatedAt: time.Now(),
	}
}

// RateLimitStats returns the most recently observed REST and GraphQL rate limit stats.
func (c *Client) RateLimitStats() (rest, graphql RateLimitStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restStats, c.graphqlStats
}

// graphqlRequest performs a GraphQL query and unmarshals the response.
func (c *Client) graphqlRequest(query string, variables map[string]interface{}, result interface{}) error {
	body := map[string]interface{}{
		"query":     query,
		"variables": variables,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", c.graphqlURL, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	// See doWithAccept in rest.go for why an empty token omits the header
	// rather than sending a blank bearer credential.
	if token := c.Token(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	// Capture rate limit headers before any error check — headers are valid even on errors.
	stats := parseRateLimitHeaders(resp.Header)
	if stats.Limit > 0 {
		c.mu.Lock()
		c.graphqlStats = stats
		c.mu.Unlock()
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub API returned %d: %s%s", resp.StatusCode, string(respBody), authErrorHint(resp.StatusCode))
	}

	// Check for GraphQL errors
	var gqlResp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &gqlResp); err == nil && len(gqlResp.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %s", gqlResp.Errors[0].Message)
	}

	return json.Unmarshal(respBody, result)
}

// FetchLatestRelease calls GET /repos/{owner}/{repo}/releases/latest and returns
// the tag name and asset list. Returns an error if the request fails or returns
// a non-2xx status.
func (c *Client) FetchLatestRelease(owner, repo string) (*LatestRelease, error) {
	url := c.baseURL + "/repos/" + owner + "/" + repo + "/releases/latest"
	var release LatestRelease
	if err := c.restGetJSON(url, &release); err != nil {
		return nil, fmt.Errorf("fetching latest release for %s/%s: %w", owner, repo, err)
	}
	return &release, nil
}

// FetchRepoAccess calls GET /repos/{owner}/{repo} and returns the allow_auto_merge
// setting alongside the authenticated token's push access (permissions.push) —
// both decoded from the same response, so this adds no extra API round-trip
// beyond what the allow_auto_merge check already required. Returns an error if
// the request fails.
func (c *Client) FetchRepoAccess(owner, repo string) (RepoAccess, error) {
	url := c.baseURL + "/repos/" + owner + "/" + repo
	var result struct {
		AllowAutoMerge bool `json:"allow_auto_merge"`
		Permissions    struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := c.restGetJSON(url, &result); err != nil {
		return RepoAccess{}, fmt.Errorf("fetching repo access for %s/%s: %w", owner, repo, err)
	}
	return RepoAccess{AllowAutoMerge: result.AllowAutoMerge, CanPush: result.Permissions.Push}, nil
}

// FetchInstalledVersion calls GET /meta and returns the installed_version
// field. On github.com this endpoint has no installed_version (github.com
// isn't versioned the way GHES is); on GHES it identifies the running
// release (e.g. "3.19.8"). Used by the engine's GHES version-floor preflight
// (FR-7) — never called on the github.com path. Requires no authentication,
// but the client's existing Authorization header is harmless to send.
func (c *Client) FetchInstalledVersion() (string, error) {
	url := c.baseURL + "/meta"
	var result struct {
		InstalledVersion string `json:"installed_version"`
	}
	if err := c.restGetJSON(url, &result); err != nil {
		return "", fmt.Errorf("fetching /meta: %w", err)
	}
	return result.InstalledVersion, nil
}
