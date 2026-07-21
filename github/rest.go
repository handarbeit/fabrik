package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrNotFound is returned by REST methods when the server responds with 404.
// Callers may use errors.Is(err, github.ErrNotFound) to distinguish "not found"
// from other failures without fragile string matching.
var ErrNotFound = errors.New("not found")

// updateRestStats parses rate limit headers from a response and stores them when present.
func (c *Client) updateRestStats(h http.Header) {
	if stats := parseRateLimitHeaders(h); stats.Limit > 0 {
		c.mu.Lock()
		c.restStats = stats
		c.mu.Unlock()
	}
}

// ErrUnprocessableEntity is returned by REST methods when the server responds
// with 422. Callers may use errors.Is(err, github.ErrUnprocessableEntity) to
// detect "already exists" or validation failures without fragile string matching.
var ErrUnprocessableEntity = errors.New("unprocessable entity")

// ErrMethodNotAllowed is returned by REST methods when the server responds
// with 405. Callers may use errors.Is(err, github.ErrMethodNotAllowed) to
// detect unsupported operations (e.g. rebase merge not allowed by repo policy).
var ErrMethodNotAllowed = errors.New("method not allowed")

// authErrorHint returns an actionable hint string for 401/403 HTTP errors and
// an empty string for all other status codes. The hint advises users to switch
// to a classic personal access token, which is required for GitHub Projects v2
// GraphQL operations that fine-grained tokens do not support.
func authErrorHint(statusCode int) string {
	if statusCode == 401 || statusCode == 403 {
		return " If you used a fine-grained access token (github_pat_...), switch to a classic personal access token with 'repo', 'project', and 'workflow' scopes. See: https://github.com/settings/tokens"
	}
	return ""
}

// do is the shared REST request core. It marshals body (when non-nil), sets
// auth/content-type/accept headers, executes the request, records rate-limit
// stats, and maps 404/405/422 responses to their sentinel errors uniformly
// across every REST verb. body may be nil for GET/DELETE-without-body calls;
// Content-Type is only set when body is non-nil, matching what each verb sent
// before this helper existed. The full response body is always read and
// returned so typed callers can decode it themselves.
func (c *Client) do(method, url string, body interface{}) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshaling request: %w", err)
		}
		reader = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	c.updateRestStats(resp.Header)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode >= 400 {
		switch resp.StatusCode {
		case 404:
			return resp, respBody, fmt.Errorf("GitHub API returned 404: %s: %w", string(respBody), ErrNotFound)
		case 405:
			return resp, respBody, fmt.Errorf("GitHub API returned 405: %s: %w", string(respBody), ErrMethodNotAllowed)
		case 422:
			return resp, respBody, fmt.Errorf("GitHub API returned 422: %s: %w", string(respBody), ErrUnprocessableEntity)
		}
		return resp, respBody, fmt.Errorf("GitHub API returned %d: %s%s", resp.StatusCode, string(respBody), authErrorHint(resp.StatusCode))
	}

	return resp, respBody, nil
}

func (c *Client) restRequest(method, url string, body interface{}) error {
	_, _, err := c.do(method, url, body)
	return err
}

func (c *Client) restGetJSON(url string, result interface{}) error {
	_, respBody, err := c.do("GET", url, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(respBody, result)
}

// SearchResult represents the response from GitHub's search API.
type SearchResult struct {
	Items []struct {
		Number int `json:"number"`
	} `json:"items"`
}

func (c *Client) restGet(url string) (*SearchResult, error) {
	_, respBody, err := c.do("GET", url, nil)
	if err != nil {
		return nil, err
	}
	var result SearchResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

func (c *Client) restPost(url string, body interface{}) error {
	return c.restRequest("POST", url, body)
}

// restPostWithResponse POSTs and decodes the response body into the provided target.
func (c *Client) restPostWithResponse(url string, body interface{}, target interface{}) error {
	_, respBody, err := c.do("POST", url, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(respBody, target); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

func (c *Client) restPatch(url string, body interface{}) error {
	return c.restRequest("PATCH", url, body)
}

// restPutWithResponse PUTs and decodes the response body into the provided target.
func (c *Client) restPutWithResponse(url string, body interface{}, target interface{}) error {
	_, respBody, err := c.do("PUT", url, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(respBody, target); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

func (c *Client) restDelete(url string) error {
	_, _, err := c.do("DELETE", url, nil)
	return err
}
