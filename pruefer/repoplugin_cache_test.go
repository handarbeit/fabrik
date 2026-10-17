package pruefer

import (
	"strconv"
	"sync"
	"testing"
)

func cacheTree(n int64) *repoPluginTree {
	return &repoPluginTree{Bytes: n, Files: []repoPluginFile{{Path: "f", Data: make([]byte, n)}}}
}

func TestRepoPluginCache_HitMissAndKeyIsolation(t *testing.T) {
	c := newRepoPluginCache(100)
	k := treeCacheKey("Acme/Tools", "sha1")
	if _, ok := c.get(k); ok {
		t.Fatal("empty cache hit")
	}
	c.put(k, cacheTree(10))
	if got, ok := c.get(treeCacheKey("acme/tools", "sha1")); !ok || got.Bytes != 10 {
		t.Errorf("repo key must be case-insensitive: %v %v", got, ok)
	}
	if _, ok := c.get(treeCacheKey("other/repo", "sha1")); ok {
		t.Error("the same SHA in another repository must not hit")
	}
	if _, ok := c.get(commitCacheKey("acme/tools", "sha1", "")); ok {
		t.Error("a commit key must not hit a tree entry")
	}
	if _, ok := c.get(commitCacheKey("acme/tools", "sha1", "a")); ok {
		t.Error("unexpected hit")
	}
	c.put(commitCacheKey("acme/tools", "sha1", "a"), cacheTree(5))
	if _, ok := c.get(commitCacheKey("acme/tools", "sha1", "b")); ok {
		t.Error("a different path in the same commit must not hit")
	}
}

func TestRepoPluginCache_EvictsLeastRecentlyUsedByBytes(t *testing.T) {
	c := newRepoPluginCache(100)
	a, b, d := treeCacheKey("o/r", "a"), treeCacheKey("o/r", "b"), treeCacheKey("o/r", "d")
	c.put(a, cacheTree(40))
	c.put(b, cacheTree(40))
	c.get(a) // a is now the more recently used
	c.put(d, cacheTree(40))
	if _, ok := c.get(b); ok {
		t.Error("b should have been evicted")
	}
	if _, ok := c.get(a); !ok {
		t.Error("a was recently used and must survive")
	}
	if _, bytes := c.size(); bytes > 100 {
		t.Errorf("bytes = %d, over the bound", bytes)
	}
	// A tree larger than the whole bound is never stored.
	c.put(treeCacheKey("o/r", "huge"), cacheTree(101))
	if _, ok := c.get(treeCacheKey("o/r", "huge")); ok {
		t.Error("an over-bound tree was stored")
	}
	// Replacing a key does not double count.
	c.put(a, cacheTree(10))
	if n, bytes := c.size(); n != 2 || bytes != 50 {
		t.Errorf("entries=%d bytes=%d, want 2 and 50", n, bytes)
	}
}

func TestRepoPluginCache_Concurrent(t *testing.T) {
	c := newRepoPluginCache(1000)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := treeCacheKey("o/r", strconv.Itoa((g*7+i)%30))
				c.put(k, cacheTree(int64(10+i%20)))
				c.get(k)
			}
		}(g)
	}
	wg.Wait()
	if _, bytes := c.size(); bytes > 1000 || bytes < 0 {
		t.Errorf("bytes = %d out of bounds", bytes)
	}
}
