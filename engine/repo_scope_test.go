package engine

import (
	"strings"
	"testing"
)

func TestRepoScope(t *testing.T) {
	cases := []struct{ owner, repo, want string }{
		{"shadoworg", "widgets", "shadoworg/widgets"},
		{"shadoworg", "", "(all repos on the board)"},
		{"", "", "(all repos on the board)"},
		{"", "widgets", "widgets"},
	}
	for _, c := range cases {
		got := RepoScope(c.owner, c.repo)
		if got != c.want {
			t.Errorf("RepoScope(%q,%q) = %q, want %q", c.owner, c.repo, got, c.want)
		}
		if strings.HasSuffix(got, "/") {
			t.Errorf("RepoScope(%q,%q) = %q has a dangling separator", c.owner, c.repo, got)
		}
	}
}

func TestProjectRef(t *testing.T) {
	if got := projectRef("o", "r", 3); got != "o/r#3" {
		t.Errorf("single-repo = %q, want o/r#3", got)
	}
	got := projectRef("o", "", 3)
	if got != "o (all repos) project #3" || strings.Contains(got, "o/") {
		t.Errorf("multi-repo = %q", got)
	}
}
