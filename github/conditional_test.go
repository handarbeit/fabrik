package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// etagServer serves a mutable set of URL → (etag, body) responses and honors
// If-None-Match the way GitHub does. It records every request's URL and
// If-None-Match header.
type etagServer struct {
	mu     sync.Mutex
	bodies map[string]string // request URI → body
	etags  map[string]string // request URI → etag
	seen   []seenReq
}

type seenReq struct{ uri, inm string }

func newETagServer(t *testing.T) (*etagServer, *Client) {
	t.Helper()
	es := &etagServer{bodies: map[string]string{}, etags: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		es.mu.Lock()
		defer es.mu.Unlock()
		uri := r.URL.RequestURI()
		es.seen = append(es.seen, seenReq{uri, r.Header.Get("If-None-Match")})
		body, ok := es.bodies[uri]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		etag := es.etags[uri]
		if etag != "" {
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(304)
				return
			}
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return es, NewClientWithBaseURL("tok", srv.URL)
}

func (es *etagServer) set(uri, etag, body string) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.bodies[uri], es.etags[uri] = body, etag
}

func (es *etagServer) requests() []seenReq {
	es.mu.Lock()
	defer es.mu.Unlock()
	return append([]seenReq(nil), es.seen...)
}

type item struct {
	N int `json:"n"`
}

func TestCondGetJSON_DisabledSendsNoConditionalHeader(t *testing.T) {
	es, c := newETagServer(t)
	es.set("/x", `"e1"`, `[{"n":1}]`)
	for i := 0; i < 2; i++ {
		var out []item
		if err := condGetJSON(c, c.baseURL+"/x", &out); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range es.requests() {
		if r.inm != "" {
			t.Fatalf("disabled client sent If-None-Match %q", r.inm)
		}
	}
	if st := c.RequestStats(); st.Total != 2 || st.NotModified != 0 {
		t.Fatalf("stats = %+v, want Total=2 NotModified=0", st)
	}
}

func TestCondGetJSON_SendsETagAndServes304FromCache(t *testing.T) {
	es, c := newETagServer(t)
	c.EnableConditionalRequests()
	es.set("/x", `"e1"`, `[{"n":1}]`)

	var first []item
	if err := condGetJSON(c, c.baseURL+"/x", &first); err != nil {
		t.Fatal(err)
	}
	// Poison the server body: a 304 must be served from the cache, never
	// from a re-read of the (now different) body.
	es.mu.Lock()
	es.bodies["/x"] = `not json at all`
	es.mu.Unlock()
	var second []item
	if err := condGetJSON(c, c.baseURL+"/x", &second); err != nil {
		t.Fatalf("304 path failed: %v", err)
	}
	if len(second) != 1 || second[0].N != 1 {
		t.Fatalf("second = %+v, want cached [{1}]", second)
	}
	reqs := es.requests()
	if reqs[0].inm != "" {
		t.Errorf("first request sent If-None-Match %q with no cache entry", reqs[0].inm)
	}
	if reqs[1].inm != `"e1"` {
		t.Errorf("second request If-None-Match = %q, want %q", reqs[1].inm, `"e1"`)
	}
	if st := c.RequestStats(); st.Total != 2 || st.NotModified != 1 {
		t.Errorf("stats = %+v, want Total=2 NotModified=1", st)
	}
}

func TestCondGetJSON_200ReplacesEntry(t *testing.T) {
	es, c := newETagServer(t)
	c.EnableConditionalRequests()
	es.set("/x", `"e1"`, `[{"n":1}]`)
	var out []item
	if err := condGetJSON(c, c.baseURL+"/x", &out); err != nil {
		t.Fatal(err)
	}
	es.set("/x", `"e2"`, `[{"n":2}]`)
	if err := condGetJSON(c, c.baseURL+"/x", &out); err != nil {
		t.Fatal(err)
	}
	if out[0].N != 2 {
		t.Fatalf("out = %+v, want the new body", out)
	}
	if err := condGetJSON(c, c.baseURL+"/x", &out); err != nil {
		t.Fatal(err)
	}
	reqs := es.requests()
	if got := reqs[2].inm; got != `"e2"` {
		t.Errorf("third request If-None-Match = %q, want the replaced validator %q", got, `"e2"`)
	}
	if out[0].N != 2 {
		t.Errorf("out after 304 = %+v, want [{2}]", out)
	}
}

func TestCondGetJSON_KeyedByExactURL(t *testing.T) {
	es, c := newETagServer(t)
	c.EnableConditionalRequests()
	// Same ETag on both URLs: if the cache ignored the URL, page 2 would
	// send the validator and get a 304 answered with page 1's data.
	es.set("/x?page=1", `"same"`, `[{"n":1}]`)
	es.set("/x?page=2", `"same"`, `[{"n":2}]`)
	var p1, p2 []item
	if err := condGetJSON(c, c.baseURL+"/x?page=1", &p1); err != nil {
		t.Fatal(err)
	}
	if err := condGetJSON(c, c.baseURL+"/x?page=2", &p2); err != nil {
		t.Fatal(err)
	}
	if p2[0].N != 2 {
		t.Fatalf("page 2 = %+v, want [{2}]", p2)
	}
	if got := es.requests()[1].inm; got != "" {
		t.Errorf("page 2 request sent If-None-Match %q from page 1's entry", got)
	}
}

func TestCondGetJSON_304WithoutSentHeaderIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(304)
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("tok", srv.URL)
	c.EnableConditionalRequests()
	var out []item
	if err := condGetJSON(c, srv.URL+"/x", &out); err == nil {
		t.Fatal("expected an error for an unsolicited 304, got nil")
	}
}

func TestCondGetJSON_ErrorStatusNeverStores(t *testing.T) {
	es, c := newETagServer(t)
	c.EnableConditionalRequests()
	var out []item
	err := condGetJSON(c, c.baseURL+"/missing", &out)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want 404", err)
	}
	if n := c.cond.len(); n != 0 {
		t.Errorf("cache holds %d entries after a 404, want 0", n)
	}
	_ = es
}

func TestCondGetJSON_NoETagDropsEntry(t *testing.T) {
	es, c := newETagServer(t)
	c.EnableConditionalRequests()
	es.set("/x", `"e1"`, `[{"n":1}]`)
	var out []item
	if err := condGetJSON(c, c.baseURL+"/x", &out); err != nil {
		t.Fatal(err)
	}
	es.set("/x", "", `[{"n":9}]`) // validator-less 200
	if err := condGetJSON(c, c.baseURL+"/x", &out); err != nil {
		t.Fatal(err)
	}
	if c.cond.len() != 0 {
		t.Errorf("entry survived a validator-less 200")
	}
}

func TestCondGetJSON_TokenChangeDropsCache(t *testing.T) {
	es, c := newETagServer(t)
	c.EnableConditionalRequests()
	es.set("/x", `"e1"`, `[{"n":1}]`)
	var out []item
	if err := condGetJSON(c, c.baseURL+"/x", &out); err != nil {
		t.Fatal(err)
	}
	c.SetToken("tok") // unchanged: keep
	if c.cond.len() != 1 {
		t.Fatalf("SetToken with the same token dropped the cache")
	}
	c.SetToken("rotated")
	if c.cond.len() != 0 {
		t.Fatalf("SetToken with a new token left %d entries", c.cond.len())
	}
	if err := condGetJSON(c, c.baseURL+"/x", &out); err != nil {
		t.Fatal(err)
	}
	if got := es.requests()[1].inm; got != "" {
		t.Errorf("request after token rotation sent If-None-Match %q", got)
	}
}

func TestCondCache_StaleGenerationStoreIgnored(t *testing.T) {
	cc := newCondCache(10, 1000)
	_, gen := cc.lookup("k")
	cc.clear()
	cc.store(gen, &condEntry{key: "k", etag: "e", value: 1, size: 1})
	if cc.len() != 0 {
		t.Fatal("a store from before clear() repopulated the cache")
	}
}

func TestCondCache_BoundsEvictLRU(t *testing.T) {
	cc := newCondCache(2, 100)
	_, g := cc.lookup("a")
	cc.store(g, &condEntry{key: "a", size: 10})
	cc.store(g, &condEntry{key: "b", size: 10})
	cc.lookup("a") // a is now most recent
	cc.store(g, &condEntry{key: "c", size: 10})
	if e, _ := cc.lookup("b"); e != nil {
		t.Error("b should have been evicted as least recently used")
	}
	if e, _ := cc.lookup("a"); e == nil {
		t.Error("a should have survived")
	}

	cc = newCondCache(100, 25)
	_, g = cc.lookup("a")
	cc.store(g, &condEntry{key: "a", size: 10})
	cc.store(g, &condEntry{key: "b", size: 10})
	cc.store(g, &condEntry{key: "c", size: 10})
	if cc.bytes > 25 || cc.len() != 2 {
		t.Errorf("bytes=%d len=%d, want <=25 and 2", cc.bytes, cc.len())
	}
	cc.store(g, &condEntry{key: "huge", size: 26})
	if e, _ := cc.lookup("huge"); e != nil {
		t.Error("an entry larger than the byte cap was stored")
	}
}

// Two-page listing: page 1 unchanged (304), page 2 changed (200). The change
// on page 2 must be visible — a page-level 304 must not hide it.
func TestPaginateREST_Page1304Page2200(t *testing.T) {
	es, c := newETagServer(t)
	c.EnableConditionalRequests()
	mk := func(from, n int, tag string) string {
		items := make([]map[string]any, n)
		for i := range items {
			items[i] = map[string]any{"n": from + i, "tag": tag}
		}
		b, _ := json.Marshal(items)
		return string(b)
	}
	urlFor := func(page int) string { return fmt.Sprintf("%s/list?page=%d", c.baseURL, page) }
	es.set("/list?page=1", `"p1"`, mk(0, restPageSize, "a"))
	es.set("/list?page=2", `"p2v1"`, mk(restPageSize, 3, "a"))

	all, err := paginateREST[map[string]any](c, "list", func(p int) string { return urlFor(p) })
	if err != nil || len(all) != restPageSize+3 {
		t.Fatalf("first fetch: len=%d err=%v", len(all), err)
	}
	es.set("/list?page=2", `"p2v2"`, mk(restPageSize, 4, "b"))
	all, err = paginateREST[map[string]any](c, "list", func(p int) string { return urlFor(p) })
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != restPageSize+4 {
		t.Fatalf("second fetch len = %d, want %d — page 2 change hidden", len(all), restPageSize+4)
	}
	if st := c.RequestStats(); st.Total != 4 || st.NotModified != 1 {
		t.Errorf("stats = %+v, want Total=4 NotModified=1 (page 1 only)", st)
	}
}

// Mutating a slice returned from a fetch must not corrupt a later 304 serve.
func TestPaginateREST_ReturnedSliceMutationDoesNotCorruptCache(t *testing.T) {
	es, c := newETagServer(t)
	c.EnableConditionalRequests()
	es.set("/l?page=1", `"e"`, `[{"n":1},{"n":2}]`)
	fetch := func() []item {
		all, err := paginateREST[item](c, "l", func(p int) string { return fmt.Sprintf("%s/l?page=%d", c.baseURL, p) })
		if err != nil {
			t.Fatal(err)
		}
		return all
	}
	a := fetch()
	a[0].N = 999
	a = append(a[:0], item{N: 7})
	_ = a
	b := fetch()
	if len(b) != 2 || b[0].N != 1 {
		t.Fatalf("second fetch = %+v, want [{1} {2}] — cache corrupted by caller mutation", b)
	}
}

func TestFetchFileAtRef_ConditionalNotModifiedServesCachedBytes(t *testing.T) {
	es, c := newETagServer(t)
	c.EnableConditionalRequests()
	body, _ := json.Marshal(map[string]any{
		"type": "file", "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString([]byte("hello")),
	})
	uri := "/repos/o/r/contents/.pruefer/config.yaml?ref=main"
	es.set(uri, `"cfg"`, string(body))
	for i := 0; i < 2; i++ {
		got, err := c.FetchFileAtRef("o", "r", ".pruefer/config.yaml", "main")
		if err != nil || string(got) != "hello" {
			t.Fatalf("call %d: got %q err=%v", i, got, err)
		}
	}
	if st := c.RequestStats(); st.NotModified != 1 {
		t.Errorf("stats = %+v, want one 304", st)
	}
	if got := es.requests()[1].inm; got != `"cfg"` {
		t.Errorf("If-None-Match = %q", got)
	}
}

func TestListOpenPRs_SurfacesUpdatedAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"number":1,"state":"open","updated_at":"2026-09-29T10:00:00Z","user":{"login":"u"},"head":{"sha":"s","ref":"b"},"base":{"ref":"main"}}]`)
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("tok", srv.URL)
	prs, err := c.ListOpenPRs("o", "r")
	if err != nil || len(prs) != 1 {
		t.Fatalf("prs=%v err=%v", prs, err)
	}
	if prs[0].UpdatedAt != "2026-09-29T10:00:00Z" {
		t.Errorf("UpdatedAt = %q", prs[0].UpdatedAt)
	}
}
