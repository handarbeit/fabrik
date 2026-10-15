package ciskip

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is FR-013's guard: the classifier may call a path skippable only if
// no Go test reads it. A Go test that starts reading a non-Go file the rule
// table calls skippable would otherwise let CI skip the very test that guards
// it, and nothing would notice.
//
// Two layers:
//
//  1. knownReaders — a hand-maintained registry of the non-Go files Go tests
//     read today (ADR-2254's enumeration). Each must classify as run-everything.
//  2. A static scan of every *_test.go for string literals that climb out of
//     the test's directory ("../docs/x.md"), alone or as the leading arguments
//     of filepath.Join, resolved against the test's directory. Anything that
//     resolves to a repo file the classifier calls skippable fails the test and
//     names the file and the reader.
//
// Known blind spot: a path computed at run time (built from a variable, a
// constant in another file, or a directory walk) is invisible to the scan. The
// registry in layer 1 is the mitigation; ADR-2254 records the gap.

// knownReaders are the non-Go files Go tests read, with the reader.
var knownReaders = map[string]string{
	"CLAUDE.md":                                "plugin/rebase_reset_regression_test.go",
	"docs/state-machine.md":                    "plugin/labels_drift_test.go, internal/channelevents/docs_test.go",
	"docs/USER_GUIDE.md":                       "internal/channelevents/docs_test.go",
	"adrs/1966-b-fabrik-mcp-channel-events.md": "internal/channelevents/docs_test.go",
	".claude-plugin/marketplace.json":          "plugin/pm_plugin_test.go, plugin/names_collision_test.go, tools/*/main_test.go",
	"plugin/fabrik/hooks/version-check.sh":     "plugin/pm_hook_test.go",
	"stages/examples/plan.yaml":                "stages (go:embed)",
	"tests/e2e/registry/registry.json":         "tests/e2e/registry (go:embed)",
	"plugin/shipped_files.json":                "plugin (go:embed)",
	"scripts/ci/worker-shaped-test.sh":         "CI job step (executes the Go suite)",
	"scripts/lib/parallel.sh":                  "CI job step (sourced by the go test step)",
	"scripts/ci/classify-diff.sh":              "tests/ciskip",
	"scripts/ci/go-skip-paths.txt":             "tests/ciskip",
	".github/workflows/ci.yml":                 "the gate itself",
	"go.mod":                                   "go toolchain",
}

// scanExemptions are literal reads the scan flags that do not read a repo file
// as an input to a Go test. Each entry needs a reason.
var scanExemptions = map[string]string{}

// classifyAll runs the classifier over paths and returns path -> class
// (skip|run). It goes through the script so the Go side never re-implements
// the rule table.
func classifyAll(t *testing.T, paths []string) map[string]string {
	t.Helper()
	var in []byte
	for _, p := range paths {
		in = append(in, p...)
		in = append(in, 0)
	}
	r := runScript(t, repoRoot(t), in, nil, "--paths-from-stdin0")
	out := map[string]string{}
	for _, line := range strings.Split(r.Stdout, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		rest := strings.TrimLeft(line, " ")
		class, afterClass, ok := strings.Cut(rest, " ")
		if !ok || (class != "skip" && class != "run") {
			continue
		}
		afterClass = strings.TrimLeft(afterClass, " ")
		idx := strings.LastIndex(afterClass, "  (")
		if idx < 0 {
			continue
		}
		out[afterClass[:idx]] = class
	}
	return out
}

func TestKnownReadersForceTheFullSuite(t *testing.T) {
	t.Parallel()
	var paths []string
	for p := range knownReaders {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	got := classifyAll(t, paths)
	for _, p := range paths {
		if got[p] != "run" {
			t.Errorf("%s is read by %s but classifies as %q; it must force the full suite", p, knownReaders[p], got[p])
		}
	}
}

// repoFiles lists every regular file in the checkout, repo-relative, skipping
// VCS and Fabrik working directories.
func repoFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".fabrik", ".e2e-coverage", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}
	sort.Strings(files)
	return files
}

type literalRead struct {
	testFile string // repo-relative test file
	rel      string // repo-relative resolved path (or glob pattern)
}

// stringLit returns the unquoted value of a string literal expression.
func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// scanReaders finds string literals in every *_test.go file that climb out of
// the test's directory ("../..."), alone or as the leading arguments of a
// filepath.Join — the form every cross-package reader uses.
func scanReaders(t *testing.T, root string, files []string) []literalRead {
	t.Helper()
	var reads []literalRead
	fset := token.NewFileSet()
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		af, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		dir := path.Dir(f)
		add := func(lits []string) {
			if len(lits) == 0 {
				return
			}
			joined := path.Clean(strings.Join(lits, "/"))
			// Only paths that leave the test's own directory are cross-package
			// reads; a read inside the package is covered by the package's own
			// directory being a Go input.
			if path.IsAbs(joined) || (joined != ".." && !strings.HasPrefix(joined, "../")) {
				return
			}
			rel := path.Clean(path.Join(dir, joined))
			if rel == ".." || strings.HasPrefix(rel, "../") || rel == "." {
				return
			}
			reads = append(reads, literalRead{testFile: f, rel: rel})
		}
		ast.Inspect(af, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				// Any "../..." literal: covers helpers such as
				// extractStateMachineLabels("../docs/state-machine.md").
				if s, ok := stringLit(x); ok {
					add([]string{s})
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Join" {
					return true
				}
				// Leading literals only: a variable argument ends what we can resolve.
				var lits []string
				for _, a := range x.Args {
					s, ok := stringLit(a)
					if !ok {
						break
					}
					lits = append(lits, s)
				}
				add(lits)
			}
			return true
		})
	}
	return reads
}

func TestNoGoTestReadsASkippableFile(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	files := repoFiles(t, root)
	reads := scanReaders(t, root, files)
	if len(reads) == 0 {
		t.Fatal("the scan found no cross-package reads at all; it is broken (plugin/labels_drift_test.go reads ../docs/state-machine.md)")
	}

	// Expand each read (a file, a directory or a glob) into the repo files it names.
	var targets []string
	readerOf := map[string][]string{}
	for _, r := range reads {
		var matched []string
		if strings.ContainsAny(r.rel, "*?[") {
			for _, f := range files {
				if ok, _ := path.Match(r.rel, f); ok {
					matched = append(matched, f)
				}
			}
		} else {
			for _, f := range files {
				if f == r.rel || strings.HasPrefix(f, r.rel+"/") {
					matched = append(matched, f)
				}
			}
		}
		for _, f := range matched {
			if _, seen := readerOf[f]; !seen {
				targets = append(targets, f)
			}
			readerOf[f] = append(readerOf[f], r.testFile)
		}
	}
	sort.Strings(targets)
	// The scan must see the readers we know about, or it is not scanning.
	for _, want := range []string{"CLAUDE.md", "docs/state-machine.md", ".claude-plugin/marketplace.json"} {
		if _, ok := readerOf[want]; !ok {
			t.Fatalf("the scan did not find the known reader of %s; it is broken", want)
		}
	}
	classes := classifyAll(t, targets)
	for _, f := range targets {
		if classes[f] != "skip" {
			continue
		}
		if _, ok := scanExemptions[f]; ok {
			continue
		}
		t.Errorf("%s is classified skippable but is read by %s: remove it from the skip rules or add a never rule in scripts/ci/go-skip-paths.txt",
			f, strings.Join(dedupe(readerOf[f]), ", "))
	}
}

// TestReaderGuardDetectsASkippableReader is the guard's own non-vacuity test:
// a synthetic reader of a file the rules call skippable must be reported.
func TestReaderGuardDetectsASkippableReader(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/x_test.go", "package pkg\nimport \"os\"\nfunc f() { _, _ = os.ReadFile(\"../docs/stage-lifecycle.md\") }\n")
	write("docs/stage-lifecycle.md", "x\n")
	files := repoFiles(t, root)
	reads := scanReaders(t, root, files)
	if len(reads) != 1 || reads[0].rel != "docs/stage-lifecycle.md" || reads[0].testFile != "pkg/x_test.go" {
		t.Fatalf("scan did not resolve the synthetic reader: %+v", reads)
	}
	if got := classifyAll(t, []string{reads[0].rel})[reads[0].rel]; got != "skip" {
		t.Fatalf("fixture is only meaningful while docs/stage-lifecycle.md is skippable, got %q", got)
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
