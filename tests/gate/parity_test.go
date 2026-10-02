package gate

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Ported from scripts/e2e/parity_summary_test.sh. The "no jq" case is dropped:
// it tested a bash dependency that the Go runner does not have.

const parityRegistry = `{"version":1,"tests":[
 {"name":"TestA","parity":"sim","sim":["TestSimA"]},
 {"name":"TestB","parity":"sim","sim":["TestSimB"]},
 {"name":"TestC","parity":"live-only","live_only_reason":"real-ci"},
 {"name":"TestD","parity":"gap"},
 {"name":"TestE","parity":"gap"},
 {"name":"TestF","parity":"gap"}
]}`

func TestPrintSimParitySummary(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir+"/registry.json", parityRegistry)
	mustWrite(t, dir+"/bad.json", "not json")

	t.Run("counts line", func(t *testing.T) {
		g, _, out, _ := testGate(t, "E2E_PARITY_REGISTRY="+dir+"/registry.json")
		g.PrintSimParitySummary()
		if got := strings.TrimSpace(out.String()); got != "sim parity: 2 covered, 1 live-only, 3 gap" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("prints when the pre-gate is skipped", func(t *testing.T) {
		g, _, out, _ := testGate(t, "E2E_SKIP_PREGATE=1", "E2E_PARITY_REGISTRY="+dir+"/registry.json")
		g.PrintSimParitySummary()
		if got := strings.TrimSpace(out.String()); got != "sim parity: 2 covered, 1 live-only, 3 gap" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("a missing registry degrades", func(t *testing.T) {
		g, _, out, _ := testGate(t, "E2E_PARITY_REGISTRY="+dir+"/nope.json")
		g.PrintSimParitySummary()
		if !strings.HasPrefix(out.String(), "sim parity: unavailable (registry not found") {
			t.Errorf("got %q", out)
		}
	})
	t.Run("a bad registry degrades", func(t *testing.T) {
		g, _, out, _ := testGate(t, "E2E_PARITY_REGISTRY="+dir+"/bad.json")
		g.PrintSimParitySummary()
		if !strings.HasPrefix(out.String(), "sim parity: unavailable (cannot parse") {
			t.Errorf("got %q", out)
		}
	})
	t.Run("the real registry yields a well-formed line", func(t *testing.T) {
		g, _, out, _ := testGate(t)
		g.Cfg.RepoRoot = "../.." // tests run in tests/gate
		if _, err := os.Stat(g.Cfg.RepoRoot + "/tests/e2e/registry/registry.json"); err != nil {
			t.Skip("registry not found relative to the test directory")
		}
		g.PrintSimParitySummary()
		line := strings.TrimSpace(out.String())
		var a, b, c int
		if n, err := fmtSscanf(line, &a, &b, &c); err != nil || n != 3 {
			t.Errorf("line %q is not 'sim parity: N covered, M live-only, K gap'", line)
		}
	})
}

func TestSimParityIsInformationalAndOutsideThePregate(t *testing.T) {
	// The summary is a Preflight in the default list, ahead of the pre-gate, and
	// returns no error whatever the registry looks like — it never gates.
	var found bool
	for _, p := range DefaultPreflights() {
		if p.Name == "sim-parity" {
			found = true
		}
	}
	if !found {
		t.Fatal("sim-parity missing from DefaultPreflights")
	}
	g, _, out, _ := testGate(t, "E2E_PARITY_REGISTRY=/nonexistent", "E2E_SKIP_PREGATE=1")
	for _, p := range DefaultPreflights() {
		if p.Name == "sim-parity" {
			if err := p.Run(context.Background(), g, &Plan{}); err != nil {
				t.Errorf("must never gate: %v", err)
			}
		}
	}
	if !strings.Contains(out.String(), "unavailable") {
		t.Errorf("out=%q", out)
	}
}
