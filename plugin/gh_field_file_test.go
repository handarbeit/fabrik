package plugin

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawFieldFromFile matches `gh api -f key=@path` (or --raw-field). -f sends its
// value literally, so `@path` is never read: the field becomes the file name.
// Reading a field from a file needs -F. The Implement skill once told workers to
// update the Plan comment with -f, and every checklist update replaced the whole
// Plan comment with "@/tmp/…".
var rawFieldFromFile = regexp.MustCompile(`(?:^|\s)(?:-f|--raw-field)[ =]+['"]?[A-Za-z0-9_\[\]]+=@`)

// TestShippedPluginsNeverPassAFileToRawField scans every text file the plugins
// ship (the embedded stage plugin and the PM plugin) for the -f form.
func TestShippedPluginsNeverPassAFileToRawField(t *testing.T) {
	scanned := 0
	for _, root := range []string{"fabrik-workflows", "fabrik"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			switch filepath.Ext(path) {
			case ".md", ".sh", ".json":
			default:
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			for i, line := range strings.Split(string(b), "\n") {
				if rawFieldFromFile.MatchString(line) {
					t.Errorf("%s:%d: `gh api -f key=@file` sends the literal path; use -F: %s", path, i+1, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no plugin files")
	}
}

func TestRawFieldFromFilePattern(t *testing.T) {
	for line, want := range map[string]bool{
		"  -f body=@/tmp/x.md":                 true,
		`gh api x --raw-field body=@/tmp/x.md`: true,
		`gh api x -f 'body=@/tmp/x.md'`:        true,
		"  -F body=@/tmp/x.md":                 false,
		`gh api x --field body=@/tmp/x.md`:     false,
		`gh api x -f body=hello`:               false,
		`gh api x -f query='... @include ...'`: false,
	} {
		if got := rawFieldFromFile.MatchString(line); got != want {
			t.Errorf("%q: got %v, want %v", line, got, want)
		}
	}
}
