package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := OpenLedger(t.TempDir(), strings.Repeat("a", 40), func() time.Time { return time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func rec(test, leg, cell, inv string, o Outcome, hash string) Record {
	return Record{Test: test, Leg: leg, Cell: cell, Invocation: inv, Outcome: o, Hash: hash}
}

func TestLedgerAppendAndCovered(t *testing.T) {
	l := testLedger(t)
	if err := l.Append(rec("TestA", "app/off", "app-off", "i1", OutcomePass, "h1")); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(rec("TestB", "app/off", "app-off", "i1", OutcomeFail, "h2")); err != nil {
		t.Fatal(err)
	}
	s := l.Load()
	if !s.Covered("app/off", "TestA", "h1") {
		t.Error("TestA should be covered")
	}
	if s.Covered("app/off", "TestA", "other") {
		t.Error("a changed hash must reopen the test (R3)")
	}
	if s.Covered("app/off", "TestB", "h2") {
		t.Error("a FAIL is never covered")
	}
	if s.Covered("pat/off", "TestA", "h1") {
		t.Error("coverage is per leg")
	}
}

func TestLedgerLatestWinsAndUnknownOutcome(t *testing.T) {
	l := testLedger(t)
	l.Append(rec("TestA", "pat/off", "pat-off", "i1", OutcomePass, "h"))
	l.Append(rec("TestA", "pat/off", "pat-off", "i2", OutcomeFail, "h"))
	if l.Load().Covered("pat/off", "TestA", "h") {
		t.Error("a later FAIL supersedes an earlier PASS")
	}
	l.Append(rec("TestA", "pat/off", "pat-off", "i3", OutcomePass, "h"))
	if !l.Load().Covered("pat/off", "TestA", "h") {
		t.Error("a later PASS restores coverage")
	}
	l.Append(rec("TestA", "pat/off", "pat-off", "i4", Outcome("MYSTERY"), "h"))
	if l.Load().Covered("pat/off", "TestA", "h") {
		t.Error("an unknown outcome value is never covered")
	}
	l.Append(rec("TestA", "pat/off", "pat-off", "i5", OutcomeInconclusive, "h"))
	if l.Load().Covered("pat/off", "TestA", "h") {
		t.Error("INCONCLUSIVE is never covered")
	}
}

func TestLedgerVoid(t *testing.T) {
	l := testLedger(t)
	l.Append(rec("TestA", "app/on", "app-on", "i1", OutcomePass, "h"))
	l.Append(rec("TestB", "app/on", "app-on", "i1", OutcomePass, "h"))
	l.Append(rec("TestC", "app/on", "app-on-isolated", "i1", OutcomePass, "h"))
	l.Append(rec("TestD", "app/on", "app-on", "i0", OutcomePass, "h"))
	if err := l.Void("app/on", "app-on", "i1"); err != nil {
		t.Fatal(err)
	}
	s := l.Load()
	if s.Covered("app/on", "TestA", "h") || s.Covered("app/on", "TestB", "h") {
		t.Error("void must discard the invocation-cell's records")
	}
	if !s.Covered("app/on", "TestC", "h") {
		t.Error("void is scoped to one cell: the isolated cell's record survives")
	}
	if !s.Covered("app/on", "TestD", "h") {
		t.Error("void is scoped to one invocation: an earlier invocation's PASS survives")
	}
}

func TestLedgerTornTailIsSkippedAndRepaired(t *testing.T) {
	l := testLedger(t)
	l.Append(rec("TestA", "app/off", "app-off", "i1", OutcomePass, "h"))
	path := filepath.Join(l.outcomesDir(), "app-off.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"v":1,"test":"TestTorn","leg":"app/off","outcome":"PA`) // killed mid-write
	f.Close()

	s := l.Load()
	if !s.Covered("app/off", "TestA", "h") {
		t.Error("records before the torn tail survive")
	}
	if _, ok := s.Record("app/off", "TestTorn"); ok {
		t.Error("a torn record must never be read")
	}
	if len(s.Warnings) == 0 || !strings.Contains(s.Warnings[0], "torn") {
		t.Errorf("torn tail must be reported: %v", s.Warnings)
	}

	// A later append must not glue onto the fragment.
	if err := l.Append(rec("TestB", "app/off", "app-off", "i2", OutcomePass, "h")); err != nil {
		t.Fatal(err)
	}
	s = l.Load()
	if !s.Covered("app/off", "TestB", "h") || !s.Covered("app/off", "TestA", "h") {
		t.Errorf("append after a torn tail lost a record: %+v", s.Latest)
	}
}

func TestLedgerFutureVersionFailsClosed(t *testing.T) {
	l := testLedger(t)
	l.Append(rec("TestA", "app/off", "app-off", "i1", OutcomePass, "h"))
	path := filepath.Join(l.outcomesDir(), "app-off.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"v":99,"test":"TestZ","leg":"app/off","outcome":"PASS","hash":"h"}` + "\n")
	f.Close()
	s := l.Load()
	if s.Covered("app/off", "TestA", "h") {
		t.Error("a file with an unknown version is corrupt: nothing on the leg is covered")
	}
	if _, bad := s.Corrupt["app/off"]; !bad {
		t.Error("the leg must be reported corrupt")
	}
	if got := s.Legs(); len(got) != 1 || got[0] != "app/off" {
		t.Errorf("Legs() = %v", got)
	}
}

func TestLedgerInvocationCountAndMeta(t *testing.T) {
	l := testLedger(t)
	if l.InvocationCount() != 0 {
		t.Fatal("fresh ledger has no invocations")
	}
	if err := l.NoteInvocation("i1", "head", false); err != nil {
		t.Fatal(err)
	}
	if err := l.NoteInvocation("i2", "head", true); err != nil {
		t.Fatal(err)
	}
	if l.InvocationCount() != 2 {
		t.Errorf("InvocationCount = %d, want 2", l.InvocationCount())
	}
	m, err := l.readMeta()
	if err != nil || m.SHA != l.SHA || m.Version != LedgerVersion {
		t.Errorf("meta = %+v, %v", m, err)
	}
}

func TestOpenLedgerRejectsEmptySHAAndUnwritableRoot(t *testing.T) {
	if _, err := OpenLedger(t.TempDir(), "", nil); err == nil {
		t.Error("empty SHA must be rejected")
	}
	file := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(file, nil, 0o644)
	if _, err := OpenLedger(filepath.Join(file, "sub"), "abc", nil); err == nil {
		t.Error("an unwritable root must be an error")
	}
}

func TestNilLedgerRecordsNothing(t *testing.T) {
	var l *Ledger
	if err := l.Append(Record{}); err != nil {
		t.Error(err)
	}
	if err := l.NoteInvocation("i", "h", false); err != nil {
		t.Error(err)
	}
}

func TestLegFileRoundTrip(t *testing.T) {
	if legFile("app/off") != "app-off" || legFromFile("app-off.jsonl") != "app/off" {
		t.Error("leg file naming")
	}
}
