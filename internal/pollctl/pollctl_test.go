package pollctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPaths(t *testing.T) {
	cp := ControlPath("/bed")
	if cp != filepath.Join("/bed", ".fabrik", "poll-control.json") {
		t.Fatalf("ControlPath = %q", cp)
	}
	if ap := AckPath(cp); ap != filepath.Join("/bed", ".fabrik", "poll-control.ack.json") {
		t.Fatalf("AckPath = %q", ap)
	}
	if got := Env("/bed"); got != EnvVar+"="+cp {
		t.Fatalf("Env = %q", got)
	}
}

func TestMissingAndTornFilesReadAsZero(t *testing.T) {
	cp := filepath.Join(t.TempDir(), "c.json")
	if r := ReadRequest(cp); r != (Request{}) {
		t.Fatalf("missing request = %+v", r)
	}
	if a := ReadAck(cp); a != (Ack{}) {
		t.Fatalf("missing ack = %+v", a)
	}
	if err := os.WriteFile(cp, []byte(`{"hold": tr`), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := ReadRequest(cp); r != (Request{}) {
		t.Fatalf("torn request = %+v", r)
	}
}

func TestRoundTrip(t *testing.T) {
	cp := ControlPath(t.TempDir()) // parent dir does not exist yet
	req := Request{Hold: true, HoldUntil: 99, HoldGen: 3, TriggerSeq: 7}
	if err := WriteRequest(cp, req); err != nil {
		t.Fatal(err)
	}
	if got := ReadRequest(cp); got != req {
		t.Fatalf("request = %+v, want %+v", got, req)
	}
	ack := Ack{Held: true, HoldGen: 3, DoneSeq: 7, Outcome: OutcomeRan, Detail: "x"}
	if err := WriteAck(cp, ack); err != nil {
		t.Fatal(err)
	}
	if got := ReadAck(cp); got != ack {
		t.Fatalf("ack = %+v, want %+v", got, ack)
	}
	// No temp files are left behind by the atomic rename.
	entries, err := os.ReadDir(filepath.Dir(cp))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestClear(t *testing.T) {
	cp := ControlPath(t.TempDir())
	if err := Clear(cp); err != nil {
		t.Fatalf("Clear on absent files: %v", err)
	}
	if err := WriteRequest(cp, Request{Hold: true}); err != nil {
		t.Fatal(err)
	}
	if err := WriteAck(cp, Ack{Held: true}); err != nil {
		t.Fatal(err)
	}
	if err := Clear(cp); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cp, AckPath(cp)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists (err=%v)", p, err)
		}
	}
}
