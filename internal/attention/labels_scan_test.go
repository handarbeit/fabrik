package attention

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

// labelLiteral matches a string literal that is, whole, a fabrik:* label name or
// label-family prefix (optionally with a %s verb for the variable part).
var labelLiteral = regexp.MustCompile(`^fabrik:[A-Za-z0-9_:%.\-]*$`)

// notStateLabels are `fabrik:*` string literals in the engine's non-test sources
// that are NOT state-bearing labels, each with the reason it needs no meaning
// (#2121 R5). Everything else the scan finds must have a MeaningOf entry, so a
// new label cannot ship without one.
var notStateLabels = map[string]string{
	"fabrik:": "the bare namespace prefix used to recognise a fabrik label, not a label",
}

// scanLabelLiterals returns every label-shaped string literal in the non-test Go
// files under the given roots, with the file that holds the first occurrence.
func scanLabelLiterals(t *testing.T, roots []string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	found := map[string]string{}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("scan root %s: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				v, uerr := strconv.Unquote(lit.Value)
				if uerr != nil || !labelLiteral.MatchString(v) {
					return true
				}
				if i := strings.IndexByte(v, '%'); i >= 0 {
					v = v[:i]
				}
				if _, seen := found[v]; !seen {
					found[v] = path
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found
}

func labelScanRoots() []string {
	root := filepath.Join("..", "..")
	var roots []string
	for _, d := range []string{"engine", "github", "cmd", "internal", "tui", "config", "stages", "plugin"} {
		roots = append(roots, filepath.Join(root, d))
	}
	return roots
}

// TestEveryFabrikLabelHasAMeaning is the R5 gate (#2121): every `fabrik:*`
// label the engine's sources name has a meaning in the attention reasons, or is
// on the audited notStateLabels list with a reason. The attention package itself
// is skipped (its table is what is being checked).
func TestEveryFabrikLabelHasAMeaning(t *testing.T) {
	found := scanLabelLiterals(t, labelScanRoots())
	var missing []string
	for label, file := range found {
		if strings.Contains(filepath.ToSlash(file), "internal/attention/") {
			continue
		}
		if _, exempt := notStateLabels[label]; exempt {
			continue
		}
		if MeaningOf(label) == "" {
			missing = append(missing, label+" ("+file+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("fabrik:* label(s) with no meaning in internal/attention's labelTable/labelPrefixes: add a meaning, or — if it is not a state-bearing label — add it to notStateLabels with a reason:\n  %s",
			strings.Join(missing, "\n  "))
	}
	if len(found) < 20 {
		t.Errorf("scan found only %d label literals — the scan is not looking where the labels are", len(found))
	}
}

func TestNotStateLabelsExemptionsAreLive(t *testing.T) {
	found := scanLabelLiterals(t, labelScanRoots())
	for label, reason := range notStateLabels {
		if reason == "" {
			t.Errorf("%s: an exemption needs a reason", label)
		}
		if _, ok := found[label]; !ok {
			t.Errorf("%s is exempt but no longer appears in the sources: remove the stale exemption", label)
		}
		if MeaningOf(label) != "" {
			t.Errorf("%s has a meaning, so it needs no exemption", label)
		}
	}
}

// Non-vacuity: the scanner finds a label planted in a scratch source, and the
// meaning check rejects it — so a new label without a meaning fails the gate.
func TestLabelScanFailsForAnUnlistedLabel(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n\nconst l = \"fabrik:brand-new-state\"\nconst fam = \"fabrik:newfamily:%s\"\nconst other = \"not a label\"\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	found := scanLabelLiterals(t, []string{dir})
	if _, ok := found["fabrik:brand-new-state"]; !ok {
		t.Fatalf("scanner missed the planted label: %v", found)
	}
	if _, ok := found["fabrik:newfamily:"]; !ok {
		t.Fatalf("scanner missed the planted label family: %v", found)
	}
	for label := range found {
		if MeaningOf(label) != "" {
			t.Errorf("%s unexpectedly has a meaning", label)
		}
	}
}
