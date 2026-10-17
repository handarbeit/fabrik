package pruefer

import (
	"container/list"
	"strings"
	"sync"
)

// repoPluginCacheBytes bounds the plugin tree cache by the bytes of file
// content it holds, not an entry count: one plugin may be up to
// maxRepoPluginBytes, so a count bound would not bound memory (#2366, R7).
const repoPluginCacheBytes = 32 << 20

// repoPluginCacheKey identifies a validated plugin tree. A tree SHA is
// content-addressed, but the repository is part of the key so a SHA that exists
// in two repositories can never carry a trust decision from one to the other.
type repoPluginCacheKey struct {
	Repo string // lower-cased owner/repo
	Kind string // "tree" (in-repo, by subtree SHA) or "commit" (external, by commit SHA + path)
	SHA  string
	Path string // the plugin directory within the commit; empty for a tree key
}

func treeCacheKey(repo, treeSHA string) repoPluginCacheKey {
	return repoPluginCacheKey{Repo: strings.ToLower(repo), Kind: "tree", SHA: treeSHA}
}

func commitCacheKey(repo, commitSHA, path string) repoPluginCacheKey {
	return repoPluginCacheKey{Repo: strings.ToLower(repo), Kind: "commit", SHA: commitSHA, Path: path}
}

type repoPluginCacheEntry struct {
	key  repoPluginCacheKey
	tree *repoPluginTree
}

// repoPluginCache is a process-wide, concurrency-safe, byte-bounded LRU of
// validated plugin trees (#2366, R7, FR-014). Reviews run in parallel
// goroutines, so every operation takes the mutex. Only trees that passed
// validation and were read in full are stored; a refusal or a failed read is
// never cached, so it is re-decided next review. The cache is in memory and does
// not survive a restart.
type repoPluginCache struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	order    *list.List // front = most recently used
	items    map[repoPluginCacheKey]*list.Element
}

func newRepoPluginCache(maxBytes int64) *repoPluginCache {
	return &repoPluginCache{maxBytes: maxBytes, order: list.New(), items: map[repoPluginCacheKey]*list.Element{}}
}

// defaultRepoPluginCache is shared by every review of the process.
var defaultRepoPluginCache = newRepoPluginCache(repoPluginCacheBytes)

// get returns the cached tree and marks it recently used. The returned tree is
// shared and must be treated as read-only.
func (c *repoPluginCache) get(k repoPluginCacheKey) (*repoPluginTree, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*repoPluginCacheEntry).tree, true
}

// put stores a tree, evicting least-recently-used entries until the byte bound
// holds. A tree larger than the whole bound is not stored.
func (c *repoPluginCache) put(k repoPluginCacheKey, t *repoPluginTree) {
	if t == nil || t.Bytes > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.bytes -= el.Value.(*repoPluginCacheEntry).tree.Bytes
		c.order.Remove(el)
		delete(c.items, k)
	}
	c.items[k] = c.order.PushFront(&repoPluginCacheEntry{key: k, tree: t})
	c.bytes += t.Bytes
	for c.bytes > c.maxBytes {
		back := c.order.Back()
		if back == nil {
			break
		}
		e := back.Value.(*repoPluginCacheEntry)
		c.bytes -= e.tree.Bytes
		c.order.Remove(back)
		delete(c.items, e.key)
	}
}

// size reports the entries and bytes held, for tests.
func (c *repoPluginCache) size() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.bytes
}
