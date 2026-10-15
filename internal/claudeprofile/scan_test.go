package claudeprofile

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// goSources returns every non-test .go file under root (excluding vendored,
// worktree and hidden directories), as repo-relative paths.
func goSources(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n != "." && strings.HasPrefix(n, ".") || n == "vendor" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// getenvExceptions lists audited raw reads of the bare variable outside this
// package, each with its reason (SC-008).
var getenvExceptions = map[string]string{
	"pruefer/tui_run.go": "Pruefer adopts the resolver in #2294; until then it reads the inherited value for its footer",
}

var rawRead = regexp.MustCompile(`(?:Getenv|LookupEnv)\(\s*"CLAUDE_CONFIG_DIR"\s*\)`)

// SC-008: nothing but this package reads the bare variable from the
// environment, so no consumer can drift from the resolution.
func TestNoRawClaudeConfigDirReadsOutsideResolver(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range goSources(t, root) {
		if strings.HasPrefix(rel, "internal/claudeprofile/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !rawRead.Match(data) {
			continue
		}
		if _, ok := getenvExceptions[filepath.ToSlash(rel)]; !ok {
			t.Errorf("%s reads CLAUDE_CONFIG_DIR from the environment; use claudeprofile.Current()", rel)
		}
	}
	for rel := range getenvExceptions {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || !rawRead.Match(data) {
			t.Errorf("stale exception %s: no longer reads CLAUDE_CONFIG_DIR", rel)
		}
	}
}

// Interactive claude launches must go through claudeprofile.Command, or they
// silently run under the ambient account.
func TestNoPlainClaudeExecInInteractiveLaunchers(t *testing.T) {
	root := repoRoot(t)
	plain := regexp.MustCompile(`exec\.Command(?:Context)?\(\s*(?:ctx,\s*)?"claude"`)
	for _, dir := range []string{"tui", "watch"} {
		for _, rel := range goSources(t, filepath.Join(root, dir)) {
			data, err := os.ReadFile(filepath.Join(root, dir, rel))
			if err != nil {
				t.Fatal(err)
			}
			if plain.Match(data) {
				t.Errorf("%s/%s execs claude directly; use claudeprofile.Command", dir, rel)
			}
		}
	}
}

// US6: the resolver depends only on the standard library.
func TestPackageImportsOnlyStdlib(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if first := strings.SplitN(path, "/", 2)[0]; strings.Contains(first, ".") {
				t.Errorf("%s imports non-stdlib %q", e.Name(), path)
			}
		}
	}
}
