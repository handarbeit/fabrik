package inconclusive

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The converted harness-race guards (#1973) sit in tests/e2e, behind the e2e
// build tag, so a neutralised-assertion check cannot run without a live bed. What
// CAN be pinned statically — go/parser ignores build tags — is the structure that
// keeps the conversion honest:
//
//   - each of the three guards the issue names ends the test through an
//     inconclusive helper;
//   - each of those tests still has assertion Fatalf/Errorf calls AFTER the guard
//     (the guard is a separate branch ahead of the assertions, never a replacement
//     for them — neutralise an assertion and the test still FAILS);
//   - Inconclusive is never called from a goroutine or a subtest, where it could
//     not end the (top-level) test the runner reads.
//
// A live neutralisation check is recorded manually in the PR.

// The awaitVisible family (#1974) ends a timed-out wait through Inconclusive, so
// its wrappers carry the same constraints as Inconclusive itself.
var inconclusiveHelpers = map[string]bool{
	"Inconclusive": true, "waitForLogMatchInconclusive": true, "failOrInconclusive": true,
	"finishAwait": true, "AwaitBoardItemVisible": true, "AwaitStatusVisible": true,
	"AwaitClosingLinkage": true, "AwaitPRMergeableComputed": true, "AwaitPRMergeableSettled": true,
	"AwaitLabelVisible": true, "AwaitPRForBranchVisible": true,
}

var namedGuards = []struct{ file, test string }{
	{"mergetrain_batchcap_test.go", "TestMergeTrainQueuedDeeperThanBatchCap"},
	{"mergetrain_coldbase_test.go", "TestMergeTrainColdCacheBaseMember"},
	{"comment_landing_gate_test.go", "TestCommentLandingGateHolds"},
}

func parseE2E(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", name), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return fset, f
}

func calleeName(c *ast.CallExpr) string {
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	return nil
}

func TestNamedGuardsUseInconclusiveAndKeepTheirAssertions(t *testing.T) {
	for _, g := range namedGuards {
		fset, f := parseE2E(t, g.file)
		fd := findFunc(f, g.test)
		if fd == nil {
			t.Fatalf("%s: %s not found", g.file, g.test)
		}
		var firstGuard token.Pos
		var assertionsAfter int
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && inconclusiveHelpers[calleeName(c)] && (firstGuard == 0 || c.Pos() < firstGuard) {
				firstGuard = c.Pos()
			}
			return true
		})
		if firstGuard == 0 {
			t.Errorf("%s: %s no longer ends through an inconclusive helper", g.file, g.test)
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && c.Pos() > firstGuard {
				switch calleeName(c) {
				case "Fatalf", "Errorf", "Fatal", "Error":
					assertionsAfter++
				}
			}
			return true
		})
		if assertionsAfter == 0 {
			t.Errorf("%s: %s has no Fatalf/Errorf after its inconclusive guard (%s) — the guard must precede, never replace, the assertions",
				g.file, g.test, fset.Position(firstGuard))
		}
	}
}

func TestInconclusiveIsNeverCalledFromAGoroutineOrSubtest(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no e2e sources found: %v", err)
	}
	for _, path := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var walk func(n ast.Node, bad string)
		walk = func(n ast.Node, bad string) {
			ast.Inspect(n, func(m ast.Node) bool {
				switch x := m.(type) {
				case *ast.GoStmt:
					if x != n {
						walk(x.Call, "a goroutine")
						return false
					}
				case *ast.CallExpr:
					if x != n && calleeName(x) == "Run" {
						for _, a := range x.Args {
							if fl, ok := a.(*ast.FuncLit); ok {
								walk(fl.Body, "a subtest")
							}
						}
					}
					if bad != "" && inconclusiveHelpers[calleeName(x)] {
						t.Errorf("%s: %s is called from %s — it must end a top-level test from the test's own goroutine",
							fset.Position(x.Pos()), calleeName(x), bad)
					}
				}
				return true
			})
		}
		walk(f, "")
	}
}

// seedPaths are the harness seed functions that hand an item to the engine. Since
// #1974 each waits for GitHub to reflect its writes through the awaitVisible
// family — and carries no wait loop of its own.
var seedPaths = []struct{ file, fn string }{
	{"mergetrain_helpers.go", "createMemberPR"},
	{"mergetrain_helpers.go", "QueueMember"},
	{"mergetrain_helpers.go", "QueueMemberOnBase"},
	{"mergetrain_helpers.go", "queueMemberPaused"},      // QueueMemberPaused / QueueMemberPausedOnBase (#1977)
	{"mergetrain_helpers.go", "prepareMemberExactPath"}, // PrepareMemberExactPath / PrepareMemberExactPathOnBase (#1977)
	{"review_authority_helpers.go", "seedReviewGateItemImpl"},
	{"comment_landing_helpers.go", "seedLandingCandidate"},
}

func TestSeedPathsUseTheAwaitFamilyAndKeepNoWaitLoopOfTheirOwn(t *testing.T) {
	for _, sp := range seedPaths {
		_, f := parseE2E(t, sp.file)
		fd := findFunc(f, sp.fn)
		if fd == nil {
			t.Fatalf("%s: %s not found", sp.file, sp.fn)
		}
		var awaits int
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ForStmt, *ast.RangeStmt:
				t.Errorf("%s: seed path %s has its own loop — waits go through the awaitVisible family (tests/e2e/awaitvisible)", sp.file, sp.fn)
			case *ast.CallExpr:
				switch name := calleeName(x); {
				case name == "Sleep" || name == "pollSleep":
					t.Errorf("%s: seed path %s sleeps itself — waits go through the awaitVisible family", sp.file, sp.fn)
				case inconclusiveHelpers[name] && strings.HasPrefix(name, "Await"):
					awaits++
				}
			}
			return true
		})
		if awaits == 0 {
			t.Errorf("%s: seed path %s never awaits visibility of its writes through the awaitVisible family", sp.file, sp.fn)
		}
	}
}

// The one-off waits #1974 folded into the family must not creep back as parallel copies.
func TestFoldedOneOffWaitsAreGone(t *testing.T) {
	for _, file := range []string{"harness.go", "review_authority_helpers.go", "mergetrain_helpers.go"} {
		_, f := parseE2E(t, file)
		for _, gone := range []string{"waitForClosingLinkage", "WaitForPRMergeableSettled"} {
			if findFunc(f, gone) != nil {
				t.Errorf("%s: %s is back — use the awaitVisible family (AwaitClosingLinkage / AwaitPRMergeableSettled)", file, gone)
			}
		}
	}
}
