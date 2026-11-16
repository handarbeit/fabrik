package workspace

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// R3 (#2470, ADR 2470): no production site builds a workspace path by hand.
// A "workspace path" is <anything>/.fabrik/<worktrees|repos|sessions|logs|debug>,
// either as filepath.Join(…, ".fabrik", …, "worktrees", …) arguments or as a
// string literal containing ".fabrik/worktrees". Comments are not literals,
// so documentation may name the default layout freely.

var movedNames = map[string]bool{
	Worktrees: true, Repos: true, Sessions: true, Logs: true, Debug: true,
}

var literalRE = regexp.MustCompile(`\.fabrik[/\\](worktrees|repos|sessions|logs|debug)\b`)

// scanSource returns one finding per hand-built workspace path in src.
func scanSource(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			seen := false
			for _, a := range x.Args {
				lit, ok := a.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				if s == ".fabrik" {
					seen = true
				} else if seen && movedNames[s] {
					out = append(out, fset.Position(x.Pos()).String()+": joins \".fabrik\" and \""+s+"\" by hand")
					break
				}
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil && literalRE.MatchString(s) {
					out = append(out, fset.Position(x.Pos()).String()+": string literal names "+literalRE.FindString(s))
				}
			}
		}
		return true
	})
	return out, nil
}

// scanExceptions lists audited files that may name the default in-bed layout,
// each with its reason. Anything else must go through a Layout.
var scanExceptions = map[string]string{
	"cmd/init.go": "writes the git-exclude entries of the default in-repo layout; a workspace outside the repository needs none",
}

func TestScanSourceFlagsHandBuiltPaths(t *testing.T) {
	violation := []byte(`package x
import "path/filepath"
func f(cwd string) string { return filepath.Join(cwd, ".fabrik", "worktrees", "o-r") }
func g(cwd string) string { return cwd + "/.fabrik/sessions/x" }
`)
	got, err := scanSource("v.go", violation)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 findings in the deliberate violation, got %v", got)
	}
	compliant := []byte(`package x
import "path/filepath"
// The default layout is <bed>/.fabrik/worktrees (comments are not literals).
func f(bed string) string { return filepath.Join(bed, ".fabrik", "state", "x") }
func g(bed string) string { return filepath.Join(bed, ".fabrik", "config.yaml") }
`)
	got, err = scanSource("c.go", compliant)
	if err != nil || len(got) != 0 {
		t.Fatalf("compliant source must pass: %v %v", got, err)
	}
}

func TestNoHandBuiltWorkspacePaths(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var findings []string
	used := map[string]bool{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != root && (strings.HasPrefix(n, ".") || n == "vendor" || n == "node_modules" || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/workspace/") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fs, err := scanSource(rel, src)
		if err != nil {
			return err
		}
		if len(fs) == 0 {
			return nil
		}
		if _, ok := scanExceptions[rel]; ok {
			used[rel] = true
			return nil
		}
		findings = append(findings, fs...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(findings)
	for _, f := range findings {
		t.Errorf("hand-built workspace path (use workspace.Layout): %s", f)
	}
	for rel := range scanExceptions {
		if !used[rel] {
			t.Errorf("stale exception %s: it no longer names a workspace path", rel)
		}
	}
}
