package hookdeck

import (
	"fmt"
	"os"
	"sync/atomic"
)

// logfHook holds the optional package-level logger hook, mirroring
// pruefer.Logf. hookdeck cannot import pruefer (pruefer imports hookdeck to
// construct Source, so the reverse would cycle), hence this package has its
// own hook rather than sharing pruefer's. A nil/unset hook falls back to
// stderr (e.g. in tests).
//
// Backed by atomic.Pointer rather than a plain var so SetLogf is safe to
// call concurrently with logf's reads from any Source goroutine (the read
// loop, the idle-ping ticker) — e.g. a future caller constructing more than
// one Source, or a test reassigning the hook mid-run, no longer risks a
// data race regardless of whether a given execution happens to interleave
// the write with a read.
var logfHook atomic.Pointer[func(format string, args ...any)]

// SetLogf installs the package-level logger hook. Safe to call at any time,
// concurrently with logf's reads.
func SetLogf(fn func(format string, args ...any)) {
	logfHook.Store(&fn)
}

// logf is the package-internal logging helper that fans out to the
// installed hook if set, otherwise stderr.
func logf(format string, args ...any) {
	if p := logfHook.Load(); p != nil {
		(*p)(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "[hookdeck] "+format, args...)
}
