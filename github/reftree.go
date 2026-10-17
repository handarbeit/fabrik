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
// Path is relative to the tree FetchTreeBySHA was asked for.
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

// RefKind says what a bare ref name names in a repository.
type RefKind string

const (
	// RefBranch: only refs/heads/<name> exists.
	RefBranch RefKind = "branch"
	// RefTag: only refs/tags/<name> exists, and it was peeled to a commit.
	RefTag RefKind = "tag"
	// RefAmbiguous: both a branch and a tag carry the name.
	RefAmbiguous RefKind = "ambiguous"
)

// ResolvedRef is the result of ResolveRefKind. SHA is the commit a tag peels
// to (empty for a branch or an ambiguous name, which a caller refuses anyway).
type ResolvedRef struct {
	Kind RefKind
	SHA  string
}

// maxTagPeel bounds the chain of tag objects followed to a commit.
const maxTagPeel = 5

// ResolveRefKind classifies a bare ref name as a branch, a tag or both, and
// peels a tag (lightweight or annotated) to the commit it names. A name that is
// neither returns an error wrapping ErrNotFound. Used by Pruefer to pin an
// external plugin source to a fixed commit (#2366): a tag is resolved and its
// SHA recorded; a branch or an ambiguous name is refused by the caller.
func (c *Client) ResolveRefKind(owner, repo, ref string) (ResolvedRef, error) {
	base := fmt.Sprintf("%s/repos/%s/%s", c.baseURL, url.PathEscape(owner), url.PathEscape(repo))
	wrap := func(e error) error {
		return fmt.Errorf("resolving ref %s in %s/%s: %w", ref, owner, repo, e)
	}
	var head, tag refResponse
	hasHead, hasTag := true, true
	if err := c.restGetJSON(base+"/git/ref/heads/"+escapeRepoPath(ref), &head); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return ResolvedRef{}, wrap(err)
		}
		hasHead = false
	}
	if err := c.restGetJSON(base+"/git/ref/tags/"+escapeRepoPath(ref), &tag); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return ResolvedRef{}, wrap(err)
		}
		hasTag = false
	}
	switch {
	case hasHead && hasTag:
		return ResolvedRef{Kind: RefAmbiguous}, nil
	case hasHead:
		return ResolvedRef{Kind: RefBranch}, nil
	case !hasTag:
		return ResolvedRef{}, wrap(ErrNotFound)
	}
	obj := tag.Object
	for i := 0; obj.Type == "tag"; i++ {
		if i >= maxTagPeel {
			return ResolvedRef{}, wrap(errors.New("tag chain is too deep"))
		}
		var t struct {
			Object struct {
				SHA  string `json:"sha"`
				Type string `json:"type"`
			} `json:"object"`
		}
		if err := c.restGetJSON(base+"/git/tags/"+url.PathEscape(obj.SHA), &t); err != nil {
			return ResolvedRef{}, wrap(err)
		}
		obj = t.Object
	}
	if obj.Type != "commit" || obj.SHA == "" {
		return ResolvedRef{}, wrap(fmt.Errorf("tag points at %q %q, not a commit", obj.Type, obj.SHA))
	}
	return ResolvedRef{Kind: RefTag, SHA: obj.SHA}, nil
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

// FetchSubtreeSHA returns the SHA of the tree at dir (a slash-separated repo
// path, "" or "." for the root) of the commit commitSHA — never a branch name,
// so the caller decides which commit it reads. dir is descended one
// non-recursive tree read per segment. A tree SHA is content-addressed, so the
// caller can key a cache on it.
//
// A commit with no dir returns an error wrapping ErrNotFound, the ordinary
// "nothing there" case. That is the only case that does: a commit or tree that
// cannot be read (a 404 for an unreachable SHA or a missing permission) is a
// read failure that does not wrap ErrNotFound, so a caller never mistakes an
// unreadable base for a repo without the directory.
func (c *Client) FetchSubtreeSHA(owner, repo, commitSHA, dir string) (string, error) {
	wrap := func(e error) error {
		return fmt.Errorf("reading tree %s at %s in %s/%s: %w", dir, commitSHA, owner, repo, e)
	}
	// unreadable wraps a failed commit or tree read. A 404 there means the object
	// could not be read, not that dir is absent, so ErrNotFound is not propagated.
	unreadable := func(e error) error {
		if errors.Is(e, ErrNotFound) {
			return wrap(fmt.Errorf("object not readable: %s", e.Error()))
		}
		return wrap(e)
	}
	commitURL := fmt.Sprintf("%s/repos/%s/%s/git/commits/%s", c.baseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(commitSHA))
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := c.restGetJSON(commitURL, &commit); err != nil {
		return "", unreadable(err)
	}
	cur := commit.Tree.SHA
	if cur == "" {
		return "", wrap(errors.New("commit has no tree"))
	}
	for _, seg := range strings.Split(strings.Trim(dir, "/"), "/") {
		if seg == "" || seg == "." {
			continue
		}
		t, err := c.fetchTree(owner, repo, cur, false)
		if err != nil {
			return "", unreadable(err)
		}
		next := ""
		for _, e := range t.Tree {
			if e.Path == seg && e.Type == "tree" {
				next = e.SHA
				break
			}
		}
		if next == "" {
			return "", wrap(fmt.Errorf("no directory %q: %w", seg, ErrNotFound))
		}
		cur = next
	}
	return cur, nil
}

// FetchTreeBySHA lists every entry under the tree treeSHA recursively; entry
// paths are relative to that tree. truncated reports that GitHub capped the
// listing, in which case the entries are incomplete and a caller must not trust
// them.
func (c *Client) FetchTreeBySHA(owner, repo, treeSHA string) (entries []TreeEntry, truncated bool, err error) {
	t, err := c.fetchTree(owner, repo, treeSHA, true)
	if err != nil {
		return nil, false, fmt.Errorf("reading tree %s in %s/%s: %w", treeSHA, owner, repo, err)
	}
	for _, e := range t.Tree {
		entries = append(entries, TreeEntry{Path: e.Path, Mode: e.Mode, Type: e.Type, SHA: e.SHA, Size: e.Size})
	}
	return entries, t.Truncated, nil
}

// FetchBlob reads one blob by SHA. A blob larger than maxBytes (when positive)
// is refused before its content is decoded, and the decoded length must match
// the size the response reported.
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
	if int64(len(decoded)) != raw.Size {
		return nil, fmt.Errorf("blob %s: decoded %d bytes but the response reported %d", sha, len(decoded), raw.Size)
	}
	if maxBytes > 0 && int64(len(decoded)) > maxBytes {
		return nil, fmt.Errorf("blob %s is %d bytes, exceeds the %d byte cap", sha, len(decoded), maxBytes)
	}
	return decoded, nil
}
