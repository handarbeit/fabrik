package github

import (
	"container/list"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// Conditional-request support (#1952, ADR-1952).
//
// GitHub does not count a 304 Not Modified against the primary rate limit, so a
// GET that re-reads an unchanged resource with If-None-Match is free. This file
// holds the opt-in machinery: a bounded, per-Client LRU of decoded response
// values keyed by the exact request URL (plus Accept), and condGetJSON, the
// typed GET that consults it.
//
// It is opt-in per Client (EnableConditionalRequests). A Client that never
// enables it — every Fabrik engine client — sends no conditional header and
// behaves byte-for-byte as before.

const (
	// condMaxEntries caps the number of cached responses per client. Closed
	// PRs stop being requested and age out of the LRU.
	condMaxEntries = 2048
	// condMaxBytes caps the summed wire size of the cached response bodies.
	condMaxBytes = 32 << 20
)

// condEntry is one cached response: the validator GitHub gave us and the
// already-decoded value (so a 304 is served without re-parsing).
type condEntry struct {
	key   string
	etag  string
	value any
	size  int
}

// condCache is a bounded LRU of condEntry values. It is safe for concurrent
// use. gen increments on clear so an in-flight request that started under a
// previous identity cannot repopulate the cache after a credential change.
type condCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element // key → element holding *condEntry
	lru        *list.List               // front = most recently used
	bytes      int
	gen        uint64
	maxEntries int
	maxBytes   int
}

func newCondCache(maxEntries, maxBytes int) *condCache {
	return &condCache{
		entries:    make(map[string]*list.Element),
		lru:        list.New(),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
	}
}

// lookup returns the entry for key (marking it recently used) and the current
// generation.
func (cc *condCache) lookup(key string) (*condEntry, uint64) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	el, ok := cc.entries[key]
	if !ok {
		return nil, cc.gen
	}
	cc.lru.MoveToFront(el)
	return el.Value.(*condEntry), cc.gen
}

// store inserts or replaces the entry for key, evicting least-recently-used
// entries past either bound. It is a no-op when gen is stale (the cache was
// cleared while the request was in flight). An entry larger than the byte cap
// is not stored at all.
func (cc *condCache) store(gen uint64, e *condEntry) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if gen != cc.gen {
		return
	}
	cc.removeLocked(e.key)
	if e.size > cc.maxBytes {
		return
	}
	cc.entries[e.key] = cc.lru.PushFront(e)
	cc.bytes += e.size
	for cc.lru.Len() > cc.maxEntries || cc.bytes > cc.maxBytes {
		oldest := cc.lru.Back()
		if oldest == nil {
			break
		}
		cc.removeLocked(oldest.Value.(*condEntry).key)
	}
}

// drop removes key (used when a 200 arrives without a validator).
func (cc *condCache) drop(key string) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.removeLocked(key)
}

func (cc *condCache) removeLocked(key string) {
	el, ok := cc.entries[key]
	if !ok {
		return
	}
	cc.bytes -= el.Value.(*condEntry).size
	cc.lru.Remove(el)
	delete(cc.entries, key)
}

// clear drops every entry and invalidates in-flight stores.
func (cc *condCache) clear() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.entries = make(map[string]*list.Element)
	cc.lru.Init()
	cc.bytes = 0
	cc.gen++
}

// len reports the number of cached entries (tests).
func (cc *condCache) len() int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.lru.Len()
}

// EnableConditionalRequests turns on ETag-based conditional GETs for the typed
// REST reads that go through condGetJSON (paginated collections and
// FetchFileAtRef). Idempotent. Only clients whose callers want this — Pruefer's
// per-installation clients — should call it; the default is off so the Fabrik
// engine's read path is unchanged.
func (c *Client) EnableConditionalRequests() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cond == nil {
		c.cond = newCondCache(condMaxEntries, condMaxBytes)
	}
}

func (c *Client) condCache() *condCache {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cond
}

const jsonAccept = "application/vnd.github+json"

// condGetJSON GETs url and decodes the JSON body into out, using the client's
// conditional-request cache when enabled.
//
//   - Disabled: identical to restGetJSON.
//   - Enabled and a cached entry exists for exactly this URL: send its ETag as
//     If-None-Match. A 304 serves the cached decoded value with no re-parse.
//   - A 200 always replaces the entry (or drops it when GitHub sent no ETag).
//   - Any error status, including 404, never stores; a 304 for a request that
//     sent no If-None-Match is an error rather than an empty result.
//
// The cache key is the full URL (query string included) so a page-2 request
// can never be answered from a page-1 entry.
func condGetJSON[T any](c *Client, url string, out *T) error {
	cache := c.condCache()
	if cache == nil {
		return c.restGetJSON(url, out)
	}
	key := jsonAccept + "\x00" + url
	entry, gen := cache.lookup(key)

	var cached T
	var haveCached bool
	var extra http.Header
	if entry != nil {
		if v, ok := entry.value.(T); ok {
			cached, haveCached = v, true
			extra = http.Header{"If-None-Match": []string{entry.etag}}
		}
	}

	resp, body, err := c.doWithHeaders("GET", url, jsonAccept, extra, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotModified {
		if !haveCached {
			return fmt.Errorf("GitHub API returned 304 for %s but no conditional request was sent", url)
		}
		*out = cached
		return nil
	}
	var decoded T
	if err := json.Unmarshal(body, &decoded); err != nil {
		return err
	}
	*out = decoded
	if etag := resp.Header.Get("ETag"); etag != "" {
		cache.store(gen, &condEntry{key: key, etag: etag, value: decoded, size: len(body)})
	} else {
		cache.drop(key)
	}
	return nil
}
