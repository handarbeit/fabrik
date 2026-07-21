package boardcache

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDeltaHealPaths_DoNotCallStoreWhileLocked statically enforces the
// CacheImpl struct-doc invariant (boardcache.go: "NEVER hold mu while calling
// any Store method — this prevents deadlock if Store observers call back
// into CacheImpl"). Store.Get is read-only today, so no test can trigger an
// actual deadlock; this test instead parses delta.go's own source and walks
// each of the four auto-heal functions' control flow, tracking whether c.mu
// is held at each c.store.* call site. A plain textual scan is not enough
// here — these functions use "Lock(); if cond { ...; Unlock(); return };
// <store call>" shapes, where an early-return branch's Unlock must not be
// mistaken for the one guarding the store call that follows it.
func TestDeltaHealPaths_DoNotCallStoreWhileLocked(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "delta.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing delta.go: %v", err)
	}

	funcNames := []string{
		"applyPullRequestDelta",
		"applyPullRequestReviewDelta",
		"applyPullRequestReviewCommentDelta",
		"applyCheckRunDelta",
		"resolveOrHealPRLinkage",
	}

	for _, name := range funcNames {
		t.Run(name, func(t *testing.T) {
			fn := findFuncDecl(file, name)
			if fn == nil {
				t.Fatalf("could not locate function %s in delta.go", name)
			}
			var violations []string
			checkLockInvariant(fset, fn.Body.List, false, &violations)
			for _, v := range violations {
				t.Error(v)
			}
		})
	}
}

func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		if fn.Recv == nil {
			continue
		}
		return fn
	}
	return nil
}

// checkLockInvariant walks stmts in sequence, tracking whether c.mu is held
// (starting from the given locked state), and records a violation for every
// c.store.* call reached while locked. It returns the locked state after the
// last statement and whether the sequence unconditionally terminates (via a
// return reached on every path), so callers (if-statement handling) can tell
// whether a branch's ending lock state propagates to code after the branch.
func checkLockInvariant(fset *token.FileSet, stmts []ast.Stmt, locked bool, violations *[]string) (finalLocked, terminates bool) {
	for _, stmt := range stmts {
		switch s := stmt.(type) {
		case *ast.ReturnStmt:
			return locked, true

		case *ast.IfStmt:
			if s.Init != nil {
				recordStoreCallViolations(fset, s.Init, locked, violations)
			}
			bodyLocked, bodyTerm := checkLockInvariant(fset, s.Body.List, locked, violations)
			elseLocked, elseTerm := locked, false
			if s.Else != nil {
				switch e := s.Else.(type) {
				case *ast.BlockStmt:
					elseLocked, elseTerm = checkLockInvariant(fset, e.List, locked, violations)
				case *ast.IfStmt:
					elseLocked, elseTerm = checkLockInvariant(fset, []ast.Stmt{e}, locked, violations)
				}
			}
			switch {
			case bodyTerm && elseTerm:
				return locked, true
			case bodyTerm && !elseTerm:
				locked = elseLocked
			case !bodyTerm && elseTerm:
				locked = bodyLocked
			default:
				// Neither branch terminates. Conservatively treat the merge
				// point as locked if either branch left it locked, so a
				// disagreement is surfaced as violations downstream rather
				// than silently swallowed.
				locked = bodyLocked || elseLocked
			}

		case *ast.BlockStmt:
			l, term := checkLockInvariant(fset, s.List, locked, violations)
			if term {
				return l, true
			}
			locked = l

		default:
			if name, ok := muCallName(s); ok {
				switch name {
				case "Lock":
					locked = true
				case "Unlock":
					locked = false
				}
				continue
			}
			recordStoreCallViolations(fset, stmt, locked, violations)
		}
	}
	return locked, false
}

// muCallName reports the method name if stmt is a bare `c.mu.<Method>()` call.
func muCallName(stmt ast.Stmt) (string, bool) {
	es, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return "", false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	recv, ok := sel.X.(*ast.SelectorExpr)
	if !ok || recv.Sel.Name != "mu" {
		return "", false
	}
	return sel.Sel.Name, true
}

// recordStoreCallViolations scans node for any c.store.<Method>(...) call and
// records a violation if locked is true. It is used both for whole statements
// and for if-statement init clauses (which always execute regardless of the
// branch taken).
func recordStoreCallViolations(fset *token.FileSet, node ast.Node, locked bool, violations *[]string) {
	if !locked {
		return
	}
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.SelectorExpr)
		if !ok || recv.Sel.Name != "store" {
			return true
		}
		pos := fset.Position(call.Pos())
		*violations = append(*violations, fmt.Sprintf(
			"%s:%d: calls c.store.%s while c.mu is held (violates boardcache.go CacheImpl struct-doc invariant)",
			pos.Filename, pos.Line, sel.Sel.Name))
		return true
	})
}
