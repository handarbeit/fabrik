package github

import (
	"fmt"
	"net/url"
)

type refResponse struct {
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// ResolveRefSHA resolves a branch name to the commit SHA it points at right
// now, via the lightweight Git refs endpoint. Pruefer pins every base-ref read
// of one review to this SHA, so the config and the guidance can never come
// from different base commits (#2307).
func (c *Client) ResolveRefSHA(owner, repo, branch string) (string, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/%s",
		c.baseURL, url.PathEscape(owner), url.PathEscape(repo), escapeRepoPath(branch))
	var raw refResponse
	// Conditional: an unchanged base answers 304, which GitHub does not bill
	// against the rate limit (#1952), and a moved base returns the new SHA.
	if err := condGetJSON(c, apiURL, &raw); err != nil {
		return "", fmt.Errorf("resolving branch %s in %s/%s: %w", branch, owner, repo, err)
	}
	if raw.Object.Type != "commit" || raw.Object.SHA == "" {
		return "", fmt.Errorf("resolving branch %s in %s/%s: ref points at %q %q, not a commit", branch, owner, repo, raw.Object.Type, raw.Object.SHA)
	}
	return raw.Object.SHA, nil
}
