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

// ErrUnprocessableEntity is returned by REST methods when the server responds
// with 422. Callers may use errors.Is(err, github.ErrUnprocessableEntity) to
// detect "already exists" or validation failures without fragile string matching.
var ErrUnprocessableEntity = errors.New("unprocessable entity")

func (c *Client) restRequest(method, url string, body interface{}) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if stats := parseRateLimitHeaders(resp.Header); stats.Limit > 0 {
		c.mu.Lock()
		c.restStats = stats
		c.mu.Unlock()
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 422 {
			return fmt.Errorf("GitHub API returned 422: %s: %w", string(respBody), ErrUnprocessableEntity)
		}
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (c *Client) restGetJSON(url string, result interface{}) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if stats := parseRateLimitHeaders(resp.Header); stats.Limit > 0 {
		c.mu.Lock()
		c.restStats = stats
		c.mu.Unlock()
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

// SearchResult represents the response from GitHub's search API.
type SearchResult struct {
	Items []struct {
		Number int `json:"number"`
	} `json:"items"`
}

func (c *Client) restGet(url string) (*SearchResult, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if stats := parseRateLimitHeaders(resp.Header); stats.Limit > 0 {
		c.mu.Lock()
		c.restStats = stats
		c.mu.Unlock()
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

func (c *Client) restPost(url string, body interface{}) error {
	return c.restRequest("POST", url, body)
}

// restPostWithResponse POSTs and decodes the response body into the provided target.
func (c *Client) restPostWithResponse(url string, body interface{}, target interface{}) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if stats := parseRateLimitHeaders(resp.Header); stats.Limit > 0 {
		c.mu.Lock()
		c.restStats = stats
		c.mu.Unlock()
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

func (c *Client) restPatch(url string, body interface{}) error {
	return c.restRequest("PATCH", url, body)
}

func (c *Client) restDelete(url string) error {
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if stats := parseRateLimitHeaders(resp.Header); stats.Limit > 0 {
		c.mu.Lock()
		c.restStats = stats
		c.mu.Unlock()
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 404 {
			return fmt.Errorf("GitHub API returned 404: %s: %w", string(respBody), ErrNotFound)
		}
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
