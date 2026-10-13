package sim

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// R5 guard (#2210): a sim test file whose tests never call t.Parallel() runs
// serially, after every parallel test has finished, and that is how the package
// crept back to the CI per-package timeout — 73 of 88 files were parallel, the
// channel files were not, and nothing noticed. This scan fails `go test
// ./tests/sim` for any *_test.go file that declares a Test function but has no
// real t.Parallel() call, unless the file is on parallelExempt with a reason.
//
// The scan parses the source (a comment mentioning t.Parallel() does not count)
// and is file-granular by design: one parallel test in a file satisfies it. It
// also rejects a stale exemption (the file is gone, or has become parallel), so
// the list cannot rot. Modelled on the repo's other static scans, e.g.
// tests/e2e/registry's ScanPollSeamCallers and internal/workerenv's literal scan.

// parallelExempt lists the sim test files that are deliberately serial, each with
// the reason. Adding a file here needs a reason that names the process-global or
// the subprocess it cannot share; "slow" is not one — make it parallel.
var parallelExempt = map[string]string{
	"advance_until_test.go":          "re-execs the test binary as a subprocess and asserts on its captured output; the inner half must run alone in that subprocess",
	"main_guard_test.go":             "re-exec probe for the TestMain testlife guard (#2128): the inner half idles until the 1s ceiling kills the process, so it must be the only thing running in it",
	"display_field_warnings_test.go": "redirects the process-global warnings.WarningsPathOverride to read the panel file back; parallel tests would write theirs into it (see github_app_auth_test.go for the same constraint)",
}

// scanSerialSimFiles returns the *_test.go files in dir that declare a Test
// function, call no t.Parallel() and are not exempt, plus any problem with the
// exemption list itself (an empty reason, a missing file, a file that is in fact
// parallel).
func scanSerialSimFiles(dir string, exempt map[string]string) (serial, problems []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		seen[name] = true
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", name, perr)
		}
		hasTest, parallel := false, false
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if isTestFunc(x) {
					hasTest = true
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Parallel" && len(x.Args) == 0 {
					parallel = true
				}
			}
			return true
		})
		reason, isExempt := exempt[name]
		switch {
		case isExempt && strings.TrimSpace(reason) == "":
			problems = append(problems, name+": exemption has no reason")
		case isExempt && parallel:
			problems = append(problems, name+": exempt but calls t.Parallel(); remove it from parallelExempt")
		case !hasTest && !isExempt:
			// Helpers only (env.go-style files are not _test.go; a _test.go with no
			// tests, e.g. a shared helper file, has nothing to run serially).
		case hasTest && !parallel && !isExempt:
			serial = append(serial, name)
		}
	}
	for name := range exempt {
		if !seen[name] {
			problems = append(problems, name+": exempt but the file does not exist")
		}
	}
	sort.Strings(serial)
	sort.Strings(problems)
	return serial, problems, nil
}

// isTestFunc reports whether fd is `func TestXxx(t *testing.T)`; TestMain takes
// *testing.M and is not a test.
func isTestFunc(fd *ast.FuncDecl) bool {
	if fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "Test") || fd.Type.Params == nil || len(fd.Type.Params.List) != 1 {
		return false
	}
	star, ok := fd.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "T"
}

// TestEverySimTestFileIsParallelOrExempt is the guard on the real package.
func TestEverySimTestFileIsParallelOrExempt(t *testing.T) {
	t.Parallel()
	serial, problems, err := scanSerialSimFiles(".", parallelExempt)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	if len(serial) > 0 {
		t.Errorf("sim test file(s) with no t.Parallel(): %s\n"+
			"A serial test runs after every parallel one has finished and pushes tests/sim toward the CI timeout.\n"+
			"Call t.Parallel() (channel seams are per-Engine, see channel_settings.go), or add the file to parallelExempt with the process-global it needs.",
			strings.Join(serial, ", "))
	}
}

// The guard must fail on a serial file, pass on a parallel one, honour a reasoned
// exemption and reject a bad one — against fixtures, so it cannot pass vacuously.
func TestParallelGuardFixtures(t *testing.T) {
	t.Parallel()
	write := func(dir, name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const serialSrc = "package sim\nimport \"testing\"\nfunc TestSerial(t *testing.T) {}\n"
	const parallelSrc = "package sim\nimport \"testing\"\nfunc TestPar(t *testing.T) { t.Parallel() }\n"
	// A comment (or string) that merely mentions t.Parallel() must not satisfy it.
	const commentOnlySrc = "package sim\nimport \"testing\"\n// t.Parallel() is mentioned here only.\nfunc TestComment(t *testing.T) { _ = \"t.Parallel()\" }\n"
	const mainOnlySrc = "package sim\nimport \"testing\"\nfunc TestMain(m *testing.M) { m.Run() }\n"
	const subtestSrc = "package sim\nimport \"testing\"\nfunc TestSub(t *testing.T) { t.Run(\"a\", func(t *testing.T) { t.Parallel() }) }\n"

	t.Run("serial file fails and is named", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "serial_test.go", serialSrc)
		write(dir, "ok_test.go", parallelSrc)
		serial, problems, err := scanSerialSimFiles(dir, nil)
		if err != nil || len(problems) != 0 {
			t.Fatalf("err=%v problems=%v", err, problems)
		}
		if len(serial) != 1 || serial[0] != "serial_test.go" {
			t.Fatalf("serial = %v, want [serial_test.go]", serial)
		}
	})
	t.Run("a comment or string is not a call", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "comment_test.go", commentOnlySrc)
		serial, _, err := scanSerialSimFiles(dir, nil)
		if err != nil || len(serial) != 1 {
			t.Fatalf("serial = %v err=%v, want the comment-only file flagged", serial, err)
		}
	})
	t.Run("TestMain only and subtest Parallel", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "main_test.go", mainOnlySrc)
		write(dir, "sub_test.go", subtestSrc)
		serial, problems, err := scanSerialSimFiles(dir, nil)
		if err != nil || len(serial) != 0 || len(problems) != 0 {
			t.Fatalf("serial=%v problems=%v err=%v", serial, problems, err)
		}
	})
	t.Run("a reasoned exemption passes", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "serial_test.go", serialSrc)
		serial, problems, err := scanSerialSimFiles(dir, map[string]string{"serial_test.go": "needs a process-global"})
		if err != nil || len(serial) != 0 || len(problems) != 0 {
			t.Fatalf("serial=%v problems=%v err=%v", serial, problems, err)
		}
	})
	t.Run("bad exemptions are reported", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "serial_test.go", serialSrc)
		write(dir, "par_test.go", parallelSrc)
		_, problems, err := scanSerialSimFiles(dir, map[string]string{
			"serial_test.go": " ",
			"par_test.go":    "stale",
			"gone_test.go":   "no such file",
		})
		if err != nil || len(problems) != 3 {
			t.Fatalf("problems = %v err=%v, want 3", problems, err)
		}
	})
	t.Run("every real exemption has a reason", func(t *testing.T) {
		for f, r := range parallelExempt {
			if strings.TrimSpace(r) == "" {
				t.Errorf("%s: empty reason", f)
			}
		}
	})
}
