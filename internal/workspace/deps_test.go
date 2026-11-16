package workspace

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The package must stay importable from every consumer, so it may use only the
// standard library and yaml.v3.
func TestImportsStayLeaf(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") && p != "gopkg.in/yaml.v3" {
				t.Errorf("%s imports %s; only the standard library and yaml.v3 are allowed", f, p)
			}
		}
	}
	_ = os.Stdout
}
