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

// R5 guard (#2210): a sim test that never calls t.Parallel() runs serially,
// after every parallel test has finished, and that is how the package crept back
// to the CI per-package timeout — 73 of 88 files were parallel, the channel files
// were not, and nothing noticed. This scan fails `go test ./tests/sim` for any
// top-level Test function whose own body has no t.Parallel() call, unless the
// test (key "file.go:TestName") or its whole file (key "file.go") is on
// parallelExempt with a reason.
//
// The scan parses the source, so a comment mentioning t.Parallel() does not
// count, and it looks only at the Test function's own statements: a t.Parallel()
// inside a t.Run closure parallelises that subtest against its siblings but
// leaves the top-level test serial against the rest of the package, so it does
// not satisfy the guard. It also rejects a stale exemption (the file or test is
// gone, or has become parallel), so the list cannot rot. Modelled on the repo's
// other static scans, e.g. tests/e2e/registry's ScanPollSeamCallers and
// internal/workerenv's literal scan.

// parallelExempt lists the sim tests that are deliberately serial, each with the
// reason. A key is a file name (every test in it) or "file.go:TestName". Adding
// an entry needs a reason that names the process-global or the subprocess it
// cannot share; "slow" is not one — make it parallel.
var parallelExempt = map[string]string{
	"advance_until_test.go":          "re-execs the test binary as a subprocess and asserts on its captured output; the inner half must run alone in that subprocess",
	"main_guard_test.go":             "re-exec probe for the TestMain testlife guard (#2128): the inner half idles until the 1s ceiling kills the process, so it must be the only thing running in it",
	"forge_hold_test.go":             "redirects the process-global warnings.WarningsPathOverride to read the forge_unreachable panel entry back; parallel tests would write theirs into it (same constraint as display_field_warnings_test.go)",
	"display_field_warnings_test.go": "redirects the process-global warnings.WarningsPathOverride to read the panel file back; parallel tests would write theirs into it (see github_app_auth_test.go for the same constraint)",

	"channel_sighup_restart_socket_test.go": "binds the real local-API socket, and localapi.Server.Start serialises a process-wide umask of 0177 while it binds (internal/localapi); a parallel sibling creating its temp directories in that window gets them without the search bit and fails with permission denied (#2484)",

	"github_app_auth_test.go:TestGitHubAppAuth_DispatchAdmission_ExcludedRepoBlocksAndWarns": "points the process-global warnings.WarningsPathOverride at a temp file to read the panel back; every parallel sibling's first poll writes to that global (a -race-flagged data race), see the comment on the test",
}

// scanSerialSimTests returns the "file.go:TestName" keys in dir of Test
// functions with no t.Parallel() in their own body that are not exempt, plus any
// problem with the exemption list itself (an empty reason, a missing file or
// test, an entry that is in fact parallel).
func scanSerialSimTests(dir string, exempt map[string]string) (serial, problems []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	type testInfo struct {
		file, name string
		parallel   bool
	}
	var tests []testInfo
	files := map[string]bool{}
	fset := token.NewFileSet()
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		files[name] = true
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", name, perr)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || !isTestFunc(fd) || fd.Body == nil {
				continue
			}
			parallel := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncLit:
					return false // a subtest's t.Parallel() does not parallelise this test
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Parallel" && len(x.Args) == 0 {
						parallel = true
					}
				}
				return true
			})
			tests = append(tests, testInfo{name, fd.Name.Name, parallel})
		}
	}
	matched := map[string]bool{}
	for _, tc := range tests {
		key := tc.file + ":" + tc.name
		ek := ""
		if _, ok := exempt[key]; ok {
			ek = key
		} else if _, ok := exempt[tc.file]; ok {
			ek = tc.file
		}
		switch {
		case ek != "" && tc.parallel:
			matched[ek] = true
			problems = append(problems, key+": exempt but calls t.Parallel(); remove it from parallelExempt")
		case ek != "":
			matched[ek] = true
		case !tc.parallel:
			serial = append(serial, key)
		}
	}
	for ek, reason := range exempt {
		file, _, _ := strings.Cut(ek, ":")
		switch {
		case strings.TrimSpace(reason) == "":
			problems = append(problems, ek+": exemption has no reason")
		case !files[file]:
			problems = append(problems, ek+": exempt but the file does not exist")
		case !matched[ek]:
			problems = append(problems, ek+": exempt but matches no Test function")
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
	serial, problems, err := scanSerialSimTests(".", parallelExempt)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	if len(serial) > 0 {
		t.Errorf("sim test(s) with no t.Parallel() in their own body: %s\n"+
			"A serial test runs after every parallel one has finished and pushes tests/sim toward the CI timeout.\n"+
			"Call t.Parallel() (channel seams are per-Engine, see channel_settings.go), or add the test (\"file.go:TestName\") or its file to parallelExempt with the process-global it needs.",
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

	const mixedSrc = "package sim\nimport \"testing\"\nfunc TestA(t *testing.T) { t.Parallel() }\nfunc TestB(t *testing.T) {}\n"
	t.Run("one serial test in a parallel file fails and is named", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "mixed_test.go", mixedSrc)
		serial, problems, err := scanSerialSimTests(dir, nil)
		if err != nil || len(problems) != 0 || len(serial) != 1 || serial[0] != "mixed_test.go:TestB" {
			t.Fatalf("serial=%v problems=%v err=%v, want [mixed_test.go:TestB]", serial, problems, err)
		}
		serial, problems, err = scanSerialSimTests(dir, map[string]string{"mixed_test.go:TestB": "needs a global"})
		if err != nil || len(problems) != 0 || len(serial) != 0 {
			t.Fatalf("per-test exemption: serial=%v problems=%v err=%v", serial, problems, err)
		}
	})
	t.Run("serial file fails and is named", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "serial_test.go", serialSrc)
		write(dir, "ok_test.go", parallelSrc)
		serial, problems, err := scanSerialSimTests(dir, nil)
		if err != nil || len(problems) != 0 {
			t.Fatalf("err=%v problems=%v", err, problems)
		}
		if len(serial) != 1 || serial[0] != "serial_test.go:TestSerial" {
			t.Fatalf("serial = %v, want [serial_test.go:TestSerial]", serial)
		}
	})
	t.Run("a comment or string is not a call", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "comment_test.go", commentOnlySrc)
		serial, _, err := scanSerialSimTests(dir, nil)
		if err != nil || len(serial) != 1 {
			t.Fatalf("serial = %v err=%v, want the comment-only file flagged", serial, err)
		}
	})
	t.Run("TestMain only is not a test", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "main_test.go", mainOnlySrc)
		serial, problems, err := scanSerialSimTests(dir, nil)
		if err != nil || len(serial) != 0 || len(problems) != 0 {
			t.Fatalf("serial=%v problems=%v err=%v", serial, problems, err)
		}
	})
	t.Run("t.Parallel only inside a subtest leaves the test serial", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "sub_test.go", subtestSrc)
		serial, _, err := scanSerialSimTests(dir, nil)
		if err != nil || len(serial) != 1 || serial[0] != "sub_test.go:TestSub" {
			t.Fatalf("serial = %v err=%v, want [sub_test.go:TestSub]", serial, err)
		}
	})
	t.Run("a reasoned exemption passes", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "serial_test.go", serialSrc)
		serial, problems, err := scanSerialSimTests(dir, map[string]string{"serial_test.go": "needs a process-global"})
		if err != nil || len(serial) != 0 || len(problems) != 0 {
			t.Fatalf("serial=%v problems=%v err=%v", serial, problems, err)
		}
	})
	t.Run("bad exemptions are reported", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "serial_test.go", serialSrc)
		write(dir, "par_test.go", parallelSrc)
		_, problems, err := scanSerialSimTests(dir, map[string]string{
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
