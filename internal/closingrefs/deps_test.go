package closingrefs

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The matcher must stay pure: no network, no process execution, no engine or GitHub client.
func TestPackageIsPure(t *testing.T) {
	forbidden := []string{"net", "os/exec", "github.com/handarbeit/fabrik"}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Errorf("%s imports %q; closingrefs must stay pure", f, path)
				}
			}
		}
	}
}
