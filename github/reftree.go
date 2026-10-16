package github

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// TreeEntry is one entry of a git tree as the Git Trees API reports it. Mode is
// the git file mode ("100644" file, "100755" executable, "120000" symlink,
// "160000" gitlink/submodule, "040000" tree); Type is "blob", "tree" or "commit".
// Path is relative to the directory FetchTreeAtRef was asked for.
type TreeEntry struct {
	Path string
	Mode string
	Type string
	SHA  string
	Size int64
}

type refResponse struct {
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// ResolveRefSHA resolves a branch name to the commit SHA it points at right
// now, via the lightweight Git refs endpoint. Pruefer pins every base-ref read
// of one review to this SHA, so the config, the guidance and the plugin tree
// can never come from different base commits (#2307).
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

type treeResponse struct {
	SHA       string `json:"sha"`
	Truncated bool   `json:"truncated"`
	Tree      []struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
		Size int64  `json:"size"`
	} `json:"tree"`
}

func (c *Client) fetchTree(owner, repo, sha string, recursive bool) (treeResponse, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s", c.baseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	if recursive {
		apiURL += "?recursive=1"
	}
	var t treeResponse
	if err := c.restGetJSON(apiURL, &t); err != nil {
		return treeResponse{}, err
	}
	return t, nil
}

// FetchTreeAtRef lists every entry under dir (a slash-separated repo path) of
// the tree that commitSHA names — never the working tree and never a branch
// name, so the caller decides which commit it reads. dir is descended one
// non-recursive tree read per segment, then the final subtree is read once
// recursively; entry paths are relative to dir. truncated reports that GitHub
// capped the recursive listing, in which case the entries are incomplete and a
// caller must not trust them.
//
// A commit with no dir returns an error wrapping ErrNotFound, the ordinary
// "nothing there" case.
func (c *Client) FetchTreeAtRef(owner, repo, commitSHA, dir string) (entries []TreeEntry, truncated bool, err error) {
	wrap := func(e error) error {
		return fmt.Errorf("reading tree %s at %s in %s/%s: %w", dir, commitSHA, owner, repo, e)
	}
	commitURL := fmt.Sprintf("%s/repos/%s/%s/git/commits/%s", c.baseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(commitSHA))
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := c.restGetJSON(commitURL, &commit); err != nil {
		return nil, false, wrap(err)
	}
	cur := commit.Tree.SHA
	if cur == "" {
		return nil, false, wrap(errors.New("commit has no tree"))
	}
	for _, seg := range strings.Split(strings.Trim(dir, "/"), "/") {
		if seg == "" {
			continue
		}
		t, err := c.fetchTree(owner, repo, cur, false)
		if err != nil {
			return nil, false, wrap(err)
		}
		next := ""
		for _, e := range t.Tree {
			if e.Path == seg && e.Type == "tree" {
				next = e.SHA
				break
			}
		}
		if next == "" {
			return nil, false, wrap(fmt.Errorf("no directory %q: %w", seg, ErrNotFound))
		}
		cur = next
	}
	t, err := c.fetchTree(owner, repo, cur, true)
	if err != nil {
		return nil, false, wrap(err)
	}
	for _, e := range t.Tree {
		entries = append(entries, TreeEntry{Path: e.Path, Mode: e.Mode, Type: e.Type, SHA: e.SHA, Size: e.Size})
	}
	return entries, t.Truncated, nil
}

// FetchBlob reads one blob by SHA. A blob larger than maxBytes (when positive)
// is refused before its content is decoded.
func (c *Client) FetchBlob(owner, repo, sha string, maxBytes int64) ([]byte, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/%s/git/blobs/%s", c.baseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	var raw struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
	}
	if err := c.restGetJSON(apiURL, &raw); err != nil {
		return nil, fmt.Errorf("fetching blob %s in %s/%s: %w", sha, owner, repo, err)
	}
	if maxBytes > 0 && raw.Size > maxBytes {
		return nil, fmt.Errorf("blob %s is %d bytes, exceeds the %d byte cap", sha, raw.Size, maxBytes)
	}
	if raw.Encoding != "base64" {
		return nil, fmt.Errorf("blob %s: unsupported encoding %q", sha, raw.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(raw.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decoding blob %s: %w", sha, err)
	}
	return decoded, nil
}
