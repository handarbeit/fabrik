package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// causeArgIndex is where each pause-writing helper takes its pauseCause.
var causeArgIndex = map[string]int{
	"reapplyPauseLabels":    1,
	"pauseInterruptedIssue": 1,
	"applyPauseLabels":      1,
	"applyPauseCause":       1,
	"addPausedLabelToItem":  3,
	"pauseMergeTrainMember": 3,
}

// rawPausedAdders are the label-write calls that, given the fabrik:paused
// literal, write a pause directly.
var rawPausedAdders = map[string]bool{
	"addLabel": true, "applyLabelAdd": true, "addLabelChecked": true, "addLabelCheckedBy": true, "AddLabelToIssue": true,
}

// rawPausedWriteAllowed lists the functions that may write fabrik:paused
// directly, each with the reason it does not need its own cause. Everything else
// must go through pauseIssue or applyPauseLabels so the cause is written with
// the pause (#2487, FR-001/FR-007).
var rawPausedWriteAllowed = map[string]string{
	"pauseIssue":            "the one pause primitive: writes the cause label from opts.cause right after",
	"reapplyPauseLabels":    "re-pause without a comment; takes the cause as a parameter and writes it",
	"applyPauseLabels":      "the direct-write helper: writes the cause label right after",
	"pauseMergeTrainMember": "writes through the raw client for the echo reservation; takes the cause and writes it",
	"pauseForDispatchLimit": "checked pre-add of the same label whose pauseIssue call, immediately after, writes the cause",
}

func causeExprOK(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		if v.Name == "cause" {
			return true
		}
		for _, c := range allPauseCauses {
			if c == pauseCauseNone {
				continue
			}
			if strings.EqualFold(v.Name, causeConstName(c)) {
				return true
			}
		}
	case *ast.SelectorExpr:
		return v.Sel.Name == "cause" // opts.cause
	}
	return false
}

// causeConstName turns "review-timeout" into "pauseCauseReviewTimeout".
func causeConstName(c pauseCause) string {
	var b strings.Builder
	b.WriteString("pauseCause")
	for _, part := range strings.Split(string(c), "-") {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

func isPausedLiteral(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING && v.Value == `"fabrik:paused"`
	case *ast.Ident:
		return v.Name == "pauseLabel"
	}
	return false
}

// scanPauseCauseSites parses the given sources (file name -> source) and returns
// one violation per pause site that does not declare a valid cause, plus the set
// of allow-listed functions that really contain a raw fabrik:paused write.
func scanPauseCauseSites(t *testing.T, srcs map[string]string) (violations []string, allowedUsed map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	allowedUsed = map[string]bool{}
	for name, src := range srcs {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := fd.Name.Name
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.CompositeLit:
					id, ok := v.Type.(*ast.Ident)
					if !ok || id.Name != "pauseOpts" {
						return true
					}
					found := false
					for _, el := range v.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "cause" {
							found = true
							if !causeExprOK(kv.Value) {
								violations = append(violations, posStr(fset, v.Pos())+": pauseOpts{cause:} is not a pauseCause* constant")
							}
						}
					}
					if !found {
						violations = append(violations, posStr(fset, v.Pos())+": pauseOpts literal in "+fn+" declares no cause")
					}
				case *ast.CallExpr:
					se, ok := v.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if idx, tracked := causeArgIndex[se.Sel.Name]; tracked {
						if len(v.Args) <= idx || !causeExprOK(v.Args[idx]) {
							violations = append(violations, posStr(fset, v.Pos())+": "+se.Sel.Name+" in "+fn+" passes no pauseCause* constant")
						}
					}
					if rawPausedAdders[se.Sel.Name] {
						for _, a := range v.Args {
							if isPausedLiteral(a) {
								if _, ok := rawPausedWriteAllowed[fn]; ok {
									allowedUsed[fn] = true
								} else {
									violations = append(violations, posStr(fset, v.Pos())+": raw fabrik:paused write in "+fn+" (use pauseIssue or applyPauseLabels so a cause is written)")
								}
							}
						}
					}
				}
				return true
			})
		}
	}
	sort.Strings(violations)
	return violations, allowedUsed
}

func posStr(fset *token.FileSet, p token.Pos) string {
	pos := fset.Position(p)
	return filepath.Base(pos.Filename) + ":" + strconv.Itoa(pos.Line)
}

func readEngineSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no engine sources: %v", err)
	}
	srcs := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := readFileForScan(f)
		if err != nil {
			t.Fatal(err)
		}
		srcs[f] = b
	}
	return srcs
}

// TestEveryPauseSiteDeclaresACause is the FR-007 gate (#2487): every pauseOpts
// literal, every pause-writing helper call and every direct fabrik:paused write
// in the engine names a pauseCause, so a new pause site cannot ship without one.
func TestEveryPauseSiteDeclaresACause(t *testing.T) {
	violations, used := scanPauseCauseSites(t, readEngineSources(t))
	if len(violations) > 0 {
		t.Errorf("pause site(s) with no declared cause:\n  %s", strings.Join(violations, "\n  "))
	}
	for fn, reason := range rawPausedWriteAllowed {
		if reason == "" {
			t.Errorf("%s: an exemption needs a reason", fn)
		}
		if !used[fn] {
			t.Errorf("%s is exempt but no longer writes fabrik:paused directly: remove the stale exemption", fn)
		}
	}
	if len(used) < 3 {
		t.Errorf("scan found raw writes in only %d allow-listed functions — it is not looking where the pauses are", len(used))
	}
}

// Non-vacuity: the scan fails on an uncaused pauseOpts literal, an uncaused
// helper call, a non-constant cause and a raw fabrik:paused write.
func TestPauseCauseScanFailsForAnUncausedSite(t *testing.T) {
	planted := map[string]string{"planted.go": `package engine

func (e *Engine) badLiteral() {
	e.pauseIssue(item, "x", pauseOpts{awaitingInput: true})
}
func (e *Engine) badHelper() {
	e.reapplyPauseLabels(item)
	e.applyPauseLabels(item, someVar, true)
}
func (e *Engine) badNone() {
	e.pauseIssue(item, "x", pauseOpts{cause: pauseCauseNone})
}
func (e *Engine) badRaw() {
	e.addLabel(item, "fabrik:paused")
}
func (e *Engine) good() {
	e.pauseIssue(item, "x", pauseOpts{cause: pauseCauseReviewTimeout})
	e.applyPauseLabels(item, pauseCauseFailed, false)
}
`}
	violations, _ := scanPauseCauseSites(t, planted)
	want := []string{"badLiteral", "reapplyPauseLabels in badHelper", "applyPauseLabels in badHelper", "is not a pauseCause* constant", "raw fabrik:paused write in badRaw"}
	joined := strings.Join(violations, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("scan missed %q; violations:\n%s", w, joined)
		}
	}
	if strings.Contains(joined, "good") {
		t.Errorf("scan flagged the compliant function:\n%s", joined)
	}
}

func readFileForScan(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
