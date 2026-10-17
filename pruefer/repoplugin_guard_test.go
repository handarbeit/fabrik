package pruefer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// prodFiles parses every non-test .go file of dir.
func prodFiles(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.File{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[n] = f
	}
	return out
}

// calledSelectors lists the files that call x.<name>(...) for any name in set.
func calledSelectors(files map[string]*ast.File, set map[string]bool) map[string][]string {
	hits := map[string][]string{}
	for fn, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && set[sel.Sel.Name] {
					hits[fn] = append(hits[fn], sel.Sel.Name)
				}
			}
			return true
		})
	}
	return hits
}

// FR-018, replacing #2307's ban on any repo plugin reader: the readers exist,
// but the only code that calls them is repoplugin_fetch.go, whose only entry
// point demands a repoPluginGate that only evaluateRepoPluginGate can open, and
// that entry point has exactly one call site, in ReviewPR, passing exactly the
// gate that function returned. Behavioural tests (TestGate_*, TestReviewPR_*)
// prove the gate's rules; this proves nothing goes around it.
func TestRepoPluginReadersAreOnlyReachableBehindTheGate(t *testing.T) {
	files := prodFiles(t, ".")

	// 1. The tree, blob, subtree and ref readers are called from one file.
	readers := map[string]bool{"FetchSubtreeSHA": true, "FetchTreeBySHA": true, "FetchBlob": true, "ResolveRefKind": true}
	for fn, names := range calledSelectors(files, readers) {
		if fn != "repoplugin_fetch.go" {
			t.Errorf("%s calls %v: only repoplugin_fetch.go may read repo plugin trees, blobs or refs", fn, names)
		}
	}

	// 2. fetchRepoPlugins is called exactly once, from review.go.
	hits := calledSelectorsOrIdents(files, "fetchRepoPlugins")
	if len(hits) != 1 || len(hits["review.go"]) != 1 {
		t.Errorf("fetchRepoPlugins call sites = %v, want exactly one, in review.go", hits)
	}

	// 3. That call passes the value evaluateRepoPluginGate returned, unchanged.
	src, err := os.ReadFile("review.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, need := range []string{
		"pluginGate, gateWhy := evaluateRepoPluginGate(",
		"if pluginGate.open {",
		"fetchRepoPlugins(client, pluginGate, ",
	} {
		if !strings.Contains(string(src), need) {
			t.Errorf("review.go no longer contains %q: the fetch must be guarded by the gate evaluateRepoPluginGate returned", need)
		}
	}

	// 4. An open gate is built in one place: evaluateRepoPluginGate.
	for fn, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "repoPluginGate" && len(lit.Elts) > 0 {
					if fn != "repoplugin_fetch.go" || fd.Name.Name != "evaluateRepoPluginGate" {
						t.Errorf("%s: %s builds a non-zero repoPluginGate: only evaluateRepoPluginGate may", fn, fd.Name.Name)
					}
				}
				return true
			})
		}
	}

	// 5. The old bespoke layout is gone, and the tree/blob URLs exist in one file of the github package.
	for fn := range files {
		b, _ := os.ReadFile(fn)
		if strings.Contains(string(b), ".pruefer/plugins") {
			t.Errorf("%s mentions the retired .pruefer/plugins layout", fn)
		}
	}
	gh := prodFiles(t, "../github")
	var urlFiles []string
	for fn := range gh {
		b, _ := os.ReadFile(filepath.Join("../github", fn))
		if strings.Contains(string(b), "git/trees/") || strings.Contains(string(b), "git/blobs/") {
			urlFiles = append(urlFiles, fn)
		}
	}
	sort.Strings(urlFiles)
	if len(urlFiles) != 1 || urlFiles[0] != "reftree.go" {
		t.Errorf("tree/blob endpoints appear in %v, want only reftree.go", urlFiles)
	}
}

// calledSelectorsOrIdents lists the call sites of a function by bare name.
func calledSelectorsOrIdents(files map[string]*ast.File, name string) map[string][]string {
	hits := map[string][]string{}
	for fn, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
					hits[fn] = append(hits[fn], name)
				}
			}
			return true
		})
	}
	return hits
}

// The clone is the untrusted PR head: the files that decide, fetch and validate
// repo plugins must be unable to read it, so they import nothing that touches
// the local filesystem or a subprocess.
func TestRepoPluginFetchCodeCannotReadTheClone(t *testing.T) {
	files := prodFiles(t, ".")
	banned := map[string]bool{"os": true, "os/exec": true, "io/ioutil": true, "path/filepath": true, "io/fs": true}
	for _, fn := range []string{"repoplugin_fetch.go", "repoplugin_decl.go", "repoplugins.go", "repoplugin_cache.go"} {
		f, ok := files[fn]
		if !ok {
			t.Fatalf("%s missing", fn)
		}
		for _, imp := range f.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); banned[p] {
				t.Errorf("%s imports %q: repo plugin code must not be able to read the clone", fn, p)
			}
		}
	}
}

// The guard itself must be able to fail: a scan over a synthetic file proves
// calledSelectors sees a reader call it should reject.
func TestRepoPluginGuardDetectsAReaderCall(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", "package p\nfunc g(c interface{ FetchBlob() }) { c.FetchBlob() }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := calledSelectors(map[string]*ast.File{"x.go": f}, map[string]bool{"FetchBlob": true})
	if len(hits["x.go"]) != 1 {
		t.Errorf("the scan missed a reader call: %v", hits)
	}
}
