package hookdeck

import (
	"fmt"
	"os"
)

// Logf is an optional package-level logger hook, mirroring pruefer.Logf.
// hookdeck cannot import pruefer (pruefer imports hookdeck to construct
// Source, so the reverse would cycle), hence this package has its own hook
// rather than sharing pruefer's. nil falls back to stderr (e.g. in tests).
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
