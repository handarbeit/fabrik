package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchCompareDiff(t *testing.T) {
	const diffText = "diff --git a/foo.go b/foo.go\n+added line\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/compare/main...abc123" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github.v3.diff" {
			t.Errorf("Accept = %q", got)
		}
		w.Write([]byte(diffText))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	got, err := c.FetchCompareDiff("owner", "repo", "main", "abc123")
	if err != nil {
		t.Fatalf("FetchCompareDiff: %v", err)
	}
	if got != diffText {
		t.Errorf("diff = %q, want %q", got, diffText)
	}
}

func TestFetchCompareDiff_ErrorClasses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"not found", 404, `{"message":"Not Found"}`, ErrNotFound},
		{"unprocessable", 422, `{"message":"No common ancestor"}`, ErrUnprocessableEntity},
		{"too large", 406, `{"message":"diff exceeded","errors":[{"code":"too_large"}]}`, ErrDiffTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClientWithBaseURL("token", srv.URL)
			_, err := c.FetchCompareDiff("owner", "repo", "main", "abc")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want wrapping %v", err, tc.want)
			}
		})
	}
}

func TestFetchCompareDiff_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.FetchCompareDiff("owner", "repo", "main", "abc")
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnprocessableEntity) {
		t.Fatalf("err = %v, want a transient (non-not-found) error", err)
	}
}

func TestFindPRByHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("head"); got != "owner:fabrik/issue-7" {
			t.Errorf("head = %q", got)
		}
		w.Write([]byte(`[{"number":9,"title":"t","state":"open","draft":false,"user":{"login":"bot"},"head":{"sha":"deadbeef","ref":"fabrik/issue-7"},"base":{"ref":"main"}}]`))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	prs, err := c.FindPRByHead("owner", "repo", "fabrik/issue-7")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].Number != 9 || prs[0].HeadSHA != "deadbeef" || prs[0].BaseRef != "main" || prs[0].Author != "bot" {
		t.Fatalf("prs = %+v", prs)
	}
}
