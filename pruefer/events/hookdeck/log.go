package hookdeck

import (
	"fmt"
	"os"
)

// Logf is an optional package-level logger hook, mirroring pruefer.Logf.
// hookdeck cannot import pruefer (pruefer imports hookdeck to construct
// Source, so the reverse would cycle), hence this package has its own hook
// rather than sharing pruefer's. nil falls back to stderr (e.g. in tests).
//
// Unsynchronized by design: callers must set it once, before starting any
// Source (execute.go does this at construction time), and never mutate it
// afterward. logf reads it from every Source goroutine (the read loop, the
// idle-ping ticker) with no lock — a write racing those reads (e.g. a second
// Source instance, or a test reassigning it mid-run) is a data race that
// `go test -race` will only catch if a run happens to interleave the write
// with a read.
var Logf func(format string, args ...any)

// logf is the package-internal logging helper that fans out to Logf if set,
// otherwise stderr.
func logf(format string, args ...any) {
	if Logf != nil {
		Logf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "[hookdeck] "+format, args...)
}
