package engine

// TestAddCommentCompliance verifies that every AddComment call site in the
// engine source passes a body that begins with the canonical "🏭 **Fabrik"
// header.  This ensures findNewComments' prefix dedup in comments.go catches
// all engine-generated comments and prevents Fabrik from processing its own
// output on the next poll.
//
// A body argument is compliant if and only if:
//   - It is a call to formatOutputComment or formatPRSummaryComment, OR
//   - It is a string literal whose value starts with "🏭 **Fabrik", OR
//   - It is a local variable where any assignment in the same function scope
//     is compliant (i.e. a fmt.Sprintf whose format string starts with
//     "🏭 **Fabrik", or a call to the canonical formatters).
//
// Test files (*_test.go) are excluded because mock AddComment implementations
// may accept arbitrary bodies.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddCommentCompliance(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no .go files found; test must run with CWD set to the engine package directory")
	}

	fset := token.NewFileSet()

	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}

		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			checkAddCommentBody(t, fset, fd.Body)
		}
	}
}

// checkAddCommentBody walks a function body, finds all AddComment calls, and
// reports any whose body argument is non-compliant.  It recurses into nested
// function literals with their own assignment context.
func checkAddCommentBody(t *testing.T, fset *token.FileSet, body *ast.BlockStmt) {
	t.Helper()

	// Collect all variable assignments within this function scope (excluding
	// nested function literals, which have their own scope).
	assigns := map[string][]ast.Expr{}
	collectVarAssignments(body, assigns)

	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncLit:
			// Recurse with a fresh assignment scope for the nested function.
			checkAddCommentBody(t, fset, v.Body)
			return false // prevent outer Inspect from also walking into this body

		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AddComment" {
				return true
			}
			// AddComment signature: (owner, repo string, issueNumber int, body string)
			// body is the 4th argument (index 3).
			if len(v.Args) < 4 {
				return true
			}
			bodyArg := v.Args[3]
			if !isCompliantAddCommentArg(bodyArg, assigns) {
				pos := fset.Position(v.Pos())
				t.Errorf(
					"non-compliant AddComment body at %s:%d — body must start with %q or go through formatOutputComment/formatPRSummaryComment",
					pos.Filename, pos.Line, "🏭 **Fabrik",
				)
			}
		}
		return true
	})
}

// collectVarAssignments collects all := and = assignments in a block into
// assigns, mapping each LHS identifier name to its RHS expressions.  It does
// not recurse into nested function literals.
func collectVarAssignments(body *ast.BlockStmt, assigns map[string][]ast.Expr) {
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		// Don't recurse into nested function literals.
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		stmt, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		// Handle two cases:
		//   1. Parallel assignment: a, b := x, y — each LHS maps to its own RHS.
		//   2. Multi-return call: a, b := f() — a single RHS maps to all LHS idents.
		for i, lhs := range stmt.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if i < len(stmt.Rhs) {
				assigns[ident.Name] = append(assigns[ident.Name], stmt.Rhs[i])
			} else if len(stmt.Rhs) == 1 {
				// Multi-return call: associate the single call with every LHS ident.
				assigns[ident.Name] = append(assigns[ident.Name], stmt.Rhs[0])
			}
		}
		return true
	})
}

// isCompliantAddCommentArg reports whether an AddComment body argument is
// compliant with the engine-wide comment header convention.
func isCompliantAddCommentArg(arg ast.Expr, assigns map[string][]ast.Expr) bool {
	switch e := arg.(type) {
	case *ast.BasicLit:
		return isFabrikHeaderLit(e)
	case *ast.CallExpr:
		return isCompliantCallExpr(e)
	case *ast.Ident:
		// Look up all assignments to this variable; compliant if any is compliant.
		for _, rhs := range assigns[e.Name] {
			if isCompliantRHS(rhs) {
				return true
			}
		}
		return false
	}
	return false
}

// isCompliantRHS checks whether an expression used as the RHS of an assignment
// is a compliant body source.
func isCompliantRHS(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return isFabrikHeaderLit(e)
	case *ast.CallExpr:
		return isCompliantCallExpr(e)
	}
	return false
}

// isCompliantCallExpr returns true for formatOutputComment, formatPRSummaryComment,
// or fmt.Sprintf whose first argument is a Fabrik-header literal (or a string
// concatenation expression whose leftmost literal is a Fabrik-header literal).
func isCompliantCallExpr(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == "formatOutputComment" || fn.Name == "formatPRSummaryComment"
	case *ast.SelectorExpr:
		// fmt.Sprintf("🏭 **Fabrik...", ...) — the first arg may be a plain literal
		// or a binary string concatenation expression ("..." + "...").
		pkg, ok := fn.X.(*ast.Ident)
		if ok && pkg.Name == "fmt" && fn.Sel.Name == "Sprintf" && len(call.Args) > 0 {
			if lit := leftmostStringLit(call.Args[0]); lit != nil {
				return isFabrikHeaderLit(lit)
			}
		}
	}
	return false
}

// leftmostStringLit returns the leftmost *ast.BasicLit (string kind) reachable
// via left-associative binary + expressions, or nil if expr is not a string
// literal chain.
func leftmostStringLit(expr ast.Expr) *ast.BasicLit {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return e
		}
	case *ast.BinaryExpr:
		return leftmostStringLit(e.X)
	}
	return nil
}

// isFabrikHeaderLit returns true when a string literal starts with the
// canonical engine comment prefix.  lit.Value is the raw Go source string,
// including surrounding double-quotes (e.g. `"🏭 **Fabrik — ..."`).
func isFabrikHeaderLit(lit *ast.BasicLit) bool {
	return lit.Kind == token.STRING && strings.HasPrefix(lit.Value, `"🏭 **Fabrik`)
}
