package engine

import (
	"strings"
	"sync"
	"testing"
)

func TestLogSink_RoutesInsteadOfStdout(t *testing.T) {
	e := &Engine{}
	var mu sync.Mutex
	var got []string
	e.SetLogSinkForTest(func(n int, tag, msg string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, tag+"|"+strings.TrimSpace(msg))
		if n != 7 && n != 0 {
			t.Errorf("issue = %d", n)
		}
	})
	out := captureStdout(func() {
		e.logf(7, "tag", "hello %d\n", 1)
		e.logf(0, "poll", "nothing to do\n")
	})
	if out != "" {
		t.Fatalf("sink set but stdout got %q", out)
	}
	if len(got) != 2 || got[0] != "tag|hello 1" || got[1] != "poll|nothing to do" {
		t.Fatalf("sink got %v", got)
	}
}

// With no sink (and after clearing one) the output is byte-identical to before.
func TestLogSink_NilKeepsStdoutFormat(t *testing.T) {
	e := &Engine{}
	e.SetLogSinkForTest(func(int, string, string) {})
	e.SetLogSinkForTest(nil)
	out := captureStdout(func() {
		e.logf(7, "tag", "hello\n")
		e.logf(0, "poll", "x\n")
	})
	if out != "[#7 tag] hello\n[poll] x\n" {
		t.Fatalf("stdout = %q", out)
	}
}
