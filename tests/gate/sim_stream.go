package gate

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/testlife"
)

// The sim pre-gate's per-test record (#2247, ADR-2247). The sim runs through
// `go test -json`; simStreamWriter decodes that stream and feeds
//
//   - simTracker: which top-level tests are running, since when, and how each
//     ended (R1, R2, R4, R7);
//   - the archive: the raw stream, a rendered timeline and the dump (R6);
//   - crashScanner: the DECODED output text, never the raw JSON (every JSON line
//     begins `{"Time"`, which would defeat the scanner's line-anchored veto and
//     let a genuine panic or race report be retried as a TSan crash);
//   - the terminal, which shows far less than the old firehose: non-JSON lines,
//     package-level output, and the buffered output of a test only when it fails.

// simRunning is one test the tracker believes is executing.
type simRunning struct {
	Test    string
	Elapsed time.Duration
}

// simTracker follows top-level tests through run/pause/cont/terminal events. Time
// is the OBSERVATION clock (the gate's Now), not the event's own Time: the
// heartbeat measures how long the gate has been waiting, and a fake Commander's
// recorded stream has no meaningful event times. A test's clock restarts on
// `cont` — a t.Parallel test queued behind -parallel is not running.
type simTracker struct {
	mu      sync.Mutex
	now     func() time.Time
	begin   time.Time
	running map[string]time.Time
	queued  map[string]bool
	events  []Event // every non-output event, for Timings/Classify
}

func newSimTracker(now func() time.Time) *simTracker {
	return &simTracker{now: now, begin: now(), running: map[string]time.Time{}, queued: map[string]bool{}}
}

func (t *simTracker) observe(e Event) {
	if e.Action == "output" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, e)
	if !topLevel(e) {
		return
	}
	switch e.Action {
	case "run":
		delete(t.queued, e.Test)
		if _, ok := t.running[e.Test]; !ok {
			t.running[e.Test] = t.now()
		}
	case "cont":
		delete(t.queued, e.Test)
		t.running[e.Test] = t.now()
	case "pause":
		delete(t.running, e.Test)
		t.queued[e.Test] = true
	case "pass", "fail", "skip":
		delete(t.running, e.Test)
		delete(t.queued, e.Test)
	}
}

// runningNow is the executing tests, longest-running first.
func (t *simTracker) runningNow() []simRunning {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	out := make([]simRunning, 0, len(t.running))
	for name, since := range t.running {
		out = append(out, simRunning{Test: name, Elapsed: now.Sub(since)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Elapsed != out[j].Elapsed {
			return out[i].Elapsed > out[j].Elapsed
		}
		return out[i].Test < out[j].Test
	})
	return out
}

func (t *simTracker) queuedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.queued)
}

func (t *simTracker) elapsed() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.now().Sub(t.begin)
}

func (t *simTracker) snapshot() []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Event(nil), t.events...)
}

// Caps on what a failing test's buffered output may add to the terminal.
const (
	simPendingMaxLines = 300
	simPendingMaxBytes = 128 << 10
	simDumpTermLines   = 1 // lines of a goroutine dump the terminal shows (its header); the file has all
)

var dumpWrittenRE = regexp.MustCompile(`goroutine dump written to (.+?)\s*$`)

// simDumpState is one package's goroutine dump in progress. A dump is
// thousands of lines the terminal shows only the header of; it is scoped to the
// package that wrote it (several test binaries run at once) and ends at the
// first line that is not dump text, so the FAIL lines that follow it, and any
// other binary's output, are never swallowed.
type simDumpState struct {
	shown  int
	hidden int
}

// dumpEndRE matches the lines a package prints after its dump: test2json's
// package result ("FAIL\tpkg\t12s", "ok ..."), the go tool's "exit status N"
// and a test's own "--- FAIL". Goroutine-dump text never starts with these.
var dumpEndRE = regexp.MustCompile(`^(FAIL|ok|PASS|exit status |--- )`)

type simPending struct {
	lines   []string
	bytes   int
	dropped int
}

// simStreamWriter consumes the sim run's combined stdout+stderr.
type simStreamWriter struct {
	tr   *simTracker
	scan *crashScanner
	term io.Writer
	raw  io.Writer // <head>.sim.jsonl: every byte as received
	log  io.Writer // <head>.sim.log: the rendered timeline

	mu          sync.Mutex
	buf         []byte
	pending     map[string]*simPending
	openDumps   map[string]*simDumpState // by package: only the binary that dumped has its output folded
	ceilingLine string
	dumpPaths   []string // one per hung test binary: each writes its own PID-named file
}

func newSimStreamWriter(tr *simTracker, scan *crashScanner, term, raw, log io.Writer) *simStreamWriter {
	return &simStreamWriter{tr: tr, scan: scan, term: term, raw: raw, log: log, pending: map[string]*simPending{}}
}

func (w *simStreamWriter) Write(p []byte) (int, error) {
	if w.raw != nil {
		_, _ = w.raw.Write(p) // an archive failure never changes the verdict
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Flush handles a final line with no newline and reports a hidden dump tail.
func (w *simStreamWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = nil
	}
	for pkg := range w.openDumps {
		w.endDump(pkg)
	}
}

// endDump closes pkg's dump, reporting how much of it only the archive holds.
func (w *simStreamWriter) endDump(pkg string) {
	d := w.openDumps[pkg]
	if d == nil {
		return
	}
	if d.hidden > 0 {
		fmt.Fprintf(w.term, "   … %d more goroutine-dump lines in the archive\n", d.hidden)
	}
	delete(w.openDumps, pkg)
}

func (w *simStreamWriter) render(format string, args ...any) {
	if w.log != nil {
		fmt.Fprintf(w.log, format, args...)
	}
}

func (w *simStreamWriter) line(s string) {
	e, ok := DecodeEvent(s)
	if !ok {
		// Not test2json: the run.sh banners, a build error, a panic printed before
		// test2json started. Shown, archived and scanned as the text it is.
		fmt.Fprintln(w.term, s)
		w.render("%s\n", s)
		_, _ = w.scan.Write([]byte(s + "\n"))
		return
	}
	w.tr.observe(e)
	if e.Action == "output" {
		w.output(e)
		return
	}
	ts := e.Time
	if ts.IsZero() {
		ts = w.tr.now()
	}
	stamp := ts.UTC().Format("15:04:05.000")
	switch e.Action {
	case "pass", "fail", "skip":
		if e.Test != "" {
			w.render("%s %-4s %s (%.2fs)\n", stamp, strings.ToUpper(e.Action), e.Test, e.Elapsed)
		} else {
			w.render("%s %-4s %s (%.2fs)\n", stamp, strings.ToUpper(e.Action), e.Package, e.Elapsed)
		}
	default:
		if e.Test != "" {
			w.render("%s %-4s %s\n", stamp, strings.ToUpper(e.Action), e.Test)
		}
	}
	if e.Action == "fail" && e.Test != "" {
		if pe := w.pending[e.Test]; pe != nil {
			for _, l := range pe.lines {
				fmt.Fprintln(w.term, l)
			}
			if pe.dropped > 0 {
				fmt.Fprintf(w.term, "   … %d more output lines of %s in the archive\n", pe.dropped, e.Test)
			}
		}
	}
	if terminal(e) || e.Action == "pause" {
		delete(w.pending, e.Test)
	}
}

func (w *simStreamWriter) output(e Event) {
	w.render("%s", e.Output)
	if !strings.HasSuffix(e.Output, "\n") {
		w.render("\n")
	}
	_, _ = w.scan.Write([]byte(e.Output))
	text := strings.TrimRight(e.Output, "\n")
	if w.ceilingLine == "" && strings.Contains(text, "hard wall-time ceiling") {
		w.ceilingLine = text // the first mention is the ceiling's own message, not the dump header repeating it
	}
	if m := dumpWrittenRE.FindStringSubmatch(text); m != nil {
		w.dumpPaths = append(w.dumpPaths, m[1])
	}
	if w.openDumps == nil {
		w.openDumps = map[string]*simDumpState{}
	}
	if d := w.openDumps[e.Package]; d != nil && dumpEndRE.MatchString(text) {
		w.endDump(e.Package)
	}
	if w.openDumps[e.Package] == nil && strings.Contains(text, testlife.DumpHeaderPrefix) {
		w.openDumps[e.Package] = &simDumpState{}
	}
	switch d := w.openDumps[e.Package]; {
	case d != nil:
		// The dump is attributed by test2json to whichever test ran last, and is
		// thousands of lines: show its head, archive all of it.
		if d.shown < simDumpTermLines {
			d.shown++
			fmt.Fprintln(w.term, text)
		} else {
			d.hidden++
		}
	case e.Test == "":
		fmt.Fprintln(w.term, text)
	default:
		pe := w.pending[e.Test]
		if pe == nil {
			pe = &simPending{}
			w.pending[e.Test] = pe
		}
		if len(pe.lines) >= simPendingMaxLines || pe.bytes+len(text) > simPendingMaxBytes {
			pe.dropped++
			return
		}
		pe.lines = append(pe.lines, text)
		pe.bytes += len(text)
	}
}

func (w *simStreamWriter) ceiling() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ceilingLine
}

// dumps is the path of every goroutine dump the run wrote, in order (several
// test binaries can hit their ceiling in one run).
func (w *simStreamWriter) dumps() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.dumpPaths...)
}
