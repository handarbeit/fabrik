package sim

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/engine"
)

// Per-test log attribution (#2247 R5). Every sim Env's engine prints its
// plain-text log through logEvent, which without a sink writes bare
// "[poll] nothing to do" lines to the one process-wide stdout — with 16 parallel
// tests interleaved there is no telling whose line is whose, or when it was
// written. NewEnv installs a sink that prefixes each line with a wall-clock
// timestamp and the owning test's name, so the output can be separated and timed
// after the fact (the gate archives it as <head>.sim.log).
//
// Only logEvent lines are attributed. The few direct prints in the engine
// (claude.go, worktree.go) and the package-global gh.Logf stay anonymous: they
// have no Engine to route through.

// logOutMu serialises writes to the shared output so lines never interleave.
var logOutMu sync.Mutex

// newLogSink returns the sink for one test and the func that closes it. After
// close the sink drops lines: an engine goroutine can outlive its test, and
// writing then would at best be unattributable noise. Each line is written with
// one Write under a process-wide mutex, so lines never interleave mid-line.
func newLogSink(testName string, out io.Writer, now func() time.Time) (engine.LogSink, func()) {
	var mu sync.Mutex
	closed := false
	sink := func(issue int, tag, msg string) {
		mu.Lock()
		isClosed := closed
		mu.Unlock()
		if isClosed {
			return
		}
		ts := now().UTC().Format("2006-01-02T15:04:05.000Z")
		msg = strings.TrimRight(msg, "\n")
		var line string
		if issue == 0 {
			line = fmt.Sprintf("%s [%s] [%s] %s\n", ts, testName, tag, msg)
		} else {
			line = fmt.Sprintf("%s [%s] [#%d %s] %s\n", ts, testName, issue, tag, msg)
		}
		logOutMu.Lock()
		_, _ = io.WriteString(out, line)
		logOutMu.Unlock()
	}
	return sink, func() { mu.Lock(); closed = true; mu.Unlock() }
}

// installLogSink attributes eng's log lines to t, writing to out (default
// stdout, where `go test` and the gate pick them up).
func installLogSink(t interface {
	Name() string
	Cleanup(func())
}, eng *engine.Engine, out io.Writer) {
	if out == nil {
		out = os.Stdout
	}
	sink, closeSink := newLogSink(t.Name(), out, time.Now)
	eng.SetLogSinkForTest(sink)
	t.Cleanup(closeSink)
}
