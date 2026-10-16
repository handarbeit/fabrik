package github

import (
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
