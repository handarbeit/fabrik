package github

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// contentsResponse is the shape of GitHub's Contents API response for a
// single file (GET /repos/{owner}/{repo}/contents/{path}). GitHub returns
// the same shape for a directory (as a JSON array) — Type lets
// FetchFileAtRef reject that case explicitly rather than failing an
// unmarshal in a confusing way.
type contentsResponse struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	Size     int    `json:"size"`
}

// FetchFileAtRef reads a single file's raw bytes from a repository at a
// specific ref (branch, tag, or SHA) via GitHub's Contents API. This is the
// generic, package-agnostic "fetch this path at this ref" primitive shared
// by any caller that must resolve repo-resident, untrusted configuration at
// a trusted ref rather than the PR head (see pruefer's base-ref resolution
// of .pruefer/config.yaml, and — per ADR-1642 — the intended reuse point for
// #1446's repo-resident review skill). It has no awareness of YAML, size
// caps, or UTF-8 validation; callers own that.
//
// Returns ErrNotFound (wrapped) when the file does not exist at ref — the
// common, non-error "no repo config" case. Returns an error if the path
// resolves to something other than a regular file (e.g. a directory) or if
// GitHub returns an encoding this method doesn't know how to decode (only
// "base64" is supported, which is what the Contents API always uses for
// file content).
func (c *Client) FetchFileAtRef(owner, repo, path, ref string) ([]byte, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		c.baseURL, url.PathEscape(owner), url.PathEscape(repo), escapeRepoPath(path), url.QueryEscape(ref))
	var resp contentsResponse
	if err := c.restGetJSON(apiURL, &resp); err != nil {
		return nil, fmt.Errorf("fetching %s at %s in %s/%s: %w", path, ref, owner, repo, err)
	}
	if resp.Type != "file" {
		return nil, fmt.Errorf("fetching %s at %s in %s/%s: expected a file, got type %q", path, ref, owner, repo, resp.Type)
	}
	if resp.Encoding != "base64" {
		return nil, fmt.Errorf("fetching %s at %s in %s/%s: unsupported content encoding %q", path, ref, owner, repo, resp.Encoding)
	}
	// GitHub's base64 content is newline-wrapped (76 chars/line, matching
	// the standard MIME convention) — strip that before decoding.
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decoding %s at %s in %s/%s: %w", path, ref, owner, repo, err)
	}
	return decoded, nil
}

// escapeRepoPath percent-encodes each "/"-separated segment of a
// repo-relative path independently, preserving the separators themselves.
// url.PathEscape alone would also encode "/", corrupting a multi-segment
// path like ".pruefer/config.yaml" into a single, non-existent path segment.
func escapeRepoPath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}
