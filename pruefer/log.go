package pruefer

import (
	"fmt"
	"os"
)

// Logf is an optional package-level logger hook, mirroring github.Logf and
// engine's claudeLogf. When set (by Daemon during construction), Pruefer's
// internal log lines are routed here instead of stderr, so they can land in
// a structured log file in Fabrik's style. nil falls back to stderr (e.g. in
// tests).
var Logf func(prNumber int, tag, format string, args ...any)

// logf is the package-internal logging helper that fans out to Logf if set,
// otherwise stderr. prNumber == 0 for messages not scoped to a specific PR.
func logf(prNumber int, tag, format string, args ...any) {
	if Logf != nil {
		Logf(prNumber, tag, format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "[pr#%d %s] "+format, append([]any{prNumber, tag}, args...)...)
}
