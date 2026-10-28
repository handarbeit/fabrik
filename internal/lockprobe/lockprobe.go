// Package lockprobe answers "does a live daemon hold this Fabrik directory?"
// without touching the lock (#2458, ADR 2458).
//
// The daemon takes an exclusive flock on <dir>/.fabrik/fabrik.lock for the whole
// of Run() and writes its PID into the file. The flock is the evidence of a live
// holder (the OS drops it when the process dies); the recorded PID is only a
// label. The probe is read-only: it never creates, writes, truncates or unlinks
// the file, and it holds a shared lock only for an instant. tests/gate's
// osProcs.LockHeld uses the same technique.
package lockprobe

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Result is the outcome of a probe.
type Result struct {
	// Held is true only on positive evidence of a live holder.
	Held bool
	// PID is the PID recorded in the lock file, 0 when unknown (the file may be
	// briefly empty right after the daemon takes the flock).
	PID int
}

// pidReadAttempts and pidReadDelay bound the wait for the holder to write its PID.
// Variables so tests can shorten them.
var (
	pidReadAttempts = 6
	pidReadDelay    = 100 * time.Millisecond
)

// Path returns the per-directory instance lock path for dir.
func Path(dir string) string {
	return filepath.Join(dir, ".fabrik", "fabrik.lock")
}

// Probe reports whether a live process holds dir's instance lock. A missing or
// unreadable lock file, a stale file with no holder, or an unsupported platform
// all report not held (fail open); no file is ever created.
func Probe(dir string) Result {
	path := Path(dir)
	if !flockHeld(path) {
		return Result{}
	}
	return Result{Held: true, PID: readPID(path)}
}

// readPID reads the label PID from the lock file, retrying while it is empty.
func readPID(path string) int {
	for i := 0; i < pidReadAttempts; i++ {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		if i < pidReadAttempts-1 {
			time.Sleep(pidReadDelay)
		}
	}
	return 0
}
