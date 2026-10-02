package gate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// R4 (#1976): every bed writes to the one per-SHA ledger, a pair covered on any
// bed counts, and every record says which bed produced it.
func TestTwoBedsShareOneLedger(t *testing.T) {
	l := testLedger(t)
	bedA, bedB := "/beds/a", "/beds/b"
	var names []string
	hashes := map[string]string{}
	for i := 0; i < 40; i++ {
		n := fmt.Sprintf("TestScenario%02d", i)
		names = append(names, n)
		hashes[n] = "h-" + n
	}
	record := func(cell Cell, bed string) {
		rec := newLegRecorder(l, cell, "inv-1", "head", hashes, nil, func(f string, a ...any) { t.Errorf("recorder warning: "+f, a...) })
		rec.bed = bed
		for _, n := range names {
			rec.Observe(Event{Action: "run", Test: n})
			rec.Observe(Event{Action: "pass", Test: n})
		}
	}
	// Both beds record concurrently — and onto the SAME leg file (pat/on runs on
	// bed B while a resumed pat/on cell could run on A) — to prove the appends
	// never interleave.
	var wg sync.WaitGroup
	for _, job := range []struct {
		cell Cell
		bed  string
	}{
		{Cell{Auth: "app", Train: "on"}, bedA},
		{Cell{Auth: "app", Train: "off"}, bedB},
		{Cell{Auth: "pat", Train: "on"}, bedA},
		{Cell{Auth: "pat", Train: "on"}, bedB},
	} {
		wg.Add(1)
		go func(c Cell, bed string) {
			defer wg.Done()
			record(c, bed)
		}(job.cell, job.bed)
	}
	wg.Wait()

	s := l.Load()
	if len(s.Warnings) != 0 || len(s.Corrupt) != 0 {
		t.Fatalf("concurrent beds corrupted the ledger: %v %v", s.Warnings, s.Corrupt)
	}
	for leg, bed := range map[string]string{"app/on": bedA, "app/off": bedB} {
		for _, n := range names {
			if !s.Covered(leg, n, hashes[n]) {
				t.Fatalf("%s %s not covered", leg, n)
			}
			if r, _ := s.Record(leg, n); r.Bed != bed {
				t.Errorf("%s %s attributed to %q, want %q", leg, n, r.Bed, bed)
			}
		}
	}
	// Every line of the shared pat/on file is whole JSON.
	f, err := os.Open(filepath.Join(l.outcomesDir(), "pat-on.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := 0
	for sc := bufio.NewScanner(f); sc.Scan(); lines++ {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.Bed == "" {
			t.Fatalf("line %d: %v %q", lines+1, err, sc.Text())
		}
	}
	if lines != 2*len(names) {
		t.Errorf("pat/on lines: %d", lines)
	}
}

func TestVoidIsScopedToItsBed(t *testing.T) {
	l := testLedger(t)
	for _, bed := range []string{"/beds/a", "/beds/b"} {
		if err := l.Append(Record{Test: "TestX" + bed, Leg: "app/off", Cell: "app-off", Invocation: "i1", Outcome: OutcomePass, Hash: "h", Bed: bed}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.VoidBed("app/off", "app-off", "i1", "/beds/b"); err != nil {
		t.Fatal(err)
	}
	s := l.Load()
	if !s.Covered("app/off", "TestX/beds/a", "h") {
		t.Error("voiding bed B's cell discarded bed A's record")
	}
	if s.Covered("app/off", "TestX/beds/b", "h") {
		t.Error("bed B's record survived its own void")
	}
	// An unscoped (legacy) void still discards every bed's.
	if err := l.Void("app/off", "app-off", "i1"); err != nil {
		t.Fatal(err)
	}
	if l.Load().Covered("app/off", "TestX/beds/a", "h") {
		t.Error("an unscoped void must discard every bed's records")
	}
}

func TestNoteBedConfigIsPerBed(t *testing.T) {
	l := testLedger(t)
	if d := l.NoteBedConfig("i1", "/beds/a", "ha"); len(d) != 0 {
		t.Errorf("first: %v", d)
	}
	if d := l.NoteBedConfig("i1", "/beds/b", "hb"); len(d) != 0 {
		t.Errorf("two beds differ legitimately (own boards and repos): %v", d)
	}
	if d := l.NoteBedConfig("i2", "/beds/b", "hb"); len(d) != 0 {
		t.Errorf("bed B unchanged: %v", d)
	}
	if d := l.NoteBedConfig("i3", "/beds/a", "ha2"); len(d) != 1 || d[0] != "ha" {
		t.Errorf("bed A's own change is drift: %v", d)
	}
	// A pre-#1976 entry keyed by invocation alone names no bed and is ignored.
	path := filepath.Join(l.Dir(), "bedconfig.json")
	data, _ := os.ReadFile(path)
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["legacy-inv"] = "hlegacy"
	if err := os.WriteFile(path, mustJSON(m), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := l.NoteBedConfig("i4", "/beds/b", "hb"); len(d) != 0 {
		t.Errorf("a legacy entry must be tolerated and ignored: %v", d)
	}
}

func TestArchiveRecordsItsBed(t *testing.T) {
	g, _, _, _ := twoBedGate(t)
	l := testLedger(t)
	cs := &covState{ledger: l, invocation: "inv-1"}
	for _, b := range g.beds() {
		cell := Cell{Auth: "pat", Train: "on"}
		if b.bed.Name == "B" {
			cell.Train = "off"
		}
		a := b.beginArchive(cell, cs)
		a.Finish()
		data, err := os.ReadFile(filepath.Join(a.dir, "bed.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got struct{ Name, Dir string }
		if err := json.Unmarshal(data, &got); err != nil || got.Name != b.bed.Name || got.Dir != b.bed.Dir {
			t.Errorf("bed %s's archive: %s %v", b.bed.Name, data, err)
		}
	}
	// Single bed: the directory alone.
	single, _, _, _ := testGate(t)
	a := single.beginArchive(defaultCell, &covState{ledger: l, invocation: "inv-2"})
	a.Finish()
	data, _ := os.ReadFile(filepath.Join(a.dir, "bed.json"))
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil || got["dir"] != single.Cfg.TestBed || len(got) != 1 {
		t.Errorf("single-bed archive: %s %v", data, err)
	}
}
