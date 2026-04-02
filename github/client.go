package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// Client is a GitHub GraphQL API client.
type Client struct {
	token      string
	baseURL    string
	httpClient *http.Client

	mu           sync.Mutex
	restStats    RateLimitStats
	graphqlStats RateLimitStats
}

func NewClient(token string) *Client {
	return &Client{
		token:   token,
		baseURL: defaultBaseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewClientWithBaseURL creates a client with a custom base URL (for testing).
func NewClientWithBaseURL(token, baseURL string) *Client {
	return &Client{
		token:   token,
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// parseRateLimitHeaders extracts GitHub rate limit headers from an HTTP response.
// Returns a zero-value RateLimitStats if the headers are absent or malformed.
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

	req, err := http.NewRequest("POST", c.baseURL+"/graphql", bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
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
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
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
