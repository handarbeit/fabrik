package pruefer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	gh "github.com/handarbeit/fabrik/github"
)

// fakeFile is one file of a fake commit. Mode defaults to 100644; Type to blob.
// Size, when positive, overrides the size the listing reports (a cap fixture
// that does not need the content to be that large).
type fakeFile struct {
	Data string
	Mode string
	Type string
	Size int64
}

type fakeGitRepo struct {
	commits  map[string]map[string]fakeFile // commit SHA -> path -> file
	tags     map[string]string              // tag name -> commit SHA
	branches map[string]string              // branch name -> commit SHA
	truncate bool                           // every recursive listing reports truncated
	// private makes every read of this repository fail the way an unreadable
	// repository does for an installation token: 404 on commits and trees.
	private bool
}

// fakeGit is a deterministic in-memory git host. Tree and blob SHAs are content
// hashes, so an unchanged plugin has the same tree SHA at two commits.
type fakeGit struct {
	mu    sync.Mutex
	repos map[string]*fakeGitRepo
	trees map[string][]gh.TreeEntry // "repo|treeSHA" -> recursive listing relative to that tree
	blobs map[string]fakeFile       // "repo|blobSHA" -> file

	blobErr error

	// read log
	SubtreeReads []string // "repo@commit:dir"
	TreeReads    []string // "repo:treeSHA"
	BlobReads    []string // "repo:blobSHA"
	RefReads     []string // "repo:ref"
	FileReads    []string // "repo@ref:path"
}

func newFakeGit() *fakeGit {
	return &fakeGit{repos: map[string]*fakeGitRepo{}, trees: map[string][]gh.TreeEntry{}, blobs: map[string]fakeFile{}}
}

// repo returns (creating) the fake repository ownerRepo.
func (g *fakeGit) repo(ownerRepo string) *fakeGitRepo {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := strings.ToLower(ownerRepo)
	if g.repos[k] == nil {
		g.repos[k] = &fakeGitRepo{commits: map[string]map[string]fakeFile{}, tags: map[string]string{}, branches: map[string]string{}}
	}
	return g.repos[k]
}

// commit adds a commit holding exactly files (path -> content, regular files).
func (r *fakeGitRepo) commit(sha string, files map[string]string) *fakeGitRepo {
	m := map[string]fakeFile{}
	for p, d := range files {
		m[p] = fakeFile{Data: d}
	}
	r.commits[sha] = m
	return r
}

// put adds or replaces one file of a commit.
func (r *fakeGitRepo) put(sha, p string, f fakeFile) {
	if r.commits[sha] == nil {
		r.commits[sha] = map[string]fakeFile{}
	}
	r.commits[sha][p] = f
}

func sum(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])[:40]
}

func (f fakeFile) mode() string {
	if f.Mode == "" {
		return "100644"
	}
	return f.Mode
}

func (f fakeFile) typ() string {
	if f.Type != "" {
		return f.Type
	}
	if f.Mode == "160000" {
		return "commit"
	}
	return "blob"
}

func (f fakeFile) size() int64 {
	if f.Size > 0 {
		return f.Size
	}
	return int64(len(f.Data))
}

func (g *fakeGit) lookup(ownerRepo string) *fakeGitRepo {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.repos[strings.ToLower(ownerRepo)]
}

func unreadable(what string) error {
	return fmt.Errorf("reading %s: %w", what, gh.ErrUnreadable)
}

// fileAtRef backs FetchFileAtRef for paths other than the repo config and the
// guidance skill.
func (g *fakeGit) fileAtRef(owner, repo, p, ref string) ([]byte, error) {
	if g == nil {
		return nil, gh.ErrNotFound
	}
	key := owner + "/" + repo
	g.mu.Lock()
	g.FileReads = append(g.FileReads, key+"@"+ref+":"+p)
	g.mu.Unlock()
	r := g.lookup(key)
	if r == nil || r.private {
		return nil, gh.ErrNotFound
	}
	files, ok := r.commits[ref]
	if !ok {
		return nil, gh.ErrNotFound
	}
	f, ok := files[p]
	if !ok || f.typ() != "blob" {
		return nil, gh.ErrNotFound
	}
	return []byte(f.Data), nil
}

// listing builds the recursive tree listing of every file under dir at a
// commit, with directory entries, and returns it with its content-addressed SHA.
func (g *fakeGit) listing(key, commit, dir string) (string, []gh.TreeEntry, bool) {
	r := g.lookup(key)
	files := r.commits[commit]
	prefix := ""
	if dir != "" {
		prefix = strings.Trim(dir, "/") + "/"
	}
	var paths []string
	for p := range files {
		if strings.HasPrefix(p, prefix) {
			paths = append(paths, strings.TrimPrefix(p, prefix))
		}
	}
	if len(paths) == 0 {
		return "", nil, false
	}
	sort.Strings(paths)
	var entries []gh.TreeEntry
	dirs := map[string]bool{}
	var hash []string
	for _, rel := range paths {
		f := files[prefix+rel]
		bsha := sum("blob", f.Data, f.mode())
		g.mu.Lock()
		g.blobs[strings.ToLower(key)+"|"+bsha] = f
		g.mu.Unlock()
		for d := path.Dir(rel); d != "." && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
			entries = append(entries, gh.TreeEntry{Path: d, Mode: "040000", Type: "tree", SHA: sum("dir", d)})
		}
		entries = append(entries, gh.TreeEntry{Path: rel, Mode: f.mode(), Type: f.typ(), SHA: bsha, Size: f.size()})
		hash = append(hash, rel, f.mode(), f.Data)
	}
	treeSHA := sum(hash...)
	g.mu.Lock()
	g.trees[strings.ToLower(key)+"|"+treeSHA] = entries
	g.mu.Unlock()
	return treeSHA, entries, true
}

func (f *fakeReviewer) FetchSubtreeSHA(owner, repo, commitSHA, dir string) (string, error) {
	g := f.git
	key := owner + "/" + repo
	if g == nil {
		return "", unreadable("no git store")
	}
	g.mu.Lock()
	g.SubtreeReads = append(g.SubtreeReads, key+"@"+commitSHA+":"+dir)
	g.mu.Unlock()
	r := g.lookup(key)
	if r == nil || r.private {
		return "", unreadable(key)
	}
	if _, ok := r.commits[commitSHA]; !ok {
		return "", unreadable("commit " + commitSHA)
	}
	if dir == "." {
		dir = ""
	}
	treeSHA, _, ok := g.listing(key, commitSHA, dir)
	if !ok {
		return "", fmt.Errorf("no directory %q: %w", dir, gh.ErrNotFound)
	}
	return treeSHA, nil
}

func (f *fakeReviewer) FetchTreeBySHA(owner, repo, treeSHA string) ([]gh.TreeEntry, bool, error) {
	g := f.git
	key := owner + "/" + repo
	g.mu.Lock()
	g.TreeReads = append(g.TreeReads, key+":"+treeSHA)
	entries, ok := g.trees[strings.ToLower(key)+"|"+treeSHA]
	g.mu.Unlock()
	if !ok {
		return nil, false, unreadable("tree " + treeSHA)
	}
	r := g.lookup(key)
	return append([]gh.TreeEntry(nil), entries...), r != nil && r.truncate, nil
}

func (f *fakeReviewer) FetchBlob(owner, repo, sha string, maxBytes int64) ([]byte, error) {
	g := f.git
	key := owner + "/" + repo
	g.mu.Lock()
	g.BlobReads = append(g.BlobReads, key+":"+sha)
	file, ok := g.blobs[strings.ToLower(key)+"|"+sha]
	err := g.blobErr
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, unreadable("blob " + sha)
	}
	if maxBytes > 0 && file.size() > maxBytes {
		return nil, fmt.Errorf("blob %s is %d bytes, exceeds the %d byte cap", sha, file.size(), maxBytes)
	}
	return []byte(file.Data), nil
}

func (f *fakeReviewer) ResolveRefKind(owner, repo, ref string) (gh.ResolvedRef, error) {
	g := f.git
	key := owner + "/" + repo
	g.mu.Lock()
	g.RefReads = append(g.RefReads, key+":"+ref)
	g.mu.Unlock()
	r := g.lookup(key)
	if r == nil || r.private {
		return gh.ResolvedRef{}, fmt.Errorf("resolving ref %s: %w", ref, gh.ErrNotFound)
	}
	b, hasB := r.branches[ref]
	t, hasT := r.tags[ref]
	switch {
	case hasB && hasT:
		return gh.ResolvedRef{Kind: gh.RefAmbiguous, SHA: t}, nil
	case hasB:
		return gh.ResolvedRef{Kind: gh.RefBranch, SHA: b}, nil
	case hasT:
		return gh.ResolvedRef{Kind: gh.RefTag, SHA: t}, nil
	}
	return gh.ResolvedRef{}, fmt.Errorf("resolving ref %s: %w", ref, gh.ErrNotFound)
}

// counts returns the number of blob and tree-listing reads so far.
func (g *fakeGit) counts() (blobs, trees int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.BlobReads), len(g.TreeReads)
}

// totalReads is every tree, blob, subtree and ref read, and every file read of
// a repo plugin declaration: the "nothing was touched" check.
func (g *fakeGit) totalReads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.SubtreeReads) + len(g.TreeReads) + len(g.BlobReads) + len(g.RefReads) + len(g.FileReads)
}
