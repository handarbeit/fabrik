package pruefer

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Defaults for pruefer.log's size-triggered rotation. The only concrete data
// point on record (an operator-created pruefer.out.log reaching 2.1 MB under
// nohup) makes 10 MB comfortable headroom; these are unexported constants
// rather than new config keys since R2 (issue #1428) mandates only log_file
// itself.
const (
	logRotateMaxBytes = 10 * 1024 * 1024 // 10 MB
	logRotateBackups  = 3
)

// fileLogger is a mutex-serialized, size-rotating writer that backs
// pruefer.Logf, mirroring engine.Engine's logFile/logMu pattern. A single
// mutex spans the rotation check, the rotation itself, the file write, and
// the optional stderr tee, so a rotation triggered mid-write by one
// goroutine can never interleave with another goroutine's write (R4).
type fileLogger struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	maxBytes int64
	backups  int
	tee      bool
}

// newFileLogger opens (creating and appending to, never truncating — a
// daemon restart must not erase the history an incident investigation
// needs) the log file at path, creating its parent directory if necessary.
// tee controls whether every write is also copied to os.Stderr (R5: additive
// in plain daemon mode, file-only in TUI mode since stderr output would
// corrupt the bubbletea display).
func newFileLogger(path string, maxBytes int64, backups int, tee bool) (*fileLogger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("creating log directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening log file %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		if cerr := f.Close(); cerr != nil {
			return nil, fmt.Errorf("stat log file %s: %w (also failed to close: %v)", path, err, cerr)
		}
		return nil, fmt.Errorf("stat log file %s: %w", path, err)
	}
	return &fileLogger{
		path:     path,
		file:     f,
		size:     info.Size(),
		maxBytes: maxBytes,
		backups:  backups,
		tee:      tee,
	}, nil
}

// Logf formats and writes a single log line, matching engine's timestamped
// format: an RFC3339 UTC timestamp, then the exact "[pr#%d %s] "+format
// construction pruefer/log.go's stderr fallback already produces (zero
// wording drift versus today's output — Out-of-scope forbids changing tags
// or wording). Suitable for direct assignment to the package-level Logf hook.
func (fl *fileLogger) Logf(prNumber int, tag, format string, args ...any) {
	msg := fmt.Sprintf("[pr#%d %s] "+format, append([]any{prNumber, tag}, args...)...)
	ts := time.Now().UTC().Format(time.RFC3339)
	line := ts + " " + msg

	fl.mu.Lock()
	defer fl.mu.Unlock()

	if fl.maxBytes > 0 && fl.size+int64(len(line)) > fl.maxBytes {
		if err := fl.rotateLocked(); err != nil && fl.tee {
			// Gated on fl.tee, matching every other stderr write in this
			// method — in TUI mode (tee=false) a raw stderr write here would
			// corrupt the bubbletea display exactly as R5 exists to prevent.
			fmt.Fprintf(os.Stderr, "pruefer: log rotation failed for %s: %v\n", fl.path, err)
		}
	}

	if n, err := fl.file.WriteString(line); err == nil {
		fl.size += int64(n)
	}
	if fl.tee {
		os.Stderr.WriteString(line)
	}
}

// rotateLocked shifts existing numbered backups (path.1 -> path.2, ...,
// dropping the oldest beyond fl.backups), renames the current file to
// path.1, and reopens a fresh empty file at path. Must be called with fl.mu
// held.
//
// Backup shifting happens before fl.file is closed, since it touches only
// the numbered *.N siblings, never fl.path itself — a failure there leaves
// fl.file untouched and still perfectly usable. Every failure from the
// Close() below onward — closing fl.file, renaming/removing fl.path itself,
// or the reopen after it — always leaves fl.file reopened and appendable
// with fl.size resynced via Stat: without this, a caller that left fl.file
// closed (or worse, left it as a handle that already failed to close, which
// must not be used further) on error would silently drop every subsequent
// WriteString forever (its error is deliberately ignored in Logf), and with
// fl.size never advancing, every future call would re-attempt — and re-fail
// — the same already-closed Close(), for the rest of the process's life.
func (fl *fileLogger) rotateLocked() error {
	oldest := fmt.Sprintf("%s.%d", fl.path, fl.backups)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing oldest backup %s: %w", oldest, err)
	}
	for i := fl.backups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", fl.path, i)
		dst := fmt.Sprintf("%s.%d", fl.path, i+1)
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("renaming %s to %s: %w", src, dst, err)
		}
	}

	if err := fl.file.Close(); err != nil {
		// fl.path hasn't been touched yet at this point (the rename below is
		// what rotates it), so reopening it in place — the same recovery the
		// later failure paths use — gives back a live, appendable handle
		// instead of leaving fl.file as one that already failed to close.
		closeErr := fmt.Errorf("closing %s before rotation: %w", fl.path, err)
		if f, reopenErr := os.OpenFile(fl.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); reopenErr == nil {
			fl.file = f
			if info, statErr := f.Stat(); statErr == nil {
				fl.size = info.Size()
			}
		}
		return closeErr
	}

	var rotateErr error
	if fl.backups >= 1 {
		dst := fmt.Sprintf("%s.1", fl.path)
		if err := os.Rename(fl.path, dst); err != nil && !os.IsNotExist(err) {
			rotateErr = fmt.Errorf("renaming %s to %s: %w", fl.path, dst, err)
		}
	} else if err := os.Remove(fl.path); err != nil && !os.IsNotExist(err) {
		rotateErr = fmt.Errorf("removing %s: %w", fl.path, err)
	}

	f, err := os.OpenFile(fl.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		// fl.file is left as the prior (closed) handle here — there is no
		// file left to reopen it to, so there is nothing to recover into.
		return fmt.Errorf("reopening %s after rotation: %w", fl.path, err)
	}
	fl.file = f

	if rotateErr != nil {
		// The rename/remove of fl.path itself failed, so the reopened
		// handle points at the still-oversized, unrotated file rather than
		// a fresh one — resync fl.size to its real size (instead of
		// resetting to 0) so the next call correctly re-attempts rotation
		// rather than assuming it already succeeded.
		if info, statErr := f.Stat(); statErr == nil {
			fl.size = info.Size()
		}
		return rotateErr
	}

	fl.size = 0
	return nil
}

// Close closes the underlying file. Safe to call once during daemon
// shutdown.
func (fl *fileLogger) Close() error {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.file.Close()
}
