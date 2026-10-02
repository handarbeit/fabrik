package gate

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

var fixtureLive = map[string]bool{"TestAlpha": true, "TestBravo": true, "TestCharlie": true, "TestDelta": true, "TestEcho": true}

func fixtureHashes() map[string]string {
	h := map[string]string{}
	for n := range fixtureLive {
		h[n] = "hash-" + n
	}
	return h
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// recordFixture streams a recorded `go test -json` fixture (optionally cut after
// n bytes, as a killed leg would be) through a real suiteWriter into the ledger.
func recordFixture(t *testing.T, l *Ledger, cell Cell, inv, fixture string, cut int) *legRecorder {
	t.Helper()
	rec := newLegRecorder(l, cell, inv, "head", fixtureHashes(), fixtureLive, func(f string, a ...any) { t.Errorf("recorder warning: "+f, a...) })
	w := newSuiteWriter(io.Discard, io.Discard, time.Now)
	w.sink = rec.Observe
	data := fixtureBytes(t, fixture)
	if cut >= 0 && cut < len(data) {
		data = data[:cut]
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	return rec
}

var appOff = Cell{Auth: "app", Train: "off", Parallel: "4"}

func TestRecorderRecordsTerminalOutcomesOnly(t *testing.T) {
	l := testLedger(t)
	rec := recordFixture(t, l, appOff, "i1", "partial-run-1.json", -1)
	if n, f := rec.Counts(); n != 4 || f != 0 {
		t.Fatalf("recorded %d (failed %d), want 4 — Echo never reached a terminal event", n, f)
	}
	s := l.Load()
	for test, want := range map[string]Outcome{"TestAlpha": OutcomePass, "TestBravo": OutcomePass, "TestCharlie": OutcomeFail, "TestDelta": OutcomeSkip} {
		r, ok := s.Record("app/off", test)
		if !ok || r.Outcome != want {
			t.Errorf("%s = %+v (found %v), want %s", test, r, ok, want)
		}
	}
	if _, ok := s.Record("app/off", "TestEcho"); ok {
		t.Error("an in-flight test must be unrecorded")
	}
	if _, ok := s.Record("app/off", "TestBravo/sub"); ok {
		t.Error("subtests roll up into their parent and are never entries")
	}
	if !s.Covered("app/off", "TestAlpha", "hash-TestAlpha") {
		t.Error("a recorded PASS against the current hash is covered")
	}
}

func TestRecorderCapturesSkipMessageAndCitedIssue(t *testing.T) {
	l := testLedger(t)
	recordFixture(t, l, appOff, "i1", "partial-run-1.json", -1)
	r, _ := l.Load().Record("app/off", "TestDelta")
	if r.SkipMsg != "blocked on #916" {
		t.Errorf("SkipMsg = %q (only the last log line before the skip is the t.Skip message)", r.SkipMsg)
	}
	if len(r.Issues) != 1 || r.Issues[0] != 916 {
		t.Errorf("Issues = %v, want [916]", r.Issues)
	}
}

// The acceptance case: a leg killed mid-stream keeps what had already finished.
func TestRecorderKilledMidStreamKeepsCompletedTests(t *testing.T) {
	data := fixtureBytes(t, "partial-run-1.json")
	// Cut right after TestAlpha's terminal event, mid-way through the stream.
	cut := bytes.Index(data, []byte(`"Test":"TestAlpha","Elapsed":10.5}`))
	if cut < 0 {
		t.Fatal("fixture drift")
	}
	cut += len(`"Test":"TestAlpha","Elapsed":10.5}`) + 1
	l := testLedger(t)
	recordFixture(t, l, appOff, "i1", "partial-run-1.json", cut)
	s := l.Load()
	if !s.Covered("app/off", "TestAlpha", "hash-TestAlpha") {
		t.Error("the completed test must be recorded as PASS")
	}
	for _, in := range []string{"TestBravo", "TestCharlie", "TestDelta", "TestEcho"} {
		if _, ok := s.Record("app/off", in); ok {
			t.Errorf("%s was in flight at the kill and must be unrecorded", in)
		}
	}
}

func TestRecorderIgnoresNonLiveTests(t *testing.T) {
	l := testLedger(t)
	rec := newLegRecorder(l, appOff, "i1", "h", fixtureHashes(), map[string]bool{"TestAlpha": true}, func(string, ...any) {})
	w := newSuiteWriter(io.Discard, io.Discard, time.Now)
	w.sink = rec.Observe
	w.Write(fixtureBytes(t, "partial-run-1.json"))
	w.Flush()
	if n, _ := rec.Counts(); n != 1 {
		t.Errorf("only live tests are ledger entries; recorded %d", n)
	}
}

func TestRecorderAppendFailureIsReportedAndNotCovered(t *testing.T) {
	l := testLedger(t)
	if err := os.RemoveAll(l.outcomesDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.outcomesDir(), nil, 0o644); err != nil { // a file where the dir should be
		t.Fatal(err)
	}
	var warned []string
	rec := newLegRecorder(l, appOff, "i1", "h", fixtureHashes(), fixtureLive, func(f string, a ...any) { warned = append(warned, f) })
	w := newSuiteWriter(io.Discard, io.Discard, time.Now)
	w.sink = rec.Observe
	w.Write(fixtureBytes(t, "partial-run-1.json"))
	w.Flush()
	if n, f := rec.Counts(); n != 0 || f != 4 || len(warned) != 4 {
		t.Errorf("recorded %d, failed %d, warned %d — failures must be loud and never count", n, f, len(warned))
	}
}

func TestSkipMessageAndCitedIssues(t *testing.T) {
	cases := []struct {
		out    string
		msg    string
		issues []int
	}{
		{"=== RUN   T\n    a_test.go:9: blocked on #916\n--- SKIP: T (0.00s)\n", "blocked on #916", []int{916}},
		{"    a_test.go:9: first\n    a_test.go:10: second, see #5 and #5 and #7\n", "second, see #5 and #5 and #7", []int{5, 7}},
		{"    a_test.go:9: wrapped message\n        continued on #44\n--- SKIP: T\n", "wrapped message continued on #44", []int{44}},
		{"--- SKIP: T (0.00s)\n", "", nil},
		{"    a_test.go:9: only a section reference README.md#17-foo\n", "only a section reference README.md#17-foo", nil},
		{"    a_test.go:9: (see #3) and x/y#4\n", "(see #3) and x/y#4", []int{3}},
	}
	for _, c := range cases {
		if got := skipMessage(c.out); got != c.msg {
			t.Errorf("skipMessage(%q) = %q, want %q", c.out, got, c.msg)
		}
		got := citedIssues(c.msg)
		if len(got) != len(c.issues) {
			t.Errorf("citedIssues(%q) = %v, want %v", c.msg, got, c.issues)
			continue
		}
		for i := range got {
			if got[i] != c.issues[i] {
				t.Errorf("citedIssues(%q) = %v, want %v", c.msg, got, c.issues)
			}
		}
	}
	if !strings.Contains(cellDirName(Cell{Auth: "app", Train: "on", Isolated: true}), "isolated") {
		t.Error("isolated cells need their own name")
	}
}
