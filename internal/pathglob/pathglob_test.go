package pathglob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"go.sum", "go.sum", true},
		{"go.sum", "a/go.sum", false},
		{"*.lock", "yarn.lock", true},
		{"*.lock", "a/b/yarn.lock", false}, // * never crosses /
		{"**/*.lock", "a/b/yarn.lock", true},
		{"**/*.lock", "yarn.lock", true}, // ** matches zero segments
		{"vendor/**", "vendor/a/b.go", true},
		{"vendor/**", "vendor", true},
		{"vendor/**", "src/vendor/a.go", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/x/a.md", false},
		{"**", "any/path/at/all", true},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"a/**/z", "a/b/c/y", false},
		{"[", "x", false}, // malformed pattern never matches
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestMatchAny(t *testing.T) {
	pats := []string{"**/*.lock", "CHANGELOG.md"}
	if !MatchAny("a/yarn.lock", pats) || !MatchAny("CHANGELOG.md", pats) {
		t.Error("expected matches")
	}
	if MatchAny("src/main.go", pats) {
		t.Error("unexpected match")
	}
	if MatchAny("src/main.go", nil) {
		t.Error("nil patterns must match nothing")
	}
}
