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
