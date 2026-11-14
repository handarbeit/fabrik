package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The #2532 neutralisation seams are per-Engine methods (zero value = production),
// so the parallel sim tests can use them; a package-level function or variable
// would leak one test's setting into another.
func TestCooldownSeamsArePerEngine(t *testing.T) {
	want := map[string]bool{
		"SetEventReplaceableStretchDisabledForTest": false,
		"SetSafetyNetDisabledForTest":               false,
		"SetTimerWakeDisabledForTest":               false,
		"AttachStubTransportForTest":                false,
		"HealthWatchTickForTest":                    false,
	}
	fset := token.NewFileSet()
	for _, file := range []string{"retry_backoff.go", "transport_seam.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if _, tracked := want[fd.Name.Name]; !tracked {
				continue
			}
			if fd.Recv == nil {
				t.Errorf("%s must be a method on *Engine, not a package-level function", fd.Name.Name)
				continue
			}
			want[fd.Name.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("seam %s not found as an Engine method", name)
		}
	}
}
