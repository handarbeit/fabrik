package sim

import (
	"errors"
	"testing"
)

func TestRealGitDir(t *testing.T) {
	t.Parallel()
	ok := func(string) bool { return true }
	no := func(string) bool { return false }
	xc := func(s string, err error) func() (string, error) { return func() (string, error) { return s, err } }
	const shim = "/usr/bin/git"
	const real = "/Library/Developer/CommandLineTools/usr/bin/git"
	cases := []struct {
		name    string
		goos    string
		pathGit string
		xcrun   func() (string, error)
		exec    func(string) bool
		want    string
		wantOK  bool
	}{
		{"darwin shim replaced", "darwin", shim, xc(real+"\n", nil), ok, "/Library/Developer/CommandLineTools/usr/bin", true},
		{"darwin no git on PATH", "darwin", "", xc(real, nil), ok, "/Library/Developer/CommandLineTools/usr/bin", true},
		{"linux does nothing", "linux", shim, xc(real, nil), ok, "", false},
		{"xcrun missing", "darwin", shim, xc("", errors.New("exec: xcrun: not found")), ok, "", false},
		{"empty output", "darwin", shim, xc("  \n", nil), ok, "", false},
		{"relative output", "darwin", shim, xc("git", nil), ok, "", false},
		{"not executable", "darwin", shim, xc(real, nil), no, "", false},
		{"homebrew git on PATH is kept", "darwin", "/opt/homebrew/bin/git", xc(real, nil), ok, "", false},
		{"same path as PATH's git", "darwin", real, xc(real, nil), ok, "", false},
		{"same path, unclean", "darwin", "/Library/Developer/CommandLineTools/usr/bin/../bin/git", xc(real, nil), ok, "", false},
		{"shim path, unclean", "darwin", "/usr/bin/../bin/git", xc(real, nil), ok, "/Library/Developer/CommandLineTools/usr/bin", true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, gotOK := realGitDir(c.goos, c.pathGit, c.xcrun, c.exec)
			if got != c.want || gotOK != c.wantOK {
				t.Fatalf("realGitDir = %q, %v; want %q, %v", got, gotOK, c.want, c.wantOK)
			}
		})
	}
}
