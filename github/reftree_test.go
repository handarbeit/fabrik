package github

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		case "/repos/o/r/git/commits/basesha":
			fmt.Fprint(w, `{"tree":{"sha":"root"}}`)
		case "/repos/o/r/git/commits/nodir":
			fmt.Fprint(w, `{"tree":{"sha":"emptyroot"}}`)
		case "/repos/o/r/git/trees/emptyroot":
			fmt.Fprint(w, `{"tree":[{"path":"README.md","type":"blob","mode":"100644","sha":"b0"}]}`)
		case "/repos/o/r/git/trees/root":
			fmt.Fprint(w, `{"tree":[{"path":".pruefer","type":"tree","mode":"040000","sha":"pr"},{"path":"plugins","type":"blob","mode":"100644","sha":"decoy"}]}`)
		case "/repos/o/r/git/trees/pr":
			fmt.Fprint(w, `{"tree":[{"path":"plugins","type":"tree","mode":"040000","sha":"pl"}]}`)
		case "/repos/o/r/git/trees/pl":
			if r.URL.Query().Get("recursive") != "1" {
				t.Errorf("final tree not read recursively")
			}
			fmt.Fprint(w, `{"truncated":true,"tree":[
{"path":"p/.claude-plugin/plugin.json","type":"blob","mode":"100644","sha":"b1","size":10},
{"path":"p/link","type":"blob","mode":"120000","sha":"b2","size":5},
{"path":"p/sub","type":"commit","mode":"160000","sha":"b3"}]}`)
		case "/repos/o/r/git/blobs/b1":
			fmt.Fprintf(w, `{"encoding":"base64","size":2,"content":%q}`, base64.StdEncoding.EncodeToString([]byte("hi")))
		case "/repos/o/r/git/blobs/big":
			fmt.Fprint(w, `{"encoding":"base64","size":999999,"content":""}`)
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

func TestFetchTreeAtRef_DescendsAndSurfacesModesAndTruncation(t *testing.T) {
	srv := refTreeServer(t)
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	entries, truncated, err := c.FetchTreeAtRef("o", "r", "basesha", ".pruefer/plugins")
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

func TestFetchTreeAtRef_MissingDirIsNotFound(t *testing.T) {
	srv := refTreeServer(t)
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	if _, _, err := c.FetchTreeAtRef("o", "r", "nodir", ".pruefer/plugins"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	// An unreadable commit is a read failure, never "the repo has no plugins".
	if _, _, err := c.FetchTreeAtRef("o", "r", "unknowncommit", ".pruefer/plugins"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("unknown commit err = %v, want a non-ErrNotFound read failure", err)
	}
}

func TestFetchTreeAtRef_FileNamedLikeDirIsNotADirectory(t *testing.T) {
	srv := refTreeServer(t)
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	// "plugins" at the root is a blob in the fixture; descending into it must not succeed.
	if _, _, err := c.FetchTreeAtRef("o", "r", "basesha", "plugins"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
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
}
