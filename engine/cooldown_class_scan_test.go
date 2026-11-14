package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/itemstate"
)

// The cooldown classification guard (#2532, ADR 2532, FR-003): a go/parser scan of
// the non-test engine sources finds every reason passed to a CooldownRecorded
// literal (and every literal read through CooldownAt) and fails when one has no
// entry in cooldownClasses, when a table entry is never recorded, or when a
// reason's value cannot be resolved statically and is not on the audited list.

// variableReasonExceptions are CooldownRecorded sites whose Reason is a variable.
// Each names the file, the identifier, and every reason it can hold; every listed
// reason must be classified. A new variable-valued site fails the scan until it is
// added here with its reasons.
var variableReasonExceptions = []struct {
	file    string
	ident   string
	reasons []string
	why     string
}{
	{"feedback_gate.go", "reason", []string{reasonFeedbackPending, feedbackReadFailedReason},
		"feedbackGateBlocks picks feedback-pending or, for a hold caused only by a failed live read, feedback-read-failed"},
}

type scanResult struct {
	recorded map[string]bool // reasons passed to CooldownRecorded
	read     map[string]bool // reasons read through CooldownAt(literal or const)
	problems []string        // unresolvable, non-audited reasons
}

func scanCooldownReasons(t *testing.T) scanResult {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", n), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		files = append(files, f)
		names = append(names, n)
	}

	// Package-level string constants (initialiser expressions kept for resolution).
	pkgConsts := map[string]ast.Expr{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i < len(vs.Values) {
						pkgConsts[id.Name] = vs.Values[i]
					}
				}
			}
		}
	}

	var resolve func(expr ast.Expr, local map[string]ast.Expr, depth int) (string, bool)
	resolve = func(expr ast.Expr, local map[string]ast.Expr, depth int) (string, bool) {
		if depth > 8 {
			return "", false
		}
		switch v := expr.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				s, err := strconv.Unquote(v.Value)
				return s, err == nil
			}
		case *ast.Ident:
			if e, ok := local[v.Name]; ok {
				return resolve(e, local, depth+1)
			}
			if e, ok := pkgConsts[v.Name]; ok {
				return resolve(e, nil, depth+1)
			}
		case *ast.SelectorExpr:
			if x, ok := v.X.(*ast.Ident); ok && x.Name == "itemstate" && v.Sel.Name == "ArchiveEligibleCooldown" {
				return itemstate.ArchiveEligibleCooldown, true
			}
		}
		return "", false
	}

	res := scanResult{recorded: map[string]bool{}, read: map[string]bool{}}
	for fi, f := range files {
		file := names[fi]
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			local := map[string]ast.Expr{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if ds, ok := n.(*ast.DeclStmt); ok {
					if gd, ok := ds.Decl.(*ast.GenDecl); ok && gd.Tok == token.CONST {
						for _, sp := range gd.Specs {
							vs := sp.(*ast.ValueSpec)
							for i, id := range vs.Names {
								if i < len(vs.Values) {
									local[id.Name] = vs.Values[i]
								}
							}
						}
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.CompositeLit:
					sel, ok := v.Type.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "CooldownRecorded" {
						return true
					}
					for _, el := range v.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Reason" {
							continue
						}
						if s, ok := resolve(kv.Value, local, 0); ok {
							res.recorded[s] = true
							continue
						}
						id, isIdent := kv.Value.(*ast.Ident)
						audited := false
						if isIdent {
							for _, ex := range variableReasonExceptions {
								if ex.file == file && ex.ident == id.Name {
									audited = true
									for _, r := range ex.reasons {
										res.recorded[r] = true
									}
								}
							}
						}
						if !audited {
							res.problems = append(res.problems, fmt.Sprintf("%s:%d: cooldown Reason cannot be resolved to a constant; use a reason constant or add an audited exception", file, fset.Position(kv.Pos()).Line))
						}
					}
				case *ast.CallExpr:
					sel, ok := v.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "CooldownAt" || len(v.Args) != 1 {
						return true
					}
					if s, ok := resolve(v.Args[0], local, 0); ok {
						res.read[s] = true
					}
				}
				return true
			})
		}
	}
	return res
}

// checkCooldownClassification is the pure comparison: reasons found in code
// against the table. It returns one message per violation.
func checkCooldownClassification(recorded, read map[string]bool, table map[string]cooldownSpec) []string {
	var out []string
	for r := range recorded {
		if _, ok := table[r]; !ok {
			out = append(out, fmt.Sprintf("cooldown reason %q is recorded but has no classification in cooldownClasses (cooldown_class.go): classify it as short, timer, event-replaceable, safety-net or exempt", r))
		}
	}
	for r := range read {
		if _, ok := table[r]; !ok && !recorded[r] {
			out = append(out, fmt.Sprintf("cooldown reason %q is read through CooldownAt but never classified", r))
		}
	}
	for r, spec := range table {
		if !recorded[r] {
			out = append(out, fmt.Sprintf("cooldownClasses entry %q is never recorded by non-test engine code: remove the stale entry", r))
		}
		if spec.Class < cooldownShort || spec.Class > cooldownExempt {
			out = append(out, fmt.Sprintf("cooldownClasses entry %q has no valid class", r))
		}
		if spec.Evidence == "" {
			out = append(out, fmt.Sprintf("cooldownClasses entry %q has no evidence", r))
		}
		if spec.Class == cooldownEventReplaceable && len(spec.Events) == 0 {
			out = append(out, fmt.Sprintf("cooldownClasses entry %q is event-replaceable but names no replacing event", r))
		}
		if spec.Class != cooldownEventReplaceable && len(spec.Events) > 0 {
			out = append(out, fmt.Sprintf("cooldownClasses entry %q names replacing events but is not event-replaceable", r))
		}
	}
	sort.Strings(out)
	return out
}

func TestEveryCooldownReasonIsClassified(t *testing.T) {
	res := scanCooldownReasons(t)
	for _, p := range res.problems {
		t.Error(p)
	}
	if len(res.recorded) < 10 {
		t.Fatalf("scan found only %d recorded reasons (%v): the scan is broken", len(res.recorded), res.recorded)
	}
	for _, m := range checkCooldownClassification(res.recorded, res.read, cooldownClasses) {
		t.Error(m)
	}
	for _, ex := range variableReasonExceptions {
		for _, r := range ex.reasons {
			if _, ok := cooldownClasses[r]; !ok {
				t.Errorf("audited exception %s/%s lists unclassified reason %q", ex.file, ex.ident, r)
			}
		}
	}
}

// The comparison must catch an unclassified reason and a stale entry (a guard
// that cannot fail proves nothing).
func TestCooldownClassificationCheckCatchesViolations(t *testing.T) {
	table := map[string]cooldownSpec{
		"known":  {Class: cooldownShort, Evidence: "x"},
		"stale":  {Class: cooldownShort, Evidence: "x"},
		"badev":  {Class: cooldownEventReplaceable, Evidence: "x"},
		"noevid": {Class: cooldownTimer},
	}
	msgs := checkCooldownClassification(map[string]bool{"known": true, "new-reason": true, "badev": true, "noevid": true}, nil, table)
	joined := strings.Join(msgs, "\n")
	for _, want := range []string{`"new-reason" is recorded but has no classification`, `"stale" is never recorded`, `"badev" is event-replaceable but names no replacing event`, `"noevid" has no evidence`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing violation %q in:\n%s", want, joined)
		}
	}
	if got := checkCooldownClassification(map[string]bool{"known": true}, nil, map[string]cooldownSpec{"known": {Class: cooldownShort, Evidence: "x"}}); len(got) != 0 {
		t.Errorf("clean input produced violations: %v", got)
	}
}

// The derived sets keep today's membership plus the intended addition (#2532).
func TestCooldownDerivedSets(t *testing.T) {
	for _, r := range []string{reasonRetryBackoff, reasonSentinelLive, mergeUnsettledCooldownReason, lockWriteRetryReason, feedbackReadFailedReason, reasonFeedbackDispatchGap} {
		if !isShortCooldown(r) {
			t.Errorf("%s must be a short cooldown", r)
		}
	}
	for _, r := range []string{reasonPeriodicReEval, reasonDepBlocked, reasonReviewBlocked, reasonFeedbackPending, reasonSpawnResume, reasonSpawnRecovery, archiveEligibleAtCooldownReason} {
		if isShortCooldown(r) {
			t.Errorf("%s must not be a short cooldown", r)
		}
	}
	wantGate := map[string]bool{reasonFeedbackPending: true, feedbackReadFailedReason: true, reasonReviewBlocked: true, mergeUnsettledCooldownReason: true, reasonDepBlocked: true}
	if len(gateCooldowns) != len(wantGate) {
		t.Errorf("gateCooldowns = %v; want exactly %v", gateCooldowns, wantGate)
	}
	for r := range wantGate {
		if !gateCooldowns[r] {
			t.Errorf("%s must be a gate cooldown", r)
		}
	}
	if cooldownClassOf("no-such-reason") != 0 {
		t.Error("an unknown reason has no class")
	}
}
