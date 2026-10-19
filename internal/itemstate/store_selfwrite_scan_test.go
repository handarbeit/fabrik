package itemstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SC-005 (#1945, ADR 1945): no staleness decision compares the daemon's wall clock
// with a GitHub timestamp. The one place the baseline used to be set from
// time.Now() is the SelfWriteObserved apply; it may read the clock only behind the
// legacy non-vacuity seam.
func TestSelfWriteObservedApplyNeverReadsHostClock(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "\tcase SelfWriteObserved:")
	if start < 0 {
		t.Fatal("SelfWriteObserved case not found in store.go")
	}
	end := strings.Index(text[start+1:], "\n\tcase ")
	if end < 0 {
		t.Fatal("end of the SelfWriteObserved case not found")
	}
	block := text[start : start+1+end]

	inLegacy := false
	for _, line := range strings.Split(block, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "if s.legacyLocalClockSelfWrite.Load()"):
			inLegacy = true
		case inLegacy && trim == "}":
			inLegacy = false
		case strings.Contains(trim, "time.Now") && !inLegacy:
			t.Errorf("SelfWriteObserved apply reads the host clock outside the legacy seam: %q", trim)
		}
	}
}

// Every engine self-write goes through the observeSelfWrite helpers, which read a
// GitHub stamp and never the host clock; no engine file applies SelfWriteObserved
// directly with a locally computed time.
func TestEngineSelfWriteSitesNeverUseHostClock(t *testing.T) {
	files, err := filepath.Glob("../../engine/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("globbing engine sources: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, "SelfWriteObserved{") {
			continue
		}
		if filepath.Base(f) != "mutate.go" {
			t.Errorf("%s applies SelfWriteObserved directly; use observeSelfWrite/observeStatusMoveWrite (engine/mutate.go)", f)
			continue
		}
		for _, name := range []string{"func (e *Engine) observeSelfWrite(", "func (e *Engine) observeStatusMoveWrite("} {
			i := strings.Index(text, name)
			if i < 0 {
				t.Fatalf("%s not found in mutate.go", name)
			}
			j := strings.Index(text[i:], "\n}\n")
			if j < 0 {
				t.Fatalf("end of %s not found", name)
			}
			if strings.Contains(text[i:i+j], "time.Now") {
				t.Errorf("%s reads the host clock", name)
			}
		}
	}
}
