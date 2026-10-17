package github

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func refTreeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/git/ref/heads/release/1":
			fmt.Fprint(w, `{"object":{"type":"commit","sha":"basesha"}}`)
		case "/repos/o/r/git/ref/heads/tagged":
			fmt.Fprint(w, `{"object":{"type":"tag","sha":"x"}}`)
		// ref kinds
		case "/repos/o/r/git/ref/heads/main":
			fmt.Fprint(w, `{"object":{"type":"commit","sha":"mainsha"}}`)
		case "/repos/o/r/git/ref/heads/both", "/repos/o/r/git/ref/tags/both":
			fmt.Fprint(w, `{"object":{"type":"commit","sha":"bothsha"}}`)
		case "/repos/o/r/git/ref/tags/v1":
			fmt.Fprint(w, `{"object":{"type":"commit","sha":"v1commit"}}`)
		case "/repos/o/r/git/ref/tags/v2":
			fmt.Fprint(w, `{"object":{"type":"tag","sha":"tagobj"}}`)
		case "/repos/o/r/git/tags/tagobj":
			fmt.Fprint(w, `{"object":{"type":"commit","sha":"v2commit"}}`)
		case "/repos/o/r/git/ref/tags/v3":
			fmt.Fprint(w, `{"object":{"type":"tag","sha":"treetag"}}`)
		case "/repos/o/r/git/tags/treetag":
			fmt.Fprint(w, `{"object":{"type":"tree","sha":"atree"}}`)
		case "/repos/o/r/git/ref/tags/loop":
			fmt.Fprint(w, `{"object":{"type":"tag","sha":"loopobj"}}`)
		case "/repos/o/r/git/tags/loopobj":
			fmt.Fprint(w, `{"object":{"type":"tag","sha":"loopobj"}}`)
		case "/repos/o/r/git/ref/heads/boom":
			http.Error(w, "oops", http.StatusInternalServerError)
		case "/repos/o/r/git/ref/tags/feat/x":
			fmt.Fprint(w, `{"object":{"type":"commit","sha":"slashtag"}}`)
		// trees
		case "/repos/o/r/git/commits/basesha":
			fmt.Fprint(w, `{"tree":{"sha":"root"}}`)
		case "/repos/o/r/git/commits/nodir":
			fmt.Fprint(w, `{"tree":{"sha":"emptyroot"}}`)
		case "/repos/o/r/git/trees/emptyroot":
			fmt.Fprint(w, `{"tree":[{"path":"README.md","type":"blob","mode":"100644","sha":"b0"}]}`)
		case "/repos/o/r/git/trees/root":
			fmt.Fprint(w, `{"tree":[{"path":"plugins","type":"tree","mode":"040000","sha":"pl"},{"path":"file","type":"blob","mode":"100644","sha":"decoy"}]}`)
		case "/repos/o/r/git/trees/pl":
			if r.URL.Query().Get("recursive") != "1" {
				// The non-recursive descent read of the "plugins" directory.
				fmt.Fprint(w, `{"tree":[{"path":"p","type":"tree","mode":"040000","sha":"ptree"}]}`)
				return
			}
			fmt.Fprint(w, `{"truncated":true,"tree":[
{"path":"p/.claude-plugin/plugin.json","type":"blob","mode":"100644","sha":"b1","size":10},
{"path":"p/link","type":"blob","mode":"120000","sha":"b2","size":5},
{"path":"p/sub","type":"commit","mode":"160000","sha":"b3"}]}`)
		case "/repos/o/r/git/trees/ptree":
			fmt.Fprint(w, `{"tree":[{"path":"a","type":"blob","mode":"100755","sha":"b1","size":2}]}`)
		case "/repos/o/r/git/blobs/b1":
			fmt.Fprintf(w, `{"encoding":"base64","size":2,"content":%q}`, base64.StdEncoding.EncodeToString([]byte("hi")))
		case "/repos/o/r/git/blobs/big":
			fmt.Fprint(w, `{"encoding":"base64","size":999999,"content":""}`)
		case "/repos/o/r/git/blobs/liar":
			// Reports a small size but carries more content than the cap.
			fmt.Fprintf(w, `{"encoding":"base64","size":2,"content":%q}`, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 50))))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestResolveRefSHA(t *testing.T) {
	srv := refTreeServer(t)
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	got, err := c.ResolveRefSHA("o", "r", "release/1")
	if err != nil || got != "basesha" {
		t.Fatalf("ResolveRefSHA = %q, %v", got, err)
	}
	if _, err := c.ResolveRefSHA("o", "r", "tagged"); err == nil {
		t.Error("a non-commit ref must be refused")
	}
	if _, err := c.ResolveRefSHA("o", "r", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing branch err = %v, want ErrNotFound", err)
	}
}

func TestResolveRefKind(t *testing.T) {
	srv := refTreeServer(t)
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	cases := []struct {
		ref  string
		kind RefKind
		sha  string
	}{
		{"main", RefBranch, "mainsha"},
		{"both", RefAmbiguous, "bothsha"},
		{"v1", RefTag, "v1commit"},
		{"v2", RefTag, "v2commit"}, // annotated: peeled through the tag object
		{"feat/x", RefTag, "slashtag"},
	}
	for _, tc := range cases {
		got, err := c.ResolveRefKind("o", "r", tc.ref)
		if err != nil || got.Kind != tc.kind || got.SHA != tc.sha {
			t.Errorf("ResolveRefKind(%q) = %+v, %v; want kind %s sha %q", tc.ref, got, err, tc.kind, tc.sha)
		}
	}
	if _, err := c.ResolveRefKind("o", "r", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown ref err = %v, want ErrNotFound", err)
	}
	if _, err := c.ResolveRefKind("o", "r", "v3"); err == nil {
		t.Error("a tag pointing at a tree must be refused")
	}
	if _, err := c.ResolveRefKind("o", "r", "loop"); err == nil {
		t.Error("an endless tag chain must be refused")
	}
	// A server error on either lookup is a failure, never "not found".
	if _, err := c.ResolveRefKind("o", "r", "boom"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("5xx err = %v, want a non-ErrNotFound failure", err)
	}
}

func TestFetchSubtreeSHAAndTree(t *testing.T) {
	srv := refTreeServer(t)
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	sha, err := c.FetchSubtreeSHA("o", "r", "basesha", "plugins")
	if err != nil || sha != "pl" {
		t.Fatalf("FetchSubtreeSHA = %q, %v", sha, err)
	}
	if root, err := c.FetchSubtreeSHA("o", "r", "basesha", "."); err != nil || root != "root" {
		t.Errorf("root = %q, %v", root, err)
	}
	if nested, err := c.FetchSubtreeSHA("o", "r", "basesha", "plugins/p"); err != nil || nested != "ptree" {
		t.Errorf("nested = %q, %v", nested, err)
	}
	entries, truncated, err := c.FetchTreeBySHA("o", "r", sha)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("truncated flag lost")
	}
	if len(entries) != 3 || entries[0].Path != "p/.claude-plugin/plugin.json" || entries[0].Size != 10 || entries[1].Mode != "120000" || entries[2].Type != "commit" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestFetchSubtreeSHA_MissingDirIsNotFound(t *testing.T) {
	srv := refTreeServer(t)
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.FetchSubtreeSHA("o", "r", "nodir", "plugins"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	// An unreadable commit is a read failure, never "the repo has no plugins".
	if _, err := c.FetchSubtreeSHA("o", "r", "unknowncommit", "plugins"); err == nil || errors.Is(err, ErrNotFound) || !errors.Is(err, ErrUnreadable) {
		t.Errorf("unknown commit err = %v, want a non-ErrNotFound read failure", err)
	}
	// A file named like the directory is not a directory.
	if _, err := c.FetchSubtreeSHA("o", "r", "basesha", "file"); !errors.Is(err, ErrNotFound) {
		t.Errorf("file-as-dir err = %v, want ErrNotFound", err)
	}
}

func TestFetchBlob(t *testing.T) {
	srv := refTreeServer(t)
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	got, err := c.FetchBlob("o", "r", "b1", 100)
	if err != nil || string(got) != "hi" {
		t.Fatalf("FetchBlob = %q, %v", got, err)
	}
	if _, err := c.FetchBlob("o", "r", "big", 1000); err == nil {
		t.Error("an oversized blob must be refused")
	}
	if _, err := c.FetchBlob("o", "r", "liar", 10); err == nil {
		t.Error("a blob whose decoded length disagrees with its reported size must be refused")
	}
}

func TestListOpenPRsAndFetchPRDetails_CarryDefaultBranch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		const pr = `{"number":7,"base":{"ref":"main","repo":{"default_branch":"trunk"}},"head":{"sha":"h","ref":"x"}}`
		switch r.URL.Path {
		case "/repos/o/r/pulls":
			fmt.Fprint(w, "["+pr+"]")
		case "/repos/o/r/pulls/7":
			fmt.Fprint(w, pr)
		case "/repos/o/r/pulls/8":
			fmt.Fprint(w, `{"number":8,"base":{"ref":"main"},"head":{"sha":"h","ref":"x"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	list, err := c.ListOpenPRs("o", "r")
	if err != nil || len(list) != 1 || list[0].DefaultBranch != "trunk" || list[0].BaseRef != "main" {
		t.Fatalf("ListOpenPRs = %+v, %v", list, err)
	}
	one, err := c.FetchPRDetails("o", "r", 7)
	if err != nil || one.DefaultBranch != "trunk" {
		t.Fatalf("FetchPRDetails = %+v, %v", one, err)
	}
	// An absent base.repo is unknown (empty), never a guess.
	unk, err := c.FetchPRDetails("o", "r", 8)
	if err != nil || unk.DefaultBranch != "" {
		t.Fatalf("absent default branch = %+v, %v", unk, err)
	}
}
