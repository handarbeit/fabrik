package selfupgrade

import (
	"os"
	"syscall"
)

// execFn is a package-level seam over syscall.Exec so tests can exercise the
// re-exec-failure path without replacing the test process itself. Production
// code leaves this as syscall.Exec; tests may swap it in and must restore it
// afterward.
var execFn = syscall.Exec

// executableFn resolves the path to the currently-running executable. A seam
// over os.Executable so tests exercising the full download-extract-replace
// or rebuild paths can point it at a scratch file instead of replacing the
// real go test binary. Production code leaves this as os.Executable; symlink
// resolution still happens separately via filepath.EvalSymlinks after the
// call.
var executableFn = os.Executable

// originURLFn reads a checkout's origin remote URL. A seam over
// `git remote get-url origin` so tests that need a real local clone (whose
// origin is a filesystem path and can never satisfy the github.com host check)
// can present a github.com URL instead. Production code leaves this as
// gitOriginURL.
var originURLFn = gitOriginURL

// SetOriginURLFnForTest replaces the origin-URL reader and returns a function
// that restores the previous one. It exists for tests in other packages that
// run the real CompareDevBuild against a local clone.
func SetOriginURLFnForTest(fn func(dir string) (string, error)) (restore func()) {
	prev := originURLFn
	originURLFn = fn
	return func() { originURLFn = prev }
}
