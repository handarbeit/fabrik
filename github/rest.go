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

// ErrForbidden is returned by REST methods when the server responds with a
// 403 that is not a rate limit (#2052). Callers use errors.Is(err,
// github.ErrForbidden) — typically alongside ErrNotFound — to treat "this
// credential lacks the permission" as a soft, degradable condition (e.g. the
// optional Actions permission) rather than a hard failure.
var ErrForbidden = errors.New("forbidden")

// forbiddenError carries the unchanged generic 403 message while matching
// ErrForbidden under errors.Is, so existing message-based callers and tests
// see exactly the text they always did.
type forbiddenError struct{ msg string }

func (e *forbiddenError) Error() string        { return e.msg }
func (e *forbiddenError) Is(target error) bool { return target == ErrForbidden }

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

// ErrConflict is returned by REST methods when the server responds with 409.
// Introduced for MergePRAtHeadSHA (#1644): GitHub's merge endpoint returns
// 409 when the caller's expected head SHA no longer matches the PR's live
// head — the caller asked to merge exactly what it validated, and the world
// moved underneath it. Callers may use errors.Is(err, github.ErrConflict) to
// detect this and retry from a fresh read rather than treating it as a
// generic failure.
var ErrConflict = errors.New("conflict")

// ErrDiffTooLarge is returned by REST methods when the server responds with
// 406 Not Acceptable and the body's errors[].code is "too_large" — GitHub's
// deterministic refusal to render a diff exceeding its 20,000-line ceiling
// on the .diff media type. This is a size verdict, not a transient failure:
// it will reproduce identically on every retry until the PR's head changes.
// Callers may use errors.Is(err, github.ErrDiffTooLarge) to distinguish this
// from a generic request failure. A 406 with a different or unparseable
// errors[].code is NOT classified as this sentinel and keeps the generic
// error path — this is a narrow classification of one specific GitHub error
// shape, not a broad "406 means fine".
var ErrDiffTooLarge = errors.New("diff too large to render")

// tooLargeErrorBody is the shape of GitHub's 406 too_large response body:
// {"message":"...","errors":[{"resource":"PullRequest","field":"diff","code":"too_large"}],"status":"406"}
type tooLargeErrorBody struct {
	Errors []struct {
		Code string `json:"code"`
	} `json:"errors"`
}

// isDiffTooLarge reports whether a 406 response body matches GitHub's
// too_large diff error shape (at least one errors[].code == "too_large").
// An unparseable or differently-shaped body returns false.
func isDiffTooLarge(body []byte) bool {
	var parsed tooLargeErrorBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	for _, e := range parsed.Errors {
		if e.Code == "too_large" {
			return true
		}
	}
	return false
}

// authErrorHint returns an actionable hint string for 401/403 HTTP errors and
// an empty string for all other status codes. It is keyed on status alone, so
// callers must classify rate limits first (classifyRateLimit): GitHub also
// returns 403 for rate limiting, where this hint would be wrong. The App
// client never uses it. The hint advises users to switch
// to a classic personal access token, which is required for GitHub Projects v2
// GraphQL operations that fine-grained tokens do not support.
func authErrorHint(statusCode int) string {
	if statusCode == 401 || statusCode == 403 {
		return " If you used a fine-grained access token (github_pat_...), switch to a classic personal access token with 'repo', 'project', and 'workflow' scopes. See: https://github.com/settings/tokens"
	}
	return ""
}

// apiStatusError builds the generic "GitHub API returned N" error for a
// non-2xx response on the PAT client. A rate-limited response becomes a
// *RateLimitError with no hint; otherwise the fine-grained-PAT hint is appended
// for a genuine 401/403 only (R4).
func apiStatusError(status int, header http.Header, body []byte) error {
	if rl := classifyRateLimit("GitHub API", status, header, body); rl != nil {
		return rl
	}
	msg := fmt.Sprintf("GitHub API returned %d: %s%s", status, string(body), authErrorHint(status))
	if status == http.StatusForbidden {
		return &forbiddenError{msg: msg}
	}
	return errors.New(msg)
}

// do is the shared REST request core, using GitHub's standard JSON media
// type. See doWithAccept for the full behavior description.
func (c *Client) do(method, url string, body interface{}) (*http.Response, []byte, error) {
	return c.doWithAccept(method, url, "application/vnd.github+json", body)
}

// doWithAccept is the shared REST request core. It marshals body (when
// non-nil), sets auth/content-type/accept headers, executes the request,
// records rate-limit stats, and maps 404/405/422 responses to their sentinel
// errors uniformly across every REST verb. body may be nil for
// GET/DELETE-without-body calls; Content-Type is only set when body is
// non-nil, matching what each verb sent before this helper existed. The full
// response body is always read and returned so typed callers can decode it
// themselves. accept lets callers request a non-default media type (e.g.
// GitHub's diff format) while sharing the rest of the request/error-handling
// pipeline.
func (c *Client) doWithAccept(method, url, accept string, body interface{}) (*http.Response, []byte, error) {
	return c.doWithHeaders(method, url, accept, nil, body)
}

// doWithHeaders is doWithAccept plus caller-supplied extra request headers
// (e.g. If-None-Match, #1952). Every request it executes is counted in the
// client's RequestStats, and a 304 is additionally counted as NotModified. A
// 304 is not an error here (status < 400) and carries an empty body — callers
// that send a conditional header own its handling; doWithAccept never sends
// one, so existing callers never see a 304.
func (c *Client) doWithHeaders(method, url, accept string, extra http.Header, body interface{}) (*http.Response, []byte, error) {
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
	// An empty token means "deliberately unauthenticated" (e.g. pruefer's
	// release-check client against the public fabrik repo) — omit the header
	// rather than sending "Bearer " with nothing after it. GitHub's REST API
	// treats a blank bearer token as invalid credentials (401 Bad
	// credentials), not as equivalent to no Authorization header at all
	// (verified against the real API), so setting it unconditionally would
	// break every unauthenticated caller.
	if token := c.Token(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", accept)
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	c.reqTotal.Add(1)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		c.reqNotModified.Add(1)
	}
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
		case 409:
			return resp, respBody, fmt.Errorf("GitHub API returned 409: %s: %w", string(respBody), ErrConflict)
		case 406:
			if isDiffTooLarge(respBody) {
				return resp, respBody, fmt.Errorf("GitHub API returned 406: %s: %w", string(respBody), ErrDiffTooLarge)
			}
		case 422:
			return resp, respBody, fmt.Errorf("GitHub API returned 422: %s: %w", string(respBody), ErrUnprocessableEntity)
		}
		return resp, respBody, apiStatusError(resp.StatusCode, resp.Header, respBody)
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

// restPageSize is the page size every paginated REST fetcher requests. GitHub
// caps per_page at 100 for the endpoints used here; asking for more is silently
// clamped, so this is the largest page we can actually get.
const restPageSize = 100

// restMaxPages bounds paginateREST so a server that never returns a short page
// cannot spin forever. 100 pages × 100 items = 10,000 records, far beyond any
// realistic PR review list, issue thread, or open-PR set. Reaching it is
// treated as an error rather than a truncated result — see paginateREST.
const restMaxPages = 100

// paginateREST accumulates every page of a GitHub REST collection endpoint that
// returns a bare JSON array. urlFor builds the request URL for a 1-based page
// number and must include per_page=restPageSize.
//
// Why this exists (#1539): every caller here previously issued a single request
// and treated page one as the whole collection. Past the boundary that returns a
// partial slice with no error and no signal — indistinguishable from a complete
// answer. On handarbeit/fabrik#1256 that let Pruefer re-review an unchanged head
// 315 times, because FetchPRReviews could not see the reviews it had just
// submitted; the same truncation reaches engine/reviews.go's authoritative
// landing gate, where a stale verdict changes a merge decision.
//
// Termination: a page shorter than restPageSize is the last one, so a collection
// of fewer than restPageSize records costs exactly one request. A collection of
// exactly restPageSize (or any exact multiple) is indistinguishable from a full
// page with more behind it, and costs one additional request that comes back
// empty — the price of not probing speculatively in the common case.
//
// Hitting restMaxPages returns an error rather than the accumulated prefix —
// silently degrading into a short list is precisely the defect being fixed, so
// the bound fails loud. Known limitation: a genuine collection of exactly
// restMaxPages*restPageSize records is indistinguishable from a server that
// never returns a short page, and is reported as the latter. At 10,000 records
// that is far outside any real review list, comment thread, or open-PR set, and
// erring toward "refuse" beats erring toward a silent truncation.
//
// Each page is fetched through condGetJSON (#1952): every page is still
// requested on every call, each with its own URL-keyed ETag, so a 304 on page 1
// cannot hide a change on page 2. Cached chunks are copied into the fresh
// accumulator, never mutated or returned directly.
func paginateREST[T any](c *Client, what string, urlFor func(page int) string) ([]T, error) {
	var all []T
	for page := 1; page <= restMaxPages; page++ {
		var chunk []T
		if err := condGetJSON(c, urlFor(page), &chunk); err != nil {
			return nil, err
		}
		all = append(all, chunk...)
		if len(chunk) < restPageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("fetching %s: exceeded %d pages (%d records) without reaching the end — refusing to return a truncated result",
		what, restMaxPages, restMaxPages*restPageSize)
}
