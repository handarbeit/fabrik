package registry

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
)

// loadEnvName is the harness function whose call marks a live scenario test.
const loadEnvName = "LoadEnv"

func parseDir(dir string, testOnly bool) (*token.FileSet, map[string]*ast.File, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, nil, fmt.Errorf("globbing %s: %w", dir, err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, p := range paths {
		if testOnly && !strings.HasSuffix(p, "_test.go") {
			continue
		}
		// Parse errors fail loudly: a file we cannot read is never silently skipped.
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", p, err)
		}
		files[p] = f
	}
	return fset, files, nil
}

// isTestFunc reports whether fd is a top-level `func TestXxx(...)` (no receiver).
func isTestFunc(fd *ast.FuncDecl) bool {
	if fd.Recv != nil || fd.Body == nil {
		return false
	}
	n := fd.Name.Name
	return strings.HasPrefix(n, "Test") && (len(n) == 4 || !isLower(n[4]))
}

func isLower(b byte) bool { return b >= 'a' && b <= 'z' }

func callsLoadEnv(n ast.Node) []token.Pos {
	var out []token.Pos
	ast.Inspect(n, func(x ast.Node) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == loadEnvName {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// ScanLiveTests applies the R1 rule to dir (tests/e2e): a live scenario test is a
// top-level `Test*` function in a *_test.go file whose body — nested closures
// included — calls LoadEnv. Harness unit tests are excluded by that rule alone.
//
// A LoadEnv call anywhere in the directory that is not inside such a Test* body
// (e.g. reached through a helper) is returned as an error rather than silently
// excluded; the `func LoadEnv` declaration itself is exempt. Result is sorted.
func ScanLiveTests(dir string) ([]string, error) {
	fset, files, err := parseDir(dir, false)
	if err != nil {
		return nil, err
	}
	var live []string
	var stray []string
	for path, f := range files {
		isTestFile := strings.HasSuffix(path, "_test.go")
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			calls := callsLoadEnv(fd.Body)
			if len(calls) == 0 {
				continue
			}
			if isTestFile && isTestFunc(fd) {
				live = append(live, fd.Name.Name)
				continue
			}
			for _, p := range calls {
				stray = append(stray, fmt.Sprintf("%s (in %s)", fset.Position(p), fd.Name.Name))
			}
		}
		// Calls in package-level var initialisers etc.
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok {
				for _, p := range callsLoadEnv(gd) {
					stray = append(stray, fmt.Sprintf("%s (package-level)", fset.Position(p)))
				}
			}
		}
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		return nil, fmt.Errorf("%s called outside a top-level Test* body (a helper-call would hide a live test from the registry check; call it directly in the test): %s",
			loadEnvName, strings.Join(stray, "; "))
	}
	sort.Strings(live)
	return live, nil
}

// ScanSimTests returns the top-level Test* names in dir's *_test.go files only
// (tests/sim itself — not its simgh/simclaude/ghfault subpackages, and not unit
// tests elsewhere). Subtests cannot be referenced. Result is sorted.
func ScanSimTests(dir string) ([]string, error) {
	_, files, err := parseDir(dir, true)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && isTestFunc(fd) {
				names = append(names, fd.Name.Name)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}
