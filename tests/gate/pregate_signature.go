package gate

import (
	"bytes"
	"regexp"
	"sync"
)

// The pre-gate's known, transient crash (#1973 R5; background in ADR-1624 and
// ADR-1677): under host load the `-race` sim suite dies from ThreadSanitizer's
// fork/exec handling — either the TSan runtime aborts (`CHECK failed:
// tsan_rtl.cpp:94`, the child exiting 66) or a child `git` is killed outright
// (`signal: segmentation fault`). It is not an engine verdict, so the gate
// retries the failing pre-gate step ONCE.
//
// The signature is deliberately narrow and fails CLOSED: anything unrecognised is
// a hard stop exactly as before. The retry can also never turn a genuine failure
// green — the step has to PASS in full on the second run — so the cost of a
// false match is one wasted sim run, never a false verdict.
//
// Matching is per line, so the output is scanned as it streams (crashScanner)
// rather than buffered: the sim suite is verbose and nothing here needs it all.
var (
	// A TSan runtime abort: the CHECK failure text, on a line that names the
	// sanitizer or its runtime source file.
	tsanAbortRE = regexp.MustCompile(`ThreadSanitizer.*CHECK failed|CHECK failed:.*tsan_\w+\.cpp`)
	// A child git killed by SIGSEGV: Go's os/exec renders that as "signal:
	// segmentation fault" on the child's error. The line must name git — a
	// segfault of any other process, or Go's own "[signal SIGSEGV: segmentation
	// violation]" (an engine crash), is NOT this crash.
	gitSegfaultRE = regexp.MustCompile(`(?i)\bgit\b.*signal: segmentation fault`)

	// Evidence of a REAL failure. Any one of these on any line vetoes the retry,
	// whatever else matched: an engine panic, a Go runtime fatal error, a
	// data-race report (an engine verdict) or a goroutine dump.
	pregateVetoRE = regexp.MustCompile(`^(?:panic: |fatal error: |WARNING: DATA RACE|goroutine \d+ \[)|\[signal SIGSEGV`)
)

// maxScanLine bounds a single line held while scanning; a longer one is scanned
// as the prefix seen so far and the remainder as a new line.
const maxScanLine = 1 << 20

// crashScanner is an io.Writer that classifies a failed pre-gate step's output
// line by line, holding at most one partial line.
type crashScanner struct {
	mu            sync.Mutex
	partial       []byte
	veto          bool
	tsan, gitSegv bool
}

func (c *crashScanner) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.partial = append(c.partial, p...)
	for {
		i := bytes.IndexByte(c.partial, '\n')
		if i < 0 {
			break
		}
		c.line(c.partial[:i])
		c.partial = c.partial[i+1:]
	}
	if len(c.partial) > maxScanLine {
		c.line(c.partial)
		c.partial = nil
	}
	return len(p), nil
}

func (c *crashScanner) line(b []byte) {
	if pregateVetoRE.Match(b) {
		c.veto = true
	}
	if tsanAbortRE.Match(b) {
		c.tsan = true
	}
	if gitSegfaultRE.Match(b) {
		c.gitSegv = true
	}
}

// finish scans a final line that had no newline.
func (c *crashScanner) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.partial) > 0 {
		c.line(c.partial)
		c.partial = nil
	}
}

// IsCrash is true when the signature matched and nothing vetoed it.
func (c *crashScanner) IsCrash() bool {
	c.finish()
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.veto && (c.tsan || c.gitSegv)
}

// Signature names which form matched, for the ledger note.
func (c *crashScanner) Signature() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.tsan:
		return "tsan-abort"
	case c.gitSegv:
		return "git-segfault"
	}
	return ""
}

func scanOutput(output string) *crashScanner {
	c := &crashScanner{}
	c.Write([]byte(output))
	c.finish()
	return c
}

// IsTSanForkCrash reports whether a failed pre-gate step's output carries the
// known TSan fork/exec crash signature and nothing that vetoes it.
func IsTSanForkCrash(output string) bool { return scanOutput(output).IsCrash() }

// crashSignature names which form matched ("" when neither did).
func crashSignature(output string) string { return scanOutput(output).Signature() }
