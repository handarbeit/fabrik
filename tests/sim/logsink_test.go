package sim

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// safeBuf is a bytes.Buffer readable while sinks write to it (sinks write under
// logOutMu, so reads take it too).
type safeBuf struct{ b bytes.Buffer }

func (s *safeBuf) Write(p []byte) (int, error) { return s.b.Write(p) }
func (s *safeBuf) String() string {
	logOutMu.Lock()
	defer logOutMu.Unlock()
	return s.b.String()
}
func (s *safeBuf) Reset() { logOutMu.Lock(); s.b.Reset(); logOutMu.Unlock() }

// R5: two tests' lines are separable by name and carry a timestamp; a closed
// sink drops lines while the other keeps writing.
func TestLogSink_AttributesAndTimestampsLines(t *testing.T) {
	t.Parallel()
	var buf safeBuf
	at := time.Date(2026, 10, 14, 16, 11, 2, 123e6, time.UTC)
	a, closeA := newLogSink("TestAlpha", &buf, func() time.Time { return at })
	b, closeB := newLogSink("TestBeta", &buf, func() time.Time { return at })
	a(7, "stage", "hello\n")
	b(0, "poll", "nothing to do\n")

	want := "2026-10-14T16:11:02.123Z [TestAlpha] [#7 stage] hello\n" +
		"2026-10-14T16:11:02.123Z [TestBeta] [poll] nothing to do\n"
	if got := buf.String(); got != want {
		t.Fatalf("lines = %q, want %q", got, want)
	}

	closeA()
	buf.Reset()
	a(7, "stage", "late\n")
	b(0, "poll", "still open\n")
	out := buf.String()
	if strings.Contains(out, "[TestAlpha]") {
		t.Fatalf("closed sink still wrote: %q", out)
	}
	if !strings.Contains(out, "[TestBeta] [poll] still open") {
		t.Fatalf("open sink lost its line: %q", out)
	}
	closeB()
}

// An Env's engine output reaches the attributed sink, not bare stdout.
func TestLogSink_EnvEngineLinesCarryTheTestName(t *testing.T) {
	t.Parallel()
	var buf safeBuf
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), LogOut: &buf})
	if err := env.Engine.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "[TestLogSink_EnvEngineLinesCarryTheTestName] [poll]") {
		t.Fatalf("engine poll lines not attributed to the test: %q", got)
	}
	if line := strings.SplitN(got, "\n", 2)[0]; len(line) < 24 || line[4] != '-' || line[10] != 'T' {
		t.Fatalf("line lacks a timestamp: %q", line)
	}
}
