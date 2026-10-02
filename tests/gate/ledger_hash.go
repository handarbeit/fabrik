package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// HashTests computes, for every top-level Test* function in the Go package at
// dir (tests/e2e), the source hash its ledger PASS is recorded against (R3). A
// recorded PASS certifies only the exact source that produced it: when the hash
// differs, that (test, leg) is uncovered again.
//
// The hash covers, for one test:
//
//   - the test function's own source,
//   - the whole file it lives in,
//   - every package-level declaration in the package that the function
//     transitively references — helpers in harness.go and mergetrain_helpers.go
//     included — and every init function.
//
// Resolution is by identifier NAME, with no type checking: a selector `x.Foo`
// pulls in every declaration named Foo, methods included. That OVER-approximates
// the real call graph, which is the safe direction — it can cause an extra rerun
// but never a false certification. Not hashed (a known, documented gap): files
// that are not Go source — testdata, embedded assets — and anything outside the
// package. Parse errors are returned, never skipped: a file that cannot be read
// must not silently leave a test hashed on less than it depends on.
func HashTests(dir string) (map[string]string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s: %w", dir, err)
	}
	sort.Strings(paths)
	fset := token.NewFileSet()

	type chunk struct {
		file string
		text string
		ids  map[string]bool // identifiers appearing in the chunk
	}
	var (
		tests    = map[string]*chunk{}   // test name -> its function chunk
		testFile = map[string]string{}   // test name -> file base name
		fileSrc  = map[string][]byte{}   // file base name -> bytes
		byName   = map[string][]*chunk{} // package-level name -> declarations
		inits    []*chunk
	)

	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		f, err := parser.ParseFile(fset, p, src, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", p, err)
		}
		base := filepath.Base(p)
		fileSrc[base] = src
		text := func(n ast.Node) string {
			start, end := fset.Position(n.Pos()).Offset, fset.Position(n.End()).Offset
			if start < 0 || end > len(src) || start > end {
				return ""
			}
			return string(src[start:end])
		}
		mk := func(n ast.Node) *chunk {
			return &chunk{file: base, text: text(n), ids: identsOf(n)}
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				c := mk(d)
				switch {
				case d.Recv == nil && d.Name.Name == "init":
					inits = append(inits, c)
				case d.Recv == nil && isTestName(d.Name.Name):
					tests[d.Name.Name] = c
					testFile[d.Name.Name] = base
					byName[d.Name.Name] = append(byName[d.Name.Name], c)
				default:
					byName[d.Name.Name] = append(byName[d.Name.Name], c)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						byName[s.Name.Name] = append(byName[s.Name.Name], mk(s))
					case *ast.ValueSpec:
						c := mk(s)
						for _, n := range s.Names {
							byName[n.Name] = append(byName[n.Name], c)
						}
					}
				}
			}
		}
	}

	out := make(map[string]string, len(tests))
	for name, root := range tests {
		closure := map[*chunk]bool{root: true}
		queue := []*chunk{root}
		for _, c := range inits {
			closure[c] = true
			queue = append(queue, c)
		}
		for len(queue) > 0 {
			c := queue[0]
			queue = queue[1:]
			for id := range c.ids {
				for _, dep := range byName[id] {
					if !closure[dep] {
						closure[dep] = true
						queue = append(queue, dep)
					}
				}
			}
		}
		// Hash the closure in a stable order: text sorted, so map iteration and
		// file order never change the result.
		texts := make([]string, 0, len(closure))
		for c := range closure {
			texts = append(texts, c.file+"\x00"+c.text)
		}
		sort.Strings(texts)
		h := sha256.New()
		fmt.Fprintf(h, "file:%s\x00", testFile[name])
		h.Write(fileSrc[testFile[name]])
		for _, t := range texts {
			h.Write([]byte("\x01"))
			h.Write([]byte(t))
		}
		out[name] = hex.EncodeToString(h.Sum(nil))
	}
	return out, nil
}

func isTestName(n string) bool {
	return strings.HasPrefix(n, "Test") && (len(n) == 4 || !(n[4] >= 'a' && n[4] <= 'z'))
}

// identsOf collects every identifier name under n (selector names included).
func identsOf(n ast.Node) map[string]bool {
	ids := map[string]bool{}
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok {
			ids[id.Name] = true
		}
		return true
	})
	return ids
}
