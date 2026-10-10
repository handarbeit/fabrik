package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// engineLogBackups is how many previous runs' logs are kept as
// fabrik.log.1 … fabrik.log.N (#2094). Deliberately a constant, not a config
// key: nobody asked for a knob, and Pruefer's own log rotation does the same.
const engineLogBackups = 5

// rotateEngineLog preserves the previous run's log before a new run opens a
// fresh one (#2094): path.(keep-1) → path.keep … path → path.1, discarding the
// oldest. It runs once per process, under the instance lock and before any
// logger holds the file, so there is nothing to coordinate with.
//
// Every failure is non-fatal: a missing slot is skipped silently (a first start
// or a gap in the chain), any other error is returned as a warning and the
// remaining shifts still run. truncateFallback is true only when the final
// path → path.1 rename failed while path still exists; the caller then opens
// with O_TRUNC, which is today's behaviour and keeps the log one file per run.
func rotateEngineLog(path string, keep int) (warnings []string, truncateFallback bool) {
	if keep < 1 {
		return nil, false
	}
	slot := func(n int) string { return fmt.Sprintf("%s.%d", path, n) }
	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	if err := os.Remove(slot(keep)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		warn("could not remove oldest backup %s: %v", slot(keep), err)
	}
	for k := keep - 1; k >= 1; k-- {
		if err := os.Rename(slot(k), slot(k+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			warn("could not shift %s to %s: %v", slot(k), slot(k+1), err)
		}
	}
	if err := os.Rename(path, slot(1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		warn("could not rotate %s to %s: %v (truncating instead)", path, slot(1), err)
		truncateFallback = true
	}
	return warnings, truncateFallback
}

// startBanner is the single first line of each run's fabrik.log (#2094): a
// timestamped [startup] line (same prefix convention as logEvent) carrying the
// version (which already holds dev(<sha>) and +dirty for source builds), the
// PID and why the process started. The line's own timestamp is the start time:
// PID alone does not tell runs apart because the SIGHUP and self-upgrade execs
// keep it, so the start time does.
func startBanner(now time.Time, version string, pid int, reason string) string {
	if reason == "" {
		reason = "unknown"
	}
	return fmt.Sprintf("%s [startup] fabrik %s pid=%d reason=%s\n", now.UTC().Format(time.RFC3339), version, pid, reason)
}
