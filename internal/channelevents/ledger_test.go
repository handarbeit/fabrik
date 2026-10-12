package channelevents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLedgerRoundTripAndTakePriorOnce(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := OpenLedger(dir, clock, nil)
	l.Observe("r#1", Observation{Labels: []string{"b", "a", "fabrik:locked:me", "stage:X:in_progress"}, LabelsKnown: true, Status: "Plan"})
	l.Observe("r#2", Observation{Status: "Specify"}) // labels not known yet
	l.SetSettle("r#1", SettleRecord{Episode: 2, HeadSHA: "abc", Autonomy: "cruise"})
	l.Close()

	l2 := OpenLedger(dir, clock, nil)
	got, ok := l2.TakePrior("r#1")
	if !ok || !got.LabelsKnown || !reflect.DeepEqual(got.Labels, []string{"a", "b"}) || got.Status != "Plan" {
		t.Fatalf("prior r#1 = %+v ok=%v (churn labels must be dropped, labels sorted)", got, ok)
	}
	if got.Settle == nil || got.Settle.Episode != 2 || got.Settle.HeadSHA != "abc" {
		t.Fatalf("settle record lost: %+v", got.Settle)
	}
	if _, ok := l2.TakePrior("r#1"); ok {
		t.Fatal("TakePrior must consume the record")
	}
	two, ok := l2.TakePrior("r#2")
	if !ok || two.LabelsKnown || two.Status != "Specify" {
		t.Fatalf("r#2 = %+v ok=%v", two, ok)
	}
	if _, ok := l2.TakePrior("r#3"); ok {
		t.Fatal("an item with no persisted state is a baseline")
	}
}

func TestLedgerCarriesForwardUnobservedAndCapsAge(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	cur := now
	clock := func() time.Time { return cur }
	l := OpenLedger(dir, clock, nil)
	l.Observe("r#old", Observation{Status: "Plan"})
	l.Observe("r#keep", Observation{Status: "Plan"})
	l.Close()

	// A new run that only observes r#keep still carries r#old forward.
	l2 := OpenLedger(dir, clock, nil)
	l2.Observe("r#keep", Observation{Status: "Review"})
	l2.Close()
	l3 := OpenLedger(dir, clock, nil)
	if r, ok := l3.PeekPrior("r#old"); !ok || r.Status != "Plan" {
		t.Fatalf("unobserved record was not carried forward: %+v ok=%v", r, ok)
	}
	if r, _ := l3.PeekPrior("r#keep"); r.Status != "Review" {
		t.Fatalf("observed record not updated: %+v", r)
	}
	l3.Close()

	// Past the age cap an unobserved record is dropped.
	cur = now.Add(ledgerMaxAge + time.Hour)
	l4 := OpenLedger(dir, clock, nil)
	if _, ok := l4.PeekPrior("r#old"); ok {
		t.Fatal("record older than the age cap must be dropped")
	}
}

func TestLedgerCorruptAndVersionMismatchAreBaselines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "items.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	l := OpenLedger(dir, nil, func(f string, a ...any) { logs = append(logs, f) })
	if _, ok := l.TakePrior("r#1"); ok {
		t.Fatal("corrupt ledger must yield no prior state")
	}
	if len(logs) == 0 {
		t.Fatal("corruption must be logged")
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("corrupt file not quarantined: %v", matches)
	}

	data, _ := json.Marshal(ledgerFile{Version: 99, Items: map[string]ItemRecord{"r#1": {Status: "Plan", Seen: time.Now()}}})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	l2 := OpenLedger(dir, nil, nil)
	if _, ok := l2.TakePrior("r#1"); ok {
		t.Fatal("version mismatch must yield no prior state")
	}
}

func TestLedgerCoalescesAndFlushesOnClose(t *testing.T) {
	dir := t.TempDir()
	l := OpenLedger(dir, nil, nil)
	for i := 0; i < 20; i++ {
		l.Observe("r#1", Observation{Status: "Plan", Labels: []string{"x"}, LabelsKnown: true})
	}
	if l.Writes() != 0 {
		t.Fatalf("Observe must not write synchronously, wrote %d times", l.Writes())
	}
	l.Flush()
	if l.Writes() != 1 {
		t.Fatalf("one coalesced write expected, got %d", l.Writes())
	}
	l.Flush()
	if l.Writes() != 1 {
		t.Fatal("a clean flush must not rewrite")
	}
	l.Observe("r#1", Observation{Status: "Review"})
	l.Close()
	l2 := OpenLedger(dir, nil, nil)
	if r, _ := l2.PeekPrior("r#1"); r.Status != "Review" {
		t.Fatalf("Close must flush: %+v", r)
	}
}

func TestLedgerMarkRevalidatedAndSetPriorStatus(t *testing.T) {
	dir := t.TempDir()
	l := OpenLedger(dir, nil, nil)
	l.SetSettle("r#1", SettleRecord{Episode: 1, HeadSHA: "a"})
	l.MarkRevalidated("r#1")
	l.MarkRevalidated("r#none")
	if s, ok := l.Settle("r#1"); !ok || !s.Revalidated {
		t.Fatalf("settle = %+v", s)
	}
	l.Observe("r#1", Observation{Status: "Plan"})
	l.Close()
	l2 := OpenLedger(dir, nil, nil)
	l2.SetPriorStatus("r#1", "Done")
	if r, _ := l2.TakePrior("r#1"); r.Status != "Done" {
		t.Fatalf("SetPriorStatus: %+v", r)
	}
}
