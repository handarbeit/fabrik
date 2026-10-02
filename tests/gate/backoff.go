package gate

import (
	"bytes"
	"io"
	"os"
)

const backoffMarker = "activating rate-limit backoff"

// DetectRateLimitBackoff is run.sh's detect_rate_limit_backoff: true when the
// fabrik.log at path contains the engine's one-shot rate-limit-backoff
// activation line. A missing file is "no match", not an error (a bed that has
// not produced a log yet).
//
// It scans the WHOLE current file, not from a captured byte offset: each leg's
// bed restart launches a brand-new engine process, and engine/poll.go's Run()
// opens fabrik.log with O_TRUNC on every startup, so by the time the restart
// completes the file holds only this leg's own content. (An earlier bash
// version tracked a pre-restart offset that almost always exceeded the
// truncated file's size and silently detected nothing — #1547.)
//
// It matches the literal one-shot activation line — NOT the companion per-poll
// "...is low (...) consider reducing poll frequency" line, which fires on every
// poll while low and would over-trigger on a run that dipped briefly without
// ever crossing the 20% hysteresis-activation threshold.
func DetectRateLimitBackoff(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return readerContains(f, []byte(backoffMarker))
}

// readerContains reports whether r contains needle, scanning in chunks with an
// overlap so a match straddling a chunk boundary is still found.
func readerContains(r io.Reader, needle []byte) bool {
	buf := make([]byte, 0, 64*1024+len(needle))
	chunk := make([]byte, 64*1024)
	carry := 0
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf[:carry], chunk[:n]...)
			if bytes.Contains(buf, needle) {
				return true
			}
			carry = len(needle) - 1
			if carry > len(buf) {
				carry = len(buf)
			}
			copy(buf, buf[len(buf)-carry:])
		}
		if err != nil {
			return false
		}
	}
}

// fileContains is `grep -q needle file`.
func fileContains(path, needle string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return readerContains(f, []byte(needle))
}
