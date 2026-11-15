package engine

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/tui"
)

// These tests capture process-wide stdout (captureStdout), so none of them may
// run in parallel.

// logBoth runs fn against an engine with a file-backed log and returns what it
// wrote to stdout and to the log file.
func logBoth(t *testing.T, off bool, fn func(e *Engine)) (stdout, file string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "fabrik-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	e := &Engine{logFile: f}
	e.SetLogLineEndingNormalisationDisabledForTest(off)
	stdout = captureStdout(func() { fn(e) })
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return stdout, string(b)
}

var logTimestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ `)

// stripTS removes the RFC3339 timestamp prefix from each log-file line.
func stripTS(file string) string {
	lines := strings.SplitAfter(file, "\n")
	for i, l := range lines {
		lines[i] = logTimestamp.ReplaceAllString(l, "")
	}
	return strings.Join(lines, "")
}

func TestLogEvent_ExactlyOneLineEnding(t *testing.T) {
	cases := []struct {
		name   string
		log    func(e *Engine)
		stdout string
		file   string
	}{
		{"no newline", func(e *Engine) { e.logf(7, "tag", "hello") }, "[#7 tag] hello\n", "[#7 tag] hello\n"},
		{"one newline", func(e *Engine) { e.logf(7, "tag", "hello\n") }, "[#7 tag] hello\n", "[#7 tag] hello\n"},
		{"empty", func(e *Engine) { e.logf(0, "poll", "") }, "[poll] \n", "[poll] \n"},
		{"internal only", func(e *Engine) { e.logf(0, "poll", "a\nb") }, "[poll] a\nb\n", "[poll] a\nb\n"},
		{"repo", func(e *Engine) { e.logfRepo("o/r", "train", "x %d", 1) }, "[train] x 1\n", "[train] x 1\n"},
		{"two lines run together", func(e *Engine) {
			e.logf(0, "a", "first")
			e.logf(0, "b", "second")
		}, "[a] first\n[b] second\n", "[a] first\n[b] second\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stdout, file := logBoth(t, false, c.log)
			if stdout != c.stdout {
				t.Errorf("stdout = %q, want %q", stdout, c.stdout)
			}
			if got := stripTS(file); got != c.file {
				t.Errorf("file = %q, want %q", got, c.file)
			}
		})
	}
}

// The guard must fail when the fix is neutralised (FR-008).
func TestLogEvent_NeutralisedFixRunsLinesTogether(t *testing.T) {
	stdout, file := logBoth(t, true, func(e *Engine) {
		e.logf(0, "a", "first")
		e.logf(0, "b", "second")
	})
	if stdout != "[a] first[b] second" {
		t.Errorf("neutralised stdout = %q; the seam no longer disables the fix", stdout)
	}
	if strings.Count(file, "\n") != 0 {
		t.Errorf("neutralised file = %q; expected no line endings", file)
	}
}

func TestLogEvent_ThrottledHelpersTerminateLines(t *testing.T) {
	stdout, file := logBoth(t, false, func(e *Engine) {
		e.logfThrottled("k", 3, "t", "msg")
		e.logfThrottled("k", 3, "t", "msg") // suppressed repeat writes nothing
		e.logfThrottledByInterval("k2", 0, "t2", "other")
		e.logfThrottledByInterval("k2", 0, "t2", "other")
	})
	want := "[#3 t] msg\n[t2] other\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if got := stripTS(file); got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

// FR-006: the sink and TUI payloads are the message exactly as formatted.
func TestLogEvent_SinkAndTUIPayloadUnchanged(t *testing.T) {
	e := &Engine{}
	var got []string
	e.SetLogSinkForTest(func(_ int, _ string, msg string) { got = append(got, msg) })
	e.logf(1, "t", "no newline")
	e.logf(1, "t", "newline\n")
	if len(got) != 2 || got[0] != "no newline" || got[1] != "newline\n" {
		t.Fatalf("sink got %q", got)
	}

	ch := make(chan tui.Event, 4)
	e2 := &Engine{events: ch}
	e2.logf(1, "t", "no newline")
	ev, ok := (<-ch).(tui.LogEvent)
	if !ok || ev.Message != "no newline" {
		t.Fatalf("tui event = %#v", ev)
	}
}

// Acceptance: the real forge-hold engage and lift lines each stand alone in the
// log file (they were the lines reported running into the next one).
func TestForgeHold_EngageAndLiftLinesStandAlone(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "fabrik-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	e, clk := newForgeTestEngine(t)
	e.logFile = f
	e.SetLogSinkForTest(func(int, string, string) {})
	captureStdout(func() {
		for i := 0; i < 20; i++ {
			e.ObserveForgeResult(errTimeout)
		}
		clk.Advance(forgeHoldWindow)
		e.ObserveForgeResult(errTimeout)
		e.ObserveForgeResult(nil)
	})
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	var engage, lift bool
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if n := strings.Count(line, "[forge-hold]"); n != 1 || !logTimestamp.MatchString(line) {
			t.Errorf("line must carry exactly one timestamped [forge-hold] prefix: %q", line)
		}
		engage = engage || strings.Contains(line, "holding all new model work")
		lift = lift || strings.Contains(line, "resuming new model work")
	}
	if !engage || !lift {
		t.Fatalf("engage=%v lift=%v in log:\n%s", engage, lift, b)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Errorf("log does not end with a line ending: %q", b)
	}
}
